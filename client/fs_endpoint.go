package client

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"

	metrics "github.com/armon/go-metrics"
	"github.com/hashicorp/nomad/acl"
	"github.com/hashicorp/nomad/client/allocdir"
	sframer "github.com/hashicorp/nomad/client/lib/streamframer"
	cstructs "github.com/hashicorp/nomad/client/structs"
	"github.com/hashicorp/nomad/helper"
	"github.com/hashicorp/nomad/nomad/structs"
	"github.com/hpcloud/tail/watch"
	"github.com/ugorji/go/codec"
)

var (
	allocIDNotPresentErr = fmt.Errorf("must provide a valid alloc id")
	pathNotPresentErr    = fmt.Errorf("must provide a file path")
	taskNotPresentErr    = fmt.Errorf("must provide task name")
	logTypeNotPresentErr = fmt.Errorf("must provide log type (stdout/stderr)")
	invalidOrigin        = fmt.Errorf("origin must be start or end")
)

const (
	// streamFramesBuffer is the number of stream frames that will be buffered
	// before back pressure is applied on the stream framer.
	streamFramesBuffer = 32

	// streamFrameSize is the maximum number of bytes to send in a single frame
	streamFrameSize = 64 * 1024

	// streamHeartbeatRate is the rate at which a heartbeat will occur to detect
	// a closed connection without sending any additional data
	streamHeartbeatRate = 1 * time.Second

	// streamBatchWindow is the window in which file content is batched before
	// being flushed if the frame size has not been hit.
	streamBatchWindow = 200 * time.Millisecond

	// nextLogCheckRate is the rate at which we check for a log entry greater
	// than what we are watching for. This is to handle the case in which logs
	// rotate faster than we can detect and we have to rely on a normal
	// directory listing.
	nextLogCheckRate = 100 * time.Millisecond

	// deleteEvent and truncateEvent are the file events that can be sent in a
	// StreamFrame
	deleteEvent   = "file deleted"
	truncateEvent = "file truncated"

	// OriginStart and OriginEnd are the available parameters for the origin
	// argument when streaming a file. They respectively offset from the start
	// and end of a file.
	OriginStart = "start"
	OriginEnd   = "end"
)

// FileSystem endpoint is used for accessing the logs and filesystem of
// allocations.
type FileSystem struct {
	c *Client
}

func NewFileSystemEndpoint(c *Client) *FileSystem {
	f := &FileSystem{c}
	f.c.streamingRpcs.Register("FileSystem.Logs", f.logs)
	f.c.streamingRpcs.Register("FileSystem.Stream", f.stream)
	return f
}

// handleStreamResultError is a helper for sending an error with a potential
// error code. The transmission of the error is ignored if the error has been
// generated by the closing of the underlying transport.
func (f *FileSystem) handleStreamResultError(err error, code *int64, encoder *codec.Encoder) {
	// Nothing to do as the conn is closed
	if err == io.EOF || strings.Contains(err.Error(), "closed") {
		return
	}

	encoder.Encode(&cstructs.StreamErrWrapper{
		Error: cstructs.NewRpcError(err, code),
	})
}

// List is used to list the contents of an allocation's directory.
func (f *FileSystem) List(args *cstructs.FsListRequest, reply *cstructs.FsListResponse) error {
	defer metrics.MeasureSince([]string{"client", "file_system", "list"}, time.Now())

	// Check read permissions
	if aclObj, err := f.c.ResolveToken(args.QueryOptions.AuthToken); err != nil {
		return err
	} else if aclObj != nil && !aclObj.AllowNsOp(args.Namespace, acl.NamespaceCapabilityReadFS) {
		return structs.ErrPermissionDenied
	}

	fs, err := f.c.GetAllocFS(args.AllocID)
	if err != nil {
		return err
	}
	files, err := fs.List(args.Path)
	if err != nil {
		return err
	}

	reply.Files = files
	return nil
}

// Stat is used to stat a file in the allocation's directory.
func (f *FileSystem) Stat(args *cstructs.FsStatRequest, reply *cstructs.FsStatResponse) error {
	defer metrics.MeasureSince([]string{"client", "file_system", "stat"}, time.Now())

	// Check read permissions
	if aclObj, err := f.c.ResolveToken(args.QueryOptions.AuthToken); err != nil {
		return err
	} else if aclObj != nil && !aclObj.AllowNsOp(args.Namespace, acl.NamespaceCapabilityReadFS) {
		return structs.ErrPermissionDenied
	}

	fs, err := f.c.GetAllocFS(args.AllocID)
	if err != nil {
		return err
	}
	info, err := fs.Stat(args.Path)
	if err != nil {
		return err
	}

	reply.Info = info
	return nil
}

