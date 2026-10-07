package pipkin

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"sync"
	"testing"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
)

func TestDecodeWindowsPowerNotifications(t *testing.T) {
	setting := func(guid windows.GUID, length, state uint32) *windowsPowerSetting {
		return &windowsPowerSetting{GUID: guid, Length: length, State: state}
	}
	for _, test := range []struct {
		name      string
		eventType uint32
		setting   *windowsPowerSetting
		kind      powerEventKind
		active    bool
		valid     bool
	}{
		{"suspend", windowsPowerSuspend, nil, powerSystemSleep, true, true},
		{"interactive resume", windowsPowerResume, nil, powerSystemSleep, false, true},
		{"automatic resume", windowsPowerResumeAutomatic, nil, powerSystemSleep, false, true},
		{"display off", windowsPowerSettingChange, setting(windowsSessionDisplayStatus, 4, 0), powerDisplaySleep, true, true},
		{"display on", windowsPowerSettingChange, setting(windowsSessionDisplayStatus, 4, 1), powerDisplaySleep, false, true},
		{"display dimmed", windowsPowerSettingChange, setting(windowsSessionDisplayStatus, 4, 2), powerDisplaySleep, false, true},
		{"unsupported display value", windowsPowerSettingChange, setting(windowsSessionDisplayStatus, 4, 3), 0, false, false},
		{"different setting", windowsPowerSettingChange, setting(windows.GUID{}, 4, 0), 0, false, false},
		{"short setting", windowsPowerSettingChange, setting(windowsSessionDisplayStatus, 3, 0), 0, false, false},
		{"oversized setting", windowsPowerSettingChange, setting(windowsSessionDisplayStatus, 8, 0), 0, false, false},
		{"missing setting", windowsPowerSettingChange, nil, 0, false, false},
		{"unrelated event", 0xffff, nil, 0, false, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			kind, active, valid := decodeWindowsPowerNotification(test.eventType, test.setting)
			if kind != test.kind || active != test.active || valid != test.valid {
				t.Fatalf("decoded (%d, %t, %t), want (%d, %t, %t)", kind, active, valid, test.kind, test.active, test.valid)
			}
		})
	}
}

func TestWindowsCommittedShutdown(t *testing.T) {
	for _, test := range []struct {
		name             string
		committed, flags uintptr
		want             bool
	}{
		{"shutdown or restart", 1, 0, true},
		{"cancelled", 0, 0, false},
		{"forced shutdown", 1, 0x40000000, true},
		{"logoff", 1, windowsEndSessionLogoff, false},
		{"app servicing", 1, windowsEndSessionCloseApp, false},
		{"forced logoff", 1, windowsEndSessionLogoff | 0x40000000, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			if got := windowsCommittedShutdown(test.committed, test.flags); got != test.want {
				t.Fatalf("committed shutdown = %t, want %t", got, test.want)
			}
		})
	}
}

type fakeWindowsPowerPlatform struct {
	fail              string
	calls             []string
	subscription      *windowsPowerSubscription
	window            uintptr
	loopStarted       chan struct{}
	initialDisplay    *windowsPowerSetting
	deliverInitial    chan struct{}
	subscribed        chan struct{}
	subscribedOnce    sync.Once
	refresh           chan struct{}
	displayReplies    chan fakeWindowsDisplayReply
	displayRegistered chan uintptr
	displayAttempted  chan uintptr
	allowRefresh      chan struct{}
	refreshEntered    chan struct{}
}

type fakeWindowsDisplayReply struct {
	setting *windowsPowerSetting
	deliver chan struct{}
	fail    bool
}

func (platform *fakeWindowsPowerPlatform) openWindow(token uintptr) (uintptr, func(), error) {
	platform.calls = append(platform.calls, "open")
	if platform.fail == "open" {
		return 0, nil, errors.New("open failed")
	}
	platform.window = token
	platform.refresh = make(chan struct{}, 1)
	return token, func() { platform.calls = append(platform.calls, "close") }, nil
}

