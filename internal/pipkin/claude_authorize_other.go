//go:build !darwin

package pipkin

import "context"

func authorizeClaudeKeychainWithNotice(ctx context.Context, notice func() error) error { return nil }
