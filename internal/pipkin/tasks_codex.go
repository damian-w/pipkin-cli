package pipkin

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net"
	"os"
	"runtime"
	"sort"
	"strconv"
	"syscall"
	"time"
)

const (
	codexTaskBudget        = 3 * time.Second
	codexTaskMaxFrame      = 16 << 20
	codexTaskMaxBytes      = 64 << 20
	codexTaskMaxCandidates = 64
	codexTaskMaxStreams    = 16
	codexTaskMaxPending    = 4
)

type codexTaskDiagnostic struct {
	Phase  string
	Method string
	Bytes  int
}

type codexTaskOptions struct {
	dial       func(context.Context) (net.Conn, error)
	candidates func(context.Context) []string
	budget     time.Duration
	diagnostic func(codexTaskDiagnostic)
}

type codexTaskMessage struct {
	Type              string          `json:"type"`
	Method            string          `json:"method"`
	Version           int             `json:"version"`
	RequestID         json.RawMessage `json:"requestId"`
	ResultType        string          `json:"resultType"`
	HandledByClientID string          `json:"handledByClientId"`
	SourceClientID    string          `json:"sourceClientId"`
	Params            json.RawMessage `json:"params"`
}

type codexTaskParams struct {
	ConversationID string          `json:"conversationId"`
	HostID         string          `json:"hostId"`
	Following      bool            `json:"following"`
	ClientID       string          `json:"clientId"`
	Status         string          `json:"status"`
	Change         json.RawMessage `json:"change"`
}

type codexTaskChange struct {
	Type              string           `json:"type"`
	Revision          *uint64          `json:"revision"`
	BaseRevision      *uint64          `json:"baseRevision"`
	ConversationState json.RawMessage  `json:"conversationState"`
	Patches           []codexTaskPatch `json:"patches"`
}

type codexTaskStream struct {
	owner    string
	revision uint64
	floor    uint64
	retired  bool
	meta     codexTaskMeta
	valid    bool
	waiting  bool
	resyncs  int
}

type codexTaskLookup struct {
	id      string
	expires time.Time
}

type codexTaskObserver struct {
	ctx         context.Context
	conn        net.Conn
	deadline    time.Time
	prefix      string
	sequence    int
	initialized bool
	catalog     map[string]bool
	queued      map[string]bool
	queue       []string
	lookups     map[string]codexTaskLookup
	lookedUp    int
	streams     map[string]*codexTaskStream
	resyncs     int
	diagnostic  func(codexTaskDiagnostic)
	diagnostics int
}

func collectCodexTasks(ctx context.Context) TaskProvider {
	if runtime.GOOS != "darwin" && runtime.GOOS != "windows" {
		return TaskProvider{Coverage: "unsupported"}
	}
	home, err := codexTaskHome()
	if err != nil {
		return TaskProvider{Coverage: "unavailable"}
	}
	return collectCodexTasksWith(ctx, codexTaskOptions{
		dial: openCodexDesktopIPC,
		candidates: func(ctx context.Context) []string {
			return codexTaskCandidates(ctx, home, codexTaskMaxCandidates)
		},
		budget: codexTaskBudget,
	})
}

