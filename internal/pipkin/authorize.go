package pipkin

import (
	"context"
	"fmt"
	"io"
)

type authorizeDependencies struct {
	authorize func(context.Context, func() error) error
	running   func() (int, error)
	restart   func() error
}

func runAuthorize(ctx context.Context, goos string, nested bool, out io.Writer, deps authorizeDependencies) error {
	if goos != "darwin" {
		return writeOutputLine(out, "", "Pipkin reads existing sign-ins automatically on this platform.")
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	notice := func() error {
		return writeOutputLine(out, "", authorizePromptNotice)
	}
	if err := deps.authorize(ctx, notice); err != nil {
		return err
	}
	if err := writeOutputLine(out, "", "Claude's saved sign-in is ready."); err != nil {
		return err
	}
	// Install and update authorize while holding the installation lock and start
	// their replacement afterward. A nested restart would deadlock that flow.
	if nested {
		return nil
	}
	pid, err := deps.running()
	if err != nil {
		return fmt.Errorf("Claude permission is ready, but Pipkin's running state could not be checked; run 'pipkin restart': %w", err)
	}
	if pid != 0 {
		if err := deps.restart(); err != nil {
			return fmt.Errorf("Claude permission is ready, but Pipkin could not restart; run 'pipkin restart' to try again: %w", err)
		}
	}
	return nil
}
