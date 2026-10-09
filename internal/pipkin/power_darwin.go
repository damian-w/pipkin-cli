package pipkin

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"time"
	"unsafe"

	"github.com/ebitengine/purego"
	"github.com/ebitengine/purego/objc"
)

const (
	darwinCanSystemSleep     uint32 = 0xe0000270
	darwinSystemWillSleep    uint32 = 0xe0000280
	darwinSystemHasPoweredOn uint32 = 0xe0000300
)

// Frameworks, the Objective-C class, and native callback trampolines have process
// lifetime. Reusing them avoids consuming purego callback slots on each restart.
var darwinPowerNative struct {
	sync.Once
	api *darwinPowerAPI
	err error
}

var darwinPowerMonitors sync.Map
var darwinPowerObservers sync.Map
var darwinPowerNextID atomic.Uint64

type darwinPowerAPI struct {
	observerClass                       objc.Class
	screensSleep, screensWake, powerOff objc.ID
	defaultMode                         uintptr
	powerCallback                       uintptr
	getRunLoop                          func() uintptr
	addSource                           func(uintptr, uintptr, uintptr)
	removeSource                        func(uintptr, uintptr, uintptr)
	runInMode                           func(uintptr, float64, uint8) int32
	registerPower                       func(uintptr, *uintptr, uintptr, *uint32) uint32
	deregisterPower                     func(*uint32) int32
	notificationSource                  func(uintptr) uintptr
	destroyNotificationPort             func(uintptr)
	closeService                        func(uint32) int32
	allowPowerChange                    func(uint32, uintptr) int32
	onlineDisplays                      func(uint32, *uint32, *uint32) int32
	displayAsleep                       func(uint32) uint32
}

type darwinPowerMonitor struct {
	ctx                               context.Context
	events                            chan<- powerEvent
	api                               *darwinPowerAPI
	id                                uintptr
	observer, center                  objc.ID
	runLoop, notificationPort, source uintptr
	rootPort, notifier                uint32
	systemSleeping                    atomic.Bool
	displaySleeping                   atomic.Bool
	displayLastPosted                 atomic.Uint32
	shutdownDeadline                  atomic.Int64
}

func startPowerMonitor(ctx context.Context, events chan<- powerEvent) (func(), error) {
	// NSWorkspace and IOKit deliver notifications through this thread's run
	// loop. Keep registration, processing and teardown on that same thread.
	return startPowerThread(ctx, func(ctx context.Context, ready chan<- error) {
		api, err := loadDarwinPowerAPI()
		if err != nil {
			ready <- err
			return
		}
		pool := darwinAutoreleasePool()
		defer pool.Send(objc.RegisterName("drain"))
		monitor := &darwinPowerMonitor{ctx: ctx, events: events, api: api, id: uintptr(darwinPowerNextID.Add(1))}
		defer monitor.close()
		if err := monitor.open(); err != nil {
			ready <- err
			return
		}
		// Seed the reducer before startup returns; restarting the helper while
		// displays are asleep must not briefly wake Pipkin.
		monitor.reconcileDisplays()
		if err := ctx.Err(); err != nil {
			ready <- err
			return
		}
		ready <- nil
		nextDisplayCheck := time.Now().Add(time.Second)
		for ctx.Err() == nil {
			iterationPool := darwinAutoreleasePool()
			// A bounded run avoids cross-thread stop races and lets cancellation
			// finish without depending on another native notification arriving.
			api.runInMode(api.defaultMode, 0.25, 0)
			now := time.Now()
			if !now.Before(nextDisplayCheck) {
				// Workspace transport can rely on a main Cocoa run loop, which a Go
				// helper does not own. Public display queries provide a safety net
				// for missed notifications without publishing unchanged states.
				monitor.reconcileDisplays()
				nextDisplayCheck = now.Add(time.Second)
			}
			monitor.recoverCancelledShutdown(now)
			iterationPool.Send(objc.RegisterName("drain"))
		}
	})
}

func darwinAutoreleasePool() objc.ID {
	return objc.ID(objc.GetClass("NSAutoreleasePool")).Send(objc.RegisterName("alloc")).Send(objc.RegisterName("init"))
}

