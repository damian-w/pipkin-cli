package pipkin

import (
	"context"
	"errors"
	"os"
	"reflect"
	"sync"
	"testing"
	"time"

	"github.com/godbus/dbus/v5"
	"golang.org/x/sys/unix"
)

type logindTestBus struct {
	t              *testing.T
	ctx            context.Context
	cancel         context.CancelFunc
	mu             sync.Mutex
	signals        chan<- *dbus.Signal
	writers        []*os.File
	acquired       chan int
	closed         chan struct{}
	closeOnce      sync.Once
	matchErr       error
	inhibitErr     error
	propertiesErr  error
	sleeping       bool
	shuttingDown   bool
	beforeSnapshot func()
}

func newLogindTestBus(t *testing.T) *logindTestBus {
	ctx, cancel := context.WithCancel(context.Background())
	bus := &logindTestBus{t: t, ctx: ctx, cancel: cancel, acquired: make(chan int, 16), closed: make(chan struct{})}
	t.Cleanup(func() {
		bus.Close()
		bus.mu.Lock()
		defer bus.mu.Unlock()
		for _, writer := range bus.writers {
			writer.Close()
		}
	})
	return bus
}

func (b *logindTestBus) Object(destination string, path dbus.ObjectPath) dbus.BusObject {
	if destination != logindDestination || path != logindPath {
		b.t.Error("unexpected logind destination")
	}
	return logindTestObject{bus: b}
}

func (b *logindTestBus) AddMatchSignalContext(ctx context.Context, options ...dbus.MatchOption) error {
	if _, ok := ctx.Deadline(); !ok {
		b.t.Error("signal subscription must be bounded")
	}
	return b.matchErr
}

func (b *logindTestBus) Signal(signals chan<- *dbus.Signal) {
	b.mu.Lock()
	b.signals = signals
	b.mu.Unlock()
}

func (b *logindTestBus) RemoveSignal(signals chan<- *dbus.Signal) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.signals != signals {
		b.t.Error("removed the wrong signal stream")
	}
	b.signals = nil
}

func (b *logindTestBus) Context() context.Context { return b.ctx }

func (b *logindTestBus) Close() error {
	b.closeOnce.Do(func() {
		b.cancel()
		close(b.closed)
	})
	return nil
}

func (b *logindTestBus) emit(signal *dbus.Signal) {
	b.mu.Lock()
	signals := b.signals
	b.mu.Unlock()
	if signals == nil {
		b.t.Fatal("power monitor has not subscribed")
	}
	select {
	case signals <- signal:
	case <-time.After(time.Second):
		b.t.Fatal("signal delivery stalled")
	}
}

type logindTestObject struct {
	dbus.BusObject
	bus *logindTestBus
}

func (o logindTestObject) CallWithContext(ctx context.Context, method string, flags dbus.Flags, args ...any) *dbus.Call {
	b := o.bus
	if _, ok := ctx.Deadline(); !ok || flags != 0 {
		b.t.Error("logind calls must have a deadline and normal reply handling")
	}
	if err := ctx.Err(); err != nil {
		return &dbus.Call{Err: err}
	}
	switch method {
	case logindInterface + ".Inhibit":
		if !reflect.DeepEqual(args, []any{"sleep:shutdown", "Pipkin", "Send display sleep state before the host stops", "delay"}) {
			b.t.Error("unexpected inhibitor scope or mode")
		}
		if b.inhibitErr != nil {
			return &dbus.Call{Err: b.inhibitErr}
		}
		reader, writer, err := os.Pipe()
		if err != nil {
			b.t.Error(err)
			return &dbus.Call{Err: err}
		}
		fd, err := unix.Dup(int(reader.Fd()))
		reader.Close()
		if err != nil {
			writer.Close()
			b.t.Error(err)
			return &dbus.Call{Err: err}
		}
		b.mu.Lock()
		b.writers = append(b.writers, writer)
		b.mu.Unlock()
		b.acquired <- fd
		return &dbus.Call{Body: []any{dbus.UnixFD(fd)}}
	case "org.freedesktop.DBus.Properties.GetAll":
		if !reflect.DeepEqual(args, []any{logindInterface}) {
			b.t.Error("unexpected power property interface")
		}
		if b.beforeSnapshot != nil {
			b.beforeSnapshot()
		}
		if b.propertiesErr != nil {
			return &dbus.Call{Err: b.propertiesErr}
		}
		return &dbus.Call{Body: []any{map[string]dbus.Variant{
			"PreparingForSleep": dbus.MakeVariant(b.sleeping), "PreparingForShutdown": dbus.MakeVariant(b.shuttingDown),
		}}, ResponseSequence: 20}
	default:
		b.t.Error("unexpected logind method:", method)
		return &dbus.Call{Err: errors.New("unexpected method")}
	}
}

func logindTestWatch(t *testing.T, bus *logindTestBus) (chan powerEvent, context.CancelFunc, <-chan error) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	events := make(chan powerEvent, 16)
	finished := make(chan error, 1)
	go func() { finished <- watchLogindPower(ctx, events, bus) }()
	t.Cleanup(func() {
		cancel()
		select {
		case <-bus.closed:
		case <-time.After(2 * time.Second):
			t.Error("logind watcher did not close its bus")
		}
	})
	return events, cancel, finished
}

