package pipkin

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const claudeTaskTestSession = "11111111-1111-4111-8111-111111111111"
const claudeTaskTestAssistant = "22222222-2222-4222-8222-222222222222"
const claudeTaskTestHost = "local_33333333-3333-4333-8333-333333333333"

type claudeTaskFixture struct {
	deps     claudeTaskDependencies
	registry map[string]any
	metadata map[string]any
	identity processIdentity
	path     string
	metaPath string
}

func newClaudeTaskFixture(t *testing.T) *claudeTaskFixture {
	t.Helper()
	dir := t.TempDir()
	desktop := filepath.Join(dir, "Claude")
	f := &claudeTaskFixture{
		deps: claudeTaskDependencies{goos: "darwin", hostname: "test-host", registryDirs: []string{filepath.Join(dir, "sessions")}, desktopDirs: []string{desktop}},
		registry: map[string]any{"pid": 42, "hostSessionId": claudeTaskTestHost, "sessionId": claudeTaskTestSession,
			"entrypoint": "claude-desktop", "kind": "interactive", "status": "idle", "statusUpdatedAt": 1800000000123,
			"procStart": "Fri Jan 15 08:00:00 2027", "pidDomain": "darwin"},
		metadata: map[string]any{"sessionId": claudeTaskTestHost, "cliSessionId": claudeTaskTestSession,
			"lastAssistantUuid": claudeTaskTestAssistant, "postTurnSummaryFor": claudeTaskTestAssistant,
			"postTurnSummary": map[string]any{"status_category": "completed", "summarizes_uuid": claudeTaskTestAssistant},
			"title":           "private-task-canary", "prompt": "private-prompt-canary"},
		identity: processIdentity{Started: "Fri Jan 15 08:00:00 2027", Executable: filepath.Join(desktop, "claude-code", "test", "claude.app", "Contents", "MacOS", "claude")},
	}
	f.path = filepath.Join(f.deps.registryDirs[0], "42.json")
	f.metaPath = filepath.Join(desktop, "claude-code-sessions", "synthetic-account", "synthetic-org", claudeTaskTestHost+".json")
	f.deps.inspect = func(pid int) (processIdentity, error) {
		if pid != 42 && pid != 43 {
			return processIdentity{}, errors.New("synthetic process unavailable")
		}
		return f.identity, nil
	}
	return f
}

func writeClaudeTaskFixture(t *testing.T, path string, value any) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		t.Fatal(err)
	}
	data, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0600); err != nil {
		t.Fatal(err)
	}
}

func (f *claudeTaskFixture) collect(t *testing.T) TaskProvider {
	t.Helper()
	writeClaudeTaskFixture(t, f.path, f.registry)
	writeClaudeTaskFixture(t, f.metaPath, f.metadata)
	return collectClaudeTasksWith(context.Background(), f.deps)
}

