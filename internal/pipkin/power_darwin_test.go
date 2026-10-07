package pipkin

import (
	"context"
	"sync/atomic"
	"testing"
	"time"
	"unsafe"

	"github.com/ebitengine/purego/objc"
)

func TestDarwinDisplaysAsleep(t *testing.T) {
	for _, test := range []struct {
		name                  string
		states                []uint32
		status                int32
		wantAsleep, wantKnown bool
	}{
		{name: "all displays sleeping", states: []uint32{1, 1}, wantAsleep: true, wantKnown: true},
		{name: "external display awake", states: []uint32{1, 0}, wantKnown: true},
		{name: "all displays awake", states: []uint32{0, 0}, wantKnown: true},
		{name: "headless"},
		{name: "WindowServer unavailable", status: 1000},
	} {
		t.Run(test.name, func(t *testing.T) {
			online := func(capacity uint32, displays *uint32, count *uint32) int32 {
				*count = uint32(len(test.states))
				if capacity != 0 {
					ids := unsafe.Slice(displays, int(capacity))
					for i := range ids {
						ids[i] = uint32(i)
					}
				}
				return test.status
			}
			asleep, known := darwinDisplaysAsleep(online, func(display uint32) uint32 { return test.states[display] })
			if asleep != test.wantAsleep || known != test.wantKnown {
				t.Fatalf("asleep=%v known=%v, want %v %v", asleep, known, test.wantAsleep, test.wantKnown)
			}
		})
	}
}

func TestDarwinSleepAcknowledgmentAfterHelper(t *testing.T) {
	events := make(chan powerEvent, 4)
	var helperFinished atomic.Bool
	var allowed atomic.Bool
	m := &darwinPowerMonitor{ctx: context.Background(), events: events, rootPort: 42,
		api: &darwinPowerAPI{allowPowerChange: func(port uint32, argument uintptr) int32 {
			if port != 42 || argument != 123 || !helperFinished.Load() {
				t.Error("sleep was acknowledged before helper finished, or with wrong native token")
			}
			allowed.Store(true)
			return 0
		}},
	}
	go func() {
		event := <-events
		if event.kind != powerSystemSleep || !event.active || event.done == nil {
			t.Error("pre-sleep event did not request an acknowledgment")
		}
		helperFinished.Store(true)
		close(event.done)
	}()
	m.systemPowerChanged(darwinSystemWillSleep, 123)
	if !allowed.Load() {
		t.Fatal("native sleep was not acknowledged")
	}
}

func TestDarwinSleepAcknowledgmentOnCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	allowed := false
	m := &darwinPowerMonitor{ctx: ctx, events: make(chan powerEvent), api: &darwinPowerAPI{allowPowerChange: func(uint32, uintptr) int32 { allowed = true; return 0 }}}
	m.systemPowerChanged(darwinSystemWillSleep, 1)
	if !allowed {
		t.Fatal("cancelled helper delayed native sleep")
	}
}

func TestDarwinTentativeSleepDoesNotSleepPipkin(t *testing.T) {
	events := make(chan powerEvent, 4)
	allowed := false
	m := &darwinPowerMonitor{ctx: context.Background(), events: events, api: &darwinPowerAPI{allowPowerChange: func(uint32, uintptr) int32 { allowed = true; return 0 }}}
	m.systemPowerChanged(darwinCanSystemSleep, 1)
	if !allowed || len(events) != 0 {
		t.Fatal("tentative idle sleep must be allowed without changing Pipkin state")
	}
}

func darwinFakeDisplayAPI(asleep uint32) *darwinPowerAPI {
	return &darwinPowerAPI{
		onlineDisplays: func(capacity uint32, displays *uint32, count *uint32) int32 {
			*count = 1
			if capacity != 0 {
				*displays = 7
			}
			return 0
		},
		displayAsleep: func(uint32) uint32 { return asleep },
	}
}

func TestDarwinDarkWakeReconcilesBeforeResume(t *testing.T) {
	events := make(chan powerEvent, 4)
	m := &darwinPowerMonitor{ctx: context.Background(), events: events, api: darwinFakeDisplayAPI(1)}
	m.systemPowerChanged(darwinSystemHasPoweredOn, 0)
	first, second := <-events, <-events
	if first.kind != powerDisplaySleep || !first.active || second.kind != powerSystemSleep || second.active {
		t.Fatalf("resume order can wake Pipkin with sleeping screens: %#v, %#v", first, second)
	}
}

func TestDarwinResumePreservesSleepWhenWindowServerUnavailable(t *testing.T) {
	events := make(chan powerEvent, 4)
	api := darwinFakeDisplayAPI(0)
	api.onlineDisplays = func(uint32, *uint32, *uint32) int32 { return 1000 }
	m := &darwinPowerMonitor{ctx: context.Background(), events: events, api: api}
	m.displaySleeping.Store(true)
	m.systemPowerChanged(darwinSystemHasPoweredOn, 0)
	first, second := <-events, <-events
	if first.kind != powerDisplaySleep || !first.active || second.kind != powerSystemSleep || second.active {
		t.Fatalf("failed screen query could wake Pipkin during maintenance: %#v, %#v", first, second)
	}
}

