package pipkin

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strconv"
	"time"

	"github.com/mattn/go-isatty"
)

var errSignedOut = errors.New("no existing sign-in found")

// signedOutError is a sign-out with a user-facing message.
type signedOutError string

func (e signedOutError) Error() string { return string(e) }

func (e signedOutError) Is(target error) bool { return target == errSignedOut }

// Never forward an existing app credential across a redirect, even on the same host.
var usageHTTPClient = &http.Client{Timeout: 20 * time.Second,
	CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}

type usageRetryError struct {
	message string
	RetryAt time.Time
}

func (e *usageRetryError) Error() string { return e.message }

func usageResponseError(response *http.Response) error {
	message := fmt.Sprintf("Usage request returned HTTP %d", response.StatusCode)
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

// requestUsage sends a provider request; statusErrors maps provider-specific statuses.
func requestUsage(request *http.Request, provider string, statusErrors map[int]error) ([]byte, error) {
	request.Header.Set("Accept", "application/json")
	response, err := usageHTTPClient.Do(request)
	if err != nil {
		return nil, offlineError{"Can't reach " + provider + ". Check your internet connection."}
	}
	defer response.Body.Close()
	if err := statusErrors[response.StatusCode]; err != nil {
		return nil, err
	}
	if response.StatusCode != http.StatusOK {
		// Provider bodies can contain account details. Never forward them to logs.
		return nil, usageResponseError(response)
	}
	data, err := readBounded(response.Body, 1<<20)
	if err != nil {
		return nil, errors.New("Usage response is unreadable or too large")
	}
	return data, nil
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

func (s Status) providerError(provider string) string {
	if provider == "codex" {
		return s.CodexError
	}
	if provider == "claude" {
		return s.ClaudeError
	}
	return ""
}

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

func usageCommandTo(args []string, out, progress io.Writer) error {
	return runUsage(context.Background(), len(args) == 1, out, progress, usageDependencies{
		now: time.Now, config: initializedConfig, collect: collectProvider,
		progressTerminal: isatty.IsTerminal(os.Stderr.Fd()) || isatty.IsCygwinTerminal(os.Stderr.Fd()),
	})
}

func printJSONTo(out io.Writer, value any) error {
	encoder := json.NewEncoder(out)
	encoder.SetIndent("", "  ")
	return encoder.Encode(value)
}
