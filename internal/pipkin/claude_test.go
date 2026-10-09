package pipkin

import (
	"bytes"
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/sha256"
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

func TestClaudeUsageNormalization(t *testing.T) {
	now := time.Date(2026, 9, 29, 12, 0, 0, 0, time.UTC)
	r, err := parseClaudeUsage([]byte(`{"five_hour":{"utilization":13.25,"resets_at":"2026-09-29T15:00:00.123Z"},"seven_day":{"utilization":50,"resets_at":null},"seven_day_sonnet":{"utilization":25,"resets_at":"2026-10-02T00:00:00Z"},"seven_day_opus":null,"limits":[{"kind":"weekly_scoped","percent":42,"scope":{"model":{"display_name":"Fable"}},"resets_at":"2026-10-01T00:00:00Z"}],"extra_usage":{"is_enabled":true,"used_credits":1250,"monthly_limit":5000},"cedar_ember":{"eligible":true,"grants":[{"resets_left":2,"ends_at":"2026-10-01T00:00:00Z"},{"resets_left":5,"ends_at":"2026-09-01T00:00:00Z"}]}}`), now)
	if err != nil {
		t.Fatal(err)
	}
	if r.Session == nil || r.Session.Used != 133 || r.Session.Reset != 1790694000 || r.Weekly.Used != 500 || r.Weekly.Reset != 0 {
		t.Fatal("window conversion failed")
	}
	if len(r.Models) != 2 || r.Models["sonnet"].Used != 250 || r.Models["fable"].Used != 420 {
		t.Fatal("model limits missing")
	}
	if *r.ExtraUsage.Used != 12.5 || *r.ExtraUsage.Limit != 50 || *r.ExtraUsage.Remaining != 37.5 {
		t.Fatal("extra usage cents were not converted")
	}
	if r.Banked == nil || *r.Banked != 2 {
		t.Fatal("expired reset grant counted")
	}
	if _, err := parseClaudeUsage([]byte(`{"five_hour":null,"seven_day":null}`), now); err == nil {
		t.Fatal("missing windows accepted")
	}
	for _, raw := range []string{`{"five_hour":{"utilization":0},"cedar_ember":{}}`, `{"five_hour":{"utilization":0},"cedar_ember":{"eligible":true}}`, `{"five_hour":{"utilization":0},"cedar_ember":{"eligible":true,"grants":[{}]}}`} {
		reading, err := parseClaudeUsage([]byte(raw), now)
		if err != nil || reading.Banked != nil {
			t.Fatal("unknown reset balance became zero")
		}
	}
}

func TestClaudeSessionNotStarted(t *testing.T) {
	now := time.Date(2026, 9, 29, 12, 0, 0, 0, time.UTC)
	for _, c := range []struct {
		name, raw  string
		notStarted bool
		missing    bool
		reset      int64
	}{
		{name: "idle", raw: `{"utilization":0,"resets_at":null}`, notStarted: true},
		{name: "missing reset", raw: `{"utilization":0}`},
		{name: "empty reset", raw: `{"utilization":0,"resets_at":""}`},
		{name: "invalid reset", raw: `{"utilization":0,"resets_at":"invalid"}`},
		{name: "out of range reset", raw: `{"utilization":0,"resets_at":"1970-01-01T00:00:00Z"}`},
		{name: "malformed reset", raw: `{"utilization":0,"resets_at":123}`, missing: true},
		{name: "missing utilization", raw: `{"resets_at":null}`, missing: true},
		{name: "missing session", raw: `null`, missing: true},
		{name: "nonzero usage", raw: `{"utilization":10,"resets_at":null}`},
		{name: "rounded usage", raw: `{"utilization":0.01,"resets_at":null}`},
		{name: "negative usage", raw: `{"utilization":-1,"resets_at":null}`},
		{name: "active zero usage", raw: `{"utilization":0,"resets_at":"2026-09-29T15:00:00Z"}`, reset: 1790694000},
		{name: "active", raw: `{"utilization":10,"resets_at":"2026-09-29T15:00:00Z"}`, reset: 1790694000},
	} {
		t.Run(c.name, func(t *testing.T) {
			r, err := parseClaudeUsage([]byte(`{"five_hour":`+c.raw+`,"seven_day":{"utilization":0,"resets_at":null}}`), now)
			if err != nil || (r.Session == nil) != c.missing {
				t.Fatalf("session parsing: reading=%+v err=%v", r, err)
			}
			if r.Weekly.NotStarted {
				t.Fatal("idle session state inferred for weekly allowance")
			}
			if c.missing {
				return
			}
			if r.Session.NotStarted != c.notStarted || r.Session.Reset != c.reset {
				t.Fatalf("session=%+v", r.Session)
			}
			fields := usageFields(r)
			raw, err := json.Marshal(r.Session)
			if err != nil || strings.Contains(string(raw), `"not_started":true`) != c.notStarted {
				t.Fatalf("JSON session=%s err=%v", raw, err)
			}
			if c.notStarted {
				if !strings.Contains(fields, "session=not_started session_id=current session_seconds=18000") || strings.Contains(fields, "session_used=") || strings.Contains(fields, "session_reset=") {
					t.Fatalf("idle packet=%s", fields)
				}
				if got := describeWindow("Session", r.Session, now.Unix()); got != "Session starts with your first message" {
					t.Fatalf("idle text=%s", got)
				}
			} else if !strings.Contains(fields, "session=metered") || strings.Contains(fields, "not_started") {
				t.Fatalf("metered packet=%s", fields)
			}
		})
	}
}

func TestClaudeActiveOrganizationCookie(t *testing.T) {
	dir := t.TempDir()
	db, err := sql.Open("sqlite", filepath.Join(dir, "Cookies"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err = db.Exec("CREATE TABLE cookies (host_key TEXT,name TEXT,value TEXT,encrypted_value BLOB,last_update_utc INTEGER)"); err != nil {
		t.Fatal(err)
	}
	if _, err = db.Exec("INSERT INTO cookies VALUES ('.claude.ai','lastActiveOrg','',?,1)", []byte("test ciphertext")); err != nil {
		t.Fatal(err)
	}
	org := "22222222-2222-2222-2222-222222222222"
	hash := sha256.Sum256([]byte(".claude.ai"))
	decrypt := func([]byte) ([]byte, error) { return append(hash[:], []byte(org)...), nil }
	if got, err := claudeActiveOrg(context.Background(), dir, decrypt); err != nil || got != org {
		t.Fatal("active organization cookie failed")
	}
	wrongHash := sha256.Sum256([]byte("untrusted.invalid"))
	decrypt = func([]byte) ([]byte, error) { return append(wrongHash[:], []byte(org)...), nil }
	if _, err := claudeActiveOrg(context.Background(), dir, decrypt); err == nil {
		t.Fatal("foreign-host cookie accepted")
	}
}

func TestClaudeDesktopSelection(t *testing.T) {
	account := "11111111-1111-1111-1111-111111111111"
	org := "22222222-2222-2222-2222-222222222222"
	key := "9d1c250a-e61b-44d9-88ed-5944d1962f5e:" + org + ":https://api.anthropic.com:user:profile user:inference"
	now := time.Date(2026, 9, 29, 12, 0, 0, 0, time.UTC)
	entry := json.RawMessage(`{"token":"synthetic-token","expiresAt":4102444800000}`)
	v1 := map[string]json.RawMessage{key: entry}
	v2 := map[string]json.RawMessage{"acct:" + account + "|" + key: json.RawMessage(`null`)}
	if _, err := selectClaudeDesktopToken(v2, v1, account, org, now); !errors.Is(err, errSignedOut) {
		t.Fatal("V2 deletion resurrected V1 login")
	}
	v2["acct:"+account+"|"+key] = entry
	if c, err := selectClaudeDesktopToken(v2, nil, account, org, now); err != nil || c.AccessToken == "" {
		t.Fatal("active scoped login missing")
	}
	if _, err := selectClaudeDesktopToken(v2, nil, "33333333-3333-3333-3333-333333333333", org, now); err == nil {
		t.Fatal("foreign account selected")
	}
	v2["acct:"+account+"|"+key] = json.RawMessage(`{"token":"expired","expiresAt":1}`)
	if _, err := selectClaudeDesktopToken(v2, v1, account, org, now); !errors.Is(err, errSignedOut) {
		t.Fatal("expired V2 login fell back to old V1 alias")
	}
}

func TestClaudeScopeSetPrecedence(t *testing.T) {
	account := "11111111-1111-1111-1111-111111111111"
	org := "22222222-2222-2222-2222-222222222222"
	prefix := "9d1c250a-e61b-44d9-88ed-5944d1962f5e:" + org + ":https://api.anthropic.com:"
	now := time.Date(2026, 9, 29, 12, 0, 0, 0, time.UTC)
	v1 := map[string]json.RawMessage{prefix + "user:profile user:inference": json.RawMessage(`{"token":"synthetic-old","expiresAt":4102444800000}`)}
	for _, scopes := range []string{"user:inference user:profile", "user:profile user:inference user:profile", "user:inference   user:profile"} {
		for _, value := range []string{`null`, `{"token":"synthetic-expired","expiresAt":1}`} {
			v2 := map[string]json.RawMessage{"acct:" + account + "|" + prefix + scopes: json.RawMessage(value)}
			if _, err := selectClaudeDesktopToken(v2, v1, account, org, now); !errors.Is(err, errSignedOut) {
				t.Fatal("V2 scope set resurrected an equivalent older login")
			}
		}
	}
	cache := map[string]json.RawMessage{
		prefix + "user:profile user:inference":                           json.RawMessage(`{"token":"synthetic-old","expiresAt":4102444800000}`),
		"acct:" + account + "|" + prefix + "user:inference user:profile": json.RawMessage(`null`),
	}
	if _, err := selectClaudeDesktopToken(cache, nil, account, org, now); !errors.Is(err, errSignedOut) {
		t.Fatal("scoped tombstone did not override an unscoped alias")
	}
}

func TestClaudeCBCValidation(t *testing.T) {
	key, err := claudeCBCKey([]byte("test-only-password"), 1003)
	if err != nil {
		t.Fatal(err)
	}
	block, _ := aes.NewCipher(key)
	want := []byte(`{"token":"test-only"}`)
	padding := aes.BlockSize - len(want)%aes.BlockSize
	ciphertext := append(append([]byte(nil), want...), bytes.Repeat([]byte{byte(padding)}, padding)...)
	cipher.NewCBCEncrypter(block, bytes.Repeat([]byte{' '}, aes.BlockSize)).CryptBlocks(ciphertext, ciphertext)
	encrypted := append([]byte("v10"), ciphertext...)
	if got, err := decryptClaudeCBC(encrypted, key); err != nil || !bytes.Equal(got, want) {
		t.Fatal("safeStorage round trip failed")
	}
	for _, invalid := range [][]byte{nil, []byte("v10"), []byte("v20unsupported"), append(encrypted, 0)} {
		if _, err := decryptClaudeCBC(invalid, key); err == nil {
			t.Fatal("malformed ciphertext accepted")
		}
	}
}

// Explicit opt-in because this reads the user's existing Desktop login and calls Anthropic.
// Only normalized usage is printed, never credentials, identity, or response bodies.
func TestClaudeDesktopLive(t *testing.T) {
	if os.Getenv("PIPKIN_LIVE_TEST_CLAUDE") != "1" {
		t.Skip("set PIPKIN_LIVE_TEST_CLAUDE=1 for a read-only Desktop probe")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	credential, err := loadClaudeCredentialFor(ctx, runtime.GOOS,
		func(context.Context) (claudeCredential, error) { return claudeCredential{}, errCredentialMissing },
		loadClaudeDesktopCredential)
	if err != nil {
		t.Fatal(err)
	}
	if credential.source != "claude-desktop" {
		t.Fatal("probe did not use the GUI login")
	}
	r, err := fetchClaudeCredentialUsage(ctx, credential, "live-test-only")
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("GUI source=%s session=%v weekly=%v model_windows=%d extra_reported=%t resets_reported=%t", r.Source, r.Session, r.Weekly, len(r.Models), r.ExtraUsage != nil, r.ReportsBanked)
}

func TestClaudeHTTPTrustBoundary(t *testing.T) {
	account := "11111111-1111-1111-1111-111111111111"
	org := "22222222-2222-2222-2222-222222222222"
	credential := claudeCredential{AccessToken: "test-only-token", source: "claude-desktop", identity: account + "|" + org}
	calls := 0
	stubUsageHTTP(t, func(req *http.Request) *http.Response {
		calls++
		if req.URL.Scheme != "https" || req.URL.Host != "api.anthropic.com" || req.Method != "GET" || req.Header.Get("Authorization") != "Bearer test-only-token" {
			t.Fatal("credential sent outside expected read-only endpoint")
		}
		response := testResponse(req, http.StatusFound, "secret response content")
		response.Header.Set("Location", "https://untrusted.invalid/")
		return response
	})
	if _, err := fetchClaudeCredentialUsage(context.Background(), credential, "salt"); err == nil || strings.Contains(err.Error(), "secret") || calls != 1 {
		t.Fatal("redirect or response error leaked data")
	}
	stubUsageHTTP(t, func(req *http.Request) *http.Response {
		body := `{"five_hour":{"utilization":10}}`
		if strings.HasSuffix(req.URL.Path, "profile") {
			body = `{"account":{"uuid":"33333333-3333-3333-3333-333333333333"},"organization":{"uuid":"` + org + `"}}`
		}
		return testResponse(req, http.StatusOK, body)
	})
	if _, err := fetchClaudeCredentialUsage(context.Background(), credential, "salt"); !errors.Is(err, errSignedOut) {
		t.Fatal("account mismatch was accepted")
	}
	stubUsageHTTP(t, func(req *http.Request) *http.Response {
		body := `{"five_hour":{"utilization":10}}`
		if strings.HasSuffix(req.URL.Path, "profile") {
			body = `{"account":{"uuid":"` + account + `"},"organization":{"uuid":"` + org + `","organization_type":"claude_max"}}`
		}
		return testResponse(req, http.StatusOK, body)
	})
	if r, err := fetchClaudeCredentialUsage(context.Background(), credential, "salt"); err != nil || r.Plan != "max" || strings.Contains(r.Account, account) {
		t.Fatal("verified account usage failed")
	}
}

func TestClaudeDesktopSignedIn(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, ".config"))
	t.Setenv("APPDATA", filepath.Join(home, "AppData", "Roaming"))
	t.Setenv("LOCALAPPDATA", filepath.Join(home, "AppData", "Local"))
	if claudeDesktopSignedIn() {
		t.Fatal("signed in without a Claude Desktop config")
	}
	dir := claudeDesktopDirs()[0]
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	config := filepath.Join(dir, "config.json")
	for _, raw := range []string{`{"locale":"en-AU"}`, `{"oauth:tokenCacheV2":null}`, `{"oauth:tokenCache":null,"oauth:tokenCacheV2":null}`} {
		if err := os.WriteFile(config, []byte(raw), 0o600); err != nil {
			t.Fatal(err)
		}
		if claudeDesktopSignedIn() {
			t.Fatal("signed in with a config that holds no sign-in")
		}
	}
	if err := os.WriteFile(config, []byte(`{"oauth:tokenCacheV2":"synthetic"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if !claudeDesktopSignedIn() {
		t.Fatal("saved Claude Desktop sign-in not found")
	}
}

func TestClaudeNullDesktopCacheAllowsCodeFallback(t *testing.T) {
	if runtime.GOOS == "darwin" {
		t.Skip("Code fallback would consult the native Keychain")
	}
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, ".config"))
	t.Setenv("APPDATA", filepath.Join(home, "AppData", "Roaming"))
	t.Setenv("LOCALAPPDATA", filepath.Join(home, "AppData", "Local"))
	t.Setenv("CLAUDE_CONFIG_DIR", filepath.Join(home, ".claude"))
	dir := claudeDesktopDirs()[0]
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "config.json"), []byte(`{"lastKnownAccountUuid":"11111111-1111-1111-1111-111111111111","oauth:tokenCacheV2":null}`), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(claudeConfig(), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(claudeConfig(), ".credentials.json"), []byte(`{"claudeAiOauth":{"accessToken":"synthetic-code","expiresAt":4102444800000}}`), 0600); err != nil {
		t.Fatal(err)
	}
	credential, err := loadClaudeCredential(context.Background())
	if err != nil || credential.source != "claude-auth-file" || credential.AccessToken != "synthetic-code" {
		t.Fatalf("null Desktop cache blocked Code fallback: %v", err)
	}
}