func (platform *fakeWindowsPowerPlatform) subscribeSleep(subscription *windowsPowerSubscription) (func(), error) {
	platform.calls = append(platform.calls, "sleep")
	if platform.fail == "sleep" {
		return nil, errors.New("sleep failed")
	}
	platform.subscription = subscription
	return func() { platform.calls = append(platform.calls, "unsleep") }, nil
}

func (platform *fakeWindowsPowerPlatform) subscribeDisplay(subscription *windowsPowerSubscription) (func(), error) {
	platform.calls = append(platform.calls, "display")
	if platform.fail == "display" {
		return nil, errors.New("display failed")
	}
	if platform.displayAttempted != nil {
		platform.displayAttempted <- subscription.Context
	}
	initial := platform.initialDisplay
	deliver := platform.deliverInitial
	if platform.displayReplies != nil {
		select {
		case reply := <-platform.displayReplies:
			if reply.fail {
				return nil, errors.New("transient display registration failure")
			}
			initial, deliver = reply.setting, reply.deliver
		default:
		}
	}
	if initial == nil {
		initial = &windowsPowerSetting{GUID: windowsSessionDisplayStatus, Length: 4, State: 1}
	}
	if deliver == nil {
		windowsPowerNotification(subscription.Context, windowsPowerSettingChange, initial)
	} else {
		go func() {
			<-deliver
			windowsPowerNotification(subscription.Context, windowsPowerSettingChange, initial)
		}()
	}
	if platform.subscribed != nil {
		platform.subscribedOnce.Do(func() { close(platform.subscribed) })
	}
	if platform.displayRegistered != nil {
		platform.displayRegistered <- subscription.Context
	}
	return func() { platform.calls = append(platform.calls, "undisplay") }, nil
}

func TestWindowsPowerMonitorReceivesInitialDisplayState(t *testing.T) {
	events := make(chan powerEvent, 8)
	platform := &fakeWindowsPowerPlatform{
		loopStarted:    make(chan struct{}),
		initialDisplay: &windowsPowerSetting{GUID: windowsSessionDisplayStatus, Length: 4, State: 0},
	}
	stop, err := startWindowsPowerMonitor(context.Background(), events, platform)
	if err != nil {
		t.Fatal(err)
	}
	defer stop()
	select {
	case event := <-events:
		if event.kind != powerDisplaySleep || !event.active || event.done != nil {
			t.Fatalf("initial display event = %+v", event)
		}
	default:
		t.Fatal("initial display state was lost during registration")
	}
}

func TestWindowsShutdownNotificationWaitsForReport(t *testing.T) {
	events := make(chan powerEvent, 8)
	platform := &fakeWindowsPowerPlatform{loopStarted: make(chan struct{})}
	stop, err := startWindowsPowerMonitor(context.Background(), events, platform)
	if err != nil {
		t.Fatal(err)
	}
	defer stop()
	<-events // Initial display-on report.
	if result := windowsPowerWindowMessage(platform.window, windowsQueryEndSession, 0, 0); result != 1 {
		t.Fatal("shutdown query must respect the requested shutdown")
	}
	windowsPowerWindowMessage(platform.window, windowsEndSession, 0, 0)
	windowsPowerWindowMessage(platform.window, windowsEndSession, 1, windowsEndSessionLogoff)
	windowsPowerWindowMessage(platform.window, windowsEndSession, 1, windowsEndSessionCloseApp)
	select {
	case event := <-events:
		t.Fatalf("query, cancellation, or app shutdown delivered %+v", event)
	default:
	}
	returned := make(chan struct{})
	go func() {
		windowsPowerWindowMessage(platform.window, windowsEndSession, 1, 0)
		close(returned)
	}()
	select {
	case event := <-events:
		if event.kind != powerShutdown || !event.active || event.done == nil {
			t.Fatalf("committed shutdown event = %+v", event)
		}
		select {
		case <-returned:
			t.Fatal("shutdown callback returned before the helper acknowledged the report")
		default:
		}
		close(event.done)
	case <-time.After(time.Second):
		t.Fatal("committed shutdown was not delivered")
	}
	select {
	case <-returned:
	case <-time.After(time.Second):
		t.Fatal("shutdown callback did not return after acknowledgment")
	}
}