func logindTestEvent(t *testing.T, events <-chan powerEvent, kind powerEventKind, active bool) powerEvent {
	t.Helper()
	select {
	case event := <-events:
		if event.kind != kind || event.active != active {
			t.Fatalf("power event=%+v, want kind=%d active=%t", event, kind, active)
		}
		return event
	case <-time.After(2 * time.Second):
		t.Fatal("missing power event")
		return powerEvent{}
	}
}

func logindTestFD(t *testing.T, bus *logindTestBus) int {
	t.Helper()
	select {
	case fd := <-bus.acquired:
		return fd
	case <-time.After(time.Second):
		t.Fatal("missing delay inhibitor")
		return -1
	}
}

func logindTestClosedFD(t *testing.T, fd int) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := unix.FcntlInt(uintptr(fd), unix.F_GETFD, 0); errors.Is(err, unix.EBADF) {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("delay inhibitor descriptor stayed open")
}

func logindTestSignal(member string, active bool) *dbus.Signal {
	return &dbus.Signal{Path: logindPath, Name: logindInterface + "." + member, Body: []any{active}, Sequence: 30}
}

func TestLogindReportsSleepBeforeReleasingDelayAndReacquiresOnResume(t *testing.T) {
	isolate(t)
	bus := newLogindTestBus(t)
	events, cancel, finished := logindTestWatch(t, bus)
	fd := logindTestFD(t, bus)
	logindTestEvent(t, events, powerSystemSleep, false)
	logindTestEvent(t, events, powerShutdown, false)
	bus.emit(logindTestSignal("PrepareForSleep", true))
	event := logindTestEvent(t, events, powerSystemSleep, true)
	if event.done == nil {
		t.Fatal("pre-sleep report did not request a helper acknowledgment")
	}
	if flags, err := unix.FcntlInt(uintptr(fd), unix.F_GETFD, 0); err != nil || flags&unix.FD_CLOEXEC == 0 {
		t.Fatal("delay inhibitor released before the sleep report completed")
	}
	close(event.done)
	logindTestClosedFD(t, fd)
	bus.emit(logindTestSignal("PrepareForSleep", false))
	logindTestEvent(t, events, powerSystemSleep, false)
	fd = logindTestFD(t, bus)
	cancel()
	select {
	case <-finished:
	case <-time.After(time.Second):
		t.Fatal("watcher cancellation stalled")
	}
	logindTestClosedFD(t, fd)
}

func TestLogindShutdownCancellationReacquiresDelay(t *testing.T) {
	isolate(t)
	bus := newLogindTestBus(t)
	events, _, _ := logindTestWatch(t, bus)
	fd := logindTestFD(t, bus)
	logindTestEvent(t, events, powerSystemSleep, false)
	logindTestEvent(t, events, powerShutdown, false)
	bus.emit(logindTestSignal("PrepareForShutdown", true))
	event := logindTestEvent(t, events, powerShutdown, true)
	close(event.done)
	logindTestClosedFD(t, fd)
	bus.emit(logindTestSignal("PrepareForShutdown", false))
	logindTestEvent(t, events, powerShutdown, false)
	logindTestFD(t, bus)
}

func TestLogindSnapshotSupersedesQueuedEarlierSleepSignals(t *testing.T) {
	isolate(t)
	bus := newLogindTestBus(t)
	bus.beforeSnapshot = func() {
		signal := logindTestSignal("PrepareForSleep", true)
		signal.Sequence = 10
		bus.emit(signal)
	}
	events, _, _ := logindTestWatch(t, bus)
	logindTestEvent(t, events, powerSystemSleep, false)
	logindTestEvent(t, events, powerShutdown, false)
	bus.emit(logindTestSignal("PrepareForShutdown", true))
	event := logindTestEvent(t, events, powerShutdown, true)
	close(event.done)
}

func TestLogindInitialPreparingStateReleasesDelayAfterReport(t *testing.T) {
	isolate(t)
	bus := newLogindTestBus(t)
	bus.sleeping = true
	events, _, _ := logindTestWatch(t, bus)
	fd := logindTestFD(t, bus)
	event := logindTestEvent(t, events, powerSystemSleep, true)
	close(event.done)
	logindTestEvent(t, events, powerShutdown, false)
	logindTestClosedFD(t, fd)
	bus.emit(logindTestSignal("PrepareForSleep", false))
	logindTestEvent(t, events, powerSystemSleep, false)
	logindTestFD(t, bus)
}

func TestLogindMissingAcknowledgmentCannotPreventSleep(t *testing.T) {
	isolate(t)
	bus := newLogindTestBus(t)
	events, _, _ := logindTestWatch(t, bus)
	fd := logindTestFD(t, bus)
	logindTestEvent(t, events, powerSystemSleep, false)
	logindTestEvent(t, events, powerShutdown, false)
	bus.emit(logindTestSignal("PrepareForSleep", true))
	logindTestEvent(t, events, powerSystemSleep, true)
	logindTestClosedFD(t, fd)
}

