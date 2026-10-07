//go:build !windows

package pipkin

func claudeCookieReadError(error) error { return errClaudeOrgUnreadable }