// stream is is used to stream the contents of file in an allocation's
// directory.
func (f *FileSystem) stream(conn io.ReadWriteCloser) {
	defer metrics.MeasureSince([]string{"client", "file_system", "stream"}, time.Now())
	defer conn.Close()

	// Decode the arguments
	var req cstructs.FsStreamRequest
	decoder := codec.NewDecoder(conn, structs.MsgpackHandle)
	encoder := codec.NewEncoder(conn, structs.MsgpackHandle)

	if err := decoder.Decode(&req); err != nil {
		f.handleStreamResultError(err, helper.Int64ToPtr(500), encoder)
		return
	}

	// Check read permissions
	if aclObj, err := f.c.ResolveToken(req.QueryOptions.AuthToken); err != nil {
		f.handleStreamResultError(err, nil, encoder)
		return
	} else if aclObj != nil && !aclObj.AllowNsOp(req.Namespace, acl.NamespaceCapabilityReadFS) {
		f.handleStreamResultError(structs.ErrPermissionDenied, nil, encoder)
		return
	}

	// Validate the arguments
	if req.AllocID == "" {
		f.handleStreamResultError(allocIDNotPresentErr, helper.Int64ToPtr(400), encoder)
		return
	}
	if req.Path == "" {
		f.handleStreamResultError(pathNotPresentErr, helper.Int64ToPtr(400), encoder)
		return
	}
	switch req.Origin {
	case "start", "end":
	case "":
		req.Origin = "start"
	default:
		f.handleStreamResultError(invalidOrigin, helper.Int64ToPtr(400), encoder)
		return
	}

	fs, err := f.c.GetAllocFS(req.AllocID)
	if err != nil {
		code := helper.Int64ToPtr(500)
		if structs.IsErrUnknownAllocation(err) {
			code = helper.Int64ToPtr(404)
		}

		f.handleStreamResultError(err, code, encoder)
		return
	}

	// Calculate the offset
	fileInfo, err := fs.Stat(req.Path)
	if err != nil {
		f.handleStreamResultError(err, helper.Int64ToPtr(400), encoder)
		return
	}
	if fileInfo.IsDir {
		f.handleStreamResultError(
			fmt.Errorf("file %q is a directory", req.Path),
			helper.Int64ToPtr(400), encoder)
		return
	}

	// If offsetting from the end subtract from the size
	if req.Origin == "end" {
		req.Offset = fileInfo.Size - req.Offset
		if req.Offset < 0 {
			req.Offset = 0
		}
	}

	frames := make(chan *sframer.StreamFrame, streamFramesBuffer)
	errCh := make(chan error)
	var buf bytes.Buffer
	frameCodec := codec.NewEncoder(&buf, structs.JsonHandle)

	// Create the framer
	framer := sframer.NewStreamFramer(frames, streamHeartbeatRate, streamBatchWindow, streamFrameSize)
	framer.Run()
	defer framer.Destroy()

	// If we aren't following end as soon as we hit EOF
	var eofCancelCh chan error
	if !req.Follow {
		eofCancelCh = make(chan error)
		close(eofCancelCh)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Start streaming
	go func() {
		if err := f.streamFile(ctx, req.Offset, req.Path, req.Limit, fs, framer, eofCancelCh); err != nil {
			select {
			case errCh <- err:
			case <-ctx.Done():
			}
		}

		framer.Destroy()
	}()

	// Create a goroutine to detect the remote side closing
	go func() {
		for {
			if _, err := conn.Read(nil); err != nil {
				if err == io.EOF {
					cancel()
					return
				}
				select {
				case errCh <- err:
				case <-ctx.Done():
					return
				}
			}
		}
	}()

	var streamErr error
OUTER:
	for {
		select {
		case streamErr = <-errCh:
			break OUTER
		case frame, ok := <-frames:
			if !ok {
				break OUTER
			}

			var resp cstructs.StreamErrWrapper
			if req.PlainText {
				resp.Payload = frame.Data
			} else {
				if err = frameCodec.Encode(frame); err != nil {
					streamErr = err
					break OUTER
				}

				resp.Payload = buf.Bytes()
				buf.Reset()
			}

			if err := encoder.Encode(resp); err != nil {
				streamErr = err
				break OUTER
			}
			encoder.Reset(conn)
		case <-ctx.Done():
			break OUTER
		}
	}

	if streamErr != nil {
		f.handleStreamResultError(streamErr, helper.Int64ToPtr(500), encoder)
		return
	}
}

// logs is is used to stream a task's logs.
func (f *FileSystem) logs(conn io.ReadWriteCloser) {
	defer metrics.MeasureSince([]string{"client", "file_system", "logs"}, time.Now())
	defer conn.Close()

	// Decode the arguments
	var req cstructs.FsLogsRequest
	decoder := codec.NewDecoder(conn, structs.MsgpackHandle)
	encoder := codec.NewEncoder(conn, structs.MsgpackHandle)

	if err := decoder.Decode(&req); err != nil {
		f.handleStreamResultError(err, helper.Int64ToPtr(500), encoder)
		return
	}

	// Check read permissions
	if aclObj, err := f.c.ResolveToken(req.QueryOptions.AuthToken); err != nil {
		f.handleStreamResultError(err, nil, encoder)
		return
	} else if aclObj != nil {
		readfs := aclObj.AllowNsOp(req.QueryOptions.Namespace, acl.NamespaceCapabilityReadFS)
		logs := aclObj.AllowNsOp(req.QueryOptions.Namespace, acl.NamespaceCapabilityReadLogs)
		if !readfs && !logs {
			f.handleStreamResultError(structs.ErrPermissionDenied, nil, encoder)
			return
		}
	}

	// Validate the arguments
	if req.AllocID == "" {
		f.handleStreamResultError(allocIDNotPresentErr, helper.Int64ToPtr(400), encoder)
		return
	}
	if req.Task == "" {
		f.handleStreamResultError(taskNotPresentErr, helper.Int64ToPtr(400), encoder)
		return
	}
	switch req.LogType {
	case "stdout", "stderr":
	default:
		f.handleStreamResultError(logTypeNotPresentErr, helper.Int64ToPtr(400), encoder)
		return
	}
	switch req.Origin {
	case "start", "end":
	case "":
		req.Origin = "start"
	default:
		f.handleStreamResultError(invalidOrigin, helper.Int64ToPtr(400), encoder)
		return
	}

	fs, err := f.c.GetAllocFS(req.AllocID)
	if err != nil {
		code := helper.Int64ToPtr(500)
		if structs.IsErrUnknownAllocation(err) {
			code = helper.Int64ToPtr(404)
		}

		f.handleStreamResultError(err, code, encoder)
		return
	}

	alloc, err := f.c.GetClientAlloc(req.AllocID)
	if err != nil {
		code := helper.Int64ToPtr(500)
		if structs.IsErrUnknownAllocation(err) {
			code = helper.Int64ToPtr(404)
		}

		f.handleStreamResultError(err, code, encoder)
		return
	}

	// Check that the task is there
	tg := alloc.Job.LookupTaskGroup(alloc.TaskGroup)
	if tg == nil {
		f.handleStreamResultError(fmt.Errorf("Failed to lookup task group for allocation"),
			helper.Int64ToPtr(500), encoder)
		return
	} else if taskStruct := tg.LookupTask(req.Task); taskStruct == nil {
		f.handleStreamResultError(
			fmt.Errorf("task group %q does not have task with name %q", alloc.TaskGroup, req.Task),
			helper.Int64ToPtr(400),
			encoder)
		return
	}

	state, ok := alloc.TaskStates[req.Task]
	if !ok || state.StartedAt.IsZero() {
		f.handleStreamResultError(fmt.Errorf("task %q not started yet. No logs available", req.Task),
			helper.Int64ToPtr(404), encoder)
		return
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	frames := make(chan *sframer.StreamFrame, streamFramesBuffer)
	errCh := make(chan error)

	// Start streaming
	go func() {
		if err := f.logsImpl(ctx, req.Follow, req.PlainText,
			req.Offset, req.Origin, req.Task, req.LogType, fs, frames); err != nil {
			select {
			case errCh <- err:
			case <-ctx.Done():
			}
		}
	}()

	// Create a goroutine to detect the remote side closing
	go func() {
		for {
			if _, err := conn.Read(nil); err != nil {
				if err == io.EOF || err == io.ErrClosedPipe {
					// One end of the pipe was explicitly closed, exit cleanly
					cancel()
					return
				}
				select {
				case errCh <- err:
				case <-ctx.Done():
				}
				return
			}
		}
	}()

	var streamErr error
	buf := new(bytes.Buffer)
	frameCodec := codec.NewEncoder(buf, structs.JsonHandle)
OUTER:
	for {
		select {
		case streamErr = <-errCh:
			break OUTER
		case frame, ok := <-frames:
			if !ok {
				break OUTER
			}

			var resp cstructs.StreamErrWrapper
			if req.PlainText {
				resp.Payload = frame.Data
			} else {
				if err = frameCodec.Encode(frame); err != nil {
					streamErr = err
					break OUTER
				}
				frameCodec.Reset(buf)

				resp.Payload = buf.Bytes()
				buf.Reset()
			}

			if err := encoder.Encode(resp); err != nil {
				streamErr = err
				break OUTER
			}
			encoder.Reset(conn)
		}
	}

	if streamErr != nil {
		f.handleStreamResultError(streamErr, helper.Int64ToPtr(500), encoder)
		return
	}
}

// logsImpl is used to stream the logs of a the given task. Output is sent on
// the passed frames channel and the method will return on EOF if follow is not
// true otherwise when the context is cancelled or on an error.
func (f *FileSystem) logsImpl(ctx context.Context, follow, plain bool, offset int64,
	origin, task, logType string,
	fs allocdir.AllocDirFS, frames chan<- *sframer.StreamFrame) error {

	// Create the framer
	framer := sframer.NewStreamFramer(frames, streamHeartbeatRate, streamBatchWindow, streamFrameSize)
	framer.Run()
	defer framer.Destroy()

	// Path to the logs
	logPath := filepath.Join(allocdir.SharedAllocName, allocdir.LogDirName)

	// nextIdx is the next index to read logs from
	var nextIdx int64
	switch origin {
	case "start":
		nextIdx = 0
	case "end":
		nextIdx = math.MaxInt64
		offset *= -1
	default:
		return invalidOrigin
	}

	for {
		// Logic for picking next file is:
		// 1) List log files
		// 2) Pick log file closest to desired index
		// 3) Open log file at correct offset
		// 3a) No error, read contents
		// 3b) If file doesn't exist, goto 1 as it may have been rotated out
		entries, err := fs.List(logPath)
		if err != nil {
			return fmt.Errorf("failed to list entries: %v", err)
		}

		// If we are not following logs, determine the max index for the logs we are
		// interested in so we can stop there.
		maxIndex := int64(math.MaxInt64)
		if !follow {
			_, idx, _, err := findClosest(entries, maxIndex, 0, task, logType)
			if err != nil {
				return err
			}
			maxIndex = idx
		}

		logEntry, idx, openOffset, err := findClosest(entries, nextIdx, offset, task, logType)
		if err != nil {
			return err
		}

		var eofCancelCh chan error
		exitAfter := false
		if !follow && idx > maxIndex {
			// Exceeded what was there initially so return
			return nil
		} else if !follow && idx == maxIndex {
			// At the end
			eofCancelCh = make(chan error)
			close(eofCancelCh)
			exitAfter = true
		} else {
			eofCancelCh = blockUntilNextLog(ctx, fs, logPath, task, logType, idx+1)
		}

		p := filepath.Join(logPath, logEntry.Name)
		err = f.streamFile(ctx, openOffset, p, 0, fs, framer, eofCancelCh)

		// Check if the context is cancelled
		select {
		case <-ctx.Done():
			return nil
		default:
		}

		if err != nil {
			// Check if there was an error where the file does not exist. That means
			// it got rotated out from under us.
			if os.IsNotExist(err) {
				continue
			}

			// Check if the connection was closed
			if err == syscall.EPIPE {
				return nil
			}

			return fmt.Errorf("failed to stream %q: %v", p, err)
		}

		if exitAfter {
			return nil
		}

		// defensively check to make sure StreamFramer hasn't stopped
		// running to avoid tight loops with goroutine leaks as in
		// #3342
		select {
		case <-framer.ExitCh():
			return nil
		default:
		}

		// Since we successfully streamed, update the overall offset/idx.
		offset = int64(0)
		nextIdx = idx + 1
	}
}

// streamFile is the internal method to stream the content of a file. If limit
// is greater than zero, the stream will end once that many bytes have been
// read. eofCancelCh is used to cancel the stream if triggered while at EOF. If
// the connection is broken an EPIPE error is returned
func (f *FileSystem) streamFile(ctx context.Context, offset int64, path string, limit int64,
	fs allocdir.AllocDirFS, framer *sframer.StreamFramer, eofCancelCh chan error) error {

	// Get the reader
	file, err := fs.ReadAt(path, offset)
	if err != nil {
		return err
	}
	defer file.Close()

	var fileReader io.Reader
	if limit <= 0 {
		fileReader = file
	} else {
		fileReader = io.LimitReader(file, limit)
	}

	// Create a tomb to cancel watch events
	waitCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	// Create a variable to allow setting the last event
	var lastEvent string

	// Only create the file change watcher once. But we need to do it after we
	// read and reach EOF.
	var changes *watch.FileChanges

	// Start streaming the data
	bufSize := int64(streamFrameSize)
	if limit > 0 && limit < streamFrameSize {
		bufSize = limit
	}
	data := make([]byte, bufSize)
OUTER:
	for {
		// Read up to the max frame size
		n, readErr := fileReader.Read(data)

		// Update the offset
		offset += int64(n)

		// Return non-EOF errors
		if readErr != nil && readErr != io.EOF {
			return readErr
		}

		// Send the frame
		if n != 0 || lastEvent != "" {
			if err := framer.Send(path, lastEvent, data[:n], offset); err != nil {
				return parseFramerErr(err)
			}
		}

		// Clear the last event
		if lastEvent != "" {
			lastEvent = ""
		}

		// Just keep reading since we aren't at the end of the file so we can
		// avoid setting up a file event watcher.
		if readErr == nil {
			continue
		}

		// If EOF is hit, wait for a change to the file
		if changes == nil {
			changes, err = fs.ChangeEvents(waitCtx, path, offset)
			if err != nil {
				return err
			}
		}

		for {
			select {
			case <-changes.Modified:
				continue OUTER
			case <-changes.Deleted:
				return parseFramerErr(framer.Send(path, deleteEvent, nil, offset))
			case <-changes.Truncated:
				// Close the current reader
				if err := file.Close(); err != nil {
					return err
				}

				// Get a new reader at offset zero
				offset = 0
				var err error
				file, err = fs.ReadAt(path, offset)
				if err != nil {
					return err
				}
				defer file.Close()

				if limit <= 0 {
					fileReader = file
				} else {
					// Get the current limit
					lr, ok := fileReader.(*io.LimitedReader)
					if !ok {
						return fmt.Errorf("unable to determine remaining read limit")
					}

					fileReader = io.LimitReader(file, lr.N)
				}

				// Store the last event
				lastEvent = truncateEvent
				continue OUTER
			case <-framer.ExitCh():
				return nil
			case <-ctx.Done():
				return nil
			case err, ok := <-eofCancelCh:
				if !ok {
					return nil
				}

				return err
			}
		}
	}
}

// blockUntilNextLog returns a channel that will have data sent when the next
// log index or anything greater is created.
func blockUntilNextLog(ctx context.Context, fs allocdir.AllocDirFS, logPath, task, logType string, nextIndex int64) chan error {
	nextPath := filepath.Join(logPath, fmt.Sprintf("%s.%s.%d", task, logType, nextIndex))
	next := make(chan error, 1)

	go func() {
		eofCancelCh, err := fs.BlockUntilExists(ctx, nextPath)
		if err != nil {
			next <- err
			close(next)
			return
		}

		ticker := time.NewTicker(nextLogCheckRate)
		defer ticker.Stop()
		scanCh := ticker.C
		for {
			select {
			case <-ctx.Done():
				next <- nil
				close(next)
				return
			case err := <-eofCancelCh:
				next <- err
				close(next)
				return
			case <-scanCh:
				entries, err := fs.List(logPath)
				if err != nil {
					next <- fmt.Errorf("failed to list entries: %v", err)
					close(next)
					return
				}

				indexes, err := logIndexes(entries, task, logType)
				if err != nil {
					next <- err
					close(next)
					return
				}

				// Scan and see if there are any entries larger than what we are
				// waiting for.
				for _, entry := range indexes {
					if entry.idx >= nextIndex {
						next <- nil
						close(next)
						return
					}
				}
			}
		}
	}()

	return next
}

// indexTuple and indexTupleArray are used to find the correct log entry to
// start streaming logs from
type indexTuple struct {
	idx   int64
	entry *cstructs.AllocFileInfo
}

type indexTupleArray []indexTuple

func (a indexTupleArray) Len() int           { return len(a) }
func (a indexTupleArray) Less(i, j int) bool { return a[i].idx < a[j].idx }
func (a indexTupleArray) Swap(i, j int)      { a[i], a[j] = a[j], a[i] }

// logIndexes takes a set of entries and returns a indexTupleArray of
// the desired log file entries. If the indexes could not be determined, an
// error is returned.
func logIndexes(entries []*cstructs.AllocFileInfo, task, logType string) (indexTupleArray, error) {
	var indexes []indexTuple
	prefix := fmt.Sprintf("%s.%s.", task, logType)
	for _, entry := range entries {
		if entry.IsDir {
			continue
		}

		// If nothing was trimmed, then it is not a match
		idxStr := strings.TrimPrefix(entry.Name, prefix)
		if idxStr == entry.Name {
			continue
		}

		// Convert to an int
		idx, err := strconv.Atoi(idxStr)
		if err != nil {
			return nil, fmt.Errorf("failed to convert %q to a log index: %v", idxStr, err)
		}

		indexes = append(indexes, indexTuple{idx: int64(idx), entry: entry})
	}

	return indexTupleArray(indexes), nil
}

// findClosest takes a list of entries, the desired log index and desired log
// offset (which can be negative, treated as offset from end), task name and log
// type and returns the log entry, the log index, the offset to read from and a
// potential error.
func findClosest(entries []*cstructs.AllocFileInfo, desiredIdx, desiredOffset int64,
	task, logType string) (*cstructs.AllocFileInfo, int64, int64, error) {

	// Build the matching indexes
	indexes, err := logIndexes(entries, task, logType)
	if err != nil {
		return nil, 0, 0, err
	}
	if len(indexes) == 0 {
		return nil, 0, 0, fmt.Errorf("log entry for task %q and log type %q not found", task, logType)
	}

	// Binary search the indexes to get the desiredIdx
	sort.Sort(indexes)
	i := sort.Search(len(indexes), func(i int) bool { return indexes[i].idx >= desiredIdx })
	l := len(indexes)
	if i == l {
		// Use the last index if the number is bigger than all of them.
		i = l - 1
	}

	// Get to the correct offset
	offset := desiredOffset
	idx := int64(i)
	for {
		s := indexes[idx].entry.Size

		// Base case
		if offset == 0 {
			break
		} else if offset < 0 {
			// Going backwards
			if newOffset := s + offset; newOffset >= 0 {
				// Current file works
				offset = newOffset
				break
			} else if idx == 0 {
				// Already at the end
				offset = 0
				break
			} else {
				// Try the file before
				offset = newOffset
				idx -= 1
				continue
			}
		} else {
			// Going forward
			if offset <= s {
				// Current file works
				break
			} else if idx == int64(l-1) {
				// Already at the end
				offset = s
				break
			} else {
				// Try the next file
				offset = offset - s
				idx += 1
				continue
			}

		}
	}

	return indexes[idx].entry, indexes[idx].idx, offset, nil
}

// parseFramerErr takes an error and returns an error. The error will
// potentially change if it was caused by the connection being closed.
func parseFramerErr(err error) error {
	if err == nil {
		return nil
	}

	errMsg := err.Error()

	if strings.Contains(errMsg, io.ErrClosedPipe.Error()) {
		// The pipe check is for tests
		return syscall.EPIPE
	}

	// The connection was closed by our peer
	if strings.Contains(errMsg, syscall.EPIPE.Error()) || strings.Contains(errMsg, syscall.ECONNRESET.Error()) {
		return syscall.EPIPE
	}

	// Windows version of ECONNRESET
	//XXX(schmichael) I could find no existing error or constant to
	//                compare this against.
	if strings.Contains(errMsg, "forcibly closed") {
		return syscall.EPIPE
	}

	return err
}
