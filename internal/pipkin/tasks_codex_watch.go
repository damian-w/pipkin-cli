package pipkin

import (
	"context"
	"slices"
	"sync"
	"time"
)

// Reconnect delays for a live watch. A missing app is retried at the third delay
// at most; a connection that failed after opening backs off to the last.
var codexTaskRetry = []time.Duration{time.Second, 2 * time.Second, 5 * time.Second, 10 * time.Second, 30 * time.Second}

// codexTaskWatch keeps one live connection to the Codex app and holds its latest
// reduced state, so task changes arrive as the app streams them.
type codexTaskWatch struct {
	cancel context.CancelFunc
	done   chan struct{}
	mu     sync.Mutex
	value  TaskProvider
}

// startLiveCodexTasks watches the installed Codex app, or returns nil where tasks
// are unsupported. changed is called when a task's status changes.
func startLiveCodexTasks(ctx context.Context, changed func()) *codexTaskWatch {
	opts, coverage := codexDesktopTasks()
	if coverage != "" {
		return nil
	}
	return startCodexTaskWatch(ctx, opts, changed)
}

func startCodexTaskWatch(ctx context.Context, opts codexTaskOptions, changed func()) *codexTaskWatch {
	ctx, cancel := context.WithCancel(ctx)
	w := &codexTaskWatch{cancel: cancel, done: make(chan struct{}), value: TaskProvider{Coverage: "unavailable"}}
	go w.run(ctx, opts, changed)
	return w
}

func (w *codexTaskWatch) run(ctx context.Context, opts codexTaskOptions, changed func()) {
	defer close(w.done)
	retry := opts.retry
	if len(retry) == 0 {
		retry = codexTaskRetry
	}
	publish := func(p TaskProvider) { w.set(p, changed) }
	failures := 0
	for ctx.Err() == nil {
		started := time.Now()
		limit := min(2, len(retry)-1)
		if observer, err := openCodexTaskObserver(ctx, opts, publish); err == nil {
			observer.run()
			observer.close()
			limit = len(retry) - 1
		}
		publish(TaskProvider{Coverage: "unavailable"})
		// A connection that lasted a minute was healthy, so start backing off afresh.
		if time.Since(started) >= time.Minute {
			failures = 0
		}
		timer := time.NewTimer(retry[min(failures, limit)])
		failures++
		select {
		case <-ctx.Done():
			timer.Stop()
		case <-timer.C:
		}
	}
}

func (w *codexTaskWatch) set(p TaskProvider, changed func()) {
	p = normalizeTaskProvider(p)
	w.mu.Lock()
	moved := !sameTaskStatuses(w.value, p)
	w.value = p
	w.mu.Unlock()
	if moved && changed != nil {
		changed()
	}
}

// current returns the latest state; it is live for as long as the watch runs.
func (w *codexTaskWatch) current() TaskProvider {
	w.mu.Lock()
	defer w.mu.Unlock()
	p := w.value
	p.Tasks = slices.Clone(p.Tasks)
	return p
}

// close stops the watch; its connection closes and its state becomes unavailable.
func (w *codexTaskWatch) close() { w.cancel() }
