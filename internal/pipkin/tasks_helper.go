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

func (h *Helper) invalidateTasks(now time.Time) {
	h.tasks = readableTaskSnapshot(h.tasks, false, now)
	h.tasksCurrent = false
	h.due["tasks"] = time.Time{}
	if h.taskCancel != nil {
		h.taskCancel()
	}
}

// pollTasks collects in the background so a slow app never delays the display.
func (h *Helper) pollTasks(ctx context.Context, now time.Time) {
	if h.collectTasks == nil || ctx.Err() != nil {
		return
	}
	select {
	case result := <-h.taskResults:
		h.taskPending = false
		if h.taskCancel != nil {
			h.taskCancel()
			h.taskCancel = nil
		}
		if result.generation == h.generation {
			h.tasks, h.tasksCurrent = &result.snapshot, true
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
	h.due["tasks"] = now.Add(taskPollEvery)
	workCtx, cancel := context.WithCancel(ctx)
	h.taskCancel = cancel
	collect, results, generation := h.collectTasks, h.taskResults, h.generation
	go func() {
		result := collect(workCtx)
		cancel()
		select {
		case results <- taskCollectionResult{snapshot: result, generation: generation}:
		case <-ctx.Done():
		}
	}()
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