func TestDarwinDisplayReconciliationPostsOnlyChanges(t *testing.T) {
	events := make(chan powerEvent, 4)
	api := darwinFakeDisplayAPI(0)
	m := &darwinPowerMonitor{ctx: context.Background(), events: events, api: api}
	m.reconcileDisplays()
	if len(events) != 1 {
		t.Fatal("startup display state was not seeded")
	}
	m.reconcileDisplays()
	if len(events) != 1 {
		t.Fatal("unchanged display state was repeated")
	}
	api.displayAsleep = func(uint32) uint32 { return 1 }
	m.reconcileDisplays()
	if len(events) != 2 {
		t.Fatal("changed display state was not delivered")
	}
}

func TestDarwinDisplayQueryCannotUndoPendingSystemSleep(t *testing.T) {
	events := make(chan powerEvent, 4)
	api := darwinFakeDisplayAPI(0)
	api.onlineDisplays = func(uint32, *uint32, *uint32) int32 {
		t.Error("display was queried during a pending system sleep")
		return 1000
	}
	m := &darwinPowerMonitor{ctx: context.Background(), events: events, api: api}
	m.systemSleeping.Store(true)
	m.displaySleeping.Store(true)
	m.reconcileDisplays()
	if event := <-events; event.kind != powerDisplaySleep || !event.active {
		t.Fatalf("pending system sleep screen state: %#v", event)
	}
}

func TestDarwinCancelledShutdownRecovery(t *testing.T) {
	events := make(chan powerEvent, 4)
	m := &darwinPowerMonitor{ctx: context.Background(), events: events, api: darwinFakeDisplayAPI(1)}
	deadline := time.Now().Add(10 * time.Second)
	m.shutdownDeadline.Store(deadline.UnixNano())
	m.recoverCancelledShutdown(deadline.Add(-time.Nanosecond))
	if len(events) != 0 {
		t.Fatal("shutdown recovered before grace elapsed")
	}
	m.recoverCancelledShutdown(deadline)
	first, second := <-events, <-events
	if first.kind != powerDisplaySleep || !first.active || second.kind != powerShutdown || second.active {
		t.Fatalf("shutdown recovery did not preserve sleeping screens: %#v, %#v", first, second)
	}
	m.recoverCancelledShutdown(deadline.Add(time.Second))
	if len(events) != 0 {
		t.Fatal("shutdown recovery repeated without another request")
	}
}

func TestDarwinNativePowerMonitorLifecycle(t *testing.T) {
	// Register observers and invoke our own Objective-C callbacks only. This test
	// never requests display sleep, system sleep, logout, or power-off.
	for i := 0; i < 2; i++ {
		events := make(chan powerEvent, 16)
		ctx, cancel := context.WithCancel(context.Background())
		stop, err := startPowerMonitor(ctx, events)
		if err != nil {
			cancel()
			t.Fatal(err)
		}
		var monitor *darwinPowerMonitor
		darwinPowerMonitors.Range(func(_, value any) bool { monitor = value.(*darwinPowerMonitor); return false })
		if monitor == nil {
			cancel()
			stop()
			t.Fatal("monitor was not registered")
		}
		asleep, known := darwinDisplaysAsleep(monitor.api.onlineDisplays, monitor.api.displayAsleep)
		t.Logf("native CoreGraphics display query: known=%t asleep=%t", known, asleep)
		for len(events) != 0 {
			<-events
		}
		// Establish an opposite synthetic state first, even on a Mac whose
		// actual screens were already sleeping when the test started.
		monitor.postDisplayState(false)
		for len(events) != 0 {
			<-events
		}
		monitor.observer.Send(objc.RegisterName("screensDidSleep:"), objc.ID(0))
		select {
		case event := <-events:
			if event.kind != powerDisplaySleep || !event.active {
				t.Errorf("native display sleep callback: %#v", event)
			}
		case <-time.After(time.Second):
			t.Error("native callback was not delivered")
		}
		monitor.observer.Send(objc.RegisterName("screensDidWake:"), objc.ID(0))
		select {
		case event := <-events:
			if event.kind != powerDisplaySleep || event.active {
				t.Errorf("native display wake callback: %#v", event)
			}
		case <-time.After(time.Second):
			t.Error("native callback was not delivered")
		}
		cancel()
		stop()
		stop() // Idempotent teardown must not free native resources twice.
		if _, ok := darwinPowerMonitors.Load(monitor.id); ok {
			t.Fatal("system power registration leaked")
		}
		if _, ok := darwinPowerObservers.Load(monitor.observer); ok {
			t.Fatal("workspace power registration leaked")
		}
	}
}