func TestLogindDeniedDelayStillObservesSleepAndResume(t *testing.T) {
	isolate(t)
	bus := newLogindTestBus(t)
	bus.inhibitErr = dbus.Error{Name: "org.freedesktop.DBus.Error.AccessDenied"}
	events, _, _ := logindTestWatch(t, bus)
	logindTestEvent(t, events, powerSystemSleep, false)
	logindTestEvent(t, events, powerShutdown, false)
	bus.emit(logindTestSignal("PrepareForSleep", true))
	close(logindTestEvent(t, events, powerSystemSleep, true).done)
	bus.emit(logindTestSignal("PrepareForSleep", false))
	logindTestEvent(t, events, powerSystemSleep, false)
}

func TestLogindCancellationDuringReportReleasesDelay(t *testing.T) {
	isolate(t)
	bus := newLogindTestBus(t)
	events, cancel, finished := logindTestWatch(t, bus)
	fd := logindTestFD(t, bus)
	logindTestEvent(t, events, powerSystemSleep, false)
	logindTestEvent(t, events, powerShutdown, false)
	bus.emit(logindTestSignal("PrepareForSleep", true))
	logindTestEvent(t, events, powerSystemSleep, true)
	cancel()
	select {
	case <-finished:
	case <-time.After(time.Second):
		t.Fatal("cancellation waited for an absent helper acknowledgment")
	}
	logindTestClosedFD(t, fd)
}

func TestLogindRestartsAndBusDisconnectsReconnectAndReseedState(t *testing.T) {
	for _, restart := range []string{"logind", "bus"} {
		t.Run(restart, func(t *testing.T) {
			isolate(t)
			first, second := newLogindTestBus(t), newLogindTestBus(t)
			first.sleeping = restart == "bus"
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			events := make(chan powerEvent, 16)
			connections := 0
			stop := startLogindPowerMonitor(ctx, events, func(context.Context) (powerBusConnection, error) {
				connections++
				if connections == 1 {
					return first, nil
				}
				return second, nil
			}, time.Millisecond)
			defer stop()
			logindTestFD(t, first)
			event := logindTestEvent(t, events, powerSystemSleep, first.sleeping)
			if event.done != nil {
				close(event.done)
			}
			logindTestEvent(t, events, powerShutdown, false)
			if restart == "logind" {
				first.emit(&dbus.Signal{Name: "org.freedesktop.DBus.NameOwnerChanged", Body: []any{logindDestination, ":1.2", ""}})
			} else {
				first.cancel()
			}
			select {
			case <-first.closed:
			case <-time.After(time.Second):
				t.Fatal("lost power bus not closed")
			}
			logindTestEvent(t, events, powerSystemSleep, false)
			logindTestEvent(t, events, powerShutdown, false)
			logindTestFD(t, second)
			first.mu.Lock()
			writer := first.writers[0]
			first.mu.Unlock()
			if _, err := writer.Write([]byte{1}); !errors.Is(err, unix.EPIPE) {
				t.Fatal("reconnect leaked the previous delay descriptor")
			}
		})
	}
}

func TestLogindSetupFailureClosesBusAndDelay(t *testing.T) {
	isolate(t)
	bus := newLogindTestBus(t)
	bus.propertiesErr = errors.New("logind unavailable")
	err := watchLogindPower(context.Background(), make(chan powerEvent, 4), bus)
	if err == nil {
		t.Fatal("invalid logind snapshot accepted")
	}
	fd := logindTestFD(t, bus)
	logindTestClosedFD(t, fd)
	select {
	case <-bus.closed:
	default:
		t.Fatal("failed initialization left the power bus open")
	}
}

func TestLogindRetriesMissingBusAndStopInterruptsRetry(t *testing.T) {
	isolate(t)
	bus := newLogindTestBus(t)
	events := make(chan powerEvent, 16)
	attempts := 0
	stop := startLogindPowerMonitor(context.Background(), events, func(context.Context) (powerBusConnection, error) {
		attempts++
		if attempts == 1 {
			return nil, errors.New("system bus temporarily unavailable")
		}
		return bus, nil
	}, time.Millisecond)
	logindTestEvent(t, events, powerSystemSleep, false)
	logindTestEvent(t, events, powerShutdown, false)
	fd := logindTestFD(t, bus)
	stop()
	logindTestClosedFD(t, fd)
	if attempts != 2 {
		t.Fatalf("connection attempts=%d, want 2", attempts)
	}

	attempted := make(chan struct{}, 1)
	stop = startLogindPowerMonitor(context.Background(), events, func(context.Context) (powerBusConnection, error) {
		attempted <- struct{}{}
		return nil, errors.New("system bus unavailable")
	}, time.Hour)
	defer stop()
	select {
	case <-attempted:
	case <-time.After(time.Second):
		t.Fatal("power monitor did not try to connect")
	}
	finished := make(chan struct{})
	go func() { stop(); close(finished) }()
	select {
	case <-finished:
	case <-time.After(time.Second):
		t.Fatal("power monitor shutdown waited for its retry timer")
	}
}