func (platform *fakeWindowsPowerPlatform) wakeDisplayRefresh() error {
	select {
	case platform.refresh <- struct{}{}:
	default:
	}
	return nil
}

func (platform *fakeWindowsPowerPlatform) runMessages(ctx context.Context, refreshDisplay func() error) error {
	platform.calls = append(platform.calls, "run")
	close(platform.loopStarted)
	firstRefresh := true
	var retry windowsDisplayRefreshRetry
	for {
		var retryTimer *time.Timer
		var retryReady <-chan time.Time
		if milliseconds := retry.waitMilliseconds(time.Now()); milliseconds != 0xffffffff {
			retryTimer = time.NewTimer(time.Duration(milliseconds) * time.Millisecond)
			retryReady = retryTimer.C
		}
		select {
		case <-ctx.Done():
			if retryTimer != nil {
				retryTimer.Stop()
			}
			return nil
		case <-platform.refresh:
			if firstRefresh && platform.allowRefresh != nil {
				firstRefresh = false
				close(platform.refreshEntered)
				select {
				case <-ctx.Done():
					return nil
				case <-platform.allowRefresh:
				}
			}
			retry.refresh(time.Now(), refreshDisplay)
		case <-retryReady:
			retry.refresh(time.Now(), refreshDisplay)
		}
		if retryTimer != nil {
			retryTimer.Stop()
		}
	}
}

func TestWindowsDisplayRenewalRetryDeadline(t *testing.T) {
	now := time.Unix(1234, 0)
	var retry windowsDisplayRefreshRetry
	if got := retry.waitMilliseconds(now); got != 0xffffffff {
		t.Fatalf("successful monitoring must wait indefinitely, got %d", got)
	}
	retry.refresh(now, func() error { return errors.New("transient failure") })
	if got := retry.waitMilliseconds(now); got != 1000 {
		t.Fatalf("retry was not delayed one second: %d", got)
	}
	if got := retry.waitMilliseconds(now.Add(500 * time.Millisecond)); got != 500 {
		t.Fatalf("unrelated wake must preserve the retry deadline: %d", got)
	}
	if got := retry.waitMilliseconds(now.Add(time.Second)); got != 0 {
		t.Fatalf("due retry must run immediately: %d", got)
	}
	retry.refresh(now.Add(time.Second), func() error { return nil })
	if got := retry.waitMilliseconds(now.Add(time.Second)); got != 0xffffffff {
		t.Fatalf("successful renewal must stop polling: %d", got)
	}
}

func TestWindowsDisplayRenewalFailureRecoversWithoutAnotherResume(t *testing.T) {
	events := make(chan powerEvent, 16)
	replies := make(chan fakeWindowsDisplayReply, 3)
	replies <- fakeWindowsDisplayReply{setting: &windowsPowerSetting{GUID: windowsSessionDisplayStatus, Length: 4, State: 1}}
	replies <- fakeWindowsDisplayReply{fail: true}
	replies <- fakeWindowsDisplayReply{setting: &windowsPowerSetting{GUID: windowsSessionDisplayStatus, Length: 4, State: 1}}
	platform := &fakeWindowsPowerPlatform{
		loopStarted: make(chan struct{}), displayReplies: replies,
		displayAttempted: make(chan uintptr, 3), displayRegistered: make(chan uintptr, 2),
	}
	stop, err := startWindowsPowerMonitor(context.Background(), events, platform)
	if err != nil {
		t.Fatal(err)
	}
	defer stop()
	initialToken := <-platform.displayRegistered
	state := hostPower{}
	state.apply(nextWindowsPowerEvent(t, events))
	applyWindowsSuspend(t, platform.subscription.Context, events, &state)
	windowsPowerNotification(platform.subscription.Context, windowsPowerResumeAutomatic, nil)
	state.apply(nextWindowsPowerEvent(t, events))
	state.apply(nextWindowsPowerEvent(t, events))
	if !state.asleep() {
		t.Fatal("renewal failure must keep the unconfirmed display asleep")
	}
	select {
	case event := <-events:
		state.apply(event)
	case <-time.After(3 * time.Second):
		t.Fatal("transient registration failure did not retry without another resume")
	}
	if state.asleep() {
		t.Fatal("renewal retry did not apply the confirmed display-on state")
	}
	if token := <-platform.displayRegistered; token == initialToken {
		t.Fatal("renewal retry reused the stale registration token")
	}
	stop()
	for i := 0; i < 3; i++ {
		token := <-platform.displayAttempted
		if _, exists := windowsPowerMonitors.Load(token); exists {
			t.Fatalf("failed or renewed subscription token %d leaked after cleanup", token)
		}
	}
}

