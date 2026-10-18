package pipkin

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
)

const (
	claudeTaskRegistryLimit = 256 << 10
	claudeTaskMetadataLimit = 8 << 20
	claudeTaskReadLimit     = 16 << 20
	claudeTaskEntryLimit    = 256
	claudeTaskDirLimit      = 128
)

type claudeTaskDependencies struct {
	goos         string
	hostname     string
	registryDirs []string
	desktopDirs  []string
	inspect      func(int) (processIdentity, error)
}

type claudeTaskRegistry struct {
	PID             int      `json:"pid"`
	HostSessionID   string   `json:"hostSessionId"`
	SessionID       string   `json:"sessionId"`
	Entrypoint      string   `json:"entrypoint"`
	Kind            string   `json:"kind"`
	Status          string   `json:"status"`
	StatusUpdatedAt *float64 `json:"statusUpdatedAt"`
	ProcStart       string   `json:"procStart"`
	ProcStartFT     string   `json:"procStartFt"`
	PIDDomain       string   `json:"pidDomain"`
}

// Decode only status fields; prompts, titles and errors are never kept.
type claudeTaskMetadata struct {
	SessionID          string             `json:"sessionId"`
	CLISessionID       string             `json:"cliSessionId"`
	Archived           bool               `json:"isArchived"`
	LastAssistantUUID  string             `json:"lastAssistantUuid"`
	PostTurnSummaryFor string             `json:"postTurnSummaryFor"`
	ErrorPresent       claudeTaskPresence `json:"error"`
	PostTurnSummary    *struct {
		StatusCategory string `json:"status_category"`
		SummarizesUUID string `json:"summarizes_uuid"`
	} `json:"postTurnSummary"`
	TurnWrapUp *struct {
		Description claudeTaskStringPresence `json:"description"`
		NeedsUser   claudeTaskStringPresence `json:"needsUser"`
	} `json:"turnWrapUp"`
}

type claudeTaskPresence bool

func (p *claudeTaskPresence) UnmarshalJSON(data []byte) error {
	data = bytes.TrimSpace(data)
	*p = claudeTaskPresence(!bytes.Equal(data, []byte("null")) && !bytes.Equal(data, []byte(`""`)))
	return nil
}

// The GUI checks only whether these strings exist, so their text is never kept.
type claudeTaskStringPresence bool

func (p *claudeTaskStringPresence) UnmarshalJSON(data []byte) error {
	data = bytes.TrimSpace(data)
	if len(data) == 0 || data[0] != '"' {
		return errors.New("invalid Claude task string field")
	}
	*p = true
	return nil
}

type claudeTaskReader struct {
	ctx       context.Context
	remaining int64
	dirs      int
}

func collectClaudeTasks(ctx context.Context) TaskProvider {
	if runtime.GOOS != "darwin" && runtime.GOOS != "windows" {
		return TaskProvider{Coverage: "unsupported"}
	}
	hostname, _ := os.Hostname()
	deps := claudeTaskDependencies{
		goos: runtime.GOOS, hostname: hostname,
		registryDirs: []string{filepath.Join(claudeConfig(), "sessions")},
		desktopDirs:  claudeTaskDesktopDirs(ctx, runtime.GOOS, homeDir()),
		inspect:      ownedProcessInfo,
	}
	return collectClaudeTasksWith(ctx, deps)
}

