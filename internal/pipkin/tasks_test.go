package pipkin

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"testing"
	"time"
)

func TestTasksMinimalJSONAndUnknownCounts(t *testing.T) {
	now := time.Unix(1800000000, 0)
	collect := func(context.Context) TaskSnapshot {
		return TaskSnapshot{SchemaVersion: 1, ObservedAt: now.Unix(), Providers: map[string]TaskProvider{
			"claude": {Coverage: "unavailable"},
			"codex":  {Coverage: "partial", Tasks: []Task{{ID: "codex:example", UpdatedAt: new(now.Unix()), Status: "needs_attention"}}},
		}}
	}
	var out bytes.Buffer
	if err := runTasks(context.Background(), true, &out, collect); err != nil {
		t.Fatal(err)
	}
	var decoded map[string]json.RawMessage
	if err := json.Unmarshal(out.Bytes(), &decoded); err != nil || len(decoded) != 3 {
		t.Fatalf("invalid envelope: %s (%v)", out.String(), err)
	}
	var values map[string]struct {
		Coverage string                       `json:"coverage"`
		Tasks    []map[string]json.RawMessage `json:"tasks"`
	}
	if err := json.Unmarshal(decoded["providers"], &values); err != nil {
		t.Fatal(err)
	}
	if values["claude"].Tasks != nil || values["claude"].Coverage != "unavailable" || len(values["codex"].Tasks[0]) != 3 {
		t.Fatalf("missing data became a count or task fields grew: %s", out.String())
	}
	out.Reset()
	if err := runTasks(context.Background(), false, &out, collect); err != nil {
		t.Fatal(err)
	}
	text := out.String()
	for _, want := range []string{"Codex        1 waiting for you\n", "Claude Code  None found\n"} {
		if !strings.Contains(text, want) {
			t.Fatalf("text lacks %q: %s", want, text)
		}
	}
	if strings.Contains(text, "0 ") {
		t.Fatalf("missing data became a zero count: %s", text)
	}
}

func TestTaskProviderNormalizationPreservesUncertainty(t *testing.T) {
	badEpoch := int64(1)
	p := normalizeTaskProvider(TaskProvider{Coverage: "partial", Tasks: []Task{
		{ID: "same", Status: "working"}, {ID: "same", Status: "idle"},
		{ID: "other", Status: "unexpected", UpdatedAt: &badEpoch}, {Status: "working"},
	}})
	if p.Coverage != "partial" || len(p.Tasks) != 2 || p.Tasks[0].Status != "unknown" || p.Tasks[0].UpdatedAt != nil || p.Tasks[1].Status != "unknown" {
		t.Fatalf("normalization lost uncertainty: %#v", p)
	}
	for _, input := range []TaskProvider{{Coverage: "partial"}, {Coverage: "complete", Tasks: []Task{{ID: "ignored", Status: "working"}}}} {
		p := normalizeTaskProvider(input)
		if p.Coverage != "unavailable" || p.Tasks != nil {
			t.Fatalf("empty or unsupported source became complete: %#v", p)
		}
	}
	if taskOpaqueID("claude", "private") == taskOpaqueID("codex", "private") || strings.Contains(taskOpaqueID("claude", "private"), "private") {
		t.Fatal("task IDs expose or merge raw identities")
	}
}

func TestTaskCollectorsAreIndependentAndCancellationIsUnknown(t *testing.T) {
	now := func() time.Time { return time.Unix(1800000000, 0) }
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	s := collectTasksWith(ctx, now, taskCollectors{"codex": func(context.Context) TaskProvider {
		t.Error("canceled collection ran")
		return TaskProvider{}
	}})
	if s.Providers["codex"].Tasks != nil || s.Providers["codex"].Coverage != "unavailable" {
		t.Fatalf("cancellation became zero: %#v", s)
	}
	s = collectTasksWith(context.Background(), now, taskCollectors{
		"codex": func(context.Context) TaskProvider { return TaskProvider{Coverage: "unavailable"} },
		"claude": func(context.Context) TaskProvider {
			return TaskProvider{Coverage: "partial", Tasks: []Task{{ID: "test", Status: "working"}}}
		},
	})
	if s.Providers["codex"].Tasks != nil || len(s.Providers["claude"].Tasks) != 1 || s.ObservedAt != now().Unix() {
		t.Fatalf("one source affected the other: %#v", s)
	}
}

