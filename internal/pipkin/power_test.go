package pipkin

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestHostPowerCombinesIndependentReasons(t *testing.T) {
	var power hostPower
	for _, step := range []struct {
		kind   powerEventKind
		active bool
		state  string
	}{
		{powerDisplaySleep, true, "asleep"},
		{powerSystemSleep, true, "asleep"},
		{powerSystemSleep, false, "asleep"}, // Maintenance resume, screens still off.
		{powerDisplaySleep, false, "awake"},
		{powerShutdown, true, "asleep"},
		{powerDisplaySleep, false, "asleep"},
		{powerSystemSleep, false, "asleep"},
		{powerShutdown, false, "awake"}, // A cancelled shutdown recovers.
	} {
		power.apply(powerEvent{kind: step.kind, active: step.active})
		if power.state() != step.state {
			t.Fatalf("event %d/%t: state=%s, want %s", step.kind, step.active, power.state(), step.state)
		}
	}
}

func TestSleepSurvivesReconnectAndHostReports(t *testing.T) {
	isolate(t)
	h, lines := testHelper()
	h.conn = &connection{port: &fakePort{}, name: "COM1"}
	h.handlePowerEvent(powerEvent{kind: powerDisplaySleep, active: true})
	h.sendHostPower() // The same path is used for regular heartbeats.
	h.resync(map[string]string{"seq": "0", "clock_epoch": "0", "unix": "null"})
	for _, line := range *lines {
		if strings.Contains(line, "kind=host") && !strings.Contains(line, "state=asleep") {
			t.Fatalf("sleep was overwritten by reconnect/heartbeat: %s", line)
		}
	}
	h.handlePowerEvent(powerEvent{kind: powerShutdown, active: true})
	h.run(cancelledContext())
	if last := (*lines)[len(*lines)-1]; !strings.Contains(last, "state=asleep") {
		t.Fatalf("shutdown cleanup overwrote sleep: %s", last)
	}
}

func TestOrdinaryHelperStopReportsDisconnectWithScreensAsleep(t *testing.T) {
	isolate(t)
	h, lines := testHelper()
	h.conn = &connection{port: &fakePort{}, name: "COM1"}
	h.power.displaySleeping = true
	h.run(cancelledContext())
	if len(*lines) != 1 || !strings.Contains((*lines)[0], "state=disconnected") {
		t.Fatal("ordinary helper termination was mislabeled as host shutdown")
	}
}

func TestPowerEventAcknowledgesAfterBoundedSerialAttempt(t *testing.T) {
	h, _ := testHelper()
	h.conn = &connection{port: &fakePort{}, name: "COM1"}
	done := make(chan struct{})
	attempted := false
	h.write = func(ctx context.Context, line string) error {
		select {
		case <-done:
			t.Fatal("native observer was acknowledged before the serial write")
		default:
		}
		deadline, bounded := ctx.Deadline()
		if !bounded || time.Until(deadline) > powerWriteTimeout || !strings.Contains(line, "state=asleep") {
			t.Fatal("pre-sleep write was not bounded or did not report sleep")
		}
		attempted = true
		return errors.New("USB suspended")
	}
	h.handlePowerEvent(powerEvent{kind: powerSystemSleep, active: true, done: done})
	select {
	case <-done:
	default:
		t.Fatal("failed pre-sleep delivery left the native observer waiting")
	}
	if !attempted || h.conn != nil || !h.power.systemSleeping {
		t.Fatal("failed USB delivery must preserve sleep and close the broken handle")
	}
}

func TestPreSleepConfirmationRequiresAcceptedSequence(t *testing.T) {
	h, _ := testHelper()
	port := &fakePort{incoming: []byte("v=1 kind=identity product=pipkin seq=0 clock_epoch=0 unix=null\n")}
	h.conn = &connection{port: port, name: "COM1"}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if h.confirmHostPower(ctx, 1) || ctx.Err() == nil {
		t.Fatal("an old identity response acknowledged a newer sleep report")
	}
	port.incoming = []byte("v=1 kind=identity product=pipkin seq=1 clock_epoch=0 unix=null\n")
	ctx, cancel = context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if !h.confirmHostPower(ctx, 1) || ctx.Err() != nil || len(port.written) != 2 {
		t.Fatal("firmware's accepted sleep sequence did not confirm delivery")
	}
}