func nextWindowsPowerEvent(t *testing.T, events <-chan powerEvent) powerEvent {
	t.Helper()
	select {
	case event := <-events:
		return event
	case <-time.After(time.Second):
		t.Fatal("power event was not delivered")
		return powerEvent{}
	}
}

func applyWindowsSuspend(t *testing.T, token uintptr, events <-chan powerEvent, state *hostPower) {
	t.Helper()
	returned := make(chan struct{})
	go func() {
		windowsPowerNotification(token, windowsPowerSuspend, nil)
		close(returned)
	}()
	event := nextWindowsPowerEvent(t, events)
	if event.kind != powerSystemSleep || !event.active || event.done == nil {
		t.Fatalf("suspend notification = %+v", event)
	}
	state.apply(event)
	close(event.done)
	<-returned
}

func TestWindowsBackgroundResumeWaitsForFreshDisplayState(t *testing.T) {
	events := make(chan powerEvent, 16)
	offReady, onReady := make(chan struct{}), make(chan struct{})
	replies := make(chan fakeWindowsDisplayReply, 3)
	replies <- fakeWindowsDisplayReply{setting: &windowsPowerSetting{GUID: windowsSessionDisplayStatus, Length: 4, State: 1}}
	replies <- fakeWindowsDisplayReply{setting: &windowsPowerSetting{GUID: windowsSessionDisplayStatus, Length: 4, State: 0}, deliver: offReady}
	replies <- fakeWindowsDisplayReply{setting: &windowsPowerSetting{GUID: windowsSessionDisplayStatus, Length: 4, State: 1}, deliver: onReady}
	platform := &fakeWindowsPowerPlatform{
		loopStarted: make(chan struct{}), displayReplies: replies, displayRegistered: make(chan uintptr, 3),
		allowRefresh: make(chan struct{}), refreshEntered: make(chan struct{}),
	}
	stop, err := startWindowsPowerMonitor(context.Background(), events, platform)
	if err != nil {
		t.Fatal(err)
	}
	defer stop()
	sleepToken, oldDisplayToken := platform.subscription.Context, <-platform.displayRegistered
	state := hostPower{}
	state.apply(nextWindowsPowerEvent(t, events))
	if state.asleep() {
		t.Fatal("initial display-on state must be awake")
	}
	applyWindowsSuspend(t, sleepToken, events, &state)
	windowsPowerNotification(sleepToken, windowsPowerResumeAutomatic, nil)
	for _, want := range []powerEvent{{kind: powerDisplaySleep, active: true}, {kind: powerSystemSleep}} {
		event := nextWindowsPowerEvent(t, events)
		if event.kind != want.kind || event.active != want.active {
			t.Fatalf("resume ordering = %+v, want %+v", event, want)
		}
		state.apply(event)
		if !state.asleep() {
			t.Fatal("automatic background resume transiently woke Pipkin")
		}
	}
	select {
	case <-platform.refreshEntered:
	case <-time.After(time.Second):
		t.Fatal("resume did not wake the subscription renewal loop")
	}
	// The previous registration still exists while renewal waits, but its
	// pre-suspend display-on observation must no longer be allowed to wake.
	windowsPowerNotification(oldDisplayToken, windowsPowerSettingChange, &windowsPowerSetting{GUID: windowsSessionDisplayStatus, Length: 4, State: 1})
	select {
	case event := <-events:
		t.Fatalf("stale display registration delivered %+v", event)
	default:
	}
	close(platform.allowRefresh)
	offToken := <-platform.displayRegistered
	close(offReady)
	state.apply(nextWindowsPowerEvent(t, events))
	if !state.asleep() {
		t.Fatal("confirmed screens-off state must keep background resume asleep")
	}
	// A foreground resume renews again. Its delayed current on state must
	// clear the gate even if Windows never emitted a separate on transition.
	windowsPowerNotification(sleepToken, windowsPowerResume, nil)
	state.apply(nextWindowsPowerEvent(t, events))
	state.apply(nextWindowsPowerEvent(t, events))
	onToken := <-platform.displayRegistered
	if !state.asleep() {
		t.Fatal("foreground resume woke before fresh display state arrived")
	}
	close(onReady)
	state.apply(nextWindowsPowerEvent(t, events))
	if state.asleep() {
		t.Fatal("fresh display-on state did not clear the resume gate")
	}
	stop()
	for _, token := range []uintptr{sleepToken, oldDisplayToken, offToken, onToken} {
		if _, exists := windowsPowerMonitors.Load(token); exists {
			t.Fatalf("subscription token %d was retained after cleanup", token)
		}
	}
}

