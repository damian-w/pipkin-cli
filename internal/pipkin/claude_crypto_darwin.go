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
	return "macOS hasn't let Pipkin read Claude's sign-in. Run 'pipkin authorize', then 'pipkin restart'."
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