func TestSleepConfirmationRejectsAheadSequenceOrChangedEpoch(t *testing.T) {
	for _, reply := range []string{
		"v=1 kind=identity product=pipkin seq=10 clock_epoch=0 unix=null\n",
		"v=1 kind=identity product=pipkin seq=1 clock_epoch=1 unix=null\n",
	} {
		h, _ := testHelper()
		h.sequence = 1
		h.due["identify"] = h.now().Add(time.Minute)
		h.conn = &connection{port: &fakePort{incoming: []byte(reply)}, name: "COM1"}
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		confirmed := h.confirmHostPower(ctx, 1)
		cancel()
		if confirmed || !h.due["identify"].IsZero() {
			t.Fatalf("unrelated identity confirmed sleep instead of scheduling USB verification: %s", reply)
		}
		if strings.Contains(reply, "seq=10") && h.sequence != 10 {
			t.Fatal("firmware's higher sequence was discarded, which could reject wake reports")
		}
	}
}

func TestPreSleepNativeAckFollowsFirmwareAcceptance(t *testing.T) {
	h, _ := testHelper()
	port := &fakePort{}
	h.conn = &connection{port: port, name: "COM1"}
	done := make(chan struct{})
	h.write = func(ctx context.Context, line string) error {
		if !strings.Contains(line, "state=asleep") {
			t.Fatal("pre-sleep report did not put the display to sleep")
		}
		port.incoming = []byte(fmt.Sprintf("v=1 kind=identity product=pipkin seq=%d clock_epoch=0 unix=null\n", h.sequence))
		return port.Write(ctx, []byte(line))
	}
	h.handlePowerEvent(powerEvent{kind: powerSystemSleep, active: true, done: done})
	select {
	case <-done:
	default:
		t.Fatal("firmware acceptance did not release native sleep")
	}
	if len(port.incoming) != 0 || len(port.written) != 2 || port.written[1] != "v=1 kind=identify\n" {
		t.Fatal("native sleep was acknowledged without checking firmware acceptance")
	}
}

func TestResumeRefreshesWithoutSendingLatePreSleepQuota(t *testing.T) {
	h, lines := testHelper()
	h.power = hostPower{systemSleeping: true, displaySleeping: true}
	h.conn = &connection{port: &fakePort{}, name: "COM1"}
	h.cooldown["codex"] = h.now().Add(time.Hour)
	h.handlePowerEvent(powerEvent{kind: powerSystemSleep, active: false})
	if h.generation != 1 || !h.due["discover"].IsZero() || !h.due["identify"].IsZero() {
		t.Fatal("resume did not request USB verification and fresh collection")
	}
	if !h.cooldown["codex"].Equal(h.now().Add(time.Hour)) {
		t.Fatal("resume discarded the provider's server cooldown")
	}
	if len(*lines) != 1 || !strings.Contains((*lines)[0], "state=asleep") {
		t.Fatal("background resume woke Pipkin with sleeping displays")
	}
	h.results <- providerResult{provider: "codex", generation: 0, reading: &Reading{Account: "old", Observed: testNow - 3600}}
	h.pollProviders(cancelledContext(), h.now())
	h.flush()
	if len(*lines) != 2 || strings.Contains(strings.Join(*lines, ""), "kind=usage") || !h.due["codex"].Equal(h.now()) {
		t.Fatal("a queued pre-sleep result must be re-observed before sending usage")
	}
	h.handlePowerEvent(powerEvent{kind: powerDisplaySleep, active: false})
	if !strings.Contains((*lines)[len(*lines)-1], "state=awake") {
		t.Fatal("screen wake did not immediately report awake")
	}
}

func TestDisplaySleepPausesProviderWork(t *testing.T) {
	h, _ := testHelper()
	h.power.displaySleeping = true
	var called atomic.Bool
	h.collect = func(context.Context, string, string) providerResult { called.Store(true); return providerResult{} }
	h.pollProviders(context.Background(), h.now())
	if h.pending["codex"] || h.pending["claude"] || called.Load() {
		t.Fatal("sleep launched provider work")
	}
}

func TestResumeReobservesQueuedFailuresAndPreservesRetryAfter(t *testing.T) {
	for _, failure := range []error{
		errors.New("old HTTP 500"),
		offlineError{"old network failure"},
		errSignedOut,
		&usageRetryError{message: "old HTTP 429", RetryAt: time.Unix(testNow, 0).Add(time.Hour)},
	} {
		t.Run(failure.Error(), func(t *testing.T) {
			h, _ := testHelper()
			h.power.systemSleeping = true
			h.handlePowerEvent(powerEvent{kind: powerSystemSleep, active: false})
			h.results <- providerResult{provider: "codex", generation: 0, err: failure}
			h.pollProviders(cancelledContext(), h.now())
			want := h.now()
			var retry *usageRetryError
			if errors.As(failure, &retry) {
				want = retry.RetryAt
			}
			if !h.due["codex"].Equal(want) {
				t.Fatalf("queued pre-sleep failure delayed refresh until %v, want %v", h.due["codex"], want)
			}
		})
	}
}

