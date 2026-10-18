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
	if err := writeOutputLine(out, "", "Pipkin can now read Claude's sign-in."); err != nil {
		return err
	}
	// Install and update hold the installation lock and start Pipkin themselves.
	if nested {
		return nil
	}
	pid, err := deps.running()
	if err != nil {
		return fmt.Errorf("Claude's sign-in is ready, but Pipkin couldn't check whether it's running. Run 'pipkin restart': %w", err)
	}
	if pid != 0 {
		if err := deps.restart(); err != nil {
			return fmt.Errorf("Claude's sign-in is ready, but Pipkin couldn't restart. Run 'pipkin restart': %w", err)
		}
	}
	return nil
}