func TestClaudeTaskStatesAndPrivateMetadata(t *testing.T) {
	for _, c := range []struct {
		name      string
		change    func(*claudeTaskFixture)
		status    string
		knownTime bool
	}{
		{"working", func(f *claudeTaskFixture) { f.registry["status"] = "busy" }, "working", true},
		{"shell", func(f *claudeTaskFixture) { f.registry["status"] = "shell" }, "working", true},
		{"native question", func(f *claudeTaskFixture) { f.registry["status"] = "waiting" }, "needs_attention", true},
		{"idle", func(*claudeTaskFixture) {}, "idle", true},
		{"blocked summary alone is idle", func(f *claudeTaskFixture) {
			f.metadata["postTurnSummary"].(map[string]any)["status_category"] = "blocked"
		}, "idle", true},
		{"need input summary alone is idle", func(f *claudeTaskFixture) {
			f.metadata["postTurnSummary"].(map[string]any)["status_category"] = "need_input"
		}, "idle", true},
		{"failed summary is not a validated blocker", func(f *claudeTaskFixture) {
			f.metadata["postTurnSummary"].(map[string]any)["status_category"] = "failed"
		}, "unknown", false},
		{"error text cannot imply blocked", func(f *claudeTaskFixture) {
			f.metadata["error"] = "private-error-canary"
			f.metadata["errorAt"] = 1800000000123
		}, "unknown", false},
		{"stale summary", func(f *claudeTaskFixture) {
			f.metadata["postTurnSummary"].(map[string]any)["status_category"] = "blocked"
			f.metadata["postTurnSummaryFor"] = "44444444-4444-4444-8444-444444444444"
		}, "idle", true},
		{"embedded stale summary", func(f *claudeTaskFixture) {
			f.metadata["postTurnSummary"].(map[string]any)["status_category"] = "blocked"
			f.metadata["postTurnSummary"].(map[string]any)["summarizes_uuid"] = "44444444-4444-4444-8444-444444444444"
		}, "idle", true},
		{"new engine ignores old summary", func(f *claudeTaskFixture) {
			f.metadata["postTurnSummary"].(map[string]any)["status_category"] = "blocked"
			f.metadata["cliSessionId"] = "44444444-4444-4444-8444-444444444444"
		}, "unknown", true},
		{"working takes priority", func(f *claudeTaskFixture) {
			f.registry["status"] = "busy"
			f.metadata["postTurnSummary"].(map[string]any)["status_category"] = "blocked"
		}, "working", true},
		{"unknown source enum", func(f *claudeTaskFixture) { f.registry["status"] = "new-status" }, "unknown", true},
		{"missing timestamp", func(f *claudeTaskFixture) { delete(f.registry, "statusUpdatedAt") }, "idle", false},
		{"invalid timestamp", func(f *claudeTaskFixture) { f.registry["statusUpdatedAt"] = 999999999999999 }, "idle", false},
	} {
		t.Run(c.name, func(t *testing.T) {
			f := newClaudeTaskFixture(t)
			c.change(f)
			provider := f.collect(t)
			if provider.Coverage != "partial" || len(provider.Tasks) != 1 {
				t.Fatalf("expected one partial observation, got %#v", provider)
			}
			task := provider.Tasks[0]
			if task.Status != c.status || (task.UpdatedAt != nil) != c.knownTime {
				t.Fatalf("unexpected observation: %#v", task)
			}
			if task.UpdatedAt != nil && *task.UpdatedAt != 1800000000 {
				t.Fatalf("source milliseconds were not converted to Unix seconds: %d", *task.UpdatedAt)
			}
			data, _ := json.Marshal(provider)
			for _, private := range []string{claudeTaskTestSession, claudeTaskTestHost, claudeTaskTestAssistant, "private-", f.identity.Executable} {
				if strings.Contains(string(data), private) {
					t.Fatal("task output retained private source content")
				}
			}
			if task.ID != taskOpaqueID("claude", claudeTaskTestHost) {
				t.Fatal("task identity is not stable and provider scoped")
			}
		})
	}
}

func TestClaudeTaskRejectsUnverifiedSessions(t *testing.T) {
	for _, c := range []struct {
		name   string
		change func(*claudeTaskFixture)
	}{
		{"pid reuse", func(f *claudeTaskFixture) { f.identity.Started = "Fri Jan 15 08:00:01 2027" }},
		{"wrong executable", func(f *claudeTaskFixture) {
			f.identity.Executable = filepath.Join(t.TempDir(), "claude.app", "Contents", "MacOS", "claude")
		}},
		{"not a desktop engine", func(f *claudeTaskFixture) { f.registry["entrypoint"] = "cli" }},
		{"background worker", func(f *claudeTaskFixture) { f.registry["kind"] = "bg" }},
		{"foreign pid domain", func(f *claudeTaskFixture) { f.registry["pidDomain"] = "wsl:test-host" }},
		{"pid filename mismatch", func(f *claudeTaskFixture) { f.registry["pid"] = 43 }},
		{"unrecognized task ID", func(f *claudeTaskFixture) { f.registry["hostSessionId"] = "local_../private" }},
		{"archived idle task", func(f *claudeTaskFixture) { f.metadata["isArchived"] = true }},
		{"dead process", func(f *claudeTaskFixture) {
			f.deps.inspect = func(int) (processIdentity, error) { return processIdentity{}, os.ErrNotExist }
		}},
	} {
		t.Run(c.name, func(t *testing.T) {
			f := newClaudeTaskFixture(t)
			c.change(f)
			provider := f.collect(t)
			if provider.Coverage != "unavailable" || provider.Tasks != nil {
				t.Fatalf("unverified task became a count: %#v", provider)
			}
		})
	}
}