func collectClaudeTasksWith(ctx context.Context, deps claudeTaskDependencies) TaskProvider {
	if deps.goos != "darwin" && deps.goos != "windows" {
		return TaskProvider{Coverage: "unsupported"}
	}
	unavailable := TaskProvider{Coverage: "unavailable"}
	if ctx.Err() != nil || deps.inspect == nil {
		return unavailable
	}
	reader := claudeTaskReader{ctx: ctx, remaining: claudeTaskReadLimit}
	var records []claudeTaskRegistry
	for _, dir := range deps.registryDirs {
		for _, entry := range reader.entries(dir, claudeTaskEntryLimit) {
			if !entry.Type().IsRegular() || !strings.HasSuffix(entry.Name(), ".json") {
				continue
			}
			pidText := strings.TrimSuffix(entry.Name(), ".json")
			pid, err := strconv.Atoi(pidText)
			if err != nil || pid <= 1 || pid > 1<<31-1 || strconv.Itoa(pid) != pidText {
				continue
			}
			var record claudeTaskRegistry
			if !reader.decode(filepath.Join(dir, entry.Name()), claudeTaskRegistryLimit, &record) ||
				record.PID != pid || record.Entrypoint != "claude-desktop" || record.Kind != "interactive" ||
				!claudeTaskHostID(record.HostSessionID) || !validClaudeUUID(record.SessionID) ||
				!claudeTaskLocalDomain(record.PIDDomain, deps.goos, deps.hostname) {
				continue
			}
			identity, err := deps.inspect(pid)
			if err != nil || !claudeTaskEnginePath(identity.Executable, deps.desktopDirs, deps.goos) ||
				!claudeTaskStartMatches(record, identity, deps.goos) {
				continue
			}
			records = append(records, record)
		}
	}
	if ctx.Err() != nil || len(records) == 0 {
		return unavailable
	}
	// Read metadata only for live sessions; transcripts are never scanned.
	var stores []string
	for _, root := range deps.desktopDirs {
		base := filepath.Join(root, "claude-code-sessions")
		for _, account := range reader.entries(base, 64) {
			if !account.IsDir() {
				continue
			}
			accountDir := filepath.Join(base, account.Name())
			for _, org := range reader.entries(accountDir, 64) {
				if org.IsDir() {
					stores = append(stores, filepath.Join(accountDir, org.Name()))
				}
			}
		}
	}
	var tasks []Task
	for _, record := range records {
		if ctx.Err() != nil {
			return unavailable
		}
		task := Task{ID: taskOpaqueID("claude", record.HostSessionID), Status: "unknown"}
		if record.StatusUpdatedAt != nil {
			if updated := validEpoch(*record.StatusUpdatedAt / 1000); updated != 0 {
				task.UpdatedAt = &updated
			}
		}
		switch record.Status {
		case "busy", "shell":
			task.Status = "working"
		case "waiting":
			task.Status = "needs_attention"
		case "idle":
			// An idle engine may still be waiting for input, so idle needs metadata.
			task.Status = "unknown"
			matchedMetadata := false
			metadataState := ""
			metadataConflict := false
			for _, store := range stores {
				var metadata claudeTaskMetadata
				if !reader.decode(filepath.Join(store, record.HostSessionID+".json"), claudeTaskMetadataLimit, &metadata) ||
					metadata.SessionID != record.HostSessionID || metadata.CLISessionID != record.SessionID {
					continue
				}
				candidate := "idle"
				switch {
				case metadata.Archived:
					candidate = "archived"
				case bool(metadata.ErrorPresent):
					// Errors are unverified, so report unknown rather than blocked.
					candidate = "unknown"
				case validClaudeUUID(metadata.LastAssistantUUID) &&
					metadata.PostTurnSummaryFor == metadata.LastAssistantUUID &&
					(metadata.PostTurnSummary == nil || metadata.PostTurnSummary.SummarizesUUID == metadata.LastAssistantUUID):
					wrap := metadata.TurnWrapUp
					switch {
					case wrap != nil && !bool(wrap.Description):
						candidate = "unknown"
					case metadata.PostTurnSummary != nil && metadata.PostTurnSummary.StatusCategory == "failed":
						candidate = "unknown"
					case wrap != nil && bool(wrap.NeedsUser):
						// Only a wrap-up that asks you something shows as waiting in
						// the app; the summary category alone is not shown.
						candidate = "needs_attention"
					}
				}
				if !matchedMetadata {
					metadataState, matchedMetadata = candidate, true
				} else if candidate != metadataState {
					metadataConflict = true
				}
			}
			if metadataConflict {
				// Copies in several profiles must agree; directory order never decides.
				task.Status, task.UpdatedAt = "unknown", nil
			} else if matchedMetadata {
				task.Status = metadataState
				if metadataState == "archived" {
					task.ID = ""
				} else if metadataState != "idle" {
					task.UpdatedAt = nil
				}
			}
		}
		tasks = append(tasks, task)
	}
	if ctx.Err() != nil {
		return unavailable
	}
	return normalizeTaskProvider(TaskProvider{Coverage: "partial", Tasks: tasks})
}

