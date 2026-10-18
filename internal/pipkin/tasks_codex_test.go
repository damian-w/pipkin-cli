package pipkin

import (
	"context"
	"database/sql"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

const (
	codexTestID1   = "00000000-0000-4000-8000-000000000001"
	codexTestID2   = "00000000-0000-4000-8000-000000000002"
	codexTestID3   = "00000000-0000-4000-8000-000000000003"
	codexTestOwner = "00000000-0000-4000-8000-000000000010"
)

func codexTestJSON(t *testing.T, value any) json.RawMessage {
	t.Helper()
	raw, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func codexTestSnapshot(runtime, status string) map[string]any {
	return map[string]any{
		"id": codexTestID1, "updatedAt": 1767225600123,
		"source": "vscode", "threadSource": "user", "resumeState": "resumed",
		"threadRuntimeStatus": map[string]any{"type": runtime, "activeFlags": []string{}},
		"requests":            []any{}, "turns": []any{map[string]any{"turnId": "current", "status": status, "items": []any{}}},
	}
}

func TestCodexTaskStateProjection(t *testing.T) {
	cases := []struct {
		name, runtime, turn, want string
		change                    func(map[string]any)
	}{
		{"working", "active", "inProgress", "working", nil},
		{"idle", "idle", "completed", "idle", nil},
		{"interrupted", "idle", "interrupted", "idle", nil},
		{"system error", "systemError", "failed", "blocked", nil},
		{"failed turn alone", "idle", "failed", "unknown", nil},
		{"unknown runtime", "future", "completed", "unknown", nil},
		{"unknown source", "active", "inProgress", "unknown", func(s map[string]any) { s["source"] = "cli" }},
		{"approval flag", "active", "inProgress", "needs_attention", func(s map[string]any) {
			s["threadRuntimeStatus"] = map[string]any{"type": "active", "activeFlags": []string{"waitingOnApproval"}}
		}},
		{"input flag", "active", "inProgress", "needs_attention", func(s map[string]any) {
			s["threadRuntimeStatus"] = map[string]any{"type": "active", "activeFlags": []string{"waitingOnUserInput"}}
		}},
		{"unknown flag", "active", "inProgress", "unknown", func(s map[string]any) {
			s["threadRuntimeStatus"] = map[string]any{"type": "active", "activeFlags": []string{"futureFlag"}}
		}},
		{"option picker", "active", "inProgress", "needs_attention", func(s map[string]any) {
			s["requests"] = []any{map[string]any{"method": "item/tool/requestOptionPicker"}}
		}},
		{"completed elicitation", "idle", "completed", "idle", func(s map[string]any) {
			s["requests"] = []any{map[string]any{"method": "mcpServer/elicitation/request", "completed": true}}
		}},
		{"unknown request", "active", "inProgress", "unknown", func(s map[string]any) {
			s["requests"] = []any{map[string]any{"method": "item/tool/call", "params": map[string]any{"tool": "futureTool"}}}
		}},
		{"legacy request unknown", "active", "inProgress", "unknown", func(s map[string]any) {
			s["requests"] = []any{map[string]any{"method": "execCommandApproval"}}
		}},
		{"goal independent", "idle", "completed", "idle", func(s map[string]any) { s["threadGoal"] = map[string]any{"status": "blocked"} }},
		{"ephemeral side", "active", "inProgress", "working", func(s map[string]any) { s["ephemeral"], s["sideConversation"] = true, true }},
		{"ambiguous ephemeral", "active", "inProgress", "unknown", func(s map[string]any) { s["ephemeral"] = true }},
		{"incomplete tail", "idle", "completed", "unknown", func(s map[string]any) { s["resumeState"] = "needs_resume" }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			snapshot := codexTestSnapshot(tc.runtime, tc.turn)
			if tc.change != nil {
				tc.change(snapshot)
			}
			meta, err := codexTaskProject(codexTestJSON(t, snapshot), codexTestID1, false)
			if err != nil || meta.status() != tc.want {
				t.Fatalf("status=%q err=%v, want %q", meta.status(), err, tc.want)
			}
			if meta.updated == nil || *meta.updated != 1767225600 {
				t.Fatal("source timestamp must be Unix seconds")
			}
		})
	}
}

func TestCodexTaskNativeCardsAndAcceptedReplies(t *testing.T) {
	for _, kind := range []string{"unanswered", "user", "accepted", "unaccepted", "malformed"} {
		t.Run(kind, func(t *testing.T) {
			snapshot := codexTestSnapshot("idle", "completed")
			items := []any{map[string]any{"type": "agentMessage", "id": "question", "questions": []any{map[string]any{"question": "private prompt"}}}}
			if kind != "unanswered" {
				identity := string(codexTestJSON(t, []any{"request_user_input_async", "question", 0}))
				reply := map[string]any{"questionItemId": identity, "question": "private prompt", "answer": "private answer"}
				if kind == "malformed" {
					delete(reply, "answer")
				}
				text := "<send_user_message_question_reply>" + string(codexTestJSON(t, []any{reply})) + "</send_user_message_question_reply>"
				item := map[string]any{"type": "userMessage", "content": []any{map[string]any{"type": "text", "text": text}}}
				if kind == "accepted" || kind == "unaccepted" {
					item = map[string]any{"type": "steeringUserMessage", "status": "accepted", "input": item["content"]}
					if kind == "unaccepted" {
						item["status"] = "pending"
					}
				}
				items = append(items, item)
			}
			snapshot["turns"] = []any{map[string]any{"turnId": "current", "status": "completed", "items": items}}
			meta, err := codexTaskProject(codexTestJSON(t, snapshot), codexTestID1, false)
			if err != nil {
				t.Fatal(err)
			}
			want := "needs_attention"
			if kind == "user" || kind == "accepted" {
				want = "idle"
			}
			if meta.status() != want {
				t.Fatalf("got %s, want %s", meta.status(), want)
			}
			observer := codexTaskObserver{streams: map[string]*codexTaskStream{codexTestID1: {meta: meta, valid: true}}}
			output := string(codexTestJSON(t, observer.result()))
			for _, private := range []string{"private prompt", "private answer", "questionItemId", codexTestID1} {
				if strings.Contains(output, private) {
					t.Fatalf("private field leaked: %s", private)
				}
			}
		})
	}
}

func codexTestCrossTurnReply(t *testing.T, kind string) map[string]any {
	t.Helper()
	if kind == "unanswered" {
		return map[string]any{"type": "userMessage", "content": []any{map[string]any{"type": "text", "text": "An independent follow-up"}}}
	}
	answerID := "older-question"
	if kind == "wrong identity" {
		answerID = "other-question"
	}
	rows := []any{}
	for index := range 2 {
		row := map[string]any{"questionItemId": string(codexTestJSON(t, []any{"request_user_input_async", answerID, index})), "question": "private question", "answer": "private answer"}
		if kind == "malformed" {
			delete(row, "answer")
		}
		rows = append(rows, row)
		if kind == "partial answer" {
			break
		}
	}
	text := "<send_user_message_question_reply>" + string(codexTestJSON(t, rows)) + "</send_user_message_question_reply>"
	content := []any{map[string]any{"type": "text", "text": text}}
	if kind == "accepted" || kind == "unaccepted" {
		status := "accepted"
		if kind == "unaccepted" {
			status = "pending"
		}
		return map[string]any{"type": "steeringUserMessage", "status": status, "input": content}
	}
	return map[string]any{"type": "userMessage", "content": content}
}

func codexTestCrossTurnSnapshot(t *testing.T, canonical bool, kind string) map[string]any {
	t.Helper()
	snapshot := codexTestSnapshot("idle", "completed")
	older := map[string]any{"turnId": "older", "status": "completed", "items": []any{map[string]any{
		"type": "agentMessage", "id": "older-question", "questions": []any{map[string]any{"title": "private first"}, map[string]any{"title": "private second"}},
	}}}
	latest := map[string]any{"turnId": "latest", "status": "completed", "items": []any{codexTestCrossTurnReply(t, kind)}}
	snapshot["turns"] = []any{older, latest}
	if canonical {
		snapshot["turns"] = []any{map[string]any{"status": "failed", "items": []any{}}}
		snapshot["turnHistory"] = map[string]any{"kind": "canonical", "history": map[string]any{
			"islands":       []any{map[string]any{"newerBoundary": map[string]any{"status": "exhausted"}, "entries": []any{map[string]any{"value": "older"}, map[string]any{"value": "latest"}}}},
			"entitiesByKey": map[string]any{"older": older, "latest": latest, "orphan": map[string]any{"status": "failed", "items": []any{map[string]any{"type": "agentMessage", "id": "orphan-question", "questions": []any{map[string]any{"title": "private orphan"}}}}}},
		}}
	}
	return snapshot
}

func TestCodexTaskCardsSpanTurnsAndUseAcceptedLaterReplies(t *testing.T) {
	for _, canonical := range []bool{false, true} {
		for _, kind := range []string{"unanswered", "user", "accepted", "unaccepted", "malformed", "wrong identity", "partial answer"} {
			name := "legacy/" + kind
			if canonical {
				name = "canonical/" + kind
			}
			t.Run(name, func(t *testing.T) {
				snapshot := codexTestCrossTurnSnapshot(t, canonical, kind)
				meta, err := codexTaskProject(codexTestJSON(t, snapshot), codexTestID1, false)
				if err != nil {
					t.Fatal(err)
				}
				want, count := "needs_attention", 2
				if kind == "user" || kind == "accepted" {
					want, count = "idle", 0
				}
				if kind == "partial answer" {
					count = 1
				}
				if meta.status() != want || meta.pendingCards != count {
					t.Fatalf("cross-turn state=%s cards=%d, want %s/%d", meta.status(), meta.pendingCards, want, count)
				}
				if kind == "unanswered" {
					snapshot["threadRuntimeStatus"] = map[string]any{"type": "active", "activeFlags": []string{}}
					meta, err = codexTaskProject(codexTestJSON(t, snapshot), codexTestID1, false)
					if err != nil || meta.status() != "needs_attention" {
						t.Fatal("new work hid an older card", err)
					}
				}
				out := string(codexTestJSON(t, (&codexTaskObserver{streams: map[string]*codexTaskStream{codexTestID1: {meta: meta, valid: true}}}).result()))
				for _, private := range []string{"private", "older-question", "questionItemId", codexTestID1} {
					if strings.Contains(out, private) {
						t.Fatal("cross-turn projection leaked private metadata")
					}
				}
			})
		}
	}
}

func TestCodexTaskCardsBeforeHistoryGapRemainUncertain(t *testing.T) {
	for _, kind := range []string{"unanswered", "user", "accepted"} {
		snapshot := codexTestCrossTurnSnapshot(t, true, kind)
		history := snapshot["turnHistory"].(map[string]any)["history"].(map[string]any)
		history["islands"] = []any{
			map[string]any{"newerBoundary": map[string]any{"status": "hasMore"}, "entries": []any{map[string]any{"value": "older"}}},
			map[string]any{"newerBoundary": map[string]any{"status": "exhausted"}, "entries": []any{map[string]any{"value": "latest"}}},
		}
		meta, err := codexTaskProject(codexTestJSON(t, snapshot), codexTestID1, false)
		want := "idle"
		if kind == "unanswered" {
			want = "unknown"
		}
		if err != nil || meta.status() != want {
			t.Fatalf("gapped %s state=%s err=%v", kind, meta.status(), err)
		}
	}
}

type codexTestFollowConn struct {
	net.Conn
	messages []map[string]any
}

func (conn *codexTestFollowConn) SetWriteDeadline(time.Time) error { return nil }

func (conn *codexTestFollowConn) Write(frame []byte) (int, error) {
	var message map[string]any
	if len(frame) < 4 || int(binary.LittleEndian.Uint32(frame)) != len(frame)-4 || json.Unmarshal(frame[4:], &message) != nil {
		return 0, errCodexTaskState
	}
	conn.messages = append(conn.messages, message)
	return len(frame), nil
}

func TestCodexTaskCrossTurnReplyPatchesRequireFreshProjection(t *testing.T) {
	snapshot := codexTestCrossTurnSnapshot(t, true, "unanswered")
	meta, err := codexTaskProject(codexTestJSON(t, snapshot), codexTestID1, true)
	if err != nil {
		t.Fatal(err)
	}
	for _, path := range [][]any{
		{"turns", float64(1), "items", float64(0)},
		{"turnHistory", "history", "entitiesByKey", "latest", "items", float64(0)},
		{"turnHistory", "history", "entitiesByKey", "latest", "items", float64(0), "status"},
	} {
		var value any = codexTestCrossTurnReply(t, "accepted")
		if path[len(path)-1] == "status" {
			value = "accepted"
		}
		refresh, err := codexTaskApply(&meta, codexTaskPatch{Op: "replace", Path: path, Value: codexTestJSON(t, value)})
		if err != nil || !refresh || meta.status() != "needs_attention" {
			t.Fatal("cross-turn reply guessed clearing instead of requesting a snapshot", path, err)
		}
	}
	answered := codexTestCrossTurnSnapshot(t, true, "accepted")
	stream := &codexTaskStream{owner: codexTestOwner, valid: true, revision: 5, floor: 5, meta: meta}
	conn := &codexTestFollowConn{}
	observer := codexTaskObserver{conn: conn, deadline: time.Now().Add(time.Second), catalog: map[string]bool{codexTestID1: true}, streams: map[string]*codexTaskStream{codexTestID1: stream}}
	patchChange := map[string]any{"type": "patches", "revision": 6, "baseRevision": 5, "patches": []any{map[string]any{
		"op": "replace", "path": []any{"turnHistory", "history", "entitiesByKey", "latest", "items", 0}, "value": codexTestCrossTurnReply(t, "accepted"),
	}}}
	if err := observer.stream(codexTaskMessage{SourceClientID: codexTestOwner}, codexTaskParams{ConversationID: codexTestID1, Change: codexTestJSON(t, patchChange)}); err != nil {
		t.Fatal(err)
	}
	if stream.valid || !stream.waiting || observer.result().Tasks[0].Status != "unknown" || len(conn.messages) != 2 {
		t.Fatal("cross-turn reply did not invalidate and resubscribe before clearing")
	}
	for index, message := range conn.messages {
		if message["method"] != "thread-stream-following-changed" || message["params"].(map[string]any)["following"] != (index == 1) || message["targetClientIds"].([]any)[0] != codexTestOwner {
			t.Fatal("resync sent an unexpected action or owner target")
		}
	}
	change := map[string]any{"type": "snapshot", "revision": 6, "conversationState": answered}
	if err := observer.stream(codexTaskMessage{SourceClientID: codexTestOwner}, codexTaskParams{ConversationID: codexTestID1, Change: codexTestJSON(t, change)}); err != nil || observer.result().Tasks[0].Status != "idle" {
		t.Fatal("fresh owner snapshot did not clear answered cross-turn cards", err)
	}
}

func TestCodexTaskMalformedMetadataPatchesInvalidateState(t *testing.T) {
	for _, c := range []struct {
		path  []any
		value any
	}{
		{[]any{"threadRuntimeStatus", "type"}, nil},
		{[]any{"threadRuntimeStatus", "type"}, ""},
		{[]any{"threadRuntimeStatus"}, nil},
		{[]any{"threadRuntimeStatus"}, map[string]any{}},
		{[]any{"threadRuntimeStatus"}, map[string]any{"type": "systemError", "activeFlags": nil}},
		{[]any{"threadRuntimeStatus", "activeFlags"}, nil},
		{[]any{"threadRuntimeStatus", "activeFlags"}, []any{nil}},
		{[]any{"requests"}, nil},
		{[]any{"requests"}, []any{nil}},
		{[]any{"requests"}, []any{map[string]any{}}},
		{[]any{"updatedAt", "nested"}, 1767225600000},
	} {
		t.Run(string(codexTestJSON(t, c.path))+string(codexTestJSON(t, c.value)), func(t *testing.T) {
			meta, err := codexTaskProject(codexTestJSON(t, codexTestSnapshot("systemError", "failed")), codexTestID1, true)
			if err != nil {
				t.Fatal(err)
			}
			stream := &codexTaskStream{owner: codexTestOwner, valid: true, revision: 1, floor: 1, meta: meta}
			observer := codexTaskObserver{streams: map[string]*codexTaskStream{codexTestID1: stream}}
			patch := map[string]any{"op": "replace", "path": c.path, "value": c.value}
			change := map[string]any{"type": "patches", "revision": 2, "baseRevision": 1, "patches": []any{patch}}
			if err := observer.stream(codexTaskMessage{SourceClientID: codexTestOwner}, codexTaskParams{ConversationID: codexTestID1, Change: codexTestJSON(t, change)}); err != nil {
				t.Fatal(err)
			}
			if stream.valid || observer.result().Tasks[0].Status != "unknown" {
				t.Fatal("malformed patch retained a trusted status")
			}
		})
	}
	meta, _ := codexTaskProject(codexTestJSON(t, codexTestSnapshot("active", "inProgress")), codexTestID1, true)
	for _, c := range []struct {
		path  []any
		value any
	}{
		{[]any{"requests"}, []any{}},
		{[]any{"threadRuntimeStatus", "activeFlags"}, []string{}},
	} {
		if refresh, err := codexTaskApply(&meta, codexTaskPatch{Op: "replace", Path: c.path, Value: codexTestJSON(t, c.value)}); err != nil || refresh || meta.status() != "working" {
			t.Fatal("valid empty arrays became unavailable", err)
		}
	}
}

func TestCodexTaskCanonicalTailAndSourceExclusions(t *testing.T) {
	snapshot := codexTestSnapshot("idle", "failed")
	snapshot["turnHistory"] = map[string]any{"kind": "canonical", "history": map[string]any{
		"islands":       []any{map[string]any{"newerBoundary": map[string]any{"status": "exhausted"}, "entries": []any{map[string]any{"value": "old"}, map[string]any{"value": "new"}}}},
		"entitiesByKey": map[string]any{"old": map[string]any{"status": "failed", "turnStartedAtMs": 1767225600000}, "new": map[string]any{"status": "completed"}},
	}}
	meta, err := codexTaskProject(codexTestJSON(t, snapshot), codexTestID1, false)
	if err != nil || meta.status() != "idle" {
		t.Fatalf("canonical tail must use entry order: %+v %v", meta, err)
	}
	snapshot["source"] = map[string]any{"subagent": map[string]any{"thread_spawn": map[string]any{"parent_thread_id": codexTestID2}}}
	meta, err = codexTaskProject(codexTestJSON(t, snapshot), codexTestID1, false)
	observer := codexTaskObserver{streams: map[string]*codexTaskStream{codexTestID1: {valid: true, meta: meta}}}
	if err != nil || len(observer.result().Tasks) != 0 {
		t.Fatal("subagent included in user task count")
	}
	if _, err := codexTaskProject(codexTestJSON(t, snapshot), codexTestID2, true); err == nil {
		t.Fatal("wrong conversation identity accepted")
	}
}

func TestCodexTaskPatchValidity(t *testing.T) {
	meta, _ := codexTaskProject(codexTestJSON(t, codexTestSnapshot("active", "inProgress")), codexTestID1, true)
	for _, tc := range []struct {
		path    []any
		value   any
		refresh bool
	}{
		{[]any{"threadRuntimeStatus", "activeFlags"}, []string{"waitingOnUserInput"}, false},
		{[]any{"requests", float64(0)}, map[string]any{}, true},
		{[]any{"turnHistory", "history", "entitiesByKey", "current", "status"}, "failed", true},
		{[]any{"turnHistory", "history", "entitiesByKey", "current", "items", float64(2)}, map[string]any{"type": "commandExecution", "aggregatedOutput": "private output"}, false},
		{[]any{"turnHistory", "history", "entitiesByKey", "current", "items", float64(2)}, map[string]any{"type": "agentMessage", "questions": []any{map[string]any{}}}, true},
	} {
		refresh, err := codexTaskApply(&meta, codexTaskPatch{Op: "replace", Path: tc.path, Value: codexTestJSON(t, tc.value)})
		if err != nil || refresh != tc.refresh {
			t.Fatalf("patch %v refresh=%v err=%v", tc.path, refresh, err)
		}
	}
	if _, err := codexTaskApply(&meta, codexTaskPatch{Op: "replace", Path: []any{"threadRuntimeStatus"}, Value: codexTestJSON(t, map[string]any{"type": "active"})}); err != nil || len(meta.runtime.ActiveFlags) != 0 {
		t.Fatal("replacement runtime retained stale waiting flags")
	}
}

func TestCodexTaskStreamGapAndOwnerInvalidate(t *testing.T) {
	for _, changeOwner := range []bool{false, true} {
		observer := codexTaskObserver{catalog: map[string]bool{codexTestID1: true}, streams: map[string]*codexTaskStream{codexTestID1: {owner: codexTestOwner, valid: true, revision: 2, meta: codexTaskMeta{scope: 1, runtime: codexTaskRuntime{Type: "active"}}}}}
		owner := codexTestOwner
		if changeOwner {
			owner = codexTestID2
		}
		change := map[string]any{"type": "patches", "revision": 4, "baseRevision": 3, "patches": []any{}}
		if err := observer.stream(codexTaskMessage{SourceClientID: owner}, codexTaskParams{ConversationID: codexTestID1, Change: codexTestJSON(t, change)}); err != nil {
			t.Fatal(err)
		}
		if result := observer.result(); len(result.Tasks) != 1 || result.Tasks[0].Status != "unknown" || result.Tasks[0].UpdatedAt != nil {
			t.Fatalf("invalidated stream kept status: %+v", result)
		}
	}
}

func codexTestBroker(t *testing.T, handler func(map[string]any) []map[string]any) (func(context.Context) (net.Conn, error), func() []map[string]any) {
	t.Helper()
	client, server := net.Pipe()
	var mu sync.Mutex
	var messages []map[string]any
	out := make(chan map[string]any, 128)
	done := make(chan struct{})
	go func() {
		defer server.Close()
		for message := range out {
			body, err := json.Marshal(message)
			if err != nil {
				return
			}
			frame := make([]byte, len(body)+4)
			binary.LittleEndian.PutUint32(frame, uint32(len(body)))
			copy(frame[4:], body)
			if _, err := server.Write(frame); err != nil {
				return
			}
		}
	}()
	go func() {
		defer close(out)
		defer close(done)
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
			mu.Lock()
			messages = append(messages, message)
			mu.Unlock()
			for _, response := range handler(message) {
				out <- response
			}
		}
	}()
	t.Cleanup(func() {
		client.Close()
		server.Close()
		select {
		case <-done:
		case <-time.After(time.Second):
			t.Error("fixture broker did not stop")
		}
	})
	return func(context.Context) (net.Conn, error) { return client, nil }, func() []map[string]any {
		mu.Lock()
		defer mu.Unlock()
		return append([]map[string]any(nil), messages...)
	}
}

