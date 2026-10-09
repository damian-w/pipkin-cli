package pipkin

import (
	"context"
	"runtime"
	"time"
)

type powerEventKind uint8

const (
	powerSystemSleep powerEventKind = iota
	powerDisplaySleep
	powerShutdown
)

const powerWriteTimeout = 500 * time.Millisecond

// Native observers only publish events. The helper owns the serial connection,
// sequence numbers and derived state, including acknowledgments before suspend.
type powerEvent struct {
	kind   powerEventKind
	active bool
	done   chan struct{}
}

type hostPower struct {
	systemSleeping  bool
	displaySleeping bool
	shuttingDown    bool
}

func (p hostPower) asleep() bool {
	return p.systemSleeping || p.displaySleeping || p.shuttingDown
}

// suspended means the host is going down; only host state may still be sent.
func (p hostPower) suspended() bool { return p.systemSleeping || p.shuttingDown }

func (p hostPower) state() string {
	if p.asleep() {
		return "asleep"
	}
	return "awake"
}

func (p *hostPower) apply(event powerEvent) {
	switch event.kind {
	case powerSystemSleep:
		p.systemSleeping = event.active
	case powerDisplaySleep:
		p.displaySleeping = event.active
	case powerShutdown:
		p.shuttingDown = event.active
	}
}

// startPowerThread runs a native observer on one locked OS thread. run must send
// exactly one startup result to ready, then observe until ctx is cancelled.
func startPowerThread(ctx context.Context, run func(context.Context, chan<- error)) (func(), error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	ctx, cancel := context.WithCancel(ctx)
	ready, done := make(chan error, 1), make(chan struct{})
	go func() {
		defer close(done)
		runtime.LockOSThread()
		defer runtime.UnlockOSThread()
		run(ctx, ready)
	}()
	if err := <-ready; err != nil {
		cancel()
		<-done
		return nil, err
	}
	return func() { cancel(); <-done }, nil
}

// A native pre-sleep callback must never hold the host up indefinitely. Even if
// delivery cannot finish, the firmware's heartbeat timeout turns its screen off.
func postPowerEvent(ctx context.Context, events chan<- powerEvent, kind powerEventKind, active, wait bool) bool {
	event := powerEvent{kind: kind, active: active}
	if wait {
		event.done = make(chan struct{})
	}
	timer := time.NewTimer(time.Second)
	defer timer.Stop()
	select {
	case events <- event:
	case <-ctx.Done():
		return false
	case <-timer.C:
		return false
	}
	if event.done == nil {
		return true
	}
	select {
	case <-event.done:
		return true
	case <-ctx.Done():
	case <-timer.C:
	}
	return false
}

func (h *Helper) sendHostPower() bool {
	if !h.handlingPower {
		h.drainPowerEvents(h.powerEvents)
	}
	return h.send("kind=host state=" + h.power.state())
}

func (h *Helper) handlePowerEvent(event powerEvent) {
	previouslyHandling := h.handlingPower
	h.handlingPower = true
	defer func() { h.handlingPower = previouslyHandling }()
	if event.done != nil {
		defer close(event.done)
	}
	was := h.power
	h.power.apply(event)
	resumed := event.kind == powerSystemSleep && !event.active
	woken := was.asleep() && !h.power.asleep()
	if resumed || woken {
		// The old handle may still work, or USB may have disappeared during sleep.
		// Try it immediately, and make rediscovery/identity checks due on failure.
		h.due["discover"] = time.Time{}
		h.due["identify"] = time.Time{}
		if resumed {
			// A worker can have finished before suspension but still have a queued
			// result. Re-observe after resume instead of postponing refresh for it.
			h.invalidateObservations()
		} else {
			h.refreshProviders(h.now())
		}
	}
	if was != h.power || resumed || event.done != nil {
		logf("host power: %s (system_sleep=%t display_sleep=%t shutdown=%t)",
			h.power.state(), h.power.systemSleeping, h.power.displaySleeping, h.power.shuttingDown)
		if h.conn != nil {
			ctx, cancel := context.WithTimeout(context.Background(), powerWriteTimeout)
			previous := h.ctx
			h.ctx = ctx
			if h.sendHostPower() && event.done != nil && h.power.asleep() {
				// A successful OS write can still leave bytes queued in the USB
				// driver. The existing identity sequence confirms firmware accepted
				// sleep before the native callback allows hardware to suspend.
				if !h.confirmHostPower(ctx, h.sequence) {
					logf("display sleep was not confirmed; using heartbeat fallback")
				}
			}
			h.ctx = previous
			cancel()
			h.due["heartbeat"] = h.now().Add(heartbeatEvery)
		}
	}
}

func (h *Helper) confirmHostPower(ctx context.Context, sequence uint64) bool {
	if h.conn == nil {
		return false
	}
	h.identifyPending = false
	if err := h.conn.port.Write(ctx, []byte(identifyPacket)); err != nil {
		h.disconnect("sleep confirmation write failed: " + err.Error())
		return false
	}
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	for ctx.Err() == nil {
		lines, err := h.conn.lines()
		if err != nil {
			h.disconnect("sleep confirmation read failed: " + err.Error())
			return false
		}
		for _, line := range lines {
			fields := parseFields(line)
			if isIdentity(fields) {
				accepted, epoch := identityClock(fields)
				if accepted == sequence && epoch == h.clockEpoch {
					return true
				}
				if accepted > sequence || epoch != h.clockEpoch {
					// A different session/sequence is not an acknowledgment of our
					// sleep. Preserve ordering if firmware is ahead, then verify its
					// clock/session again on wake before sending cached observations.
					if accepted > h.sequence {
						h.sequence = accepted
					}
					h.due["identify"] = time.Time{}
					return false
				}
			}
		}
		select {
		case <-ctx.Done():
		case <-ticker.C:
		}
	}
	return false
}

func (h *Helper) drainPowerEvents(events <-chan powerEvent) {
	for {
		select {
		case event, open := <-events:
			if !open {
				return
			}
			h.handlePowerEvent(event)
		default:
			return
		}
	}
}