func claudeTaskHostID(id string) bool {
	if !strings.HasPrefix(id, "local_") || len(id) <= 6 || len(id) > 70 {
		return false
	}
	for _, c := range id[6:] {
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '-') {
			return false
		}
	}
	return true
}

func claudeTaskLocalDomain(domain, goos, hostname string) bool {
	if domain == "" || strings.EqualFold(domain, goos) {
		return true
	}
	if goos == "windows" && strings.EqualFold(domain, "win32") {
		return true
	}
	return hostname != "" && (strings.EqualFold(domain, goos+":"+hostname) ||
		goos == "windows" && strings.EqualFold(domain, "win32:"+hostname))
}

func claudeTaskStartMatches(record claudeTaskRegistry, identity processIdentity, goos string) bool {
	if identity.Started == "" {
		return false
	}
	if goos == "windows" && record.ProcStartFT != "" {
		return record.ProcStartFT == identity.Started
	}
	return record.ProcStart != "" && strings.Join(strings.Fields(record.ProcStart), " ") ==
		strings.Join(strings.Fields(identity.Started), " ")
}

func claudeTaskEnginePath(executable string, roots []string, goos string) bool {
	normalize := func(value string) string {
		value = strings.ReplaceAll(filepath.Clean(value), `\`, "/")
		if goos == "windows" {
			value = strings.ToLower(strings.TrimPrefix(value, "//?/"))
		}
		return strings.TrimRight(value, "/")
	}
	executable = normalize(executable)
	suffix := "/claude.app/Contents/MacOS/claude"
	if goos == "windows" {
		suffix = "/claude.exe"
	}
	if !strings.HasSuffix(executable, suffix) {
		return false
	}
	for _, root := range roots {
		if strings.HasPrefix(executable, normalize(root)+"/claude-code/") {
			return true
		}
	}
	return false
}

func (r *claudeTaskReader) entries(path string, limit int) []os.DirEntry {
	if r.ctx.Err() != nil || r.dirs >= claudeTaskDirLimit {
		return nil
	}
	r.dirs++
	file, err := os.Open(path)
	if err != nil {
		return nil
	}
	defer file.Close()
	entries, err := file.ReadDir(limit)
	if err != nil && !errors.Is(err, io.EOF) {
		return nil
	}
	return entries
}

func (r *claudeTaskReader) decode(path string, limit int64, value any) bool {
	if r.ctx.Err() != nil || r.remaining <= 0 {
		return false
	}
	before, err := os.Lstat(path)
	if err != nil || !before.Mode().IsRegular() || before.Size() > limit || before.Size() > r.remaining {
		return false
	}
	file, err := os.Open(path)
	if err != nil {
		return false
	}
	defer file.Close()
	after, err := file.Stat()
	if err != nil || !os.SameFile(before, after) {
		return false
	}
	data, err := readBounded(file, min(limit, r.remaining))
	if err != nil {
		r.remaining = 0
		return false
	}
	r.remaining -= int64(len(data))
	return r.ctx.Err() == nil && json.Unmarshal(data, value) == nil
}

func claudeTaskDesktopDirs(ctx context.Context, goos, home string) []string {
	var dirs []string
	if override := os.Getenv("CLAUDE_USER_DATA_DIR"); override != "" {
		dirs = append(dirs, override)
	}
	if goos == "darwin" {
		return append(dirs, filepath.Join(home, "Library", "Application Support", "Claude"))
	}
	local := envOr("LOCALAPPDATA", filepath.Join(home, "AppData", "Local"))
	dirs = append(dirs, filepath.Join(envOr("APPDATA", filepath.Join(home, "AppData", "Roaming")), "Claude"))
	moved := filepath.Join(local, "Claude-Data")
	dirs = append(dirs, moved)
	reader := claudeTaskReader{ctx: ctx}
	for _, child := range reader.entries(moved, 32) {
		if child.IsDir() {
			dirs = append(dirs, filepath.Join(moved, child.Name()))
		}
	}
	for _, family := range []string{"Claude_pzs8sxrjxfjjc", "AnthropicPBC.Claude_fnn82j28hfe8t"} {
		dirs = append(dirs, filepath.Join(local, "Packages", family, "LocalCache", "Roaming", "Claude"))
	}
	return dirs
}