func collectCodexTasksWith(ctx context.Context, opts codexTaskOptions) TaskProvider {
	budget := opts.budget
	if budget <= 0 || budget > codexTaskBudget {
		budget = codexTaskBudget
	}
	ctx, cancel := context.WithTimeout(ctx, budget)
	defer cancel()
	if ctx.Err() != nil {
		return TaskProvider{Coverage: "unavailable"}
	}
	deadline, _ := ctx.Deadline()
	observer := codexTaskObserver{ctx: ctx, deadline: deadline, catalog: map[string]bool{}, queued: map[string]bool{},
		lookups: map[string]codexTaskLookup{}, streams: map[string]*codexTaskStream{}, diagnostic: opts.diagnostic}
	if opts.candidates != nil {
		catalogCtx, done := context.WithTimeout(ctx, 400*time.Millisecond)
		for _, id := range opts.candidates(catalogCtx) {
			if codexTaskID(id) && len(observer.catalog) < codexTaskMaxCandidates {
				observer.catalog[id] = true
				observer.enqueue(id, false)
			}
		}
		done()
	}
	var nonce [12]byte
	if _, err := rand.Read(nonce[:]); err != nil || ctx.Err() != nil {
		return TaskProvider{Coverage: "unavailable"}
	}
	observer.prefix = hex.EncodeToString(nonce[:])
	observer.trace("dial_start", "", 0)
	conn, err := opts.dial(ctx)
	if err != nil {
		observer.trace("dial_failed", "", 0)
		return TaskProvider{Coverage: "unavailable"}
	}
	observer.trace("dial_ready", "", 0)
	observer.conn = conn
	defer conn.Close()
	stop := context.AfterFunc(ctx, func() { conn.Close() })
	defer stop()
	if err := observer.send(map[string]any{"type": "request", "method": "initialize", "version": 0,
		"requestId": observer.prefix, "params": map[string]any{"clientType": "monitor-observer"}}); err != nil {
		return TaskProvider{Coverage: "unavailable"}
	}
	initialUntil := time.Now().Add(min(250*time.Millisecond, budget/4))
	buffer := make([]byte, 0, 64<<10)
	chunk := make([]byte, 64<<10)
	received, frames := 0, 0
	failed := false
	for ctx.Err() == nil {
		now := time.Now()
		for key, pending := range observer.lookups {
			if !now.Before(pending.expires) {
				delete(observer.lookups, key)
			}
		}
		if observer.initialized && !now.Before(initialUntil) {
			for len(observer.queue) > 0 && len(observer.lookups) < codexTaskMaxPending && observer.lookedUp < codexTaskMaxCandidates {
				id := observer.queue[0]
				observer.queue = observer.queue[1:]
				observer.sequence++
				key := observer.prefix + ":" + strconv.Itoa(observer.sequence)
				observer.lookups[key] = codexTaskLookup{id, now.Add(750 * time.Millisecond)}
				observer.lookedUp++
				if observer.send(map[string]any{"type": "request", "method": "thread-owner-discovery", "version": 1,
					"requestId": key, "params": map[string]any{"hostId": "local", "conversationId": id}}) != nil {
					failed = true
					break
				}
			}
		}
		if failed {
			break
		}
		if err := conn.SetReadDeadline(minTime(time.Now().Add(30*time.Millisecond), deadline)); err != nil {
			failed = true
			break
		}
		n, readErr := conn.Read(chunk)
		if n > 0 {
			observer.trace("read_data", "", n)
			received += n
			if received > codexTaskMaxBytes {
				observer.trace("total_byte_limit", "", received)
				failed = true
				break
			}
			buffer = append(buffer, chunk[:n]...)
			for len(buffer) >= 4 {
				size := int(binary.LittleEndian.Uint32(buffer[:4]))
				if size <= 0 || size > codexTaskMaxFrame || frames >= 4096 {
					observer.trace("frame_limit", "", size)
					failed = true
					break
				}
				if len(buffer) < size+4 {
					break
				}
				frames++
				if observer.receive(buffer[4:size+4]) != nil {
					observer.trace("receive_failed", "", size)
					failed = true
					break
				}
				buffer = buffer[size+4:]
			}
		}
		if failed {
			break
		}
		if readErr != nil {
			if ctx.Err() != nil || !time.Now().Before(deadline) {
				break
			}
			if !codexTaskReadTimedOut(readErr) {
				observer.trace(codexTaskReadFailure(readErr), "", n)
				failed = true
				break
			}
		}
	}
	// Closing the connection also ends its stream subscriptions.
	if !failed && observer.initialized {
		observer.trace("complete_partial", "", 0)
		return observer.result()
	}
	observer.trace("complete_unavailable", "", 0)
	return TaskProvider{Coverage: "unavailable"}
}

func minTime(a, b time.Time) time.Time {
	if a.Before(b) {
		return a
	}
	return b
}

func (o *codexTaskObserver) enqueue(id string, priority bool) {
	if !codexTaskID(id) {
		return
	}
	if o.queued[id] {
		if priority {
			for index, queued := range o.queue {
				if queued == id && index > 0 {
					copy(o.queue[1:index+1], o.queue[:index])
					o.queue[0] = id
					break
				}
			}
		}
		return
	}
	if len(o.queued) >= codexTaskMaxCandidates*2 {
		return
	}
	o.queued[id] = true
	if priority {
		o.queue = append([]string{id}, o.queue...)
	} else {
		o.queue = append(o.queue, id)
	}
}