func TestCodexTaskCollectorFindsBackgroundWithoutTaskActions(t *testing.T) {
	for _, fileTimeout := range []bool{false, true} {
		name := "network_timeout"
		if fileTimeout {
			name = "wrapped_file_timeout"
		}
		t.Run(name, func(t *testing.T) { codexTestBackgroundCollection(t, fileTimeout) })
	}
}

type codexTestFileTimeoutConn struct{ net.Conn }

func (c codexTestFileTimeoutConn) Read(buffer []byte) (int, error) {
	n, err := c.Conn.Read(buffer)
	var timeout interface{ Timeout() bool }
	if errors.As(err, &timeout) && timeout.Timeout() {
		return n, &os.PathError{Op: "read", Path: "fixture-pipe", Err: os.ErrDeadlineExceeded}
	}
	return n, err
}

func codexTestBackgroundCollection(t *testing.T, fileTimeout bool) {
	t.Helper()
	dial, sent := codexTestBroker(t, func(message map[string]any) []map[string]any {
		switch message["method"] {
		case "initialize":
			return []map[string]any{
				{"type": "response", "method": "initialize", "requestId": message["requestId"], "resultType": "success"},
				{"type": "client-discovery-request", "requestId": "challenge"},
				{"type": "broadcast", "method": "thread-stream-following-changed", "version": 1, "params": map[string]any{"conversationId": codexTestID1, "hostId": "local", "following": true}},
			}
		case "thread-owner-discovery":
			return []map[string]any{{"type": "response", "method": "thread-owner-discovery", "requestId": message["requestId"], "resultType": "success", "handledByClientId": codexTestOwner}}
		case "thread-stream-following-changed":
			params := message["params"].(map[string]any)
			if params["following"] != true {
				return nil
			}
			id := params["conversationId"].(string)
			snapshot := codexTestSnapshot("active", "inProgress")
			snapshot["id"] = id
			if id == codexTestID1 {
				snapshot = codexTestSnapshot("idle", "completed")
				snapshot["turns"] = []any{map[string]any{"status": "completed", "items": []any{map[string]any{"type": "agentMessage", "id": "native", "questions": []any{map[string]any{"question": "private question"}}}}}}
			}
			return []map[string]any{{"type": "broadcast", "method": "thread-stream-state-changed", "version": 11, "sourceClientId": codexTestOwner,
				"params": map[string]any{"conversationId": id, "hostId": "local", "change": map[string]any{"type": "snapshot", "revision": 1, "conversationState": snapshot}}}}
		}
		return nil
	})
	if fileTimeout {
		originalDial := dial
		dial = func(ctx context.Context) (net.Conn, error) {
			conn, err := originalDial(ctx)
			if err != nil {
				return nil, err
			}
			return codexTestFileTimeoutConn{conn}, nil
		}
	}
	result := collectCodexTasksWith(context.Background(), codexTaskOptions{dial: dial, budget: 180 * time.Millisecond,
		candidates: func(context.Context) []string { return []string{codexTestID2} }})
	if result.Coverage != "partial" || len(result.Tasks) != 2 {
		t.Fatalf("result=%+v", result)
	}
	statuses := map[string]string{}
	for _, task := range result.Tasks {
		statuses[task.ID] = task.Status
	}
	if statuses[taskOpaqueID("codex", codexTestID1)] != "needs_attention" || statuses[taskOpaqueID("codex", codexTestID2)] != "working" {
		t.Fatalf("statuses=%v", statuses)
	}
	negative := false
	for _, message := range sent() {
		method, _ := message["method"].(string)
		if method == "thread-stream-following-changed" {
			targets, ok := message["targetClientIds"].([]any)
			if !ok || len(targets) != 1 || targets[0] != codexTestOwner {
				t.Fatal("following broadcast was not restricted to confirmed owner")
			}
		}
		if method != "" && method != "initialize" && method != "thread-owner-discovery" && method != "thread-stream-following-changed" {
			t.Fatalf("task action emitted: %s", method)
		}
		if message["type"] == "client-discovery-response" {
			negative = message["response"].(map[string]any)["canHandle"] == false
		}
	}
	if !negative {
		t.Fatal("observer did not decline ownership")
	}
}

