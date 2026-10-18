package pipkin

import (
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
	"strconv"
	"time"
)

const (
	codexTaskBudget        = 3 * time.Second
	codexTaskMaxFrame      = 16 << 20
	codexTaskMaxCandidates = 64
	codexTaskMaxStreams    = 16
	codexTaskMaxPending    = 4
	// Budgets per minute of connection; a one-off check spends them within its window.
	codexTaskMaxBytes   = 64 << 20
	codexTaskMaxFrames  = 4096
	codexTaskMaxResyncs = 6
	// Each stream may resync this often before it waits as unknown.
	codexTaskStreamResyncs      = 2
	codexTaskStreamResyncWindow = 30 * time.Second
	codexTaskLookupWait         = 750 * time.Millisecond
	codexTaskLookupAttempts     = 3
	// A live watch reads in longer waits when idle and publishes at most this often.
	codexTaskIdleRead        = time.Second
	codexTaskPublishGap      = 250 * time.Millisecond
	codexTaskRediscoverEvery = time.Minute
	codexTaskInitializeWait  = 5 * time.Second
)

var errCodexTaskDial = errors.New("Codex local connection is unavailable")

type codexTaskOptions struct {
	dial       func(context.Context) (net.Conn, error)
	candidates func(context.Context) []string
	budget     time.Duration
	// retry holds a live watch's reconnect delays; tests shorten them.
	retry []time.Duration
}

type codexTaskMessage struct {
	Type              string          `json:"type"`
	Method            string          `json:"method"`
	Version           int             `json:"version"`
	RequestID         string          `json:"requestId"`
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
	owner        string
	revision     uint64
	floor        uint64
	retired      bool
	meta         codexTaskMeta
	valid        bool
	waiting      bool
	resyncs      int
	resyncWindow time.Time
	resyncWanted bool
}

type codexTaskLookup struct {
	id      string
	expires time.Time
}

type codexTaskObserver struct {
	ctx         context.Context
	conn        net.Conn
	stop        func() bool
	deadline    time.Time // zero for a live watch
	prefix      string
	sequence    int
	initialized bool
	catalog     map[string]bool
	queued      map[string]bool
	queue       []string
	lookups     map[string]codexTaskLookup
	streams     map[string]*codexTaskStream
	// Counts for the current minute's budgets.
	window   time.Time
	received int
	frames   int
	lookedUp int
	resyncs  int
	// A live watch keeps discovering chats and publishes its reduced state.
	live        bool
	publish     func(TaskProvider)
	candidates  func(context.Context) []string
	attempts    map[string]int
	connected   time.Time
	rediscover  time.Time
	ready       bool
	dirty       bool
	publishedAt time.Time
}

// codexDesktopTasks returns options for the Codex app, or the coverage to report instead.
func codexDesktopTasks() (codexTaskOptions, string) {
	if runtime.GOOS != "darwin" && runtime.GOOS != "windows" {
		return codexTaskOptions{}, "unsupported"
	}
	home, err := codexTaskHome()
	if err != nil {
		return codexTaskOptions{}, "unavailable"
	}
	return codexTaskOptions{
		dial: openCodexDesktopIPC,
		candidates: func(ctx context.Context) []string {
			return codexTaskCandidates(ctx, home, codexTaskMaxCandidates)
		},
		budget: codexTaskBudget,
	}, ""
}

func collectCodexTasks(ctx context.Context) TaskProvider {
	opts, coverage := codexDesktopTasks()
	if coverage != "" {
		return TaskProvider{Coverage: coverage}
	}
	return collectCodexTasksWith(ctx, opts)
}