func (o *codexTaskObserver) send(message map[string]any) error {
	typeName, _ := message["type"].(string)
	method, _ := message["method"].(string)
	allowed := typeName == "request" && (method == "initialize" || method == "thread-owner-discovery") ||
		typeName == "broadcast" && method == "thread-stream-following-changed"
	if typeName == "client-discovery-response" {
		response, _ := message["response"].(map[string]any)
		allowed = len(response) == 1 && response["canHandle"] == false
	}
	if !allowed {
		return errCodexTaskState
	}
	o.trace("send", method, 0)
	body, err := json.Marshal(message)
	if err != nil || len(body) > 4096 {
		return errCodexTaskState
	}
	frame := make([]byte, len(body)+4)
	binary.LittleEndian.PutUint32(frame, uint32(len(body)))
	copy(frame[4:], body)
	if err := o.conn.SetWriteDeadline(minTime(time.Now().Add(200*time.Millisecond), o.deadline)); err != nil {
		return err
	}
	for len(frame) > 0 {
		n, err := o.conn.Write(frame)
		if err != nil {
			o.trace("write_failed", method, n)
			return err
		}
		if n == 0 {
			return io.ErrShortWrite
		}
		frame = frame[n:]
	}
	return nil
}

func (o *codexTaskObserver) follow(id string, value bool) error {
	stream := o.streams[id]
	if stream == nil || !codexTaskID(stream.owner) || stream.retired {
		return errCodexTaskState
	}
	return o.send(map[string]any{"type": "broadcast", "method": "thread-stream-following-changed", "version": 1,
		"targetClientIds": []string{stream.owner}, "params": map[string]any{"conversationId": id, "hostId": "local", "following": value}})
}

func (o *codexTaskObserver) receive(frame []byte) error {
	if raw := bytes.TrimSpace(frame); len(raw) == 0 || raw[0] != '{' {
		o.trace("frame_shape_invalid", "", len(frame))
		return errCodexTaskState
	}
	var message codexTaskMessage
	if json.Unmarshal(frame, &message) != nil {
		o.trace("frame_json_invalid", "", len(frame))
		return errCodexTaskState
	}
	o.trace("receive", message.Method, len(frame))
	var requestID string
	if len(message.RequestID) > 0 && (json.Unmarshal(message.RequestID, &requestID) != nil || len(requestID) > 128) {
		o.trace("request_id_invalid", message.Method, 0)
		return errCodexTaskState
	}
	switch message.Type {
	case "client-discovery-request":
		if requestID == "" {
			o.trace("discovery_id_empty", "", 0)
			return errCodexTaskState
		}
		return o.send(map[string]any{"type": "client-discovery-response", "requestId": requestID,
			"response": map[string]any{"canHandle": false}})
	case "response":
		if requestID == o.prefix {
			if message.Method != "initialize" || message.ResultType != "success" {
				o.trace("initialize_rejected", message.Method, 0)
				return errCodexTaskState
			}
			o.trace("initialized", message.Method, 0)
			o.initialized = true
			return nil
		}
		lookup, ok := o.lookups[requestID]
		if !ok {
			return nil
		}
		delete(o.lookups, requestID)
		if message.Method == "thread-owner-discovery" && message.ResultType == "success" && codexTaskID(message.HandledByClientID) && len(o.streams) < codexTaskMaxStreams {
			o.streams[lookup.id] = &codexTaskStream{owner: message.HandledByClientID, waiting: true, meta: codexTaskMeta{scope: 2}}
			return o.follow(lookup.id, true)
		}
		return nil
	case "broadcast":
		if !o.initialized {
			return nil
		}
		var params codexTaskParams
		if json.Unmarshal(message.Params, &params) != nil {
			o.trace("broadcast_params_invalid", message.Method, 0)
			return errCodexTaskState
		}
		if message.Method == "ipc-connection-reset" {
			o.trace("source_reset", message.Method, 0)
			return errCodexTaskState
		}
		if message.Method == "client-status-changed" && params.Status == "disconnected" {
			for _, stream := range o.streams {
				if stream.owner == params.ClientID {
					stream.valid, stream.waiting, stream.retired = false, false, true
				}
			}
			return nil
		}
		if params.HostID != "local" || !codexTaskID(params.ConversationID) {
			return nil
		}
		if message.Method == "thread-stream-following-changed" && params.Following || message.Method == "thread-stream-following-status-requested" {
			if message.Version != 1 {
				o.trace("following_version_invalid", message.Method, 0)
				return errCodexTaskState
			}
			o.enqueue(params.ConversationID, true)
			if _, ok := o.streams[params.ConversationID]; ok && message.Method == "thread-stream-following-status-requested" {
				return o.follow(params.ConversationID, true)
			}
			return nil
		}
		if message.Method == "thread-stream-state-changed" {
			if message.Version != 11 {
				o.trace("stream_version_invalid", message.Method, 0)
				return errCodexTaskState
			}
			return o.stream(message, params)
		}
	}
	return nil
}

