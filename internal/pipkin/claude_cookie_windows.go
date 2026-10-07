package pipkin

import (
	"errors"
	"fmt"

	"golang.org/x/sys/windows"
)

func claudeCookieReadError(err error) error {
	if errors.Is(err, windows.ERROR_SHARING_VIOLATION) {
		return fmt.Errorf("%w: Windows has locked its cookie database", errClaudeOrgUnreadable)
	}
	if errors.Is(err, windows.ERROR_ACCESS_DENIED) {
		return fmt.Errorf("%w: access to its cookie database was denied", errClaudeOrgUnreadable)
	}
	return errClaudeOrgStorage
}