// collectCodexTasksWith is a one-off check: it follows chats until its budget ends.
func collectCodexTasksWith(ctx context.Context, opts codexTaskOptions) TaskProvider {
	budget := opts.budget
	if budget <= 0 || budget > codexTaskBudget {
		budget = codexTaskBudget
	}
	ctx, cancel := context.WithTimeout(ctx, budget)
	defer cancel()
	observer, err := openCodexTaskObserver(ctx, opts, nil)
	if err != nil {
		return TaskProvider{Coverage: "unavailable"}
	}
	// Closing the connection also ends its stream subscriptions.
	defer observer.close()
	if observer.run() != nil || !observer.initialized {
		return TaskProvider{Coverage: "unavailable"}
	}
	return observer.result()
}

// openCodexTaskObserver dials and initializes one verified connection. Without
// publish it is a one-off check that ends at ctx's deadline.
func openCodexTaskObserver(ctx context.Context, opts codexTaskOptions, publish func(TaskProvider)) (*codexTaskObserver, error) {
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}
	deadline, _ := ctx.Deadline()
	now := time.Now()
	o := &codexTaskObserver{ctx: ctx, deadline: deadline, catalog: map[string]bool{}, queued: map[string]bool{},
		lookups: map[string]codexTaskLookup{}, streams: map[string]*codexTaskStream{}, attempts: map[string]int{},
		window: now, live: publish != nil, publish: publish, candidates: opts.candidates}
	o.discover(now)
	var nonce [12]byte
	if _, err := rand.Read(nonce[:]); err != nil || ctx.Err() != nil {
		return nil, errCodexTaskState
	}
	o.prefix = hex.EncodeToString(nonce[:])
	conn, err := opts.dial(ctx)
	if err != nil {
		return nil, errCodexTaskDial
	}
	o.conn, o.connected = conn, time.Now()
	o.stop = context.AfterFunc(ctx, func() { conn.Close() })
	if err := o.send(map[string]any{"type": "request", "method": "initialize", "version": 0,
		"requestId": o.prefix, "params": map[string]any{"clientType": "monitor-observer"}}); err != nil {
		o.close()
		return nil, err
	}
	return o, nil
}

func (o *codexTaskObserver) close() {
	o.stop()
	o.conn.Close()
}

// discover queues recent chats from Codex's catalog; the app must still confirm each one.
func (o *codexTaskObserver) discover(now time.Time) {
	o.rediscover = now.Add(codexTaskRediscoverEvery)
	if o.candidates == nil {
		return
	}
	ctx, done := context.WithTimeout(o.ctx, 400*time.Millisecond)
	ids := o.candidates(ctx)
	done()
	o.catalog, o.attempts = map[string]bool{}, map[string]int{}
	for _, id := range ids {
		if len(o.catalog) < codexTaskMaxCandidates {
			o.catalog[id] = true
			o.enqueue(id, false)
		}
	}
}

// run reads until ctx ends, returning an error only when the connection fails.
func (o *codexTaskObserver) run() error {
	wait := 250 * time.Millisecond
	if !o.deadline.IsZero() {
		wait = min(wait, time.Until(o.deadline)/4)
	}
	initialUntil := time.Now().Add(wait)
	buffer := make([]byte, 0, 64<<10)
	chunk := make([]byte, 64<<10)
	for o.ctx.Err() == nil {
		now := time.Now()
		if err := o.pump(now, now.Before(initialUntil)); err != nil {
			return err
		}
		if err := o.conn.SetReadDeadline(o.readDeadline(now)); err != nil {
			return err
		}
		n, readErr := o.conn.Read(chunk)
		if n > 0 {
			o.received += n
			if o.received > codexTaskMaxBytes {
				return errCodexTaskState
			}
			buffer = append(buffer, chunk[:n]...)
			clear(chunk[:n])
			for len(buffer) >= 4 {
				size := int(binary.LittleEndian.Uint32(buffer[:4]))
				if size <= 0 || size > codexTaskMaxFrame || o.frames >= codexTaskMaxFrames {
					return errCodexTaskState
				}
				if len(buffer) < size+4 {
					break
				}
				o.frames++
				if err := o.receive(buffer[4 : size+4]); err != nil {
					return err
				}
				buffer = buffer[size+4:]
			}
			if len(buffer) == 0 {
				buffer = codexTaskRelease(buffer)
			}
		}
		if readErr != nil {
			if o.ctx.Err() != nil || !o.deadline.IsZero() && !time.Now().Before(o.deadline) {
				return nil
			}
			if !codexTaskReadTimedOut(readErr) {
				return readErr
			}
		}
	}
	return nil
}

