package pipkin

import (
	"context"
	"fmt"
	"io"
	"time"
)

type usageDependencies struct {
	now              func() time.Time
	config           func() (Config, error)
	collect          func(context.Context, string, string) providerResult
	progressTerminal bool
}

func runUsage(ctx context.Context, jsonOutput bool, out, progress io.Writer, deps usageDependencies) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	now := deps.now()
	config, err := deps.config()
	if err != nil {
		return err
	}
	if !jsonOutput && deps.progressTerminal {
		if _, err := fmt.Fprintln(progress, "Checking Codex and Claude..."); err != nil {
			return err
		}
	}
	results := make(chan providerResult, len(providers))
	for _, provider := range providers {
		go func() { results <- deps.collect(ctx, provider, config.Salt) }()
	}
	status := Status{SchemaVersion: 1, States: map[string]string{}, Version: version, Readings: map[string]*Reading{}, Updated: now.Unix()}
	messages := map[string]string{}
	for range providers {
		select {
		case result := <-results:
			status.Readings[result.provider] = result.reading
			status.States[result.provider] = providerState(result.err)
			messages[result.provider] = providerMessage(result.err)
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	status.CodexError, status.ClaudeError = messages["codex"], messages["claude"]
	if jsonOutput {
		return printJSONTo(out, status)
	}
	return writeProviders(out, status, deps.now().Unix(), true)
}
