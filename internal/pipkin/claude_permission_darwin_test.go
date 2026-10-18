package pipkin

import (
	"errors"
	"strings"
	"testing"
)

func TestClaudeDesktopPermissionErrorNamesRecovery(t *testing.T) {
	err := &claudePermissionError{errCredentialLocked}
	if !errors.Is(err, errCredentialLocked) || !strings.Contains(err.Error(), "pipkin authorize") || !strings.Contains(err.Error(), "pipkin restart") || strings.Contains(err.Error(), "silent access") {
		t.Fatalf("permission remedy or sentinel lost: %v", err)
	}
}