func TestCodexTaskCatalogIsReadOnlyAndFiltersSources(t *testing.T) {
	home := t.TempDir()
	path := filepath.Join(home, "state_5.sqlite")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec("CREATE TABLE threads (id TEXT,source TEXT,thread_source TEXT,archived INTEGER,updated_at_ms INTEGER)"); err != nil {
		t.Fatal(err)
	}
	for i, row := range []struct{ id, source, kind string }{{codexTestID1, `"vscode"`, "user"}, {codexTestID2, `"cli"`, "user"}, {codexTestID3, `"vscode"`, "chatgpt_hidden"}} {
		if _, err := db.Exec("INSERT INTO threads VALUES (?,?,?,0,?)", row.id, row.source, row.kind, i); err != nil {
			t.Fatal(err)
		}
	}
	db.Close()
	before, _ := os.ReadFile(path)
	got := codexTaskCandidates(context.Background(), home, 64)
	after, _ := os.ReadFile(path)
	if len(got) != 1 || got[0] != codexTestID1 || string(before) != string(after) {
		t.Fatalf("catalog changed or wrong candidates: %v", got)
	}
}

func TestCodexTaskCollectorRejectsVersionsResetAndDisconnect(t *testing.T) {
	for _, kind := range []string{"version", "reset", "disconnect"} {
		t.Run(kind, func(t *testing.T) {
			dial, _ := codexTestBroker(t, func(message map[string]any) []map[string]any {
				if message["method"] != "initialize" {
					return nil
				}
				responses := []map[string]any{{"type": "response", "method": "initialize", "requestId": message["requestId"], "resultType": "success"}}
				if kind == "version" {
					return append(responses, map[string]any{"type": "broadcast", "method": "thread-stream-state-changed", "version": 99, "params": map[string]any{"conversationId": codexTestID1, "hostId": "local"}})
				}
				if kind == "reset" {
					return append(responses, map[string]any{"type": "broadcast", "method": "ipc-connection-reset", "version": 1, "params": map[string]any{}})
				}
				return nil
			})
			if kind == "disconnect" {
				dial = func(context.Context) (net.Conn, error) {
					client, server := net.Pipe()
					server.Close()
					return client, nil
				}
			}
			result := collectCodexTasksWith(context.Background(), codexTaskOptions{dial: dial, budget: 80 * time.Millisecond})
			if result.Coverage != "unavailable" || result.Tasks != nil {
				t.Fatalf("unsafe evidence accepted: %+v", result)
			}
		})
	}
}

