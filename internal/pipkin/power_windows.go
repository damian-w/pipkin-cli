package pipkin

import (
	"context"
	"fmt"
	"os"
	"runtime"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
)

const (
	windowsPowerSuspend         = 0x0004
	windowsPowerResume          = 0x0007
	windowsPowerResumeAutomatic = 0x0012
	windowsPowerSettingChange   = 0x8013
	windowsQueryEndSession      = 0x0011
	windowsEndSession           = 0x0016
	windowsEndSessionCloseApp   = 0x00000001
	windowsEndSessionLogoff     = 0x80000000
	windowsQuit                 = 0x0012
)

var windowsSessionDisplayStatus = windows.GUID{
	Data1: 0x2b84c20e, Data2: 0xad23, Data3: 0x4ddf,
	Data4: [8]byte{0x93, 0xdb, 0x05, 0xff, 0xbd, 0x7e, 0xfc, 0xa5},
}

var (
	windowsPowerUser32     = windows.NewLazySystemDLL("user32.dll")
	windowsPowerPowrprof   = windows.NewLazySystemDLL("powrprof.dll")
	windowsPowerMonitors   sync.Map
	windowsPowerWindows    sync.Map
	windowsPowerNextToken  atomic.Uint64
	windowsPowerCallback   = windows.NewCallback(windowsPowerNotification)
	windowsPowerWindowProc = windows.NewCallback(windowsPowerWindowMessage)
)

// Native code receives an integer registry key, never a retained Go pointer.
type windowsPowerSubscription struct {
	Callback uintptr
	Context  uintptr
}

type windowsPowerMonitor struct {
	ctx               context.Context
	events            chan<- powerEvent
	token             uintptr
	subscription      windowsPowerSubscription
	displayReady      chan struct{}
	displayOnce       sync.Once
	displayMu         sync.Mutex
	displayKnown      bool
	displayGeneration uint64
	refreshDisplay    func() error
}

type windowsPowerReceiver struct {
	monitor           *windowsPowerMonitor
	displayGeneration uint64
}

type windowsPowerPlatform interface {
	openWindow(token uintptr) (uintptr, func(), error)
	subscribeSleep(*windowsPowerSubscription) (func(), error)
	subscribeDisplay(*windowsPowerSubscription) (func(), error)
	wakeDisplayRefresh() error
	runMessages(context.Context, func() error) error
}

func startPowerMonitor(ctx context.Context, events chan<- powerEvent) (func(), error) {
	return startWindowsPowerMonitor(ctx, events, &nativeWindowsPowerPlatform{})
}