// Raw frames can hold conversation text, so they are wiped once reduced.
func codexTaskRelease(buffer []byte) []byte {
	clear(buffer[:cap(buffer)])
	if cap(buffer) > 64<<10 {
		return make([]byte, 0, 64<<10)
	}
	return buffer[:0]
}

// pump runs the work between reads: budgets, lookups, resyncs, discovery and publishing.
func (o *codexTaskObserver) pump(now time.Time, initial bool) error {
	if now.Sub(o.window) >= time.Minute {
		o.window, o.received, o.frames, o.lookedUp, o.resyncs = now, 0, 0, 0, 0
	}
	for key, pending := range o.lookups {
		if !now.Before(pending.expires) {
			delete(o.lookups, key)
			o.lookupMissed(pending.id)
		}
	}
	if o.live {
		if !o.initialized && now.Sub(o.connected) >= codexTaskInitializeWait {
			return errCodexTaskState
		}
		if !now.Before(o.rediscover) {
			o.discover(now)
		}
	}
	if o.initialized && !initial {
		for len(o.queue) > 0 && len(o.lookups) < codexTaskMaxPending && o.lookedUp < codexTaskMaxCandidates {
			id := o.queue[0]
			o.queue = o.queue[1:]
			delete(o.queued, id)
			o.sequence++
			key := o.prefix + ":" + strconv.Itoa(o.sequence)
			o.lookups[key] = codexTaskLookup{id, now.Add(codexTaskLookupWait)}
			o.lookedUp++
			o.attempts[id]++
			if err := o.send(map[string]any{"type": "request", "method": "thread-owner-discovery", "version": 1,
				"requestId": key, "params": map[string]any{"hostId": "local", "conversationId": id}}); err != nil {
				return err
			}
		}
	}
	if !o.live {
		return nil
	}
	for id, stream := range o.streams {
		if stream.resyncWanted && !stream.retired {
			if err := o.resync(id, stream, now); err != nil {
				return err
			}
		}
	}
	if !o.ready && o.initialized && (o.settled() || now.Sub(o.connected) >= codexTaskBudget) {
		o.ready, o.dirty = true, true
	}
	if o.ready && o.dirty && now.Sub(o.publishedAt) >= codexTaskPublishGap {
		o.dirty, o.publishedAt = false, now
		o.publish(o.result())
	}
	return nil
}

// settled reports that initial discovery has followed or ruled out every chat.
func (o *codexTaskObserver) settled() bool {
	if len(o.queue) > 0 || len(o.lookups) > 0 {
		return false
	}
	for _, stream := range o.streams {
		if stream.waiting {
			return false
		}
	}
	return true
}

// readDeadline waits briefly while work is pending, and longer when a live watch is idle.
func (o *codexTaskObserver) readDeadline(now time.Time) time.Time {
	wait := 30 * time.Millisecond
	if o.live && o.ready && !o.dirty && len(o.queue) == 0 && len(o.lookups) == 0 {
		wait = codexTaskIdleRead
		for _, stream := range o.streams {
			if stream.resyncWanted {
				wait = 30 * time.Millisecond
			}
		}
	}
	return o.until(now, wait)
}

// until caps a wait at a one-off check's deadline.
func (o *codexTaskObserver) until(now time.Time, wait time.Duration) time.Time {
	at := now.Add(wait)
	if !o.deadline.IsZero() && o.deadline.Before(at) {
		return o.deadline
	}
	return at
}

