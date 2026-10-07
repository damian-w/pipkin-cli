//go:build !darwin

package pipkin

import "context"

func authorizeClaudeKeychain(ctx context.Context) error { return nil }