func startWindowsPowerMonitor(ctx context.Context, events chan<- powerEvent, platform windowsPowerPlatform) (func(), error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	monitorCtx, cancel := context.WithCancel(ctx)
	monitor := &windowsPowerMonitor{ctx: monitorCtx, events: events, token: uintptr(windowsPowerNextToken.Add(1)), displayReady: make(chan struct{}), displayGeneration: 1, refreshDisplay: platform.wakeDisplayRefresh}
	monitor.subscription = windowsPowerSubscription{Callback: windowsPowerCallback, Context: monitor.token}
	ready, finished := make(chan error, 1), make(chan struct{})
	go func() {
		runtime.LockOSThread()
		defer runtime.UnlockOSThread()
		var cleanup []func()
		var window uintptr
		defer func() {
			windowsPowerMonitors.Delete(monitor.token)
			windowsPowerWindows.Delete(window)
			for i := len(cleanup) - 1; i >= 0; i-- {
				cleanup[i]()
			}
			runtime.KeepAlive(monitor)
			close(finished)
		}()
		var closeWindow func()
		var err error
		window, closeWindow, err = platform.openWindow(monitor.token)
		if err != nil {
			ready <- err
			return
		}
		cleanup = append(cleanup, closeWindow)
		windowsPowerMonitors.Store(monitor.token, &windowsPowerReceiver{monitor: monitor})
		windowsPowerWindows.Store(window, monitor)
		unsubscribeSleep, err := platform.subscribeSleep(&monitor.subscription)
		if err != nil {
			ready <- err
			return
		}
		cleanup = append(cleanup, unsubscribeSleep)
		var displayToken uintptr
		var displaySubscription *windowsPowerSubscription
		var unsubscribeDisplay func()
		cleanup = append(cleanup, func() {
			windowsPowerMonitors.Delete(displayToken)
			if unsubscribeDisplay != nil {
				unsubscribeDisplay()
			}
			runtime.KeepAlive(displaySubscription)
		})
		registerDisplay := func() error {
			monitor.displayMu.Lock()
			generation := monitor.displayGeneration
			monitor.displayMu.Unlock()
			token := uintptr(windowsPowerNextToken.Add(1))
			subscription := &windowsPowerSubscription{Callback: windowsPowerCallback, Context: token}
			windowsPowerMonitors.Store(token, &windowsPowerReceiver{monitor: monitor, displayGeneration: generation})
			unsubscribe, err := platform.subscribeDisplay(subscription)
			if err != nil {
				windowsPowerMonitors.Delete(token)
				return err
			}
			// Renew on this thread, never inside a native callback. A fresh
			// registration supplies the current display state even if no
			// off/on transition occurred during a cancelled suspend.
			windowsPowerMonitors.Delete(displayToken)
			if unsubscribeDisplay != nil {
				unsubscribeDisplay()
			}
			runtime.KeepAlive(displaySubscription)
			displayToken, displaySubscription, unsubscribeDisplay = token, subscription, unsubscribe
			return nil
		}
		if err := registerDisplay(); err != nil {
			ready <- err
			return
		}
		initial := time.NewTimer(time.Second)
		select {
		case <-monitor.displayReady:
		case <-monitorCtx.Done():
		case <-initial.C:
			// Initial callbacks may be asynchronous. Keep the display asleep
			// until Windows supplies a confirmed state, without blocking startup.
			monitor.displayMu.Lock()
			if !monitor.displayKnown {
				postPowerEvent(monitorCtx, events, powerDisplaySleep, true, false)
				logf("initial host display state is delayed; waiting with Pipkin asleep")
			}
			monitor.displayMu.Unlock()
		}
		initial.Stop()
		if err := monitorCtx.Err(); err != nil {
			ready <- err
			return
		}
		ready <- nil
		if err := platform.runMessages(monitorCtx, registerDisplay); err != nil {
			logf("host power monitor stopped: %v", err)
		}
	}()
	if err := <-ready; err != nil {
		cancel()
		<-finished
		return nil, err
	}
	var once sync.Once
	return func() {
		once.Do(cancel)
		<-finished
	}, nil
}

func windowsPowerNotification(token uintptr, eventType uint32, setting *windowsPowerSetting) uintptr {
	value, ok := windowsPowerMonitors.Load(token)
	if !ok {
		return 0
	}
	receiver := value.(*windowsPowerReceiver)
	monitor := receiver.monitor
	kind, active, valid := decodeWindowsPowerNotification(eventType, setting)
	if valid {
		if kind == powerDisplaySleep {
			monitor.displayMu.Lock()
			if receiver.displayGeneration != monitor.displayGeneration {
				monitor.displayMu.Unlock()
				return 0
			}
			if postPowerEvent(monitor.ctx, monitor.events, kind, active, false) && !monitor.displayKnown {
				monitor.displayKnown = true
				monitor.displayOnce.Do(func() { close(monitor.displayReady) })
			}
			monitor.displayMu.Unlock()
		} else {
			if !active {
				// A maintenance resume need not turn screens on. Invalidate
				// the pre-suspend display-on observation before clearing the
				// system-sleep reason, and reject stale registration updates.
				monitor.displayMu.Lock()
				monitor.displayGeneration++
				monitor.displayKnown = false
				postPowerEvent(monitor.ctx, monitor.events, powerDisplaySleep, true, false)
				monitor.displayMu.Unlock()
			}
			postPowerEvent(monitor.ctx, monitor.events, kind, active, active)
			if !active && monitor.ctx.Err() == nil {
				if err := monitor.refreshDisplay(); err != nil {
					logf("could not refresh host display state after resume: %v", err)
				}
			}
		}
	}
	return 0
}

type windowsPowerSetting struct {
	GUID   windows.GUID
	Length uint32
	State  uint32
}

func decodeWindowsPowerNotification(eventType uint32, setting *windowsPowerSetting) (powerEventKind, bool, bool) {
	switch eventType {
	case windowsPowerSuspend:
		return powerSystemSleep, true, true
	case windowsPowerResume, windowsPowerResumeAutomatic:
		return powerSystemSleep, false, true
	case windowsPowerSettingChange:
		if setting == nil {
			return 0, false, false
		}
		// POWERBROADCAST_SETTING has a 16-byte GUID, DWORD length, then data.
		if setting.GUID != windowsSessionDisplayStatus || setting.Length != 4 {
			return 0, false, false
		}
		state := setting.State
		if state > 2 {
			return 0, false, false
		}
		return powerDisplaySleep, state == 0, true
	default:
		return 0, false, false
	}
}

