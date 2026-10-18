package pipkin

import (
	"context"
	"fmt"
	"time"
)

// The display shows observed counts up to this bound.
const maxTaskCount = 999

type taskCollectionResult struct {
	snapshot   TaskSnapshot
	generation uint64
}

// Ticker jitter must not skip a poll.
const taskPollSlack = 250 * time.Millisecond

func (h *Helper) invalidateTasks(now time.Time) {
	h.tasks = readableTaskSnapshot(h.tasks, false, now)
	h.tasksCurrent = false
	h.due["tasks"] = time.Time{}
	if h.taskCancel != nil {
		h.taskCancel()
	}
	// A fresh Codex watch starts on the next poll.
	if watch := h.codexWatch.Swap(nil); watch != nil {
		watch.close()
	}
	h.codexChanged.Store(false)
}

// pollTasks collects in the background so a slow app never delays the display.
// Claude is read every taskPollEvery; Codex comes from its live watch, and a
// change there polls at once. Task reads pause while the host sleeps.
func (h *Helper) pollTasks(ctx context.Context, now time.Time) {
	if h.collectTasks == nil || ctx.Err() != nil {
		return
	}
	if h.power.asleep() {
		if h.tasksCurrent || h.codexWatch.Load() != nil {
			h.invalidateTasks(now)
		}
		return
	}
	h.watchCodexTasks(ctx)
	if h.codexChanged.Swap(false) {
		h.due["tasks"] = time.Time{}
	}
	select {
	case result := <-h.taskResults:
		h.taskPending = false
		if h.taskCancel != nil {
			h.taskCancel()
			h.taskCancel = nil
		}
		if result.generation == h.generation {
			// Keep the earlier observation while statuses hold, so the cache and
			// display renew on change and every taskRefreshEvery, not every read.
			if !h.tasksCurrent || h.tasks == nil || !sameTaskSnapshot(h.tasks, &result.snapshot) ||
				result.snapshot.ObservedAt-h.tasks.ObservedAt >= int64(taskRefreshEvery/time.Second) {
				h.tasks = &result.snapshot
			}
			h.tasksCurrent = true
		} else {
			// A result from before sleep is stale; collect again.
			h.due["tasks"] = time.Time{}
		}
	default:
	}
	if h.taskPending || now.Before(h.due["tasks"]) {
		return
	}
	h.taskPending = true
	h.due["tasks"] = now.Add(taskPollEvery - taskPollSlack)
	workCtx, cancel := context.WithCancel(ctx)
	h.taskCancel = cancel
	collect, results, generation, wake := h.collectTasks, h.taskResults, h.generation, h.taskWake
	go func() {
		result := collect(workCtx)
		cancel()
		select {
		case results <- taskCollectionResult{snapshot: result, generation: generation}:
			signalTasks(wake)
		case <-ctx.Done():
		}
	}()
}

func (h *Helper) watchCodexTasks(ctx context.Context) {
	if h.startCodexWatch == nil || h.codexWatch.Load() != nil {
		return
	}
	wake := h.taskWake
	h.codexWatch.Store(h.startCodexWatch(ctx, func() {
		h.codexChanged.Store(true)
		signalTasks(wake)
	}))
}

// collectLiveTasks reads Claude afresh and takes Codex from the live watch.
func (h *Helper) collectLiveTasks(ctx context.Context) TaskSnapshot {
	return collectTasksWith(ctx, time.Now, taskCollectors{
		"claude": collectClaudeTasks,
		"codex": func(context.Context) TaskProvider {
			if watch := h.codexWatch.Load(); watch != nil {
				return watch.current()
			}
			_, coverage := codexDesktopTasks()
			if coverage != "unsupported" {
				coverage = "unavailable"
			}
			return TaskProvider{Coverage: coverage}
		},
	})
}

// signalTasks wakes the helper loop without blocking; one pending wake is enough.
func signalTasks(wake chan struct{}) {
	select {
	case wake <- struct{}{}:
	default:
	}
}

// sameTaskSnapshot compares every provider's coverage and task statuses.
func sameTaskSnapshot(a, b *TaskSnapshot) bool {
	if len(a.Providers) != len(b.Providers) {
		return false
	}
	for provider, value := range a.Providers {
		other, ok := b.Providers[provider]
		if !ok || !sameTaskStatuses(value, other) {
			return false
		}
	}
	return true
}

func readableTaskSnapshot(snapshot *TaskSnapshot, running bool, now time.Time) *TaskSnapshot {
	if snapshot == nil {
		return nil
	}
	age := now.Unix() - snapshot.ObservedAt
	if running && snapshot.ObservedAt > 0 && age >= 0 && age < int64(taskFreshFor/time.Second) {
		return snapshot
	}
	copy := *snapshot
	copy.Providers = make(map[string]TaskProvider, len(snapshot.Providers))
	for provider, value := range snapshot.Providers {
		if value.Coverage != "unsupported" {
			value.Coverage = "unavailable"
		}
		value.Tasks = nil
		copy.Providers[provider] = value
	}
	return &copy
}

// Protocol 2 displays accept task records; earlier firmware rejects them.
func acceptsTasks(identity map[string]string) bool {
	maximum, ok := decimalUint(identity["protocol_max"])
	return ok && maximum >= 2
}

// taskFields formats a provider's observed counts for the display. Idle and unknown
// tasks, task IDs and source update times stay on the computer.
func taskFields(provider string, snapshot *TaskSnapshot) string {
	if validEpoch(float64(snapshot.ObservedAt)) == 0 {
		return ""
	}
	p := snapshot.Providers[provider]
	coverage := p.Coverage
	if coverage != "partial" && coverage != "unsupported" {
		coverage = "unavailable"
	}
	fields := fmt.Sprintf("kind=tasks provider=%s coverage=%s observed=%d", provider, coverage, snapshot.ObservedAt)
	if coverage != "partial" {
		return fields
	}
	counts := map[string]int{}
	for _, task := range p.Tasks {
		counts[task.Status]++
	}
	return fields + fmt.Sprintf(" working=%d attention=%d blocked=%d", min(counts["working"], maxTaskCount),
		min(counts["needs_attention"], maxTaskCount), min(counts["blocked"], maxTaskCount))
}