func loadDarwinPowerAPI() (*darwinPowerAPI, error) {
	darwinPowerNative.Do(func() {
		api := &darwinPowerAPI{}
		var err error
		open := func(name string) (library uintptr) {
			if err == nil {
				library, err = purego.Dlopen("/System/Library/Frameworks/"+name+".framework/"+name, purego.RTLD_NOW|purego.RTLD_LOCAL)
			}
			return library
		}
		symbol := func(library uintptr, name string) (address uintptr) {
			if err == nil {
				address, err = purego.Dlsym(library, name)
			}
			return address
		}
		bind := func(destination any, library uintptr, name string) {
			if address := symbol(library, name); err == nil {
				purego.RegisterFunc(destination, address)
			}
		}
		constant := func(library uintptr, name string) uintptr {
			address := symbol(library, name)
			if err != nil {
				return 0
			}
			// The symbol is a framework-owned pointer to a CF/Objective-C object.
			pointer := *(*unsafe.Pointer)(unsafe.Pointer(&address))
			value := *(*uintptr)(pointer)
			if value == 0 {
				err = fmt.Errorf("empty native constant %s", name)
			}
			return value
		}
		appkit, cf, io, cg := open("AppKit"), open("CoreFoundation"), open("IOKit"), open("CoreGraphics")
		bind(&api.getRunLoop, cf, "CFRunLoopGetCurrent")
		bind(&api.addSource, cf, "CFRunLoopAddSource")
		bind(&api.removeSource, cf, "CFRunLoopRemoveSource")
		bind(&api.runInMode, cf, "CFRunLoopRunInMode")
		bind(&api.registerPower, io, "IORegisterForSystemPower")
		bind(&api.deregisterPower, io, "IODeregisterForSystemPower")
		bind(&api.notificationSource, io, "IONotificationPortGetRunLoopSource")
		bind(&api.destroyNotificationPort, io, "IONotificationPortDestroy")
		bind(&api.closeService, io, "IOServiceClose")
		bind(&api.allowPowerChange, io, "IOAllowPowerChange")
		bind(&api.onlineDisplays, cg, "CGGetOnlineDisplayList")
		bind(&api.displayAsleep, cg, "CGDisplayIsAsleep")
		api.defaultMode = constant(cf, "kCFRunLoopDefaultMode")
		api.screensSleep = objc.ID(constant(appkit, "NSWorkspaceScreensDidSleepNotification"))
		api.screensWake = objc.ID(constant(appkit, "NSWorkspaceScreensDidWakeNotification"))
		api.powerOff = objc.ID(constant(appkit, "NSWorkspaceWillPowerOffNotification"))
		if err == nil {
			api.observerClass, err = objc.RegisterClass("PipkinPowerObserver", objc.GetClass("NSObject"), nil, nil, []objc.MethodDef{
				{Cmd: objc.RegisterName("screensDidSleep:"), Fn: darwinScreensDidSleep},
				{Cmd: objc.RegisterName("screensDidWake:"), Fn: darwinScreensDidWake},
				{Cmd: objc.RegisterName("willPowerOff:"), Fn: darwinWillPowerOff},
			})
		}
		if err != nil {
			darwinPowerNative.err = err
			return
		}
		api.powerCallback = purego.NewCallback(darwinSystemPowerChanged)
		darwinPowerNative.api = api
	})
	if darwinPowerNative.err != nil {
		return nil, fmt.Errorf("macOS power notifications unavailable: %w", darwinPowerNative.err)
	}
	return darwinPowerNative.api, nil
}

func (m *darwinPowerMonitor) open() error {
	m.runLoop = m.api.getRunLoop()
	workspace := objc.ID(objc.GetClass("NSWorkspace")).Send(objc.RegisterName("sharedWorkspace"))
	m.center = workspace.Send(objc.RegisterName("notificationCenter"))
	m.observer = objc.ID(m.api.observerClass).Send(objc.RegisterName("alloc")).Send(objc.RegisterName("init"))
	if m.runLoop == 0 || m.center == 0 || m.observer == 0 {
		return errors.New("macOS workspace power observer is unavailable")
	}
	darwinPowerMonitors.Store(m.id, m)
	darwinPowerObservers.Store(m.observer, m)
	m.rootPort = m.api.registerPower(m.id, &m.notificationPort, m.api.powerCallback, &m.notifier)
	if m.rootPort == 0 {
		return errors.New("macOS system power observer registration failed")
	}
	m.source = m.api.notificationSource(m.notificationPort)
	if m.source == 0 {
		return errors.New("macOS power notification run loop source is unavailable")
	}
	m.api.addSource(m.runLoop, m.source, m.api.defaultMode)
	add := objc.RegisterName("addObserver:selector:name:object:")
	m.center.Send(add, m.observer, objc.RegisterName("screensDidSleep:"), m.api.screensSleep, objc.ID(0))
	m.center.Send(add, m.observer, objc.RegisterName("screensDidWake:"), m.api.screensWake, objc.ID(0))
	m.center.Send(add, m.observer, objc.RegisterName("willPowerOff:"), m.api.powerOff, objc.ID(0))
	return nil
}

func (m *darwinPowerMonitor) close() {
	if m.observer != 0 && m.center != 0 {
		m.center.Send(objc.RegisterName("removeObserver:"), m.observer)
	}
	if m.source != 0 {
		m.api.removeSource(m.runLoop, m.source, m.api.defaultMode)
	}
	if m.notifier != 0 {
		m.api.deregisterPower(&m.notifier)
	}
	if m.rootPort != 0 {
		m.api.closeService(m.rootPort)
	}
	if m.notificationPort != 0 {
		m.api.destroyNotificationPort(m.notificationPort)
	}
	darwinPowerMonitors.Delete(m.id)
	if m.observer != 0 {
		darwinPowerObservers.Delete(m.observer)
		m.observer.Send(objc.RegisterName("release"))
	}
}