func (o *codexTaskObserver) stream(message codexTaskMessage, params codexTaskParams) error {
	stream := o.streams[params.ConversationID]
	if stream == nil || stream.retired {
		return nil
	}
	var change codexTaskChange
	if json.Unmarshal(params.Change, &change) != nil || change.Revision == nil {
		stream.valid = false
		return nil
	}
	if message.SourceClientID != stream.owner {
		stream.valid, stream.waiting, stream.retired = false, false, true
		return nil
	}
	if *change.Revision < stream.floor {
		return nil
	}
	stream.floor = *change.Revision
	switch change.Type {
	case "snapshot":
		if stream.valid && *change.Revision < stream.revision {
			return nil
		}
		meta, err := codexTaskProject(change.ConversationState, params.ConversationID, o.catalog[params.ConversationID])
		stream.valid, stream.waiting = err == nil, false
		if err == nil {
			if stream.meta.scope == 0 && meta.scope == 2 {
				meta.scope = 0
			}
			stream.meta, stream.revision = meta, *change.Revision
		}
	case "patches":
		if stream.waiting {
			return nil
		}
		if stream.valid && *change.Revision <= stream.revision {
			return nil
		}
		if !stream.valid || change.BaseRevision == nil || *change.BaseRevision != stream.revision {
			stream.valid = false
			return nil
		}
		meta := stream.meta
		for _, patch := range change.Patches {
			refresh, err := codexTaskApply(&meta, patch)
			if err != nil {
				stream.valid = false
				return nil
			}
			if refresh {
				stream.valid = false
				stream.resyncs++
				o.resyncs++
				if stream.resyncs > 2 || o.resyncs > 6 {
					return nil
				}
				stream.waiting = true
				if err := o.follow(params.ConversationID, false); err != nil {
					return err
				}
				return o.follow(params.ConversationID, true)
			}
		}
		stream.meta, stream.revision = meta, *change.Revision
	default:
		stream.valid = false
	}
	return nil
}

func (o *codexTaskObserver) result() TaskProvider {
	result := TaskProvider{Coverage: "partial", Tasks: []Task{}}
	for id, stream := range o.streams {
		if stream.meta.scope == 0 {
			continue
		}
		task := Task{ID: taskOpaqueID("codex", id), Status: "unknown"}
		if stream.valid {
			task.Status = stream.meta.status()
			if stream.meta.scope == 1 {
				task.UpdatedAt = stream.meta.updated
			}
		}
		result.Tasks = append(result.Tasks, task)
	}
	sort.Slice(result.Tasks, func(i, j int) bool { return result.Tasks[i].ID < result.Tasks[j].ID })
	return result
}

// trace feeds the opt-in live test; it never sees IDs, payloads or paths.
func (o *codexTaskObserver) trace(phase, method string, size int) {
	if o.diagnostic == nil || o.diagnostics >= 80 {
		return
	}
	switch method {
	case "", "initialize", "thread-owner-discovery", "thread-stream-following-changed", "thread-stream-following-status-requested", "thread-stream-state-changed", "thread-read-state-changed", "client-status-changed", "ipc-connection-reset":
	default:
		method = "other"
	}
	o.diagnostics++
	o.diagnostic(codexTaskDiagnostic{Phase: phase, Method: method, Bytes: size})
}

func codexTaskReadFailure(err error) string {
	if errors.Is(err, io.EOF) {
		return "read_eof"
	}
	if errors.Is(err, net.ErrClosed) {
		return "read_closed"
	}
	var errno syscall.Errno
	if runtime.GOOS == "windows" && errors.As(err, &errno) {
		switch uintptr(errno) {
		case 234:
			return "read_more_data"
		case 109:
			return "read_broken_pipe"
		case 232:
			return "read_no_data"
		case 233:
			return "read_pipe_not_connected"
		case 995:
			return "read_aborted"
		case 5:
			return "read_access_denied"
		}
	}
	return "read_failed"
}

func codexTaskReadTimedOut(err error) bool {
	if errors.Is(err, os.ErrDeadlineExceeded) || errors.Is(err, context.DeadlineExceeded) {
		return true
	}
	// Named-pipe deadline errors implement Timeout but not net.Error.
	var timeout interface{ Timeout() bool }
	return errors.As(err, &timeout) && timeout.Timeout()
}
