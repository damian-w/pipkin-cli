package pipkin

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"testing"
)

func TestStatusSummarizesFreshDeduplicatedTasksWithoutChangingJSON(t *testing.T) {
	tasks := &TaskSnapshot{SchemaVersion: 1, ObservedAt: testNow, Providers: map[string]TaskProvider{
		"codex":  {Coverage: "partial", Tasks: []Task{{ID: "one", Status: "working"}, {ID: "one", Status: "working"}, {ID: "two", Status: "needs_attention"}, {ID: "three", Status: "blocked"}}},
		"claude": {Coverage: "partial", Tasks: []Task{{ID: "four", Status: "idle"}, {ID: "five", Status: "unknown"}}},
	}}
	view := statusView{installed: true, pid: 42, snapshot: Status{Device: map[string]string{"port": "synthetic-port"}, Tasks: tasks}}
	before, err := json.Marshal(view.snapshot)
	if err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	if err := writeStatus(&out, view, testNow, "darwin"); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"Local tasks (observed; complete counts unknown)", "1 working, 1 need attention, 1 blocked (partial)", "1 idle, 1 unknown state (partial)", "pipkin tasks"} {
		if !strings.Contains(out.String(), want) {
			t.Fatalf("missing task summary %q: %s", want, out.String())
		}
	}
	if strings.Contains(out.String(), "2 working") || strings.Contains(out.String(), "0 blocked") {
		t.Fatal("duplicate or absent state became a count")
	}
	after, err := json.Marshal(view.snapshot)
	if err != nil || !bytes.Equal(before, after) {
		t.Fatal("summary changed the cached JSON")
	}
}

func TestStatusTasksDoNotReviveStaleStoppedStartingOrUnsupportedData(t *testing.T) {
	for _, kind := range []string{"stale", "future", "stopped", "starting", "missing", "unsupported", "invalid schema"} {
		t.Run(kind, func(t *testing.T) {
			tasks := &TaskSnapshot{SchemaVersion: 1, ObservedAt: testNow, Providers: map[string]TaskProvider{"codex": {Coverage: "partial", Tasks: []Task{{ID: "private", Status: "working"}}}}}
			view := statusView{installed: true, pid: 42, snapshot: Status{Tasks: tasks}}
			goos := "darwin"
			switch kind {
			case "stale":
				tasks.ObservedAt -= 60
			case "future":
				tasks.ObservedAt++
			case "stopped":
				view.pid = 0
			case "starting":
				view.starting = true
			case "missing":
				view.snapshot.Tasks = nil
			case "unsupported":
				goos = "linux"
			case "invalid schema":
				tasks.SchemaVersion = 99
			}
			var out bytes.Buffer
			if err := writeStatus(&out, view, testNow, goos); err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(out.String(), "counts unknown") || strings.Contains(out.String(), "1 working") || strings.Contains(out.String(), "private") {
				t.Fatalf("unknown tasks looked current: %s", out.String())
			}
			if kind == "unsupported" && (!strings.Contains(out.String(), "Unsupported") || strings.Contains(out.String(), "pipkin tasks")) {
				t.Fatal("unsupported platform offered an ineffective fresh check")
			}
		})
	}
}

func TestStatusTaskSummaryPropagatesWriterFailures(t *testing.T) {
	if err := writeTaskStatusSummary(failingCLIWriter{}, nil, true, testNow, "windows"); !errors.Is(err, io.ErrClosedPipe) {
		t.Fatal("summary hid writer failure")
	}
}
