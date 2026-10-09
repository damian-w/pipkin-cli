package pipkin

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
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
		if !errors.As(usageResponseError("Claude", response), &retry) || time.Until(retry.RetryAt) < 29*time.Minute {
			t.Fatal("long server cooldown must be respected")
		}
	}
}