func (m *darwinPowerMonitor) reconcileDisplays() {
	if !m.systemSleeping.Load() {
		asleep, known := darwinDisplaysAsleep(m.api.onlineDisplays, m.api.displayAsleep)
		if known {
			m.displaySleeping.Store(asleep)
		}
	}
	// WindowServer can be temporarily unavailable on resume. Preserve the last
	// known screen state (sleep sets it conservatively) until a query succeeds
	// or a documented screen-wake event arrives. Always seed startup explicitly.
	m.postDisplayState(m.displaySleeping.Load())
}

func (m *darwinPowerMonitor) postDisplayState(asleep bool) {
	state := uint32(1) // A separate initialized bit ensures the startup seed sends.
	if asleep {
		state |= 2
	}
	previous := m.displayLastPosted.Swap(state)
	if previous != state && !postPowerEvent(m.ctx, m.events, powerDisplaySleep, asleep, false) {
		// Retrying on a subsequent poll is safe if delivery was temporarily full.
		m.displayLastPosted.CompareAndSwap(state, previous)
	}
}

func darwinDisplaysAsleep(online func(uint32, *uint32, *uint32) int32, asleep func(uint32) uint32) (bool, bool) {
	// Include sleeping displays; CGGetActiveDisplayList excludes them. Bound the
	// allocation, and tolerate headless sessions or transient WindowServer errors.
	var count uint32
	if online(0, nil, &count) != 0 || count == 0 || count > 256 {
		return false, false
	}
	displays := make([]uint32, count)
	if online(count, &displays[0], &count) != 0 || count == 0 || count > uint32(len(displays)) {
		return false, false
	}
	for _, display := range displays[:count] {
		if asleep(display) == 0 {
			return false, true
		}
	}
	return true, true
}

func darwinScreensDidSleep(self objc.ID, _ objc.SEL, _ objc.ID) { darwinScreensChanged(self, true) }

func darwinScreensDidWake(self objc.ID, _ objc.SEL, _ objc.ID) { darwinScreensChanged(self, false) }

func darwinScreensChanged(self objc.ID, asleep bool) {
	if value, ok := darwinPowerObservers.Load(self); ok {
		m := value.(*darwinPowerMonitor)
		m.displaySleeping.Store(asleep)
		m.postDisplayState(asleep)
	}
}

func darwinWillPowerOff(self objc.ID, _ objc.SEL, _ objc.ID) {
	if value, ok := darwinPowerObservers.Load(self); ok {
		m := value.(*darwinPowerMonitor)
		// WillPowerOff reports a logout/shutdown request; macOS has no public
		// matching cancellation notification. If our process survives, recover
		// after a short grace so cancelling the request cannot latch sleep.
		m.shutdownDeadline.Store(time.Now().Add(10 * time.Second).UnixNano())
		postPowerEvent(m.ctx, m.events, powerShutdown, true, true)
	}
}

func (m *darwinPowerMonitor) recoverCancelledShutdown(now time.Time) {
	deadline := m.shutdownDeadline.Load()
	if deadline == 0 || now.UnixNano() < deadline || !m.shutdownDeadline.CompareAndSwap(deadline, 0) {
		return
	}
	m.reconcileDisplays()
	postPowerEvent(m.ctx, m.events, powerShutdown, false, false)
}

func darwinSystemPowerChanged(id uintptr, _ uint32, message uint32, argument uintptr) {
	if value, ok := darwinPowerMonitors.Load(id); ok {
		value.(*darwinPowerMonitor).systemPowerChanged(message, argument)
	}
}

func (m *darwinPowerMonitor) systemPowerChanged(message uint32, argument uintptr) {
	switch message {
	case darwinCanSystemSleep:
		// Pipkin follows the host; it never vetoes or delays tentative idle sleep.
		m.api.allowPowerChange(m.rootPort, argument)
	case darwinSystemWillSleep:
		// The helper gets a short opportunity to send sleep before the OS powers
		// off hardware. Always acknowledge, including cancellation/send failure.
		defer m.api.allowPowerChange(m.rootPort, argument)
		m.systemSleeping.Store(true)
		m.displaySleeping.Store(true)
		postPowerEvent(m.ctx, m.events, powerSystemSleep, true, true)
	case darwinSystemHasPoweredOn:
		// macOS may wake for maintenance with displays still off. Reconcile
		// those first so clearing system sleep cannot wake Pipkin in a dark wake.
		m.systemSleeping.Store(false)
		m.reconcileDisplays()
		postPowerEvent(m.ctx, m.events, powerSystemSleep, false, false)
	}
}
