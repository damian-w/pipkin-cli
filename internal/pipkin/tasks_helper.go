package pipkin

import (
	"context"
	"time"
)

type taskCollectionResult struct {
	snapshot   TaskSnapshot
	generation uint64
}

func (h *Helper) invalidateTasks(now time.Time) {
	h.taskGeneration = h.generation
	h.tasks = readableTaskSnapshot(h.tasks, false, now)
	h.due["tasks"] = time.Time{}
	if h.taskCancel != nil {
		h.taskCancel()
	}
}

// Local task reads run independently of USB and subscription polling. A slow or
// unsupported GUI cannot delay the display loop or change its wire protocol.
func (h *Helper) pollTasks(ctx context.Context, now time.Time) {
	if h.collectTasks == nil || ctx.Err() != nil {
		return
	}
	if h.taskGeneration != h.generation {
		h.invalidateTasks(now)
	}
	select {
	case result := <-h.taskResults:
		h.taskPending = false
		if h.taskCancel != nil {
			h.taskCancel()
			h.taskCancel = nil
		}
		if result.generation == h.generation {
			h.tasks = &result.snapshot
		} else {
			// A pre-wake worker may finish after resume and stamp its old
			// observations with the new wall clock. Re-read before publishing.
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
	if running && snapshot.ObservedAt > 0 && age >= 0 && age <= int64((taskPollEvery+taskCollectionBudget+tick)/time.Second) {
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