func TestPreSleepEventInterruptsSerialFlush(t *testing.T) {
	h, lines := testHelper()
	h.conn = &connection{port: &fakePort{}, name: "COM1"}
	h.apps["codex"] = "available"
	h.readings["codex"] = &Reading{Account: "account", Observed: testNow, Weekly: &Window{Used: 10}}
	events := make(chan powerEvent, 2)
	h.powerEvents = events
	ack := make(chan struct{})
	h.write = func(_ context.Context, line string) error {
		*lines = append(*lines, line)
		if strings.Contains(line, "kind=app") {
			events <- powerEvent{kind: powerSystemSleep, active: true, done: ack}
		}
		return nil
	}
	h.flush()
	if len(*lines) != 2 || !strings.Contains((*lines)[1], "state=asleep") || strings.Contains(strings.Join(*lines, ""), "kind=usage") {
		t.Fatalf("sleep did not interrupt queued serial work: %q", *lines)
	}
	select {
	case <-ack:
	default:
		t.Fatal("sleep was not acknowledged between serial writes")
	}
}

func TestResumeAtSerialBoundaryRejectsBuiltPreSleepUsage(t *testing.T) {
	h, lines := testHelper()
	h.conn = &connection{port: &fakePort{}, name: "COM1"}
	h.power.systemSleeping = true
	h.apps["codex"], h.sent["codex_app"] = "available", "available"
	h.readings["codex"] = &Reading{Account: "account", Observed: testNow - 3600, Weekly: &Window{Used: 10}}
	events := make(chan powerEvent, 2)
	h.powerEvents = events
	events <- powerEvent{kind: powerSystemSleep, active: false}
	h.flush()
	if h.generation != 1 || len(*lines) != 1 || !strings.Contains((*lines)[0], "state=awake") || strings.Contains(strings.Join(*lines, ""), "kind=usage") {
		t.Fatalf("resume sent an already-built pre-sleep reading: %q", *lines)
	}
	if _, cached := h.sent["codex"]; cached {
		t.Fatal("undelivered usage was cached as sent")
	}
}

func TestNativeEventWaitIsBoundedAndCancellationSafe(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	postResult := make(chan bool, 1)
	events := make(chan powerEvent, 1)
	go func() { postResult <- postPowerEvent(ctx, events, powerSystemSleep, true, true) }()
	event := <-events
	select {
	case <-postResult:
		t.Fatal("pre-sleep callback did not wait for its acknowledgment")
	default:
	}
	close(event.done)
	if !<-postResult {
		t.Fatal("acknowledged event was not reported as delivered")
	}
	cancel()
	if postPowerEvent(ctx, make(chan powerEvent), powerShutdown, true, true) {
		t.Fatal("cancelled observer reported delivery")
	}
	started := time.Now()
	if postPowerEvent(context.Background(), make(chan powerEvent), powerShutdown, true, true) {
		t.Fatal("unconsumed event reported delivery")
	}
	if elapsed := time.Since(started); elapsed > 1500*time.Millisecond {
		t.Fatalf("unconsumed notification held up the host for %v", elapsed)
	}
}

func TestPowerEventsInterruptHelperTicker(t *testing.T) {
	isolate(t)
	h, _ := testHelper()
	h.conn = &connection{port: &fakePort{}, name: "COM1"}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ready := make(chan chan<- powerEvent, 1)
	h.monitor = func(_ context.Context, events chan<- powerEvent) (func(), error) {
		ready <- events
		return func() {}, nil
	}
	h.pending["codex"], h.pending["claude"] = true, true
	reported := make(chan string, 16)
	h.write = func(_ context.Context, line string) error { reported <- line; return nil }
	stopped := make(chan struct{})
	go func() { h.run(ctx); close(stopped) }()
	events := <-ready
	// The helper's timer is two seconds. Notification delivery should interrupt
	// it and finish a host report within the native callback's one-second budget.
	ack := make(chan struct{})
	events <- powerEvent{kind: powerSystemSleep, active: true, done: ack}
	select {
	case <-ack:
	case <-time.After(time.Second):
		t.Fatal("sleep notification waited for the periodic helper timer")
	}
	seen := false
	for !seen {
		select {
		case line := <-reported:
			seen = strings.Contains(line, "state=asleep")
		case <-time.After(time.Second):
			t.Fatal("sleep was acknowledged without reporting it")
		}
	}
	cancel()
	select {
	case <-stopped:
	case <-time.After(time.Second):
		t.Fatal("sleeping helper did not stop promptly")
	}
}
