package pipkin

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestUsageRunPreservesJSONAndSeparatesProgress(t *testing.T) {
	now := time.Unix(testNow, 0)
	reading := &Reading{Account: "invented-account", Source: "invented-source", Observed: testNow,
		Plan: "sample_plan", SessionNoCap: true, Weekly: &Window{Used: 250, Reset: testNow + 86400},
		Models: map[string]*Window{"sample": nil}, ReportsBanked: true, Banked: outputTestInt(0)}
	for _, c := range []struct{ json, terminal bool }{{true, true}, {true, false}, {false, true}, {false, false}} {
		var out, progress bytes.Buffer
		var mu sync.Mutex
		calls := map[string]int{}
		deps := usageDependencies{now: func() time.Time { return now }, progressTerminal: c.terminal,
			config: func() (Config, error) { return Config{Salt: "synthetic-salt"}, nil },
			collect: func(ctx context.Context, provider, salt string) providerResult {
				mu.Lock()
				calls[provider]++
				mu.Unlock()
				if salt != "synthetic-salt" || ctx.Err() != nil {
					return providerResult{provider: provider, err: errors.New("wrong injected collection inputs")}
				}
				if provider == "codex" {
					return providerResult{provider: provider, reading: reading}
				}
				return providerResult{provider: provider, reading: &Reading{Account: provider}, err: signedOutError("not signed in")}
			}}
		if err := runUsage(context.Background(), c.json, &out, &progress, deps); err != nil {
			t.Fatal(err)
		}
		if calls["codex"] != 1 || calls["claude"] != 1 || len(calls) != 2 {
			t.Fatalf("collection changed: %v", calls)
		}
		if c.json {
			expected := Status{SchemaVersion: 1, States: map[string]string{"codex": "available", "claude": "signed_out"}, Version: version,
				Readings: map[string]*Reading{"codex": reading, "claude": {Account: "claude"}}, Updated: testNow, ClaudeError: "not signed in"}
			want, err := json.MarshalIndent(expected, "", "  ")
			if err != nil || out.String() != string(want)+"\n" {
				t.Fatalf("JSON contract changed: %v\n%s", err, out.String())
			}
			if progress.Len() != 0 {
				t.Fatal("JSON mode printed progress")
			}
		} else {
			for _, value := range []string{"Codex (sample_plan)", "No session cap", "Banked resets", "Not signed in"} {
				if !strings.Contains(out.String(), value) {
					t.Fatalf("missing %q in %s", value, out.String())
				}
			}
			if strings.Contains(out.String(), "Checking") || (progress.Len() != 0) != c.terminal {
				t.Fatal("progress reached stdout or a redirected stream")
			}
		}
	}
}

func TestUsageRunFailuresDoNotChangeCollectionOrHideWriterErrors(t *testing.T) {
	want := errors.New("fixture configuration failure")
	var collections atomic.Int32
	deps := usageDependencies{now: time.Now, config: func() (Config, error) { return Config{}, want },
		collect: func(_ context.Context, provider, _ string) providerResult {
			collections.Add(1)
			return providerResult{provider: provider, reading: &Reading{Account: provider}}
		}}
	if err := runUsage(context.Background(), false, io.Discard, io.Discard, deps); !errors.Is(err, want) || collections.Load() != 0 {
		t.Fatal("configuration failure started collection or lost its error")
	}
	deps.config = func() (Config, error) { return Config{}, nil }
	deps.progressTerminal = true
	if err := runUsage(context.Background(), false, io.Discard, failingCLIWriter{}, deps); !errors.Is(err, io.ErrClosedPipe) || collections.Load() != 0 {
		t.Fatal("progress failure was hidden or started collection")
	}
	for _, jsonOutput := range []bool{true, false} {
		if err := runUsage(context.Background(), jsonOutput, failingCLIWriter{}, io.Discard, deps); !errors.Is(err, io.ErrClosedPipe) {
			t.Fatalf("output failure was hidden: %v", err)
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	before := collections.Load()
	if err := runUsage(ctx, true, io.Discard, io.Discard, deps); !errors.Is(err, context.Canceled) || collections.Load() != before {
		t.Fatal("cancelled command started collection")
	}
}

func TestStatusJSONMissingSnapshotKeepsContractAndCreatesNothing(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "absent")
	t.Setenv("PIPKIN_HOME", dir)
	var out bytes.Buffer
	err := statusJSONTo(&out)
	if err == nil || err.Error() != "no helper snapshot yet; run `pipkin usage --json` for a fresh read" || out.Len() != 0 {
		t.Fatalf("missing-snapshot JSON contract changed: %v %s", err, out.String())
	}
	if _, err := os.Stat(dir); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("missing JSON snapshot created files")
	}
}

func TestStatusJSONPreservesCachedFieldsAndDoesNotRewriteSnapshot(t *testing.T) {
	t.Setenv("PIPKIN_HOME", t.TempDir())
	saved := Status{SchemaVersion: 1, Version: "fixture", PID: 42, Updated: testNow,
		Device:     map[string]string{"port": "COM9", "firmware": "1.0.0"},
		States:     map[string]string{"codex": "unavailable", "claude": "signed_out"},
		Readings:   map[string]*Reading{"codex": {Account: "invented", Observed: testNow, SessionNoCap: true}, "claude": {Account: "claude"}},
		CodexError: "Codex: synthetic cached error", ClaudeError: "Claude: no existing sign-in found",
		Tasks: &TaskSnapshot{SchemaVersion: 1, ObservedAt: testNow, Providers: map[string]TaskProvider{
			"codex": {Coverage: "partial", Tasks: []Task{{ID: "codex:invented", Status: "working"}}},
		}}}
	if err := writeJSON(statusPath(), saved); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(statusPath())
	if err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	if err := statusJSONTo(&out); err != nil {
		t.Fatal(err)
	}
	expected := saved
	expected.PID = 0
	expected.Tasks = &TaskSnapshot{SchemaVersion: 1, ObservedAt: testNow, Providers: map[string]TaskProvider{"codex": {Coverage: "unavailable"}}}
	want, err := json.MarshalIndent(expected, "", "  ")
	if err != nil || out.String() != string(want)+"\n" {
		t.Fatalf("cached JSON contract changed: %v\n%s", err, out.String())
	}
	after, err := os.ReadFile(statusPath())
	if err != nil || !bytes.Equal(before, after) {
		t.Fatal("reading JSON rewrote the cached snapshot")
	}
}
