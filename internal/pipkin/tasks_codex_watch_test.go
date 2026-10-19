package pipkin

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"io"
	"net"
	"sync"
	"testing"
	"time"
)

// codexTestApp serves a fixture Codex app, one connection per dial, records every
// frame it receives, and can push unsolicited frames to the latest connection or
// drop it as if the app quit.
type codexTestApp struct {
	handler  func(map[string]any) []map[string]any
	mu       sync.Mutex
	dials    int
	conns    []*codexTestAppConn
	received []map[string]any
}

type codexTestAppConn struct {
	server net.Conn
	out    chan map[string]any
	done   chan struct{}
	once   sync.Once
}

func (c *codexTestAppConn) close() {
	c.once.Do(func() {
		close(c.done)
		c.server.Close()
	})
}

func (c *codexTestAppConn) push(message map[string]any) {
	select {
	case c.out <- message:
	case <-c.done:
	}
}

func newCodexTestApp(t *testing.T, handler func(map[string]any) []map[string]any) *codexTestApp {
	app := &codexTestApp{handler: handler}
	t.Cleanup(func() {
		app.mu.Lock()
		defer app.mu.Unlock()
		for _, conn := range app.conns {
			conn.close()
		}
	})
	return app
}

func (a *codexTestApp) dial(context.Context) (net.Conn, error) {
	client, server := net.Pipe()
	conn := &codexTestAppConn{server: server, out: make(chan map[string]any, 128), done: make(chan struct{})}
	a.mu.Lock()
	a.dials++
	a.conns = append(a.conns, conn)
	a.mu.Unlock()
	go func() {
		for {
			select {
			case message := <-conn.out:
				body, _ := json.Marshal(message)
				frame := binary.LittleEndian.AppendUint32(nil, uint32(len(body)))
				if _, err := server.Write(append(frame, body...)); err != nil {
					conn.close()
					return
				}
			case <-conn.done:
				return
			}
		}
	}()
	go func() {
		defer conn.close()
		for {
			var header [4]byte
			if _, err := io.ReadFull(server, header[:]); err != nil {
				return
			}
			size := binary.LittleEndian.Uint32(header[:])
			if size > 4096 {
				return
			}
			body := make([]byte, size)
			if _, err := io.ReadFull(server, body); err != nil {
				return
			}
			var message map[string]any
			if json.Unmarshal(body, &message) != nil {
				return
			}
			a.mu.Lock()
			a.received = append(a.received, message)
			a.mu.Unlock()
			for _, response := range a.handler(message) {
				conn.push(response)
			}
		}
	}()
	return client, nil
}

func (a *codexTestApp) latest() *codexTestAppConn {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.conns[len(a.conns)-1]
}

func (a *codexTestApp) dialCount() int {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.dials
}

func (a *codexTestApp) sent() []map[string]any {
	a.mu.Lock()
	defer a.mu.Unlock()
	return append([]map[string]any(nil), a.received...)
}

func codexTestState(id string, revision int, runtime, status string) map[string]any {
	snapshot := codexTestSnapshot(runtime, status)
	snapshot["id"] = id
	return map[string]any{"type": "broadcast", "method": "thread-stream-state-changed", "version": 11, "sourceClientId": codexTestOwner,
		"params": map[string]any{"conversationId": id, "hostId": "local", "change": map[string]any{"type": "snapshot", "revision": revision, "conversationState": snapshot}}}
}

// codexTestFollowingHandler owns every chat and answers each follow with a working snapshot.
func codexTestFollowingHandler(message map[string]any) []map[string]any {
	switch message["method"] {
	case "initialize":
		return []map[string]any{{"type": "response", "method": "initialize", "requestId": message["requestId"], "resultType": "success"}}
	case "thread-owner-discovery":
		return []map[string]any{{"type": "response", "method": "thread-owner-discovery", "requestId": message["requestId"], "resultType": "success", "handledByClientId": codexTestOwner}}
	case "thread-stream-following-changed":
		params := message["params"].(map[string]any)
		if params["following"] == true {
			return []map[string]any{codexTestState(params["conversationId"].(string), 1, "active", "inProgress")}
		}
	}
	return nil
}

func codexTestFollowingApp(t *testing.T) *codexTestApp {
	return newCodexTestApp(t, codexTestFollowingHandler)
}

func codexTestWaitStatus(t *testing.T, changed <-chan struct{}, watch *codexTaskWatch, coverage, status string) {
	t.Helper()
	timeout := time.After(2 * time.Second)
	for {
		current := watch.current()
		if current.Coverage == coverage && (status == "" || len(current.Tasks) == 1 && current.Tasks[0].Status == status) {
			return
		}
		select {
		case <-changed:
		case <-timeout:
			t.Fatalf("watch did not reach %s %s: %+v", coverage, status, current)
		}
	}
}

func TestCodexTaskWatchFollowsLiveChangesAndReconnects(t *testing.T) {
	app := codexTestFollowingApp(t)
	changed := make(chan struct{}, 16)
	watch := startCodexTaskWatch(context.Background(), codexTaskOptions{dial: app.dial,
		candidates: func(context.Context) []string { return []string{codexTestID1} },
		retry:      []time.Duration{200 * time.Millisecond}}, func() { changed <- struct{}{} })
	defer func() {
		watch.close()
		<-watch.done
	}()
	codexTestWaitStatus(t, changed, watch, "partial", "working")

	// A later state arrives over the same connection, without another check.
	started := time.Now()
	app.latest().push(codexTestState(codexTestID1, 2, "idle", "completed"))
	codexTestWaitStatus(t, changed, watch, "partial", "idle")
	if elapsed := time.Since(started); elapsed > time.Second {
		t.Fatalf("live change took %v", elapsed)
	}
	if app.dialCount() != 1 {
		t.Fatal("a live change needed a new connection")
	}

	// Codex quitting makes tasks unavailable until the watch reconnects.
	app.latest().close()
	codexTestWaitStatus(t, changed, watch, "unavailable", "")
	codexTestWaitStatus(t, changed, watch, "partial", "working")
	if app.dialCount() != 2 {
		t.Fatalf("dials = %d, want a single reconnect", app.dialCount())
	}
}

