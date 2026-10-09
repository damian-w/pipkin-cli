//go:build windows

package pipkin

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"unsafe"

	"golang.org/x/sys/windows"
)

// Exercise the real Windows Desktop loader with data protected for this user.
// No test reads a user's Desktop profile or sends an actual HTTP request.
func claudeIdentityTestConfig(t *testing.T, account, token string, orgs ...string) claudeDesktopConfig {
	t.Helper()
	cache := make(map[string]json.RawMessage)
	entry, err := json.Marshal(struct {
		Token     string `json:"token"`
		ExpiresAt int64  `json:"expiresAt"`
	}{token, 4102444800000})
	if err != nil {
		t.Fatal(err)
	}
	for _, org := range orgs {
		cache[claudeOrgTestKey(account, org, "user:profile user:inference")] = entry
	}
	clear, err := json.Marshal(cache)
	if err != nil {
		t.Fatal(err)
	}
	input := windows.DataBlob{Size: uint32(len(clear)), Data: &clear[0]}
	var output windows.DataBlob
	if err := windows.CryptProtectData(&input, nil, nil, 0, nil, windows.CRYPTPROTECT_UI_FORBIDDEN, &output); err != nil {
		t.Fatal(err)
	}
	defer windows.LocalFree(windows.Handle(unsafe.Pointer(output.Data)))
	if output.Data == nil || output.Size == 0 {
		t.Fatal("DPAPI returned no synthetic ciphertext")
	}
	protected := unsafe.Slice(output.Data, int(output.Size))
	encoded, err := json.Marshal(base64.StdEncoding.EncodeToString(protected))
	if err != nil {
		t.Fatal(err)
	}
	return claudeDesktopConfig{AccountUUID: account, TokenCacheV2: encoded}
}

func writeClaudeIdentityTestConfig(t *testing.T, dir string, config claudeDesktopConfig) {
	t.Helper()
	encoded, err := json.Marshal(config)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "config.json"), encoded, 0600); err != nil {
		t.Fatal(err)
	}
}

func TestClaudeDesktopWindowsIdentityRevalidation(t *testing.T) {
	for _, c := range []struct {
		name            string
		mutate          func(*testing.T, string, claudeDesktopConfig)
		profileMismatch bool
		wantErr         error
	}{
		{name: "unchanged identity succeeds"},
		{
			name: "same organization token rotation succeeds",
			mutate: func(t *testing.T, dir string, _ claudeDesktopConfig) {
				writeClaudeIdentityTestConfig(t, dir, claudeIdentityTestConfig(t, claudeOrgTestAccount, "synthetic-only-rotated", claudeOrgTestFirst))
			},
		},
		{
			name: "account switch rejects captured usage",
			mutate: func(t *testing.T, dir string, _ claudeDesktopConfig) {
				writeClaudeIdentityTestConfig(t, dir, claudeIdentityTestConfig(t, claudeOrgTestForeign, "synthetic-only-other-account", claudeOrgTestFirst))
			},
			wantErr: errSignedOut,
		},
		{
			name: "newly ambiguous inventory rejects captured usage",
			mutate: func(t *testing.T, dir string, _ claudeDesktopConfig) {
				writeClaudeIdentityTestConfig(t, dir, claudeIdentityTestConfig(t, claudeOrgTestAccount, "synthetic-only-token", claudeOrgTestFirst, claudeOrgTestSecond))
			},
			wantErr: errClaudeOrgAmbiguous,
		},
		{
			name: "organization switch rejects captured usage",
			mutate: func(t *testing.T, dir string, _ claudeDesktopConfig) {
				writeClaudeIdentityTestConfig(t, dir, claudeIdentityTestConfig(t, claudeOrgTestAccount, "synthetic-only-other-org", claudeOrgTestSecond))
			},
			wantErr: errSignedOut,
		},
		{
			name: "sign out rejects captured usage",
			mutate: func(t *testing.T, dir string, _ claudeDesktopConfig) {
				writeClaudeIdentityTestConfig(t, dir, claudeDesktopConfig{AccountUUID: claudeOrgTestAccount})
			},
			wantErr: errSignedOut,
		},
		{
			name: "removed config rejects captured usage",
			mutate: func(t *testing.T, dir string, _ claudeDesktopConfig) {
				if err := os.Remove(filepath.Join(dir, "config.json")); err != nil {
					t.Fatal(err)
				}
			},
			wantErr: errSignedOut,
		},
		{
			name:            "profile account mismatch remains strict",
			profileMismatch: true,
			wantErr:         errSignedOut,
		},
	} {
		t.Run(c.name, func(t *testing.T) {
			dir := t.TempDir()
			config := claudeIdentityTestConfig(t, claudeOrgTestAccount, "synthetic-only-token", claudeOrgTestFirst)
			writeClaudeIdentityTestConfig(t, dir, config)
			credential, err := loadClaudeDesktopCredential(context.Background(), dir, config)
			if err != nil {
				t.Fatalf("synthetic DPAPI Desktop login did not load: %v", err)
			}
			if credential.source != "claude-desktop" || credential.desktopDir != dir || credential.identity != claudeOrgTestAccount+"|"+claudeOrgTestFirst {
				t.Fatal("synthetic Desktop identity was not retained for revalidation")
			}
			calls := 0
			stubUsageHTTP(t, func(req *http.Request) *http.Response {
				calls++
				if req.URL.Scheme != "https" || req.URL.Host != "api.anthropic.com" || req.Method != http.MethodGet || req.Header.Get("Authorization") != "Bearer synthetic-only-token" {
					t.Fatal("synthetic login was not confined to the mocked read-only endpoint")
				}
				body := `{"five_hour":{"utilization":10},"seven_day":{"utilization":50}}`
				if strings.HasSuffix(req.URL.Path, "/profile") {
					if c.mutate != nil {
						c.mutate(t, dir, config)
					}
					account := claudeOrgTestAccount
					if c.profileMismatch {
						account = claudeOrgTestForeign
					}
					body = `{"account":{"uuid":"` + account + `"},"organization":{"uuid":"` + claudeOrgTestFirst + `","organization_type":"claude_max"}}`
				} else if !strings.HasSuffix(req.URL.Path, "/usage") {
					t.Fatalf("unexpected mocked endpoint %q", req.URL.Path)
				}
				return testResponse(req, http.StatusOK, body)
			})
			reading, err := fetchClaudeCredentialUsage(context.Background(), credential, "synthetic-test-salt")
			if calls != 2 {
				t.Fatalf("expected mocked usage and identity verification requests, got %d", calls)
			}
			if c.wantErr != nil {
				if !errors.Is(err, c.wantErr) || reading != nil {
					t.Fatalf("changed identity published usage: reading=%v err=%v, want %v", reading, err, c.wantErr)
				}
				return
			}
			if err != nil || reading == nil || reading.Source != "claude-desktop" || reading.Plan != "max" || reading.Session == nil || reading.Weekly == nil {
				t.Fatalf("stable identity failed to publish usage: reading=%v err=%v", reading, err)
			}
			if strings.Contains(reading.Account, claudeOrgTestAccount) || strings.Contains(reading.Account, claudeOrgTestFirst) {
				t.Fatal("published usage contains a raw identity")
			}
		})
	}
}