func TestCodexTaskCollectorFrameCapAndCancellation(t *testing.T) {
	client, server := net.Pipe()
	defer server.Close()
	go func() {
		var header [4]byte
		if _, err := io.ReadFull(server, header[:]); err != nil {
			return
		}
		body := make([]byte, binary.LittleEndian.Uint32(header[:]))
		if _, err := io.ReadFull(server, body); err != nil {
			return
		}
		binary.LittleEndian.PutUint32(header[:], codexTaskMaxFrame+1)
		server.Write(header[:])
	}()
	result := collectCodexTasksWith(context.Background(), codexTaskOptions{dial: func(context.Context) (net.Conn, error) { return client, nil }, budget: 80 * time.Millisecond})
	if result.Coverage != "unavailable" || result.Tasks != nil {
		t.Fatal("oversized frame accepted")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	result = collectCodexTasksWith(ctx, codexTaskOptions{dial: func(context.Context) (net.Conn, error) { t.Fatal("dialed with canceled context"); return nil, nil }})
	if result.Coverage != "unavailable" {
		t.Fatal("canceled collection has evidence")
	}
}

func TestCodexTaskAnnouncementPrioritizesExistingCatalogCandidate(t *testing.T) {
	observer := codexTaskObserver{queued: map[string]bool{}}
	observer.enqueue(codexTestID1, false)
	observer.enqueue(codexTestID2, false)
	observer.enqueue(codexTestID2, true)
	if len(observer.queue) != 2 || observer.queue[0] != codexTestID2 {
		t.Fatalf("foreground announcement did not precede historical candidate: %v", observer.queue)
	}
}

func TestCodexTaskStaleSnapshotsCannotRepairInvalidatedStream(t *testing.T) {
	for _, cause := range []string{"gap", "unsupported patch", "owner change", "owner disconnected"} {
		t.Run(cause, func(t *testing.T) {
			stream := &codexTaskStream{owner: codexTestOwner, valid: true, revision: 5, floor: 5,
				meta: codexTaskMeta{scope: 1, runtime: codexTaskRuntime{Type: "active", ActiveFlags: []string{}}}}
			observer := codexTaskObserver{initialized: true, catalog: map[string]bool{codexTestID1: true}, streams: map[string]*codexTaskStream{codexTestID1: stream}, resyncs: 6}
			owner := codexTestOwner
			base := 5
			patches := []any{map[string]any{"op": "replace", "path": []any{"turns", 0, "status"}, "value": "failed"}}
			switch cause {
			case "gap":
				base = 4
			case "owner change":
				owner = codexTestID2
			case "owner disconnected":
				if err := observer.receive(codexTestJSON(t, map[string]any{"type": "broadcast", "method": "client-status-changed", "params": map[string]any{"clientId": codexTestOwner, "status": "disconnected"}})); err != nil {
					t.Fatal(err)
				}
			}
			if err := observer.stream(codexTaskMessage{SourceClientID: owner}, codexTaskParams{ConversationID: codexTestID1, Change: codexTestJSON(t, map[string]any{"type": "patches", "revision": 6, "baseRevision": base, "patches": patches})}); err != nil {
				t.Fatal(err)
			}
			if stream.valid {
				t.Fatal("fixture did not invalidate")
			}
			if err := observer.stream(codexTaskMessage{SourceClientID: codexTestOwner}, codexTaskParams{ConversationID: codexTestID1, Change: codexTestJSON(t, map[string]any{"type": "snapshot", "revision": 4, "conversationState": codexTestSnapshot("idle", "completed")})}); err != nil {
				t.Fatal(err)
			}
			if stream.valid || observer.result().Tasks[0].Status != "unknown" {
				t.Fatal("stale snapshot revived status after invalidation")
			}
		})
	}
}

func TestCodexTaskPlanOfferDoesNotImplyAttention(t *testing.T) {
	snapshot := codexTestSnapshot("idle", "completed")
	snapshot["turns"] = []any{map[string]any{"status": "completed", "items": []any{map[string]any{"type": "planImplementation", "isCompleted": false}}}}
	snapshot["requests"] = []any{map[string]any{"method": "item/plan/requestImplementation", "id": "implement-plan:current", "params": map[string]any{"threadId": codexTestID1, "turnId": "current", "planContent": "private plan"}}}
	meta, err := codexTaskProject(codexTestJSON(t, snapshot), codexTestID1, false)
	if err != nil || meta.status() != "idle" {
		t.Fatalf("plan offer became attention: %s %v", meta.status(), err)
	}
	snapshot["threadRuntimeStatus"] = map[string]any{"type": "active", "activeFlags": []string{}}
	meta, err = codexTaskProject(codexTestJSON(t, snapshot), codexTestID1, false)
	if err != nil || meta.status() != "working" {
		t.Fatalf("a nonblocking plan offer hid current work: %s %v", meta.status(), err)
	}
}

func TestCodexTaskHistoricalQuestionProjectionIsBounded(t *testing.T) {
	snapshot := codexTestSnapshot("idle", "completed")
	turns := make([]any, codexTaskMaxHistoryTurns+1)
	for index := range turns {
		turns[index] = map[string]any{"status": "completed", "items": []any{}}
	}
	snapshot["turns"] = turns
	if _, err := codexTaskProject(codexTestJSON(t, snapshot), codexTestID1, false); err == nil {
		t.Fatal("unbounded loaded history was accepted")
	}
	snapshot = codexTestSnapshot("idle", "completed")
	items := []any{}
	for index := range codexTaskMaxQuestionKeys/64 + 1 {
		rows := make([]any, 64)
		for row := range rows {
			rows[row] = map[string]any{"title": "private"}
		}
		items = append(items, map[string]any{"type": "agentMessage", "id": fmt.Sprint(index), "questions": rows})
	}
	snapshot["turns"] = []any{map[string]any{"status": "completed", "items": items}}
	if _, err := codexTaskProject(codexTestJSON(t, snapshot), codexTestID1, false); err == nil {
		t.Fatal("unbounded question identities were accepted")
	}
}

func TestCodexTaskKnownInternalRemainsExcludedAfterInvalidation(t *testing.T) {
	for _, cause := range []string{"gap", "owner loss", "invalid snapshot", "ambiguous snapshot"} {
		t.Run(cause, func(t *testing.T) {
			stream := &codexTaskStream{owner: codexTestOwner, valid: true, revision: 1, floor: 1, meta: codexTaskMeta{scope: 0}}
			observer := codexTaskObserver{initialized: true, catalog: map[string]bool{}, streams: map[string]*codexTaskStream{codexTestID1: stream}}
			change := map[string]any{"type": "patches", "revision": 3, "baseRevision": 2, "patches": []any{}}
			switch cause {
			case "owner loss":
				if err := observer.receive(codexTestJSON(t, map[string]any{"type": "broadcast", "method": "client-status-changed", "params": map[string]any{"clientId": codexTestOwner, "status": "disconnected"}})); err != nil {
					t.Fatal(err)
				}
			case "invalid snapshot", "ambiguous snapshot":
				snapshot := codexTestSnapshot("active", "inProgress")
				if cause == "invalid snapshot" {
					snapshot["id"] = codexTestID2
				} else {
					snapshot["source"] = "unknown"
				}
				change = map[string]any{"type": "snapshot", "revision": 3, "conversationState": snapshot}
			}
			if err := observer.stream(codexTaskMessage{SourceClientID: codexTestOwner}, codexTaskParams{ConversationID: codexTestID1, Change: codexTestJSON(t, change)}); err != nil {
				t.Fatal(err)
			}
			if result := observer.result(); len(result.Tasks) != 0 {
				t.Fatalf("known internal thread reappeared: %+v", result)
			}
		})
	}
	observer := codexTaskObserver{streams: map[string]*codexTaskStream{codexTestID1: {meta: codexTaskMeta{scope: 2}, waiting: true}}}
	if result := observer.result(); len(result.Tasks) != 1 || result.Tasks[0].Status != "unknown" {
		t.Fatal("unclassified owned candidate was silently excluded")
	}
}

func TestCodexTaskLive(t *testing.T) {
	if os.Getenv("PIPKIN_LIVE_TEST_TASKS") != "1" {
		t.Skip("live task collection is opt-in")
	}
	home, err := codexTaskHome()
	if err != nil {
		t.Fatal("task home unavailable")
	}
	result := collectCodexTasksWith(context.Background(), codexTaskOptions{
		dial:       openCodexDesktopIPC,
		candidates: func(ctx context.Context) []string { return codexTaskCandidates(ctx, home, codexTaskMaxCandidates) },
		budget:     codexTaskBudget,
		diagnostic: func(event codexTaskDiagnostic) {
			t.Logf("phase=%s method=%s bytes=%d", event.Phase, event.Method, event.Bytes)
		},
	})
	t.Logf("coverage=%s tasks=%d", result.Coverage, len(result.Tasks))
	if result.Coverage == "unavailable" {
		t.Fatal("existing GUI task collection unavailable")
	}
}
