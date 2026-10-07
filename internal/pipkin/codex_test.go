package pipkin

import (
	"context"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestCodexUsage(t *testing.T) {
	const observed = 1790685542
	r, err := parseCodexUsage([]byte(`{
		"account_id":"private-account", "plan_type":"prolite",
		"rate_limit":{"primary_window":{"used_percent":37.55,"limit_window_seconds":604800,"reset_at":1791136117},"secondary_window":null},
		"credits":{"has_credits":true,"unlimited":false,"balance":"2403.0475820000"},
		"rate_limit_reset_credits":{"available_count":0}
	}`), "", "test-salt", observed)
	if err != nil {
		t.Fatal(err)
	}
	if r.Session != nil || !r.SessionNoCap || r.Weekly == nil || r.Weekly.Used != 376 || r.Weekly.Reset != 1791136117 || r.Weekly.Seconds != 604800 {
		t.Fatalf("weekly-only plan classified incorrectly: %+v", r)
	}
	if r.Credits == nil || r.Credits.Balance == nil || *r.Credits.Balance != 2403.047582 || r.Banked == nil || *r.Banked != 0 {
		t.Fatalf("credits and measured zero must be preserved: %+v", r)
	}
	encoded, _ := json.Marshal(r)
	if strings.Contains(string(encoded), "private-account") || r.Account == "codex" {
		t.Fatal("account identifier must be hashed")
	}

	r, err = parseCodexUsage([]byte(`{
		"rate_limit":{"primary_window":{"used_percent":0,"limit_window_seconds":18000,"reset_after_seconds":60},"secondary_window":{"used_percent":75.25,"limit_window_seconds":604800}},
		"credits":{"has_credits":false},
		"additional_rate_limits":[{"metered_feature":"codex_spark","rate_limit":{"primary_window":{"used_percent":20,"limit_window_seconds":604800}}}]
	}`), "", "", observed)
	if err != nil || r.Session == nil || r.Session.Used != 0 || r.Session.Reset != observed+60 || r.SessionNoCap || r.Weekly.Used != 753 || *r.Credits.Balance != 0 {
		t.Fatalf("session, reset fallback, or credits incorrect: %+v (%v)", r, err)
	}
	if r.Models["codex_spark_weekly"] == nil || r.Models["codex_spark_session"] != nil {
		t.Fatal("model windows also need duration classification")
	}

	for _, input := range []string{`{}`, `null`, `{"rate_limit":{"primary_window":{"used_percent":null}}}`} {
		if r, err := parseCodexUsage([]byte(input), "", "", observed); r != nil || err == nil {
			t.Fatal("missing usage must remain unavailable")
		}
	}
	for _, raw := range []string{`null`, `"NaN"`, `"+Inf"`, `-1`, `"invalid"`} {
		if codexCreditBalance(json.RawMessage(raw)) != nil {
			t.Fatalf("invalid credit balance accepted: %s", raw)
		}
	}
	for _, raw := range []string{`0`, `"0"`} {
		if balance := codexCreditBalance(json.RawMessage(raw)); balance == nil || *balance != 0 {
			t.Fatal("measured zero credits must survive")
		}
	}

	r, err = parseCodexUsage([]byte(`{"rate_limit":{"primary_window":{"used_percent":null,"limit_window_seconds":18000},"secondary_window":{"used_percent":30,"limit_window_seconds":604800}}}`), "", "", observed)
	if err != nil || r.SessionNoCap {
		t.Fatal("an unreported session percentage is not an unlimited session")
	}
	r, err = parseCodexUsage([]byte(`{"rate_limit":{"primary_window":{"used_percent":40,"limit_window_seconds":604800},"secondary_window":{"used_percent":5,"limit_window_seconds":3600}}}`), "", "", observed)
	if err != nil || r.Session == nil || r.Session.Seconds != 3600 || r.Weekly == nil || r.Weekly.Seconds != 604800 {
		t.Fatal("durations must override both window positions")
	}
	if r, err := parseCodexUsage([]byte(`{"rate_limit":{"primary_window":{"used_percent":40,"limit_window_seconds":999999999}}}`), "", "", observed); r != nil || err == nil {
		t.Fatal("invalid window durations must not enter the display protocol")
	}
}

func TestCodexAuth(t *testing.T) {
	claims := base64.RawURLEncoding.EncodeToString([]byte(`{"https://api.openai.com/auth":{"chatgpt_account_id":"account-from-claim"}}`))
	raw := []byte(`{"tokens":{"access_token":"access-secret","id_token":"header.` + claims + `.signature"}}`)
	for _, data := range [][]byte{raw, []byte(hex.EncodeToString(raw))} {
		auth, err := parseCodexAuth(data)
		if err != nil || auth.AccessToken != "access-secret" || auth.AccountID != "account-from-claim" {
			t.Fatal("credential JSON and hex exports must resolve the account claim")
		}
	}
	for _, data := range []string{`{"OPENAI_API_KEY":"api-secret"}`, `{"tokens":{"access_token":""}}`, `{"tokens":{"access_token":"secret\nheader"}}`, `invalid-secret`} {
		_, err := parseCodexAuth([]byte(data))
		if err == nil || strings.Contains(err.Error(), "secret") {
			t.Fatal("invalid credentials must fail without leaking their contents")
		}
	}
	if _, err := parseCodexAuth([]byte(`{"auth_mode":"apikey","tokens":{"access_token":"stale-secret"}}`)); !errors.Is(err, errSignedOut) {
		t.Fatal("an explicit alternate auth mode must not expose a stale subscription account")
	}
	dir := t.TempDir()
	t.Setenv("CODEX_HOME", dir)
	if homes := codexAuthHomes(); len(homes) != 1 || homes[0] != dir {
		t.Fatal("explicit CODEX_HOME must not fall back to another account")
	}
	if err := os.WriteFile(filepath.Join(dir, "config.toml"), []byte("cli_auth_credentials_store = 'keyring' # comment\n[profiles.example]\ncli_auth_credentials_store = 'file'\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if mode, err := codexAuthStoreMode(dir); err != nil || mode != "keyring" {
		t.Fatal("keyring preference must be read from top-level config")
	}
	if err := os.WriteFile(filepath.Join(dir, "config.toml"), []byte("[profiles.example]\ncli_auth_credentials_store = 'keyring'\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if mode, err := codexAuthStoreMode(dir); err != nil || mode != "file" {
		t.Fatal("profile fields must not override top-level auth storage")
	}
	if err := os.WriteFile(filepath.Join(dir, "auth.json"), raw, 0600); err != nil {
		t.Fatal(err)
	}
	auth, err := loadCodexAuth(context.Background())
	if err != nil || auth.Source != "codex_auth_file" {
		t.Fatal("desktop credential file discovery failed")
	}
	if err := os.WriteFile(filepath.Join(dir, "auth.json"), []byte(`{"OPENAI_API_KEY":"api-secret"}`), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := loadCodexAuth(context.Background()); !errors.Is(err, errSignedOut) {
		t.Fatal("selected API-key account must not fall back to another sign-in")
	}
	if err := os.WriteFile(filepath.Join(dir, "config.toml"), []byte("cli_auth_credentials_store = 'auto'\n[features]\nsecret_auth_storage = true\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := loadCodexAuth(context.Background()); err == nil || errors.Is(err, errSignedOut) {
		t.Fatal("unsupported encrypted backend must fail without selecting stale credentials")
	}
}

func TestCodexAuthStoreTOML(t *testing.T) {
	for _, config := range []string{
		"cli_auth_credentials_store = 'auto'\n[\"features\"]\nsecret_auth_storage = true\n",
		"cli_auth_credentials_store = 'keyring'\nfeatures = { secret_auth_storage = true }\n",
		"cli_auth_credentials_store = 'auto'\nfeatures.secret_auth_storage = true\n",
	} {
		dir := t.TempDir()
		if err := os.WriteFile(filepath.Join(dir, "config.toml"), []byte(config), 0600); err != nil {
			t.Fatal(err)
		}
		if _, err := codexAuthStoreMode(dir); err == nil || errors.Is(err, errSignedOut) {
			t.Fatal("encrypted storage must be rejected in every valid TOML form")
		}
	}
	for _, test := range []struct {
		config, mode string
		wantError    bool
	}{
		{"cli_auth_credentials_store = \"\\u006beyring\"\n", "keyring", false},
		{"instructions = '''\ncli_auth_credentials_store = 'ephemeral'\n[features]\nsecret_auth_storage = true\n'''\ncli_auth_credentials_store = 'auto'\n", "auto", false},
		{"[profiles.example]\ncli_auth_credentials_store = 'ephemeral'\n", "file", false},
		{"cli_auth_credentials_store = ''\n", "", true},
		{"cli_auth_credentials_store = 'private-unsupported-mode'\n", "", true},
		{"cli_auth_credentials_store = 'private-malformed\n", "", true},
	} {
		dir := t.TempDir()
		if err := os.WriteFile(filepath.Join(dir, "config.toml"), []byte(test.config), 0600); err != nil {
			t.Fatal(err)
		}
		mode, err := codexAuthStoreMode(dir)
		if (err != nil) != test.wantError || mode != test.mode {
			t.Fatalf("mode=%q, error=%v", mode, err)
		}
		if err != nil && strings.Contains(err.Error(), "private") {
			t.Fatal("configuration contents leaked into error")
		}
	}
}

func TestCodexMalformedWindowDoesNotImplyNoCap(t *testing.T) {
	for _, primary := range []string{
		`{"used_percent":50,"limit_window_seconds":999999999}`,
		`{"used_percent":null,"limit_window_seconds":604800}`,
		`{"used_percent":50,"limit_window_seconds":-1}`,
	} {
		input := `{"rate_limit":{"primary_window":` + primary + `,"secondary_window":{"used_percent":30,"limit_window_seconds":604800}}}`
		reading, err := parseCodexUsage([]byte(input), "", "", 1790685542)
		if err != nil || reading.Weekly == nil || reading.SessionNoCap {
			t.Fatalf("malformed session became no cap: %+v (%v)", reading, err)
		}
	}
}

type codexTestTransport func(*http.Request) (*http.Response, error)

func (fn codexTestTransport) RoundTrip(req *http.Request) (*http.Response, error) { return fn(req) }

func TestCodexRequestKeepsCredentialsPrivate(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("CODEX_HOME", dir)
	if err := os.WriteFile(filepath.Join(dir, "auth.json"), []byte(`{"tokens":{"access_token":"private-token","account_id":"private-account"}}`), 0600); err != nil {
		t.Fatal(err)
	}
	previous := usageHTTPClient
	t.Cleanup(func() { usageHTTPClient = previous })
	usageHTTPClient = &http.Client{Transport: codexTestTransport(func(req *http.Request) (*http.Response, error) {
		if req.URL.String() != codexUsageURL || req.Method != http.MethodGet || req.Header.Get("Authorization") != "Bearer private-token" || req.Header.Get("ChatGPT-Account-Id") != "private-account" {
			t.Fatal("credential request destination or headers incorrect")
		}
		return &http.Response{StatusCode: http.StatusUnauthorized, Body: io.NopCloser(strings.NewReader(`{"error":"private-token private-account"}`))}, nil
	})}
	_, err := fetchCodexUsage(context.Background(), "test-salt")
	if !errors.Is(err, errSignedOut) || strings.Contains(err.Error(), "private") {
		t.Fatal("expired sign-in must be identifiable without exposing provider body")
	}
	data, err := os.ReadFile(filepath.Join(dir, "auth.json"))
	if err != nil || !strings.Contains(string(data), "private-token") {
		t.Fatal("usage fetching must leave provider credentials untouched")
	}
}
