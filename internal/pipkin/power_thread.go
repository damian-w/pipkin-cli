//go:build darwin || windows

package pipkin

import (
	"context"
	"runtime"
)

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
