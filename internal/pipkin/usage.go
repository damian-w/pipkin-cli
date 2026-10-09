package pipkin

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"net/http"
	"os"
	"runtime"
	"slices"
	"strconv"
	"time"
)

var errSignedOut = errors.New("no existing sign-in found")

// Never forward an existing app credential across a redirect, even on the same host.
var usageHTTPClient = &http.Client{Timeout: 20 * time.Second,
	CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}

type usageRetryError struct {
	message string
	RetryAt time.Time
}

func (e *usageRetryError) Error() string { return e.message }

func usageResponseError(provider string, response *http.Response) error {
	message := fmt.Sprintf("%s usage returned HTTP %d", provider, response.StatusCode)
	if response.StatusCode != http.StatusTooManyRequests && response.StatusCode != http.StatusServiceUnavailable {
		return errors.New(message)
	}
	now := time.Now()
	retry := now.Add(15 * time.Minute)
	value := response.Header.Get("Retry-After")
	if seconds, err := strconv.ParseInt(value, 10, 64); err == nil && seconds >= 0 && seconds <= 7*86400 {
		if candidate := now.Add(time.Duration(seconds) * time.Second); candidate.After(retry) {
			retry = candidate
		}
	} else if candidate, err := http.ParseTime(value); err == nil && candidate.After(retry) {
		retry = candidate
	}
	return &usageRetryError{message: message, RetryAt: retry}
}

// Requests that never reach the provider are retried sooner than provider errors.
type offlineError struct{ message string }

func (e offlineError) Error() string { return e.message }

type providerResult struct {
	provider   string
	reading    *Reading
	err        error
	generation uint64
}

func collectProvider(parent context.Context, provider, salt string) providerResult {
	ctx, cancel := context.WithTimeout(parent, 45*time.Second)
	defer cancel()
	var reading *Reading
	var err error
	if provider == "claude" {
		reading, err = fetchClaudeUsage(ctx, salt)
	} else {
		reading, err = fetchCodexUsage(ctx, salt)
	}
	if reading == nil {
		reading = &Reading{Account: provider}
	}
	return providerResult{provider: provider, reading: reading, err: err}
}

func providerState(err error) string {
	if err == nil {
		return "available"
	}
	if errors.Is(err, errSignedOut) {
		return "signed_out"
	}
	return "unavailable"
}

func providerMessage(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

func (s *Status) setProviderError(provider string, err error) {
	if provider == "codex" {
		s.CodexError = providerMessage(err)
	} else if provider == "claude" {
		s.ClaudeError = providerMessage(err)
	}
}

func (s Status) providerError(provider string) string {
	if provider == "codex" {
		return s.CodexError
	}
	if provider == "claude" {
		return s.ClaudeError
	}
	return ""
}

func usageCommand(args []string) error {
	jsonOutput := len(args) == 1 && args[0] == "--json"
	if len(args) > 0 && !jsonOutput {
		return errors.New("usage: pipkin usage [--json]")
	}
	now := time.Now()
	config, err := initializedConfig()
	if err != nil {
		return err
	}
	results := make(chan providerResult, 2)
	for _, provider := range providers {
		go func() { results <- collectProvider(context.Background(), provider, config.Salt) }()
	}
	status := Status{SchemaVersion: 1, States: map[string]string{}, Version: version, Readings: map[string]*Reading{}, Updated: now.Unix()}
	for range providers {
		result := <-results
		status.Readings[result.provider] = result.reading
		status.States[result.provider] = providerState(result.err)
		status.setProviderError(result.provider, result.err)
	}
	if jsonOutput {
		return printJSON(status)
	}
	for _, provider := range providers {
		r := status.Readings[provider]
		message := status.providerError(provider)
		fmt.Printf("%s", provider)
		if r.Plan != "" {
			fmt.Printf(" (%s)", terminalText(r.Plan))
		}
		fmt.Println(":")
		if message != "" {
			fmt.Printf("  %s\n", message)
		} else {
			if r.SessionNoCap {
				fmt.Println("  Session: no cap")
			} else {
				fmt.Println("  " + describeWindow("Session", r.Session, now.Unix()))
			}
			fmt.Println("  " + describeWindow("Weekly", r.Weekly, now.Unix()))
		}
		for _, model := range slices.Sorted(maps.Keys(r.Models)) {
			fmt.Println("  " + describeWindow(model, r.Models[model], now.Unix()))
		}
		if r.ExtraUsage != nil {
			if !r.ExtraUsage.Enabled {
				fmt.Println("  Extra usage: disabled")
			} else if r.ExtraUsage.Used != nil {
				fmt.Printf("  Extra usage: %.2f %s used\n", *r.ExtraUsage.Used, r.ExtraUsage.Currency)
			} else {
				fmt.Println("  Extra usage: enabled; spend unavailable")
			}
		}
		if r.Credits != nil {
			if r.Credits.Unlimited {
				fmt.Println("  Credits: unlimited")
			} else if r.Credits.Balance != nil {
				fmt.Printf("  Credits: %.2f\n", *r.Credits.Balance)
			}
		}
		if r.Banked != nil {
			fmt.Printf("  Banked resets: %d\n", *r.Banked)
		}
	}
	return nil
}

func printJSON(value any) error {
	encoder := json.NewEncoder(os.Stdout)
	encoder.SetIndent("", "  ")
	return encoder.Encode(value)
}

func statusJSONCommand() error {
	var status Status
	if err := readJSON(statusPath(), &status); err != nil {
		return errors.New("no helper snapshot yet; run `pipkin usage --json` for a fresh read")
	}
	pid, err := runningPID()
	if err != nil {
		return err
	}
	status.PID = pid
	return printJSON(status)
}

func authorizeCommand() error {
	if runtime.GOOS != "darwin" {
		fmt.Println("Pipkin reads existing sign-ins automatically on this platform.")
		return nil
	}
	fmt.Println("macOS may ask to read Claude's saved sign-in. Choose Always Allow for background access.")
	if err := authorizeClaudeKeychain(context.Background()); err != nil {
		return err
	}
	fmt.Println("Claude's saved sign-in is ready. No Claude login was needed.")
	return nil
}
