package pipkin

import (
	"context"
	"errors"
)

// Called only by an explicit foreground authorize command, never by polling.
func authorizeClaudeKeychain(ctx context.Context) error {
	if credential, err := loadClaudeCredential(ctx); err == nil && credential.source != claudeDesktopSource {
		return nil
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if !claudeDesktopSignedIn() {
		return errors.New("Claude Desktop sign-in was not found")
	}
	secret, err := readClaudeSafeStorage(ctx, true)
	clear(secret)
	return err
}