func windowsCommittedShutdown(committed, flags uintptr) bool {
	return committed != 0 && flags&(windowsEndSessionLogoff|windowsEndSessionCloseApp) == 0
}

func windowsPowerWindowMessage(window uintptr, message uint32, wparam, lparam uintptr) uintptr {
	switch message {
	case windowsQueryEndSession:
		// A query can still be cancelled by another application.
		return 1
	case windowsEndSession:
		if windowsCommittedShutdown(wparam, lparam) {
			if value, ok := windowsPowerWindows.Load(window); ok {
				monitor := value.(*windowsPowerMonitor)
				postPowerEvent(monitor.ctx, monitor.events, powerShutdown, true, true)
			}
		}
		return 0
	default:
		result, _, _ := windowsPowerUser32.NewProc("DefWindowProcW").Call(window, uintptr(message), wparam, lparam)
		return result
	}
}

type windowsPowerWindowClass struct {
	Size, Style  uint32
	WindowProc   uintptr
	ClassExtra   int32
	WindowExtra  int32
	Instance     windows.Handle
	Icon, Cursor windows.Handle
	Background   windows.Handle
	MenuName     *uint16
	ClassName    *uint16
	SmallIcon    windows.Handle
}

type windowsPowerMessage struct {
	Window  uintptr
	Message uint32
	WParam  uintptr
	LParam  uintptr
	Time    uint32
	Point   struct{ X, Y int32 }
	Private uint32
}

type windowsDisplayRefreshRetry struct{ due time.Time }

func (retry *windowsDisplayRefreshRetry) refresh(now time.Time, register func() error) {
	if err := register(); err != nil {
		logf("could not refresh host display state after resume: %v", err)
		retry.due = now.Add(time.Second)
	} else {
		retry.due = time.Time{}
	}
}

func (retry windowsDisplayRefreshRetry) waitMilliseconds(now time.Time) uint32 {
	if retry.due.IsZero() {
		return 0xffffffff
	}
	remaining := retry.due.Sub(now)
	if remaining <= 0 {
		return 0
	}
	return uint32((remaining + time.Millisecond - 1) / time.Millisecond)
}

type nativeWindowsPowerPlatform struct {
	stopEvent    windows.Handle
	refreshEvent windows.Handle
}

func (platform *nativeWindowsPowerPlatform) openWindow(token uintptr) (uintptr, func(), error) {
	stopEvent, err := windows.CreateEvent(nil, 1, 0, nil)
	if err != nil {
		return 0, nil, fmt.Errorf("create power monitor cancellation event: %w", err)
	}
	platform.stopEvent = stopEvent
	refreshEvent, err := windows.CreateEvent(nil, 0, 0, nil)
	if err != nil {
		windows.CloseHandle(stopEvent)
		return 0, nil, fmt.Errorf("create display refresh event: %w", err)
	}
	platform.refreshEvent = refreshEvent
	var instance windows.Handle
	err = windows.GetModuleHandleEx(windows.GET_MODULE_HANDLE_EX_FLAG_UNCHANGED_REFCOUNT, nil, &instance)
	if err != nil {
		windows.CloseHandle(refreshEvent)
		windows.CloseHandle(stopEvent)
		return 0, nil, err
	}
	name, _ := windows.UTF16PtrFromString(fmt.Sprintf("PipkinPowerMonitor-%d-%d", os.Getpid(), token))
	class := windowsPowerWindowClass{Size: uint32(unsafe.Sizeof(windowsPowerWindowClass{})), WindowProc: windowsPowerWindowProc, Instance: instance, ClassName: name}
	atom, _, callErr := windowsPowerUser32.NewProc("RegisterClassExW").Call(uintptr(unsafe.Pointer(&class)))
	if atom == 0 {
		windows.CloseHandle(refreshEvent)
		windows.CloseHandle(stopEvent)
		return 0, nil, fmt.Errorf("register power monitor window: %w", callErr)
	}
	// Parent=0 gives a hidden top-level window. HWND_MESSAGE windows miss
	// broadcast shutdown messages, and WS_VISIBLE must remain unset.
	window, _, callErr := windowsPowerUser32.NewProc("CreateWindowExW").Call(0, uintptr(unsafe.Pointer(name)), 0, 0, 0, 0, 0, 0, 0, 0, uintptr(instance), 0)
	if window == 0 {
		windowsPowerUser32.NewProc("UnregisterClassW").Call(uintptr(unsafe.Pointer(name)), uintptr(instance))
		windows.CloseHandle(refreshEvent)
		windows.CloseHandle(stopEvent)
		return 0, nil, fmt.Errorf("create power monitor window: %w", callErr)
	}
	return window, func() {
		windowsPowerUser32.NewProc("DestroyWindow").Call(window)
		windowsPowerUser32.NewProc("UnregisterClassW").Call(uintptr(unsafe.Pointer(name)), uintptr(instance))
		windows.CloseHandle(refreshEvent)
		windows.CloseHandle(stopEvent)
	}, nil
}

