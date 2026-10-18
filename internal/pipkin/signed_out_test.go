package pipkin

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

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
	for _, provider := range []string{"Claude", "Codex"} {
		t.Run(provider, func(t *testing.T) {
			if provider == "Codex" {
				dir := t.TempDir()
				t.Setenv("CODEX_HOME", dir)
				if err := os.WriteFile(filepath.Join(dir, "auth.json"), []byte(`{"tokens":{"access_token":"synthetic-private-token","account_id":"synthetic-private-account"}}`), 0600); err != nil {
					t.Fatal(err)
				}
			}
			calls := 0
			stubUsageHTTP(t, func(req *http.Request) *http.Response {
				calls++
				return testResponse(req, http.StatusUnauthorized, `{"error":"synthetic-private-token synthetic-private-account"}`)
			})
			var err error
			if provider == "Claude" {
				_, err = requestClaudeOAuth(context.Background(), "synthetic-private-token", "profile")
			} else {
				_, err = fetchCodexUsage(context.Background(), "synthetic-salt")
			}
			want := "Sign-in expired. Open " + provider + " to sign in again."
			if calls != 1 || !errors.Is(err, errSignedOut) || err.Error() != want {
				t.Fatalf("unauthorized sign-in lost its clear action or classification: %v", err)
			}
		})
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
