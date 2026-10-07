package pipkin

import (
	"context"
	"errors"
)

// Called only by an explicit foreground authorize command, never by polling.
func authorizeClaudeKeychain(ctx context.Context) error {
	if !claudeDesktopSignedIn() {
		return errors.New("Claude Desktop sign-in was not found")
	}
	for _, account := range []string{"Claude Key", "Claude"} {
		secret, err := readGenericPasswordMode(ctx, "Claude Safe Storage", account, true)
		clear(secret)
		if !errors.Is(err, errCredentialMissing) {
			return err
		}
	}
	return errCredentialMissing
}
