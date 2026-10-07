package pipkin

import (
	"context"
	"errors"
	"fmt"
	"os"
	"time"

	"github.com/godbus/dbus/v5"
	"golang.org/x/sys/unix"
)

const (
	logindDestination = "org.freedesktop.login1"
	logindInterface   = "org.freedesktop.login1.Manager"
	logindPath        = dbus.ObjectPath("/org/freedesktop/login1")
	logindCallTimeout = 2 * time.Second
)

type powerBusConnection interface {
	Object(string, dbus.ObjectPath) dbus.BusObject
	AddMatchSignalContext(context.Context, ...dbus.MatchOption) error
	Signal(chan<- *dbus.Signal)
	RemoveSignal(chan<- *dbus.Signal)
	Context() context.Context
	Close() error
}

func startPowerMonitor(ctx context.Context, events chan<- powerEvent) (func(), error) {
	stop := startLogindPowerMonitor(ctx, events, func(ctx context.Context) (powerBusConnection, error) {
		return dbus.ConnectSystemBus(dbus.WithContext(ctx), dbus.WithSignalHandler(dbus.NewSequentialSignalHandler()))
	}, 30*time.Second)
	return stop, nil
}

// Linux desktops do not share a reliable display-off signal. logind's lock and
// idle hints are deliberately excluded: they do not mean the displays are off.
func startLogindPowerMonitor(ctx context.Context, events chan<- powerEvent, connect func(context.Context) (powerBusConnection, error), retry time.Duration) func() {
	ctx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() {
		defer close(done)
		reported := false
		for ctx.Err() == nil {
			conn, err := connect(ctx)
			if err == nil {
				err = watchLogindPower(ctx, events, conn)
			}
			if ctx.Err() != nil {
				return
			}
			if !reported {
				logf("host power monitor unavailable; retrying: %v", err)
				reported = true
			}
			timer := time.NewTimer(retry)
			select {
			case <-ctx.Done():
				timer.Stop()
				return
			case <-timer.C:
			}
		}
	}()
	return func() {
		cancel()
		<-done
	}
}

func watchLogindPower(ctx context.Context, events chan<- powerEvent, conn powerBusConnection) error {
	defer conn.Close()
	signals := make(chan *dbus.Signal, 16)
	conn.Signal(signals)
	defer conn.RemoveSignal(signals)
	for _, match := range [][]dbus.MatchOption{
		{dbus.WithMatchSender(logindDestination), dbus.WithMatchInterface(logindInterface), dbus.WithMatchObjectPath(logindPath)},
		{dbus.WithMatchSender("org.freedesktop.DBus"), dbus.WithMatchInterface("org.freedesktop.DBus"), dbus.WithMatchMember("NameOwnerChanged"), dbus.WithMatchArg(0, logindDestination)},
	} {
		callCtx, cancel := context.WithTimeout(ctx, logindCallTimeout)
		err := conn.AddMatchSignalContext(callCtx, match...)
		cancel()
		if err != nil {
			return err
		}
	}
	manager := conn.Object(logindDestination, logindPath)
	var inhibitor *os.File
	release := func() {
		if inhibitor != nil {
			inhibitor.Close()
			inhibitor = nil
		}
	}
	defer release()
	acquire := func() {
		if inhibitor != nil || ctx.Err() != nil {
			return
		}
		var err error
		inhibitor, err = logindDelayInhibitor(ctx, manager)
		if err != nil && ctx.Err() == nil {
			// Signal observation remains useful when policy denies a delay lock.
			logf("host power reporting has no suspend/shutdown delay: %v", err)
		}
	}
	acquire()
	callCtx, cancel := context.WithTimeout(ctx, logindCallTimeout)
	call := manager.CallWithContext(callCtx, "org.freedesktop.DBus.Properties.GetAll", 0, logindInterface)
	var properties map[string]dbus.Variant
	err := call.Store(&properties)
	cancel()
	if err != nil {
		return err
	}
	sleeping, sleepOK := properties["PreparingForSleep"].Value().(bool)
	shuttingDown, shutdownOK := properties["PreparingForShutdown"].Value().(bool)
	if !sleepOK || !shutdownOK {
		return errors.New("logind returned invalid power preparation states")
	}
	// The snapshot follows subscription. Older queued signals are already
	// represented by its state; replaying them could reverse a newer resume.
	snapshotSequence := call.ResponseSequence
	postPowerEvent(ctx, events, powerSystemSleep, sleeping, sleeping)
	postPowerEvent(ctx, events, powerShutdown, shuttingDown, shuttingDown)
	if sleeping || shuttingDown {
		release()
	}
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-conn.Context().Done():
			return errors.New("system D-Bus disconnected")
		case signal, ok := <-signals:
			if !ok {
				return errors.New("system D-Bus signal stream closed")
			}
			if signal == nil {
				continue
			}
			if signal.Name == "org.freedesktop.DBus.NameOwnerChanged" && len(signal.Body) == 3 && signal.Body[0] == logindDestination {
				return errors.New("logind restarted")
			}
			if signal.Path != logindPath || signal.Sequence != dbus.NoSequence && signal.Sequence <= snapshotSequence || len(signal.Body) != 1 {
				continue
			}
			active, valid := signal.Body[0].(bool)
			if !valid {
				continue
			}
			var kind powerEventKind
			switch signal.Name {
			case logindInterface + ".PrepareForSleep":
				kind, sleeping = powerSystemSleep, active
			case logindInterface + ".PrepareForShutdown":
				kind, shuttingDown = powerShutdown, active
			default:
				continue
			}
			postPowerEvent(ctx, events, kind, active, active)
			if active {
				// A delay inhibitor only gives the helper a bounded opportunity to
				// write asleep. Always release it, including a missing helper ack.
				release()
			} else if !sleeping && !shuttingDown {
				acquire()
			}
		}
	}
}

func logindDelayInhibitor(ctx context.Context, manager dbus.BusObject) (*os.File, error) {
	callCtx, cancel := context.WithTimeout(ctx, logindCallTimeout)
	defer cancel()
	call := manager.CallWithContext(callCtx, logindInterface+".Inhibit", 0,
		"sleep:shutdown", "Pipkin", "Send display sleep state before the host stops", "delay")
	var fd dbus.UnixFD
	if err := call.Store(&fd); err != nil {
		return nil, err
	}
	if fd < 0 {
		return nil, fmt.Errorf("logind returned invalid inhibitor descriptor %d", fd)
	}
	unix.CloseOnExec(int(fd))
	file := os.NewFile(uintptr(fd), "pipkin-host-power-delay")
	if err := ctx.Err(); err != nil {
		file.Close()
		return nil, err
	}
	return file, nil
}
