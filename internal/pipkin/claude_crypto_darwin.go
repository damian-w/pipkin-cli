package pipkin

import (
	"context"
	"errors"
)

func claudeDecryptor(ctx context.Context, dir string) (func([]byte) ([]byte, error), error) {
	password, err := readClaudeSafeStorage(ctx, false)
	if err != nil {
		if errors.Is(err, errCredentialLocked) {
			return nil, &claudePermissionError{err}
		}
		return nil, err
	}
	key, err := claudeCBCKey(password, 1003)
	if err != nil {
		return nil, err
	}
	return func(data []byte) ([]byte, error) { return decryptClaudeCBC(data, key) }, nil
}

type claudePermissionError struct{ cause error }

func (e *claudePermissionError) Error() string {
	return "macOS hasn't let Pipkin read Claude's sign-in. Run 'pipkin authorize'."
}

func (e *claudePermissionError) Unwrap() error { return e.cause }

// readClaudeSafeStorage reads Claude Desktop's storage key. Only the foreground
// authorize command lets macOS show its permission dialog.
func readClaudeSafeStorage(ctx context.Context, allowUI bool) ([]byte, error) {
	for _, account := range []string{"Claude Key", "Claude"} {
		password, err := readGenericPasswordMode(ctx, "Claude Safe Storage", account, allowUI)
		if !errors.Is(err, errCredentialMissing) {
			return password, err
		}
	}
	return nil, errCredentialMissing
}

// Called only by an explicit foreground authorize command, never by polling.
func authorizeClaudeKeychainWithNotice(ctx context.Context, notice func() error) error {
	if credential, err := loadClaudeCredential(ctx); err == nil && credential.source != claudeDesktopSource {
		return nil
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if !claudeDesktopSignedIn() {
		return errors.New("Claude Desktop sign-in was not found. Open Claude and sign in, then run 'pipkin authorize'.")
	}
	if err := notice(); err != nil {
		return err
	}
	secret, err := readClaudeSafeStorage(ctx, true)
	clear(secret)
	return err
}
