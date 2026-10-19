package pipkin

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"os/signal"
	"slices"
	"strings"
	"syscall"
	"time"
)

const (
	taskCollectionBudget = 5 * time.Second
	// The helper reads Claude every taskPollEvery and watches Codex live. Unchanged
	// statuses renew the cache and display every taskRefreshEvery.
	taskPollEvery    = 2 * time.Second
	taskRefreshEvery = 15 * time.Second
	taskFreshFor     = 45 * time.Second // matches the display's task expiry
)

// Task holds only status metadata. UpdatedAt is the source's time, not a heartbeat.
type Task struct {
	ID        string `json:"id"`
	UpdatedAt *int64 `json:"updated_at"`
	Status    string `json:"status"`
}

// Coverage is partial, unavailable or unsupported; no source is ever complete.
type TaskProvider struct {
	Coverage string `json:"coverage"`
	Tasks    []Task `json:"tasks"`
}

type TaskSnapshot struct {
	SchemaVersion int                     `json:"schema_version"`
	ObservedAt    int64                   `json:"observed_at"`
	Providers     map[string]TaskProvider `json:"providers"`
}

func taskOpaqueID(provider, rawID string) string {
	digest := sha256.Sum256([]byte(provider + "\x00" + rawID))
	return provider + ":" + hex.EncodeToString(digest[:16])
}

type taskCollectors map[string]func(context.Context) TaskProvider

func collectTaskSnapshot(ctx context.Context) TaskSnapshot {
	return collectTasksWith(ctx, time.Now, taskCollectors{
		"claude": collectClaudeTasks,
		"codex":  collectCodexTasks,
	})
}

func collectTasksWith(ctx context.Context, now func() time.Time, collectors taskCollectors) TaskSnapshot {
	ctx, cancel := context.WithTimeout(ctx, taskCollectionBudget)
	defer cancel()
	type result struct {
		provider string
		value    TaskProvider
	}
	results := make(chan result, len(collectors))
	snapshot := TaskSnapshot{SchemaVersion: 1, Providers: make(map[string]TaskProvider, len(collectors))}
	for provider, collect := range collectors {
		snapshot.Providers[provider] = TaskProvider{Coverage: "unavailable"}
		if ctx.Err() == nil {
			go func() { results <- result{provider, collect(ctx)} }()
		}
	}
	for range collectors {
		select {
		case r := <-results:
			snapshot.Providers[r.provider] = normalizeTaskProvider(r.value)
		case <-ctx.Done():
			snapshot.ObservedAt = now().Unix()
			return snapshot
		}
	}
	snapshot.ObservedAt = now().Unix()
	return snapshot
}

// normalizeTaskProvider never turns missing data into an empty list.
func normalizeTaskProvider(p TaskProvider) TaskProvider {
	if p.Coverage != "partial" {
		if p.Coverage != "unsupported" {
			p.Coverage = "unavailable"
		}
		p.Tasks = nil
		return p
	}
	tasks := make(map[string]Task, len(p.Tasks))
	for _, task := range p.Tasks {
		if task.ID == "" {
			continue
		}
		if !slices.ContainsFunc(taskStatuses, func(s taskStatus) bool { return s.key == task.Status }) {
			task.Status = "unknown"
		}
		if task.UpdatedAt != nil && validEpoch(float64(*task.UpdatedAt)) == 0 {
			task.UpdatedAt = nil
		}
		if previous, ok := tasks[task.ID]; ok {
			// Conflicting identities/states are uncertainty, not extra tasks.
			if previous.Status != task.Status {
				task.Status, task.UpdatedAt = "unknown", nil
			} else if previous.UpdatedAt != nil && (task.UpdatedAt == nil || *previous.UpdatedAt > *task.UpdatedAt) {
				task.UpdatedAt = previous.UpdatedAt
			}
		}
		tasks[task.ID] = task
	}
	if len(tasks) == 0 {
		return TaskProvider{Coverage: "unavailable"}
	}
	p.Tasks = make([]Task, 0, len(tasks))
	for _, task := range tasks {
		p.Tasks = append(p.Tasks, task)
	}
	slices.SortFunc(p.Tasks, func(a, b Task) int { return strings.Compare(a.ID, b.ID) })
	return p
}

func tasksCommand(args []string) error {
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	return runTasks(ctx, len(args) == 1, os.Stdout, collectTaskSnapshot)
}

func runTasks(ctx context.Context, jsonOutput bool, out io.Writer, collect func(context.Context) TaskSnapshot) error {
	snapshot := collect(ctx)
	if err := ctx.Err(); err != nil {
		return err
	}
	if jsonOutput {
		return printJSONTo(out, snapshot)
	}
	for _, provider := range taskProviders {
		summary := taskSummary(snapshot.Providers[provider.key])
		if summary == "" {
			summary = "None found"
			if snapshot.Providers[provider.key].Coverage == "unsupported" {
				summary = "Not available on this system"
			}
		}
		if err := writeOutputLine(out, "", fmt.Sprintf("%-11s  %s", provider.name, summary)); err != nil {
			return err
		}
	}
	return nil
}

var taskProviders = []struct{ key, name string }{{"codex", "Codex"}, {"claude", "Claude Code"}}

type taskStatus struct{ key, label string }

var taskStatuses = []taskStatus{{"working", "working"}, {"needs_attention", "waiting for you"}, {"blocked", "blocked"}, {"idle", "idle"}, {"unknown", "status unknown"}}

// sameTaskStatuses compares normalized providers by coverage and each task's status.
func sameTaskStatuses(a, b TaskProvider) bool {
	return a.Coverage == b.Coverage && slices.EqualFunc(a.Tasks, b.Tasks, func(x, y Task) bool {
		return x.ID == y.ID && x.Status == y.Status
	})
}

// taskSummary counts a provider's tasks by status, or returns "" when none were seen.
func taskSummary(p TaskProvider) string {
	counts := map[string]int{}
	for _, task := range normalizeTaskProvider(p).Tasks {
		counts[task.Status]++
	}
	var parts []string
	for _, state := range taskStatuses {
		if count := counts[state.key]; count > 0 {
			parts = append(parts, fmt.Sprintf("%d %s", count, state.label))
		}
	}
	return strings.Join(parts, ", ")
}
