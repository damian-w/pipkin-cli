package pipkin

import (
	"context"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestClaudeCredentialPreference(t *testing.T) {
	const account = "aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa"
	const org = "bbbbbbbb-bbbb-bbbb-bbbb-bbbbbbbbbbbb"
	const codeJSON = `{"claudeAiOauth":{"accessToken":"synthetic-code","expiresAt":4102444800000,"scopes":["user:profile"]}}`
	const profile = `{"account":{"uuid":"` + account + `"},"organization":{"uuid":"` + org + `","organization_type":"claude_max"}}`
	for _, c := range []struct {
		wantRetry                                     bool
		name, goos, desktopAccount, codeJSON, profile string
		status                                        int
		codeLocked, desktopSignedOut, wantCode        bool
	}{
		{name: "matching account", wantCode: true},
		{name: "case insensitive account", desktopAccount: strings.ToUpper(account), wantCode: true},
		{name: "different account", desktopAccount: "cccccccc-cccc-cccc-cccc-cccccccccccc"},
		{name: "unknown Desktop account", desktopAccount: "invalid"},
		{name: "inaccessible Code login", codeLocked: true},
		{name: "expired Code login", codeJSON: `{"claudeAiOauth":{"accessToken":"expired","expiresAt":1}}`},
		{name: "inference only Code login", codeJSON: `{"claudeAiOauth":{"accessToken":"limited","scopes":["user:inference"]}}`},
		{name: "revoked Code login", status: http.StatusUnauthorized},
		{name: "Code profile access denied", status: http.StatusForbidden},
		{name: "rate limit with unavailable Desktop", status: http.StatusTooManyRequests, wantRetry: true},
		{name: "unreadable profile", profile: `invalid`},
		{name: "missing profile organization", profile: `{"account":{"uuid":"` + account + `"}}`},
		{name: "no Desktop login", desktopSignedOut: true, wantCode: true},
		{name: "Windows keeps Desktop first", goos: "windows"},
		{name: "Linux keeps Desktop first", goos: "linux"},
	} {
		t.Run(c.name, func(t *testing.T) {
			home := t.TempDir()
			t.Setenv("HOME", home)
			t.Setenv("USERPROFILE", home)
			t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, ".config"))
			t.Setenv("APPDATA", filepath.Join(home, "AppData", "Roaming"))
			t.Setenv("LOCALAPPDATA", filepath.Join(home, "AppData", "Local"))
			dir := claudeDesktopDirs()[0]
			if err := os.MkdirAll(dir, 0700); err != nil {
				t.Fatal(err)
			}
			if c.desktopAccount == "" {
				c.desktopAccount = account
			}
			config := `{"lastKnownAccountUuid":"` + c.desktopAccount + `","oauth:tokenCacheV2":"synthetic-encrypted-cache"}`
			if c.desktopSignedOut {
				config = `{"oauth:tokenCacheV2":null}`
			}
			if err := os.WriteFile(filepath.Join(dir, "config.json"), []byte(config), 0600); err != nil {
				t.Fatal(err)
			}
			original := usageHTTPClient
			t.Cleanup(func() { usageHTTPClient = original })
			profileCalls, usageCalls := 0, 0
			usageHTTPClient = &http.Client{Transport: claudeTestTransport(func(req *http.Request) (*http.Response, error) {
				if req.URL.Path == "/api/oauth/usage" {
					usageCalls++
					return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(`{"five_hour":{"utilization":10}}`)), Header: make(http.Header), Request: req}, nil
				}
				profileCalls++
				if req.URL.String() != "https://api.anthropic.com/api/oauth/profile" || req.Header.Get("Authorization") != "Bearer synthetic-code" {
					t.Fatal("unexpected credential verification request")
				}
				body, status := c.profile, c.status
				if body == "" {
					body = profile
				}
				if status == 0 {
					status = http.StatusOK
				}
				return &http.Response{StatusCode: status, Body: io.NopCloser(strings.NewReader(body)), Header: make(http.Header), Request: req}, nil
			})}
			codeCalls, desktopCalls := 0, 0
			loadCode := func(context.Context) (claudeCredential, error) {
				codeCalls++
				if c.codeLocked {
					return claudeCredential{}, errCredentialLocked
				}
				raw := c.codeJSON
				if raw == "" {
					raw = codeJSON
				}
				return parseClaudeCodeCredential([]byte(raw), "claude-keychain")
			}
			loadDesktop := func(context.Context, string, claudeDesktopConfig) (claudeCredential, error) {
				desktopCalls++
				if c.wantRetry {
					return claudeCredential{}, errCredentialLocked
				}
				return claudeCredential{AccessToken: "synthetic-desktop", source: "claude-desktop"}, nil
			}
			goos := c.goos
			if goos == "" {
				goos = "darwin"
			}
			credential, err := loadClaudeCredentialFor(context.Background(), goos, loadCode, loadDesktop)
			if c.wantRetry {
				var retry *usageRetryError
				if !errors.As(err, &retry) || desktopCalls != 1 {
					t.Fatal("provider backoff lost when Desktop was unavailable")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if c.wantCode {
				if credential.source != "claude-keychain" || desktopCalls != 0 || codeCalls != 1 {
					t.Fatal("usable Code login did not avoid Desktop access")
				}
				if !c.desktopSignedOut && credential.identity != account+"|"+org {
					t.Fatal("Code login lost its verified account and organization")
				}
				if !c.desktopSignedOut {
					reading, err := fetchClaudeCredentialUsage(context.Background(), credential, "test-salt")
					if err != nil || reading.Source != "claude-keychain" || reading.Plan != "max" || profileCalls != 1 || usageCalls != 1 {
						t.Fatalf("verified Code usage did not reuse its profile: %v", err)
					}
				}
			} else if credential.source != "claude-desktop" || desktopCalls != 1 {
				t.Fatal("Desktop fallback was not used")
			}
			if goos != "darwin" && (codeCalls != 0 || profileCalls != 0) {
				t.Fatal("Code preference changed another platform")
			}
			if c.desktopSignedOut && profileCalls != 0 {
				t.Fatal("unneeded Desktop account verification")
			}
		})
	}
}

func TestClaudeCredentialPreferenceCanceled(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := loadClaudeCredentialFor(ctx, "darwin", nil, nil); !errors.Is(err, context.Canceled) {
		t.Fatal("canceled credential selection continued")
	}
}