func TestWindowsCancelledSuspendRecoversWithoutDisplayTransition(t *testing.T) {
	for _, resume := range []uint32{windowsPowerResumeAutomatic, windowsPowerResume} {
		t.Run(fmt.Sprintf("resume-%d", resume), func(t *testing.T) {
			events := make(chan powerEvent, 16)
			platform := &fakeWindowsPowerPlatform{loopStarted: make(chan struct{}), displayRegistered: make(chan uintptr, 2)}
			stop, err := startWindowsPowerMonitor(context.Background(), events, platform)
			if err != nil {
				t.Fatal(err)
			}
			defer stop()
			initialToken := <-platform.displayRegistered
			state := hostPower{}
			state.apply(nextWindowsPowerEvent(t, events))
			applyWindowsSuspend(t, platform.subscription.Context, events, &state)
			windowsPowerNotification(platform.subscription.Context, resume, nil)
			for i := 0; i < 3; i++ {
				state.apply(nextWindowsPowerEvent(t, events))
			}
			if token := <-platform.displayRegistered; token == initialToken {
				t.Fatal("resume did not renew the display subscription")
			}
			if state.asleep() {
				t.Fatal("cancelled suspend with screens still on left Pipkin asleep")
			}
		})
	}
}

func TestWindowsPowerMonitorRegistrationCleanup(t *testing.T) {
	for _, test := range []struct {
		fail  string
		calls []string
	}{
		{"open", []string{"open"}},
		{"sleep", []string{"open", "sleep", "close"}},
		{"display", []string{"open", "sleep", "display", "unsleep", "close"}},
	} {
		t.Run(test.fail, func(t *testing.T) {
			platform := &fakeWindowsPowerPlatform{fail: test.fail}
			stop, err := startWindowsPowerMonitor(context.Background(), make(chan powerEvent, 8), platform)
			if stop != nil || err == nil {
				t.Fatal("failed registration must return an error after cleanup")
			}
			if !reflect.DeepEqual(platform.calls, test.calls) {
				t.Fatalf("lifecycle = %v, want %v", platform.calls, test.calls)
			}
			if _, exists := windowsPowerWindows.Load(platform.window); exists {
				t.Fatal("failed registration retained window callback routing")
			}
		})
	}
}