func TestCodexTaskWatchStopsOnClose(t *testing.T) {
	app := codexTestFollowingApp(t)
	watch := startCodexTaskWatch(context.Background(), codexTaskOptions{dial: app.dial,
		candidates: func(context.Context) []string { return []string{codexTestID1} }}, nil)
	watch.close()
	select {
	case <-watch.done:
	case <-time.After(2 * time.Second):
		t.Fatal("watch kept running after close")
	}
	if watch.current().Coverage != "unavailable" {
		t.Fatal("a closed watch still reports tasks")
	}
}

func codexTestLiveObserver(conn *codexTestFollowConn, streams map[string]*codexTaskStream) (*codexTaskObserver, *[]TaskProvider) {
	published := &[]TaskProvider{}
	now := time.Now()
	return &codexTaskObserver{ctx: context.Background(), conn: conn, live: true, initialized: true, connected: now.Add(-time.Minute), window: now,
		rediscover: now.Add(time.Hour), catalog: map[string]bool{codexTestID1: true}, queued: map[string]bool{},
		lookups: map[string]codexTaskLookup{}, attempts: map[string]int{}, streams: streams,
		publish: func(p TaskProvider) { *published = append(*published, p) }}, published
}

func TestCodexTaskWatchResyncsGapsWithinRateLimits(t *testing.T) {
	stream := &codexTaskStream{owner: codexTestOwner, valid: true, revision: 5, floor: 5,
		meta: codexTaskMeta{scope: 1, runtime: codexTaskRuntime{Type: "active", ActiveFlags: []string{}}}}
	conn := &codexTestFollowConn{}
	observer, _ := codexTestLiveObserver(conn, map[string]*codexTaskStream{codexTestID1: stream})
	gap := func(revision int) {
		t.Helper()
		change := map[string]any{"type": "patches", "revision": revision, "baseRevision": 0, "patches": []any{}}
		if err := observer.stream(codexTaskMessage{SourceClientID: codexTestOwner}, codexTaskParams{ConversationID: codexTestID1, Change: codexTestJSON(t, change)}); err != nil {
			t.Fatal(err)
		}
	}
	snapshot := func(revision int) {
		t.Helper()
		change := map[string]any{"type": "snapshot", "revision": revision, "conversationState": codexTestSnapshot("active", "inProgress")}
		if err := observer.stream(codexTaskMessage{SourceClientID: codexTestOwner}, codexTaskParams{ConversationID: codexTestID1, Change: codexTestJSON(t, change)}); err != nil || !stream.valid {
			t.Fatal("fresh snapshot did not restore the stream", err)
		}
	}
	gap(7)
	if stream.valid || !stream.waiting || len(conn.messages) != 2 {
		t.Fatalf("a gap did not request a fresh snapshot: %d messages", len(conn.messages))
	}
	snapshot(8)
	gap(10)
	snapshot(11)
	gap(13)
	if stream.valid || stream.waiting || !stream.resyncWanted || len(conn.messages) != 4 {
		t.Fatal("a third gap within the window resynced instead of waiting")
	}
	if err := observer.pump(time.Now(), false); err != nil || len(conn.messages) != 4 {
		t.Fatal("a deferred resync ran before its window", err)
	}
	if err := observer.pump(time.Now().Add(codexTaskStreamResyncWindow), false); err != nil || len(conn.messages) != 6 || !stream.waiting {
		t.Fatal("a deferred resync did not run once allowed", err)
	}
}

func TestCodexTaskWatchReleasesOwnerlessChatsAndRetriesLookups(t *testing.T) {
	stream := &codexTaskStream{owner: codexTestOwner, valid: true, revision: 1, floor: 1, meta: codexTaskMeta{scope: 1}}
	conn := &codexTestFollowConn{}
	observer, published := codexTestLiveObserver(conn, map[string]*codexTaskStream{codexTestID1: stream})
	if err := observer.receive(codexTestJSON(t, map[string]any{"type": "broadcast", "method": "client-status-changed",
		"params": map[string]any{"clientId": codexTestOwner, "status": "disconnected"}})); err != nil {
		t.Fatal(err)
	}
	if !stream.retired || observer.followed() != 0 || len(observer.queue) != 1 || observer.queue[0] != codexTestID1 {
		t.Fatal("an ownerless chat kept its slot or was not looked up again")
	}
	now := time.Now()
	for attempt := 1; attempt <= codexTaskLookupAttempts; attempt++ {
		if err := observer.pump(now, false); err != nil || len(observer.lookups) != 1 {
			t.Fatalf("attempt %d did not look up the chat: %v", attempt, err)
		}
		now = now.Add(codexTaskLookupWait)
	}
	if err := observer.pump(now, false); err != nil {
		t.Fatal(err)
	}
	if _, ok := observer.streams[codexTestID1]; ok || len(observer.lookups) != 0 {
		t.Fatal("a chat no app window answered for was still observed")
	}
	lookups := 0
	for _, message := range conn.messages {
		if message["method"] == "thread-owner-discovery" {
			lookups++
		}
	}
	if lookups != codexTaskLookupAttempts {
		t.Fatalf("lookups = %d, want %d", lookups, codexTaskLookupAttempts)
	}
	if last := (*published)[len(*published)-1]; last.Coverage != "unavailable" {
		t.Fatalf("published %+v after the only chat closed", last)
	}
}