func TestClaudeTaskUnavailableAndBoundedReads(t *testing.T) {
	f := newClaudeTaskFixture(t)
	if p := collectClaudeTasksWith(context.Background(), f.deps); p.Coverage != "unavailable" || p.Tasks != nil {
		t.Fatal("missing files became an empty task list")
	}
	writeClaudeTaskFixture(t, f.path, f.registry)
	file, err := os.OpenFile(f.path, os.O_WRONLY, 0600)
	if err != nil {
		t.Fatal(err)
	}
	if err := file.Truncate(claudeTaskRegistryLimit + 1); err != nil {
		t.Fatal(err)
	}
	file.Close()
	if p := collectClaudeTasksWith(context.Background(), f.deps); p.Tasks != nil {
		t.Fatal("oversized registry was accepted")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	f.deps.inspect = func(int) (processIdentity, error) {
		t.Fatal("canceled collector inspected a process")
		return processIdentity{}, nil
	}
	if p := collectClaudeTasksWith(ctx, f.deps); p.Coverage != "unavailable" || p.Tasks != nil {
		t.Fatal("canceled collector published task data")
	}
	f.deps.goos = "linux"
	if p := collectClaudeTasksWith(context.Background(), f.deps); p.Coverage != "unsupported" || p.Tasks != nil {
		t.Fatal("Linux should be explicitly unsupported")
	}
}

func TestClaudeTaskDuplicateHostIdentity(t *testing.T) {
	f := newClaudeTaskFixture(t)
	writeClaudeTaskFixture(t, f.path, f.registry)
	f.registry["pid"], f.registry["status"] = 43, "busy"
	writeClaudeTaskFixture(t, filepath.Join(f.deps.registryDirs[0], "43.json"), f.registry)
	provider := collectClaudeTasksWith(context.Background(), f.deps)
	if len(provider.Tasks) != 1 || provider.Tasks[0].Status != "unknown" || provider.Tasks[0].UpdatedAt != nil {
		t.Fatalf("conflicting engines must not double count or select a status: %#v", provider)
	}
}

func TestClaudeTaskIdleRequiresMatchingMetadata(t *testing.T) {
	for _, name := range []string{"missing", "malformed", "oversized", "wrong host", "matching cleared"} {
		t.Run(name, func(t *testing.T) {
			f := newClaudeTaskFixture(t)
			writeClaudeTaskFixture(t, f.path, f.registry)
			writeClaudeTaskFixture(t, f.metaPath, f.metadata)
			want := "unknown"
			switch name {
			case "missing":
				if err := os.Remove(f.metaPath); err != nil {
					t.Fatal(err)
				}
			case "malformed":
				if err := os.WriteFile(f.metaPath, []byte(`{"sessionId":`), 0600); err != nil {
					t.Fatal(err)
				}
			case "oversized":
				file, err := os.OpenFile(f.metaPath, os.O_WRONLY, 0600)
				if err != nil {
					t.Fatal(err)
				}
				err = file.Truncate(claudeTaskMetadataLimit + 1)
				file.Close()
				if err != nil {
					t.Fatal(err)
				}
			case "wrong host":
				f.metadata["sessionId"] = "local_44444444-4444-4444-8444-444444444444"
				writeClaudeTaskFixture(t, f.metaPath, f.metadata)
			case "matching cleared":
				delete(f.metadata, "postTurnSummary")
				delete(f.metadata, "postTurnSummaryFor")
				writeClaudeTaskFixture(t, f.metaPath, f.metadata)
				want = "idle"
			}
			provider := collectClaudeTasksWith(context.Background(), f.deps)
			if provider.Coverage != "partial" || len(provider.Tasks) != 1 || provider.Tasks[0].Status != want {
				t.Fatalf("idle metadata handling lost source uncertainty: %#v", provider)
			}
		})
	}
}

func TestClaudeTaskMigratedMetadataCopiesMustAgree(t *testing.T) {
	for _, c := range []struct {
		first, second, want string
	}{
		{"completed", "needs user", "unknown"},
		{"needs user", "completed", "unknown"},
		{"error", "needs user", "unknown"},
		{"archived", "completed", "unknown"},
		{"completed", "archived", "unknown"},
		{"archived", "archived", "excluded"},
		{"needs user", "needs user", "needs_attention"},
		{"completed", "completed", "idle"},
	} {
		t.Run(c.first+" and "+c.second, func(t *testing.T) {
			f := newClaudeTaskFixture(t)
			writeClaudeTaskFixture(t, f.path, f.registry)
			setState := func(metadata map[string]any, state string) {
				switch state {
				case "error":
					metadata["error"] = "private-error-canary"
				case "archived":
					metadata["isArchived"] = true
				case "needs user":
					metadata["turnWrapUp"] = map[string]any{"description": "private-description-canary", "needsUser": "private-action-canary"}
				default:
					metadata["postTurnSummary"].(map[string]any)["status_category"] = state
				}
			}
			second := newClaudeTaskFixture(t).metadata
			setState(f.metadata, c.first)
			setState(second, c.second)
			writeClaudeTaskFixture(t, f.metaPath, f.metadata)
			migrated := filepath.Join(t.TempDir(), "Claude-migrated")
			f.deps.desktopDirs = append(f.deps.desktopDirs, migrated)
			copyPath := filepath.Join(migrated, "claude-code-sessions", "synthetic-account", "synthetic-org", claudeTaskTestHost+".json")
			writeClaudeTaskFixture(t, copyPath, second)
			provider := collectClaudeTasksWith(context.Background(), f.deps)
			if c.want == "excluded" {
				if provider.Coverage != "unavailable" || provider.Tasks != nil {
					t.Fatalf("consistently archived copies were not excluded: %#v", provider)
				}
				return
			}
			if provider.Coverage != "partial" || len(provider.Tasks) != 1 || provider.Tasks[0].Status != c.want {
				t.Fatalf("metadata order selected an inconsistent task state: %#v", provider)
			}
			if (provider.Tasks[0].UpdatedAt != nil) != (c.want == "idle") {
				t.Fatal("conflicting or semantic metadata acquired a source transition time")
			}
		})
	}
}

func TestClaudeTaskCurrentWrapUpDecidesAttention(t *testing.T) {
	for _, c := range []struct {
		name, category, want string
		change               func(map[string]any)
		knownTime            bool
	}{
		{"completed wrap needs input", "completed", "needs_attention", func(m map[string]any) {
			m["turnWrapUp"].(map[string]any)["needsUser"] = "private-action-canary"
		}, false},
		{"empty input string remains present", "completed", "needs_attention", func(m map[string]any) {
			m["turnWrapUp"].(map[string]any)["needsUser"] = ""
		}, false},
		{"current wrap without raw summary", "completed", "needs_attention", func(m map[string]any) {
			delete(m, "postTurnSummary")
			m["turnWrapUp"].(map[string]any)["needsUser"] = "private-action-canary"
		}, false},
		{"wrap clears blocked", "blocked", "idle", nil, true},
		{"wrap clears need input", "need_input", "idle", nil, true},
		{"wrap cannot clear failed summary", "failed", "unknown", nil, false},
		{"wrap cannot override failed summary", "failed", "unknown", func(m map[string]any) {
			m["turnWrapUp"].(map[string]any)["needsUser"] = "private-action-canary"
		}, false},
		{"stale wrap cannot request input", "completed", "idle", func(m map[string]any) {
			m["postTurnSummaryFor"] = "44444444-4444-4444-8444-444444444444"
			m["turnWrapUp"].(map[string]any)["needsUser"] = "private-action-canary"
		}, true},
		{"embedded stale summary guards wrap", "completed", "idle", func(m map[string]any) {
			m["postTurnSummary"].(map[string]any)["summarizes_uuid"] = "44444444-4444-4444-8444-444444444444"
			m["turnWrapUp"].(map[string]any)["needsUser"] = "private-action-canary"
		}, true},
		{"unsupported wrap stays unknown", "blocked", "unknown", func(m map[string]any) {
			delete(m["turnWrapUp"].(map[string]any), "description")
		}, false},
		{"current error stays unknown", "completed", "unknown", func(m map[string]any) {
			m["error"] = "private-error-canary"
			m["turnWrapUp"].(map[string]any)["needsUser"] = "private-action-canary"
		}, false},
	} {
		t.Run(c.name, func(t *testing.T) {
			f := newClaudeTaskFixture(t)
			f.metadata["postTurnSummary"].(map[string]any)["status_category"] = c.category
			f.metadata["turnWrapUp"] = map[string]any{"description": "private-description-canary"}
			if c.change != nil {
				c.change(f.metadata)
			}
			provider := f.collect(t)
			if provider.Coverage != "partial" || len(provider.Tasks) != 1 || provider.Tasks[0].Status != c.want ||
				(provider.Tasks[0].UpdatedAt != nil) != c.knownTime {
				t.Fatalf("current effective summary was not reconciled: %#v", provider)
			}
			data, _ := json.Marshal(f.metadata)
			var metadata claudeTaskMetadata
			if err := json.Unmarshal(data, &metadata); err != nil {
				t.Fatal(err)
			}
			retained, _ := json.Marshal(metadata)
			if strings.Contains(string(retained), "private-") {
				t.Fatal("reduced wrap-up metadata retained private prose")
			}
		})
	}
}

func TestClaudeTaskInvalidWrapUpTypesCannotEstablishIdle(t *testing.T) {
	for _, field := range []string{"description", "needsUser"} {
		for _, value := range []any{nil, false, 1, []string{"private-canary"}} {
			f := newClaudeTaskFixture(t)
			f.metadata["turnWrapUp"] = map[string]any{"description": "private-description-canary", field: value}
			provider := f.collect(t)
			if len(provider.Tasks) != 1 || provider.Tasks[0].Status != "unknown" {
				t.Fatalf("malformed %s became a known task status: %#v", field, provider)
			}
		}
	}
}

func TestClaudeTaskWindowsIdentityAndPaths(t *testing.T) {
	f := newClaudeTaskFixture(t)
	f.deps.goos = "windows"
	f.identity.Started = "134000000000000000"
	f.identity.Executable = filepath.Join(f.deps.desktopDirs[0], "claude-code", "test", "claude.exe")
	f.registry["pidDomain"], f.registry["procStart"] = "windows:test-host", f.identity.Started
	provider := f.collect(t)
	if provider.Coverage != "partial" || len(provider.Tasks) != 1 {
		t.Fatalf("native Windows FILETIME record was rejected: %#v", provider)
	}
	f.registry["procStartFt"] = "134000000000000001"
	if p := f.collect(t); p.Tasks != nil {
		t.Fatal("mismatched explicit Windows FILETIME was accepted")
	}
	root := `C:\Users\Test\AppData\Local\Claude-Data\test-family`
	if !claudeTaskEnginePath(`\\?\C:\Users\Test\AppData\Local\Claude-Data\test-family\claude-code\v1\CLAUDE.EXE`, []string{root}, "windows") ||
		claudeTaskEnginePath(root+`\claude-code-other\claude.exe`, []string{root}, "windows") {
		t.Fatal("Windows engine path boundary or case handling is incorrect")
	}
}

func TestClaudeTaskSymlinkRegistryIsNotRead(t *testing.T) {
	f := newClaudeTaskFixture(t)
	writeClaudeTaskFixture(t, f.path+"-target", f.registry)
	if err := os.Symlink(f.path+"-target", f.path); err != nil {
		t.Skip("symlinks are unavailable for this test user")
	}
	f.deps.inspect = func(int) (processIdentity, error) {
		t.Fatal("symlink registry was followed")
		return processIdentity{}, nil
	}
	if p := collectClaudeTasksWith(context.Background(), f.deps); p.Tasks != nil {
		t.Fatal("symlink registry produced a task")
	}
}