func TestWindowsPowerMonitorCancellationAndLateCallbacks(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	events := make(chan powerEvent, 8)
	platform := &fakeWindowsPowerPlatform{loopStarted: make(chan struct{})}
	stop, err := startWindowsPowerMonitor(ctx, events, platform)
	if err != nil {
		t.Fatal(err)
	}
	defer stop()
	<-events // Initial display-on report.
	select {
	case <-platform.loopStarted:
	case <-time.After(time.Second):
		t.Fatal("message loop did not start")
	}
	token := platform.subscription.Context
	callbackReturned := make(chan struct{})
	go func() {
		windowsPowerNotification(token, windowsPowerSuspend, nil)
		close(callbackReturned)
	}()
	select {
	case event := <-events:
		if event.kind != powerSystemSleep || !event.active || event.done == nil {
			t.Fatalf("suspend = %+v", event)
		}
		close(event.done)
	case <-time.After(time.Second):
		t.Fatal("registered callback did not deliver suspend")
	}
	<-callbackReturned
	cancel()
	stop()
	stop()
	if want := []string{"open", "sleep", "display", "run", "undisplay", "unsleep", "close"}; !reflect.DeepEqual(platform.calls, want) {
		t.Fatalf("lifecycle = %v, want %v", platform.calls, want)
	}
	windowsPowerNotification(token, windowsPowerSuspend, nil)
	windowsPowerWindowMessage(platform.window, windowsEndSession, 1, 0)
	select {
	case event := <-events:
		t.Fatalf("callback after cleanup delivered %+v", event)
	default:
	}
	if _, exists := windowsPowerMonitors.Load(token); exists {
		t.Fatal("cancellation retained native callback routing")
	}
}

func TestWindowsPowerMonitorWaitsForAsynchronousInitialDisplay(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	events := make(chan powerEvent, 8)
	platform := &fakeWindowsPowerPlatform{
		loopStarted: make(chan struct{}), deliverInitial: make(chan struct{}), subscribed: make(chan struct{}),
	}
	type result struct {
		stop func()
		err  error
	}
	started := make(chan result, 1)
	go func() {
		stop, err := startWindowsPowerMonitor(ctx, events, platform)
		started <- result{stop, err}
	}()
	select {
	case <-platform.subscribed:
	case <-time.After(time.Second):
		close(platform.deliverInitial)
		t.Fatal("display subscription did not start")
	}
	select {
	case got := <-started:
		if got.stop != nil {
			got.stop()
		}
		close(platform.deliverInitial)
		t.Fatal("monitor became ready before its initial display state arrived")
	default:
	}
	close(platform.deliverInitial)
	select {
	case got := <-started:
		if got.err != nil {
			t.Fatal(got.err)
		}
		defer got.stop()
	case <-time.After(time.Second):
		t.Fatal("monitor did not become ready after its initial display state")
	}
	select {
	case event := <-events:
		if event.kind != powerDisplaySleep || event.active {
			t.Fatalf("asynchronous display-on report = %+v", event)
		}
	default:
		t.Fatal("initial display event was not queued before readiness")
	}
}

func TestWindowsPowerMonitorLateDisplayOverridesStartupFallback(t *testing.T) {
	events := make(chan powerEvent, 8)
	platform := &fakeWindowsPowerPlatform{loopStarted: make(chan struct{}), deliverInitial: make(chan struct{})}
	stop, err := startWindowsPowerMonitor(context.Background(), events, platform)
	if err != nil {
		close(platform.deliverInitial)
		t.Fatal(err)
	}
	defer stop()
	select {
	case event := <-events:
		if event.kind != powerDisplaySleep || !event.active {
			t.Fatalf("unknown initial display state must remain asleep: %+v", event)
		}
	default:
		t.Fatal("unknown initial display state was not queued before readiness")
	}
	close(platform.deliverInitial)
	select {
	case event := <-events:
		if event.kind != powerDisplaySleep || event.active {
			t.Fatalf("late confirmed display-on state = %+v", event)
		}
	case <-time.After(time.Second):
		t.Fatal("late display state did not replace the conservative fallback")
	}
}

func TestWindowsPowerNativeStructLayout(t *testing.T) {
	// amd64 and arm64 use the same Win64 ABI. Keep DLL writes within the
	// correctly sized MSG and WNDCLASSEXW buffers on both targets.
	if unsafe.Sizeof(uintptr(0)) == 8 && (unsafe.Sizeof(windowsPowerMessage{}) != 48 || unsafe.Sizeof(windowsPowerWindowClass{}) != 80) {
		t.Fatal("native window structures do not match the Win64 ABI")
	}
	if unsafe.Offsetof(windowsPowerSetting{}.State) != 20 {
		t.Fatal("display state does not match POWERBROADCAST_SETTING data offset")
	}
}
