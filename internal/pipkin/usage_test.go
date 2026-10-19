package pipkin

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

// stubUsageHTTP answers provider requests in-process, keeping the production redirect policy.
func stubUsageHTTP(t *testing.T, respond func(*http.Request) *http.Response) {
	t.Helper()
	original := usageHTTPClient
	t.Cleanup(func() { usageHTTPClient = original })
	usageHTTPClient = &http.Client{CheckRedirect: original.CheckRedirect,
		Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) { return respond(req), nil })}
}

func testResponse(req *http.Request, status int, body string) *http.Response {
	return &http.Response{StatusCode: status, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(body)), Request: req}
}

func TestCollectionPreservesQuotaAndHandlesSignOut(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("CODEX_HOME", dir)
	if err := os.WriteFile(filepath.Join(dir, "auth.json"), []byte(`{"tokens":{"access_token":"test-token"}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, status := range []int{http.StatusOK, http.StatusUnauthorized} {
		stubUsageHTTP(t, func(req *http.Request) *http.Response {
			deadline, bounded := req.Context().Deadline()
			if !bounded || time.Until(deadline) > 45*time.Second || time.Until(deadline) <= 0 {
				t.Fatal("collection must have a live, bounded context")
			}
			return testResponse(req, status, `{"rate_limit":{"secondary_window":{"used_percent":25,"limit_window_seconds":604800}}}`)
		})
		result := collectProvider(context.Background(), "codex", "test-salt")
		if result.provider != "codex" || result.reading == nil {
			t.Fatalf("collection = %+v", result)
		}
		if status == http.StatusOK {
			if result.err != nil || result.reading.Weekly == nil || result.reading.Weekly.Used != 250 {
				t.Fatalf("collection lost provider quota: %+v", result)
			}
		} else if !errors.Is(result.err, errSignedOut) || result.reading.Account != "codex" || result.reading.Weekly != nil {
			t.Fatalf("signed-out collection = %+v", result)
		}
	}
}

func TestProviderClassificationAndErrorProjection(t *testing.T) {
	for _, item := range []struct {
		err   error
		state string
	}{{nil, "available"}, {fmt.Errorf("expired: %w", errSignedOut), "signed_out"}, {errors.New("store unavailable"), "unavailable"}} {
		if got := providerState(item.err); got != item.state {
			t.Fatalf("state = %q, want %q", got, item.state)
		}
		status := Status{CodexError: providerMessage(item.err), ClaudeError: providerMessage(item.err)}
		for _, provider := range providers {
			if got := status.providerError(provider); got != providerMessage(item.err) {
				t.Fatalf("%s error = %q", provider, got)
			}
		}
		if status.providerError("unknown") != "" {
			t.Fatal("unknown provider reported an error")
		}
	}
}

func TestProviderFailureKeepsObservationButSignOutClearsQuota(t *testing.T) {
	h, lines := testHelper()
	h.results = make(chan providerResult, 2)
	h.pending = map[string]bool{}
	now := h.now()
	h.due["claude"], h.due["codex"] = now.Add(time.Hour), now.Add(time.Hour)
	h.readings["codex"] = &Reading{Account: "c123", Observed: testNow - 60, Weekly: &Window{Used: 200}}
	h.results <- providerResult{provider: "codex", err: errors.New("network unavailable")}
	h.pollProviders(context.Background(), now)
	if h.readings["codex"].Observed != testNow-60 || h.readings["codex"].Weekly.Used != 200 || h.apps["codex"] != "unavailable" {
		t.Fatal("transient failure must retain the last observed quota")
	}
	h.results <- providerResult{provider: "codex", err: errSignedOut}
	h.pollProviders(context.Background(), now)
	h.flush()
	reading := h.readings["codex"]
	if reading == nil || reading.Account != "codex" || reading.Observed != 0 || reading.Weekly != nil || h.apps["codex"] != "signed_out" {
		t.Fatal("sign-out must clear quota and retain the provider placeholder")
	}
	if len(*lines) != 1 || !strings.Contains((*lines)[0], "state=signed_out") {
		t.Fatal("a signed-out provider must not resend its old quota to the display")
	}
	if !h.due["codex"].Equal(now.Add(15 * time.Minute)) {
		t.Fatal("provider errors must back off")
	}
}

func TestUsageClientRefusesCredentialRedirect(t *testing.T) {
	redirected := false
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/other" {
			redirected = true
			return
		}
		http.Redirect(w, r, "/other", http.StatusFound)
	}))
	defer server.Close()
	req, _ := http.NewRequest(http.MethodGet, server.URL, nil)
	req.Header.Set("Authorization", "Bearer test-secret")
	response, err := usageHTTPClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if response.StatusCode != http.StatusFound || redirected {
		t.Fatal("usage credentials must never follow redirects")
	}
}

func TestStableIdentityAndServerBackoff(t *testing.T) {
	isolate(t)
	first, err := initializedConfig()
	if err != nil {
		t.Fatal(err)
	}
	second, err := initializedConfig()
	if err != nil || first.Salt == "" || second.Salt != first.Salt {
		t.Fatal("usage and daemon need the same installation salt")
	}
	if err := os.WriteFile(configPath(), []byte("{broken"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := initializedConfig(); err == nil {
		t.Fatal("corrupt config must not be overwritten")
	}
	for _, header := range []string{"1800", time.Now().Add(time.Hour).UTC().Format(http.TimeFormat)} {
		response := &http.Response{StatusCode: 429, Header: http.Header{"Retry-After": []string{header}}}
		var retry *usageRetryError
		if !errors.As(usageResponseError(response), &retry) || time.Until(retry.RetryAt) < 29*time.Minute {
			t.Fatal("long server cooldown must be respected")
		}
	}
}

func TestSignedOutErrorPreservesClassificationWithoutSentinelText(t *testing.T) {
	err := signedOutError("Sign-in expired. Open Claude to sign in again.")
	if err.Error() != "Sign-in expired. Open Claude to sign in again." || !errors.Is(fmt.Errorf("context: %w", err), errSignedOut) ||
		errors.Is(err, errCredentialLocked) || providerState(err) != "signed_out" {
		t.Fatal("typed sign-out lost its message or classification")
	}
}

func TestMissingSavedSignInIsCalm(t *testing.T) {
	_, err := parseClaudeCodeCredential([]byte(`{"private":"synthetic-private-value"}`), "synthetic")
	if !errors.Is(err, errSignedOut) || err.Error() != "not signed in" {
		t.Fatalf("missing Code sign-in was not a calm state: %v", err)
	}
	for _, configured := range []bool{false, true} {
		dir := t.TempDir()
		t.Setenv("CODEX_HOME", dir)
		if configured {
			if err := os.WriteFile(filepath.Join(dir, "config.toml"), []byte("cli_auth_credentials_store = 'file'\n"), 0600); err != nil {
				t.Fatal(err)
			}
		}
		_, err := loadCodexAuth(context.Background())
		if !errors.Is(err, errSignedOut) || err.Error() != "not signed in" {
			t.Fatalf("missing Codex sign-in was not a calm state: %v", err)
		}
	}
}

func TestExpiredAndUnsupportedSignInsRemainActionable(t *testing.T) {
	_, codeErr := parseClaudeCodeCredential([]byte(`{"claudeAiOauth":{"accessToken":"synthetic-private-token","expiresAt":1}}`), "synthetic")
	cache := map[string]json.RawMessage{
		claudeOrgTestKey(claudeOrgTestAccount, claudeOrgTestFirst, "user:profile"): json.RawMessage(`{"token":"synthetic-private-token","expiresAt":1}`),
	}
	_, desktopErr := selectClaudeDesktopToken(cache, nil, claudeOrgTestAccount, claudeOrgTestFirst, time.Unix(1800000000, 0))
	_, subscriptionErr := parseCodexAuth([]byte(`{"auth_mode":"apikey","OPENAI_API_KEY":"synthetic-private-key"}`))
	dir := t.TempDir()
	t.Setenv("CODEX_HOME", dir)
	if err := os.WriteFile(filepath.Join(dir, "config.toml"), []byte("cli_auth_credentials_store = 'ephemeral'\n"), 0600); err != nil {
		t.Fatal(err)
	}
	_, ephemeralErr := loadCodexAuth(context.Background())
	for _, c := range []struct {
		err  error
		step string
	}{{codeErr, "Open Claude Code"}, {desktopErr, "Open Claude"}, {subscriptionErr, "subscription account"}, {ephemeralErr, "saved Codex sign-in"}} {
		if !errors.Is(c.err, errSignedOut) || !strings.Contains(c.err.Error(), c.step) || c.err.Error() == "not signed in" {
			t.Fatalf("non-ordinary sign-out lost its next step: %v", c.err)
		}
		for _, private := range []string{"synthetic-private", claudeOrgTestAccount, claudeOrgTestFirst, errSignedOut.Error()} {
			if strings.Contains(c.err.Error(), private) {
				t.Fatal("sign-out exposed source data or appended sentinel text")
			}
		}
	}
}

func TestUnauthorizedSignInMessagesKeepProviderResponsesPrivate(t *testing.T) {
	calls := 0
	stubUsageHTTP(t, func(req *http.Request) *http.Response {
		calls++
		return testResponse(req, http.StatusUnauthorized, `{"error":"synthetic-private-token synthetic-private-account"}`)
	})
	_, err := requestClaudeOAuth(context.Background(), "synthetic-private-token", "profile")
	if calls != 1 || !errors.Is(err, errSignedOut) || err.Error() != "Sign-in expired. Open Claude to sign in again." {
		t.Fatalf("unauthorized sign-in lost its clear action or classification: %v", err)
	}
}

func TestChangedClaudeSignInKeepsActionWithoutPrivateIdentity(t *testing.T) {
	dir := t.TempDir()
	err := validateClaudeDesktopIdentity(context.Background(), claudeCredential{desktopDir: dir})
	if !errors.Is(err, errSignedOut) || !strings.Contains(err.Error(), "Try again") {
		t.Fatalf("changed sign-in was not actionable: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "config.json"), []byte(`{"lastKnownAccountUuid":"aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa","oauth:tokenCacheV2":"synthetic-private-cache"}`), 0600); err != nil {
		t.Fatal(err)
	}
	err = validateClaudeDesktopIdentity(context.Background(), claudeCredential{desktopDir: dir, identity: "synthetic-private-account|synthetic-private-org"})
	if !errors.Is(err, errSignedOut) || !strings.Contains(err.Error(), "Active account changed") || strings.Contains(err.Error(), "synthetic-private") {
		t.Fatalf("changed account was not safely actionable: %v", err)
	}
}

func TestUsageRunPreservesJSONAndSeparatesProgress(t *testing.T) {
	now := time.Unix(testNow, 0)
	reading := &Reading{Account: "invented-account", Source: "invented-source", Observed: testNow,
		Plan: "sample_plan", SessionNoCap: true, Weekly: &Window{Used: 250, Reset: testNow + 86400},
		Models: map[string]*Window{"sample": nil}, ReportsBanked: true, Banked: new(int64(0))}
	for _, c := range []struct{ json, terminal bool }{{true, true}, {true, false}, {false, true}, {false, false}} {
		var out, progress bytes.Buffer
		var mu sync.Mutex
		calls := map[string]int{}
		deps := usageDependencies{now: func() time.Time { return now }, progressTerminal: c.terminal,
			config: func() (Config, error) { return Config{Salt: "synthetic-salt"}, nil },
			collect: func(ctx context.Context, provider, salt string) providerResult {
				mu.Lock()
				calls[provider]++
				mu.Unlock()
				if salt != "synthetic-salt" || ctx.Err() != nil {
					return providerResult{provider: provider, err: errors.New("wrong injected collection inputs")}
				}
				if provider == "codex" {
					return providerResult{provider: provider, reading: reading}
				}
				return providerResult{provider: provider, reading: &Reading{Account: provider}, err: signedOutError("not signed in")}
			}}
		if err := runUsage(context.Background(), c.json, &out, &progress, deps); err != nil {
			t.Fatal(err)
		}
		if calls["codex"] != 1 || calls["claude"] != 1 || len(calls) != 2 {
			t.Fatalf("collection changed: %v", calls)
		}
		if c.json {
			expected := Status{SchemaVersion: 1, States: map[string]string{"codex": "available", "claude": "signed_out"}, Version: version,
				Readings: map[string]*Reading{"codex": reading, "claude": {Account: "claude"}}, Updated: testNow, ClaudeError: "not signed in"}
			want, err := json.MarshalIndent(expected, "", "  ")
			if err != nil || out.String() != string(want)+"\n" {
				t.Fatalf("JSON contract changed: %v\n%s", err, out.String())
			}
			if progress.Len() != 0 {
				t.Fatal("JSON mode printed progress")
			}
		} else {
			for _, value := range []string{"Codex (sample_plan)", "No session cap", "Banked resets", "Not signed in"} {
				if !strings.Contains(out.String(), value) {
					t.Fatalf("missing %q in %s", value, out.String())
				}
			}
			if strings.Contains(out.String(), "Checking") || (progress.Len() != 0) != c.terminal {
				t.Fatal("progress reached stdout or a redirected stream")
			}
		}
	}
}

func TestUsageRunFailuresDoNotChangeCollectionOrHideWriterErrors(t *testing.T) {
	want := errors.New("fixture configuration failure")
	var collections atomic.Int32
	deps := usageDependencies{now: time.Now, config: func() (Config, error) { return Config{}, want },
		collect: func(_ context.Context, provider, _ string) providerResult {
			collections.Add(1)
			return providerResult{provider: provider, reading: &Reading{Account: provider}}
		}}
	if err := runUsage(context.Background(), false, io.Discard, io.Discard, deps); !errors.Is(err, want) || collections.Load() != 0 {
		t.Fatal("configuration failure started collection or lost its error")
	}
	deps.config = func() (Config, error) { return Config{}, nil }
	deps.progressTerminal = true
	if err := runUsage(context.Background(), false, io.Discard, &outputTestWriter{err: io.ErrClosedPipe}, deps); !errors.Is(err, io.ErrClosedPipe) || collections.Load() != 0 {
		t.Fatal("progress failure was hidden or started collection")
	}
	for _, jsonOutput := range []bool{true, false} {
		if err := runUsage(context.Background(), jsonOutput, &outputTestWriter{err: io.ErrClosedPipe}, io.Discard, deps); !errors.Is(err, io.ErrClosedPipe) {
			t.Fatalf("output failure was hidden: %v", err)
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	before := collections.Load()
	if err := runUsage(ctx, true, io.Discard, io.Discard, deps); !errors.Is(err, context.Canceled) || collections.Load() != before {
		t.Fatal("cancelled command started collection")
	}
}