func TestTaskOutputReportsWriterErrors(t *testing.T) {
	collect := func(context.Context) TaskSnapshot { return TaskSnapshot{} }
	for _, jsonOutput := range []bool{false, true} {
		if err := runTasks(context.Background(), jsonOutput, &outputTestWriter{err: io.ErrClosedPipe}, collect); !errors.Is(err, io.ErrClosedPipe) {
			t.Fatalf("writer error lost: %v", err)
		}
	}
}

func TestTaskCacheFreshnessDoesNotInventZeroOrMutateSnapshot(t *testing.T) {
	now := time.Unix(1800000000, 0)
	s := &TaskSnapshot{SchemaVersion: 1, ObservedAt: now.Unix(), Providers: map[string]TaskProvider{"codex": {Coverage: "partial", Tasks: []Task{{ID: "test", Status: "working"}}}}}
	if readableTaskSnapshot(s, true, now) != s || readableTaskSnapshot(s, true, now.Add(taskFreshFor-time.Second)) != s {
		t.Fatal("fresh cache was discarded")
	}
	for _, current := range []struct {
		running bool
		now     time.Time
	}{{false, now}, {true, now.Add(taskFreshFor)}, {true, now.Add(-time.Second)}} {
		p := readableTaskSnapshot(s, current.running, current.now).Providers["codex"]
		if p.Coverage != "unavailable" || p.Tasks != nil {
			t.Fatalf("stale or stopped cache looked live: %#v", p)
		}
	}
	if len(s.Providers["codex"].Tasks) != 1 {
		t.Fatal("reading mutated the saved task snapshot")
	}
}

func TestHelperTaskPollingIsAsynchronousAndIndependentOfUSB(t *testing.T) {
	h, _ := testHelper()
	now := h.now()
	started, release, finished := make(chan struct{}, 2), make(chan struct{}), make(chan struct{}, 2)
	h.collectTasks = func(context.Context) TaskSnapshot {
		started <- struct{}{}
		<-release
		finished <- struct{}{}
		return TaskSnapshot{SchemaVersion: 1, ObservedAt: now.Unix(), Providers: map[string]TaskProvider{"codex": {Coverage: "unavailable"}}}
	}
	h.conn = nil
	h.pollTasks(context.Background(), now)
	<-started
	h.pollTasks(context.Background(), now.Add(time.Second))
	if !h.taskPending || h.tasks != nil || len(started) != 0 {
		t.Fatal("overlapping task scan or blocking helper poll")
	}
	close(release)
	<-finished
	// Join the completed worker through its result channel, without sleeping.
	result := <-h.taskResults
	h.taskResults <- result
	h.pollTasks(context.Background(), now.Add(time.Second))
	if h.taskPending || h.tasks == nil {
		t.Fatal("completed task scan was not cached")
	}
	h.pollTasks(context.Background(), now.Add(taskPollEvery))
	<-started
	<-finished
	<-h.taskResults
}

func TestHelperTaskRejectsPreWakeResults(t *testing.T) {
	t.Setenv("PIPKIN_HOME", t.TempDir())
	h, _ := testHelper()
	clock := h.now()
	h.now = func() time.Time { return clock }
	old := &TaskSnapshot{SchemaVersion: 1, ObservedAt: clock.Unix(), Providers: map[string]TaskProvider{
		"claude": {Coverage: "partial", Tasks: []Task{{ID: "claude:old", Status: "working"}}},
	}}
	h.tasks = old
	started, canceled, release := make(chan struct{}), make(chan struct{}), make(chan struct{})
	h.collectTasks = func(ctx context.Context) TaskSnapshot {
		close(started)
		<-ctx.Done()
		close(canceled)
		<-release
		// The underlying read began before sleep. Completion time alone would
		// make this captured state look newly observed after resume.
		return TaskSnapshot{SchemaVersion: 1, ObservedAt: h.now().Unix(), Providers: old.Providers}
	}
	h.pollTasks(context.Background(), clock)
	<-started
	h.handlePowerEvent(powerEvent{kind: powerSystemSleep, active: true})
	clock = clock.Add(time.Hour)
	h.handlePowerEvent(powerEvent{kind: powerSystemSleep, active: false})
	<-canceled
	if !h.taskPending || h.tasks.Providers["claude"].Coverage != "unavailable" || h.tasks.Providers["claude"].Tasks != nil {
		t.Fatal("wake overlapped an in-flight scan or kept cached task lists live")
	}
	h.pollTasks(context.Background(), clock)
	if len(old.Providers["claude"].Tasks) != 1 {
		t.Fatal("invalidating a cache changed the earlier snapshot")
	}
	close(release)
	stale := <-h.taskResults
	h.taskResults <- stale
	freshStarted, freshRelease := make(chan struct{}), make(chan struct{})
	h.collectTasks = func(context.Context) TaskSnapshot {
		close(freshStarted)
		<-freshRelease
		return TaskSnapshot{SchemaVersion: 1, ObservedAt: clock.Unix(), Providers: map[string]TaskProvider{
			"claude": {Coverage: "partial", Tasks: []Task{{ID: "claude:fresh", Status: "idle"}}},
		}}
	}
	h.pollTasks(context.Background(), clock)
	<-freshStarted
	if !h.taskPending || h.tasks.Providers["claude"].Coverage != "unavailable" {
		t.Fatal("a pre-wake result was published instead of immediately re-observed")
	}
	close(freshRelease)
	fresh := <-h.taskResults
	h.taskResults <- fresh
	h.pollTasks(context.Background(), clock.Add(time.Second))
	if h.taskPending || h.tasks.Providers["claude"].Tasks[0].ID != "claude:fresh" {
		t.Fatal("the post-wake observation was not accepted")
	}
}

