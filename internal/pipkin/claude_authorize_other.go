//go:build !darwin

package pipkin

import "context"

func authorizeClaudeKeychain(ctx context.Context) error { return nil }

func authorizeClaudeKeychainWithNotice(ctx context.Context, notice func() error) error { return nil }