func (o *codexTaskObserver) enqueue(id string, priority bool) {
	if !codexTaskID(id) || o.watching(id) {
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
	if len(o.queue) >= codexTaskMaxCandidates*2 {
		return
	}
	o.queued[id] = true
	if priority {
		o.queue = append([]string{id}, o.queue...)
	} else {
		o.queue = append(o.queue, id)
	}
}

// watching reports a chat that is followed or has a lookup in flight.
func (o *codexTaskObserver) watching(id string) bool {
	if stream := o.streams[id]; stream != nil && !stream.retired {
		return true
	}
	for _, lookup := range o.lookups {
		if lookup.id == id {
			return true
		}
	}
	return false
}

// followed counts the streams holding one of the limited slots.
func (o *codexTaskObserver) followed() int {
	count := 0
	for _, stream := range o.streams {
		if !stream.retired {
			count++
		}
	}
	return count
}

// lookupMissed lets a live watch retry an unanswered lookup, then forget a chat whose owner left.
func (o *codexTaskObserver) lookupMissed(id string) {
	if !o.live {
		return
	}
	if o.attempts[id] < codexTaskLookupAttempts {
		o.enqueue(id, false)
	} else if stream := o.streams[id]; stream != nil && stream.retired {
		delete(o.streams, id)
		o.dirty = true
	}
}

func (o *codexTaskObserver) send(message map[string]any) error {
	body, err := json.Marshal(message)
	if err != nil {
		return errCodexTaskState
	}
	frame := make([]byte, len(body)+4)
	binary.LittleEndian.PutUint32(frame, uint32(len(body)))
	copy(frame[4:], body)
	if err := o.conn.SetWriteDeadline(o.until(time.Now(), 200*time.Millisecond)); err != nil {
		return err
	}
	for len(frame) > 0 {
		n, err := o.conn.Write(frame)
		if err != nil {
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
	if stream == nil || stream.retired {
		return errCodexTaskState
	}
	return o.send(map[string]any{"type": "broadcast", "method": "thread-stream-following-changed", "version": 1,
		"targetClientIds": []string{stream.owner}, "params": map[string]any{"conversationId": id, "hostId": "local", "following": value}})
}

// retire stops trusting a stream whose owner left or changed; a live watch looks for its new owner.
func (o *codexTaskObserver) retire(id string, stream *codexTaskStream) {
	stream.valid, stream.waiting, stream.retired = false, false, true
	o.dirty = true
	if o.live {
		o.enqueue(id, true)
	}
}

// invalidate marks a stream unknown; a live watch asks its owner for a fresh snapshot.
func (o *codexTaskObserver) invalidate(id string, stream *codexTaskStream) error {
	stream.valid = false
	if !o.live {
		return nil
	}
	return o.resync(id, stream, time.Now())
}

// resync re-follows a stream for a fresh snapshot within its rate limits. Past
// them the stream stays unknown, and a live watch tries again once allowed.
func (o *codexTaskObserver) resync(id string, stream *codexTaskStream, now time.Time) error {
	stream.valid = false
	if now.Sub(stream.resyncWindow) >= codexTaskStreamResyncWindow {
		stream.resyncWindow, stream.resyncs = now, 0
	}
	if stream.resyncs >= codexTaskStreamResyncs || o.resyncs >= codexTaskMaxResyncs {
		stream.resyncWanted = o.live
		return nil
	}
	stream.resyncs++
	o.resyncs++
	stream.waiting, stream.resyncWanted = true, false
	if err := o.follow(id, false); err != nil {
		return err
	}
	return o.follow(id, true)
}

func (o *codexTaskObserver) receive(frame []byte) error {
	var message codexTaskMessage
	if json.Unmarshal(frame, &message) != nil || message.Type == "" || len(message.RequestID) > 128 {
		return errCodexTaskState
	}
	requestID := message.RequestID
	switch message.Type {
	case "client-discovery-request":
		if requestID == "" {
			return errCodexTaskState
		}
		return o.send(map[string]any{"type": "client-discovery-response", "requestId": requestID,
			"response": map[string]any{"canHandle": false}})
	case "response":
		if requestID == o.prefix {
			if message.Method != "initialize" || message.ResultType != "success" {
				return errCodexTaskState
			}
			o.initialized = true
			return nil
		}
		lookup, ok := o.lookups[requestID]
		if !ok {
			return nil
		}
		delete(o.lookups, requestID)
		previous := o.streams[lookup.id]
		if message.Method == "thread-owner-discovery" && message.ResultType == "success" && codexTaskID(message.HandledByClientID) {
			if previous != nil && !previous.retired || o.followed() >= codexTaskMaxStreams {
				return nil
			}
			stream := &codexTaskStream{owner: message.HandledByClientID, waiting: true, meta: codexTaskMeta{scope: 2}}
			if previous != nil && previous.meta.scope == 0 {
				stream.meta.scope = 0
			}
			o.streams[lookup.id] = stream
			o.dirty = true
			return o.follow(lookup.id, true)
		}
		if o.live && previous != nil && previous.retired {
			// No app window owns the chat any more, so it is no longer observed.
			delete(o.streams, lookup.id)
			o.dirty = true
		}
		return nil
	case "broadcast":
		if !o.initialized {
			return nil
		}
		var params codexTaskParams
		if json.Unmarshal(message.Params, &params) != nil {
			return errCodexTaskState
		}
		if message.Method == "ipc-connection-reset" {
			return errCodexTaskState
		}
		if message.Method == "client-status-changed" && params.Status == "disconnected" {
			for id, stream := range o.streams {
				if stream.owner == params.ClientID && !stream.retired {
					o.retire(id, stream)
				}
			}
			return nil
		}
		if params.HostID != "local" || !codexTaskID(params.ConversationID) {
			return nil
		}
		if message.Method == "thread-stream-following-changed" && params.Following || message.Method == "thread-stream-following-status-requested" {
			if message.Version != 1 {
				return errCodexTaskState
			}
			o.enqueue(params.ConversationID, true)
			if stream, ok := o.streams[params.ConversationID]; ok && !stream.retired && message.Method == "thread-stream-following-status-requested" {
				return o.follow(params.ConversationID, true)
			}
			return nil
		}
		if message.Method == "thread-stream-state-changed" {
			if message.Version != 11 {
				return errCodexTaskState
			}
			return o.stream(message, params)
		}
	}
	return nil
}

func (o *codexTaskObserver) stream(message codexTaskMessage, params codexTaskParams) error {
	id := params.ConversationID
	stream := o.streams[id]
	if stream == nil || stream.retired {
		return nil
	}
	o.dirty = true
	var change codexTaskChange
	if json.Unmarshal(params.Change, &change) != nil || change.Revision == nil {
		return o.invalidate(id, stream)
	}
	if message.SourceClientID != stream.owner {
		o.retire(id, stream)
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
		meta, err := codexTaskProject(change.ConversationState, id, o.catalog[id])
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
			return o.invalidate(id, stream)
		}
		meta := stream.meta
		for _, patch := range change.Patches {
			refresh, err := codexTaskApply(&meta, patch)
			if err != nil {
				return o.invalidate(id, stream)
			}
			if refresh {
				return o.resync(id, stream, time.Now())
			}
		}
		stream.meta, stream.revision = meta, *change.Revision
	default:
		return o.invalidate(id, stream)
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
	return normalizeTaskProvider(result)
}

func codexTaskReadTimedOut(err error) bool {
	if errors.Is(err, os.ErrDeadlineExceeded) || errors.Is(err, context.DeadlineExceeded) {
		return true
	}
	// Named-pipe deadline errors implement Timeout but not net.Error.
	var timeout interface{ Timeout() bool }
	return errors.As(err, &timeout) && timeout.Timeout()
}