func TestHelperTasksRenewOnStatusChangeAndEveryRefresh(t *testing.T) {
	h, _ := testHelper()
	start := h.now()
	poll := func(at time.Time, status string) *TaskSnapshot {
		t.Helper()
		h.collectTasks = func(context.Context) TaskSnapshot {
			return TaskSnapshot{SchemaVersion: 1, ObservedAt: at.Unix(), Providers: map[string]TaskProvider{
				"claude": {Coverage: "partial", Tasks: []Task{{ID: "claude:one", Status: status}}}}}
		}
		h.due["tasks"] = time.Time{}
		h.pollTasks(context.Background(), at)
		result := <-h.taskResults
		h.taskResults <- result
		h.due["tasks"] = at.Add(time.Hour)
		h.pollTasks(context.Background(), at)
		return h.tasks
	}
	first := poll(start, "working")
	if poll(start.Add(taskPollEvery), "working") != first || poll(start.Add(taskRefreshEvery-time.Second), "working") != first {
		t.Fatal("an unchanged read renewed the cache before the refresh interval")
	}
	changed := poll(start.Add(taskRefreshEvery-time.Second), "needs_attention")
	if changed == first || changed.ObservedAt != start.Add(taskRefreshEvery-time.Second).Unix() {
		t.Fatal("a status change waited for the refresh interval")
	}
	if renewed := poll(time.Unix(changed.ObservedAt, 0).Add(taskRefreshEvery), "needs_attention"); renewed == changed {
		t.Fatal("unchanged statuses were never renewed")
	}
}

func TestHelperTasksPauseWhileAsleepAndWakeOnCodexChange(t *testing.T) {
	h, _ := testHelper()
	h.collectTasks = func(context.Context) TaskSnapshot {
		return TaskSnapshot{SchemaVersion: 1, ObservedAt: testNow, Providers: map[string]TaskProvider{"codex": {Coverage: "unavailable"}}}
	}
	var changed func()
	closed := 0
	h.startCodexWatch = func(_ context.Context, notify func()) *codexTaskWatch {
		changed = notify
		return &codexTaskWatch{cancel: func() { closed++ }}
	}
	now := h.now()
	h.pollTasks(context.Background(), now)
	<-h.taskWake // the finished read wakes the loop
	h.pollTasks(context.Background(), now)
	if h.codexWatch.Load() == nil || h.taskPending || !h.tasksCurrent {
		t.Fatal("polling did not start a Codex watch and cache the read")
	}
	changed()
	select {
	case <-h.taskWake:
	default:
		t.Fatal("a Codex change did not wake the helper loop")
	}
	h.pollTasks(context.Background(), now)
	if !h.taskPending {
		t.Fatal("a Codex change waited for the next poll")
	}
	result := <-h.taskResults
	h.taskResults <- result
	h.power.displaySleeping = true
	h.pollTasks(context.Background(), now)
	if h.codexWatch.Load() != nil || closed != 1 || h.tasksCurrent || h.tasks.Providers["codex"].Coverage != "unavailable" {
		t.Fatal("task reads kept running while the display slept")
	}
	h.pollTasks(context.Background(), now.Add(time.Minute))
	if len(h.taskResults) != 1 || h.codexWatch.Load() != nil {
		t.Fatal("a sleeping host started a task read")
	}
}