func (platform *nativeWindowsPowerPlatform) subscribeSleep(subscription *windowsPowerSubscription) (func(), error) {
	register := windowsPowerPowrprof.NewProc("PowerRegisterSuspendResumeNotification")
	if err := register.Find(); err != nil {
		return nil, err
	}
	var handle windows.Handle
	result, _, _ := register.Call(2, uintptr(unsafe.Pointer(subscription)), uintptr(unsafe.Pointer(&handle)))
	if result != 0 {
		return nil, fmt.Errorf("subscribe to system sleep: %w", syscall.Errno(result))
	}
	return func() { windowsPowerPowrprof.NewProc("PowerUnregisterSuspendResumeNotification").Call(uintptr(handle)) }, nil
}

func (platform *nativeWindowsPowerPlatform) subscribeDisplay(subscription *windowsPowerSubscription) (func(), error) {
	register := windowsPowerPowrprof.NewProc("PowerSettingRegisterNotification")
	if err := register.Find(); err != nil {
		return nil, err
	}
	var handle windows.Handle
	result, _, _ := register.Call(uintptr(unsafe.Pointer(&windowsSessionDisplayStatus)), 2, uintptr(unsafe.Pointer(subscription)), uintptr(unsafe.Pointer(&handle)))
	if result != 0 {
		return nil, fmt.Errorf("subscribe to display sleep: %w", syscall.Errno(result))
	}
	return func() { windowsPowerPowrprof.NewProc("PowerSettingUnregisterNotification").Call(uintptr(handle)) }, nil
}

func (platform *nativeWindowsPowerPlatform) wakeDisplayRefresh() error {
	return windows.SetEvent(platform.refreshEvent)
}

func (platform *nativeWindowsPowerPlatform) runMessages(ctx context.Context, refreshDisplay func() error) error {
	watchDone, watchExited := make(chan struct{}), make(chan struct{})
	go func() {
		defer close(watchExited)
		select {
		case <-ctx.Done():
			windows.SetEvent(platform.stopEvent)
		case <-watchDone:
		}
	}()
	defer func() { close(watchDone); <-watchExited }()
	handles := [2]windows.Handle{platform.stopEvent, platform.refreshEvent}
	var retry windowsDisplayRefreshRetry
	for ctx.Err() == nil {
		// A kernel event wakes this thread on cancellation without depending
		// on a posted window message fitting into the thread's message queue.
		result, _, callErr := windowsPowerUser32.NewProc("MsgWaitForMultipleObjectsEx").Call(2, uintptr(unsafe.Pointer(&handles[0])), uintptr(retry.waitMilliseconds(time.Now())), 0x04ff, 0x0004)
		switch result {
		case 0:
			return nil
		case 1:
			retry.refresh(time.Now(), refreshDisplay)
		case 0x0102: // WAIT_TIMEOUT: only enabled while a renewal retry is pending.
			retry.refresh(time.Now(), refreshDisplay)
		case 2:
			for ctx.Err() == nil {
				var message windowsPowerMessage
				present, _, _ := windowsPowerUser32.NewProc("PeekMessageW").Call(uintptr(unsafe.Pointer(&message)), 0, 0, 0, 1)
				if present == 0 {
					break
				}
				if message.Message == windowsQuit {
					return nil
				}
				windowsPowerUser32.NewProc("TranslateMessage").Call(uintptr(unsafe.Pointer(&message)))
				windowsPowerUser32.NewProc("DispatchMessageW").Call(uintptr(unsafe.Pointer(&message)))
			}
		default:
			return fmt.Errorf("wait for host power events: %w", callErr)
		}
	}
	return nil
}
