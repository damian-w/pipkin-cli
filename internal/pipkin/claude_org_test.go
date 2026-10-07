package pipkin

import (
	"context"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const (
	claudeOrgTestAccount = "11111111-1111-1111-1111-111111111111"
	claudeOrgTestForeign = "33333333-3333-3333-3333-333333333333"
	claudeOrgTestFirst   = "a2222222-2222-2222-2222-222222222222"
	claudeOrgTestSecond  = "b4444444-4444-4444-4444-444444444444"
	claudeOrgTestClient  = "9d1c250a-e61b-44d9-88ed-5944d1962f5e"
)

func claudeOrgTestKey(account, org, scopes string) string {
	key := claudeOrgTestClient + ":" + org + ":https://api.anthropic.com:" + scopes
	if account != "" {
		key = "acct:" + account + "|" + key
	}
	return key
}

func claudeOrgTestEntry() json.RawMessage {
	return json.RawMessage(`{"token":"synthetic-only-token","expiresAt":4102444800000}`)
}

func claudeOrgTestDecrypt(raw []byte) ([]byte, error) { return raw, nil }

func TestClaudeDesktopOrgCacheFallback(t *testing.T) {
	for _, c := range []struct {
		name    string
		v2      map[string]json.RawMessage
		v1      map[string]json.RawMessage
		want    string
		wantErr error
	}{
		{
			name: "legacy singleton",
			v1:   map[string]json.RawMessage{claudeOrgTestKey("", claudeOrgTestFirst, "user:profile user:inference"): claudeOrgTestEntry()},
			want: claudeOrgTestFirst,
		},
		{
			name: "scoped singleton",
			v2:   map[string]json.RawMessage{claudeOrgTestKey(claudeOrgTestAccount, claudeOrgTestFirst, "user:profile user:inference"): claudeOrgTestEntry()},
			want: claudeOrgTestFirst,
		},
		{
			name: "aliases normalize organization",
			v2:   map[string]json.RawMessage{claudeOrgTestKey(claudeOrgTestAccount, strings.ToUpper(claudeOrgTestFirst), "user:inference user:profile user:profile"): claudeOrgTestEntry()},
			v1:   map[string]json.RawMessage{claudeOrgTestKey("", claudeOrgTestFirst, "user:profile user:inference"): claudeOrgTestEntry()},
			want: claudeOrgTestFirst,
		},
		{
			name: "foreign account ignored",
			v2: map[string]json.RawMessage{
				claudeOrgTestKey(claudeOrgTestAccount, claudeOrgTestFirst, "user:profile"):  claudeOrgTestEntry(),
				claudeOrgTestKey(claudeOrgTestForeign, claudeOrgTestSecond, "user:profile"): claudeOrgTestEntry(),
			},
			want: claudeOrgTestFirst,
		},
		{
			name: "multiple organizations",
			v2: map[string]json.RawMessage{
				claudeOrgTestKey(claudeOrgTestAccount, claudeOrgTestFirst, "user:profile"):  claudeOrgTestEntry(),
				claudeOrgTestKey(claudeOrgTestAccount, claudeOrgTestSecond, "user:profile"): claudeOrgTestEntry(),
			},
			wantErr: errClaudeOrgAmbiguous,
		},
		{
			name:    "organizations across cache versions",
			v2:      map[string]json.RawMessage{claudeOrgTestKey(claudeOrgTestAccount, claudeOrgTestFirst, "user:profile"): claudeOrgTestEntry()},
			v1:      map[string]json.RawMessage{claudeOrgTestKey("", claudeOrgTestSecond, "user:profile"): claudeOrgTestEntry()},
			wantErr: errClaudeOrgAmbiguous,
		},
		{
			name:    "V2 tombstone still counts an organization",
			v2:      map[string]json.RawMessage{claudeOrgTestKey(claudeOrgTestAccount, claudeOrgTestFirst, "user:profile"): json.RawMessage(`null`)},
			v1:      map[string]json.RawMessage{claudeOrgTestKey("", claudeOrgTestSecond, "user:profile"): claudeOrgTestEntry()},
			wantErr: errClaudeOrgAmbiguous,
		},
		{
			name:    "expired token still counts an organization",
			v2:      map[string]json.RawMessage{claudeOrgTestKey(claudeOrgTestAccount, claudeOrgTestFirst, "user:profile"): json.RawMessage(`{"token":"synthetic-expired","expiresAt":1}`)},
			v1:      map[string]json.RawMessage{claudeOrgTestKey("", claudeOrgTestSecond, "user:profile"): claudeOrgTestEntry()},
			wantErr: errClaudeOrgAmbiguous,
		},
		{
			name:    "nonprofile cache key still counts an organization",
			v2:      map[string]json.RawMessage{claudeOrgTestKey(claudeOrgTestAccount, claudeOrgTestFirst, "user:inference"): claudeOrgTestEntry()},
			v1:      map[string]json.RawMessage{claudeOrgTestKey("", claudeOrgTestSecond, "user:profile"): claudeOrgTestEntry()},
			wantErr: errClaudeOrgAmbiguous,
		},
		{
			name:    "malformed entry still counts a parseable organization",
			v2:      map[string]json.RawMessage{claudeOrgTestKey(claudeOrgTestAccount, claudeOrgTestFirst, "user:profile"): json.RawMessage(`{"unexpected":"synthetic"}`)},
			v1:      map[string]json.RawMessage{claudeOrgTestKey("", claudeOrgTestSecond, "user:profile"): claudeOrgTestEntry()},
			wantErr: errClaudeOrgAmbiguous,
		},
		{
			name:    "foreign account only",
			v2:      map[string]json.RawMessage{claudeOrgTestKey(claudeOrgTestForeign, claudeOrgTestFirst, "user:profile"): claudeOrgTestEntry()},
			wantErr: errClaudeOrgMissing,
		},
		{
			name:    "malformed keys only",
			v2:      map[string]json.RawMessage{"unparseable": claudeOrgTestEntry(), claudeOrgTestKey("", "invalid-org", "user:profile"): claudeOrgTestEntry()},
			wantErr: errClaudeOrgMissing,
		},
		{name: "empty caches", wantErr: errClaudeOrgMissing},
	} {
		t.Run(c.name, func(t *testing.T) {
			got, err := resolveClaudeDesktopOrg(context.Background(), t.TempDir(), claudeOrgTestDecrypt, c.v2, c.v1, claudeOrgTestAccount, true)
			if c.wantErr != nil {
				if !errors.Is(err, c.wantErr) || got != "" {
					t.Fatalf("org=%q err=%v, want %v", got, err, c.wantErr)
				}
				if strings.Contains(err.Error(), claudeOrgTestAccount) || strings.Contains(err.Error(), claudeOrgTestFirst) || strings.Contains(err.Error(), claudeOrgTestSecond) {
					t.Fatal("organization resolution error leaked an account or organization identifier")
				}
				return
			}
			if err != nil || got != c.want {
				t.Fatalf("org=%q err=%v, want %q", got, err, c.want)
			}
		})
	}
}

func TestClaudeDesktopCacheDecodeRejectsInvalidCache(t *testing.T) {
	envelope := func(clear string) json.RawMessage {
		raw, err := json.Marshal(base64.StdEncoding.EncodeToString([]byte(clear)))
		if err != nil {
			t.Fatal(err)
		}
		return raw
	}
	validCache, err := json.Marshal(map[string]json.RawMessage{
		claudeOrgTestKey(claudeOrgTestAccount, claudeOrgTestFirst, "user:profile"): claudeOrgTestEntry(),
	})
	if err != nil {
		t.Fatal(err)
	}
	valid := envelope(string(validCache))
	decryptErr := errors.New("synthetic cache decryption failure")
	decrypt := func(raw []byte) ([]byte, error) {
		if string(raw) == "synthetic-only-ciphertext" {
			return nil, decryptErr
		}
		return raw, nil
	}
	for _, c := range []struct {
		name    string
		raw     json.RawMessage
		wantErr error
	}{
		{name: "malformed envelope", raw: json.RawMessage(`"unterminated`)},
		{name: "object envelope", raw: json.RawMessage(`{"unexpected":"synthetic"}`)},
		{name: "invalid base64", raw: json.RawMessage(`"synthetic-private-invalid-%%"`)},
		{name: "decryption failure", raw: envelope("synthetic-only-ciphertext"), wantErr: decryptErr},
		{name: "malformed cache JSON", raw: envelope(`{"unterminated":`)},
		{name: "array cache", raw: envelope(`[]`)},
		{name: "string cache", raw: envelope(`"synthetic-private-cache"`)},
		{name: "number cache", raw: envelope(`123`)},
		{name: "boolean cache", raw: envelope(`true`)},
	} {
		for _, version := range []string{"V2", "V1"} {
			t.Run(c.name+"/"+version, func(t *testing.T) {
				config := claudeDesktopConfig{TokenCacheV2: valid, TokenCache: c.raw}
				if version == "V2" {
					config.TokenCacheV2, config.TokenCache = c.raw, valid
				}
				_, err := decodeClaudeDesktopCaches(config, decrypt)
				if err == nil {
					t.Fatal("invalid cache was ignored alongside a valid cache")
				}
				if c.wantErr != nil {
					if !errors.Is(err, c.wantErr) {
						t.Fatalf("err=%v, want %v", err, c.wantErr)
					}
				} else if err.Error() != "Claude Desktop token cache is invalid" {
					t.Fatalf("cache error was not sanitized: %v", err)
				}
			})
		}
	}
}

func TestClaudeDesktopOrgFallbackDisabled(t *testing.T) {
	cache := map[string]json.RawMessage{claudeOrgTestKey(claudeOrgTestAccount, claudeOrgTestFirst, "user:profile"): claudeOrgTestEntry()}
	if org, err := resolveClaudeDesktopOrg(context.Background(), t.TempDir(), claudeOrgTestDecrypt, cache, nil, claudeOrgTestAccount, false); !errors.Is(err, errClaudeOrgMissing) || org != "" {
		t.Fatalf("disabled fallback used token cache: org=%q err=%v", org, err)
	}
}

type claudeOrgTestCookie struct {
	host, value string
	encrypted   []byte
	updated     int
}

func writeClaudeOrgTestCookies(t *testing.T, path string, cookies ...claudeOrgTestCookie) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec("CREATE TABLE cookies (host_key TEXT,name TEXT,value TEXT,encrypted_value BLOB,last_update_utc INTEGER)"); err != nil {
		t.Fatal(err)
	}
	for _, cookie := range cookies {
		if _, err := db.Exec("INSERT INTO cookies VALUES (?, 'lastActiveOrg', ?, ?, ?)", cookie.host, cookie.value, cookie.encrypted, cookie.updated); err != nil {
			t.Fatal(err)
		}
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestClaudeDesktopOrgCookieWins(t *testing.T) {
	for _, relative := range []string{"Cookies", filepath.Join("Network", "Cookies")} {
		t.Run(relative, func(t *testing.T) {
			dir := t.TempDir()
			writeClaudeOrgTestCookies(t, filepath.Join(dir, relative), claudeOrgTestCookie{host: ".claude.ai", value: strings.ToUpper(claudeOrgTestSecond), updated: 1})
			cache := map[string]json.RawMessage{
				claudeOrgTestKey(claudeOrgTestAccount, claudeOrgTestFirst, "user:profile"):  claudeOrgTestEntry(),
				claudeOrgTestKey(claudeOrgTestAccount, claudeOrgTestSecond, "user:profile"): claudeOrgTestEntry(),
			}
			got, err := resolveClaudeDesktopOrg(context.Background(), dir, claudeOrgTestDecrypt, cache, nil, claudeOrgTestAccount, true)
			if err != nil || got != claudeOrgTestSecond {
				t.Fatalf("readable cookie lost precedence: org=%q err=%v", got, err)
			}
		})
	}
}

func TestClaudeDesktopOrgStrictCookieFailures(t *testing.T) {
	cache := map[string]json.RawMessage{claudeOrgTestKey(claudeOrgTestAccount, claudeOrgTestFirst, "user:profile"): claudeOrgTestEntry()}
	t.Run("corrupt database", func(t *testing.T) {
		dir := t.TempDir()
		if err := os.WriteFile(filepath.Join(dir, "Cookies"), []byte("synthetic corrupt database"), 0600); err != nil {
			t.Fatal(err)
		}
		if org, err := resolveClaudeDesktopOrg(context.Background(), dir, claudeOrgTestDecrypt, cache, nil, claudeOrgTestAccount, true); !errors.Is(err, errClaudeOrgStorage) || org != "" {
			t.Fatalf("corrupt database allowed cache fallback: org=%q err=%v", org, err)
		}
	})
	t.Run("unsupported cookie schema", func(t *testing.T) {
		dir := t.TempDir()
		db, err := sql.Open("sqlite", filepath.Join(dir, "Cookies"))
		if err != nil {
			t.Fatal(err)
		}
		if _, err := db.Exec("CREATE TABLE cookies (name TEXT)"); err != nil {
			db.Close()
			t.Fatal(err)
		}
		if err := db.Close(); err != nil {
			t.Fatal(err)
		}
		if org, err := resolveClaudeDesktopOrg(context.Background(), dir, claudeOrgTestDecrypt, cache, nil, claudeOrgTestAccount, true); !errors.Is(err, errClaudeOrgStorage) || org != "" {
			t.Fatalf("unsupported schema allowed cache fallback: org=%q err=%v", org, err)
		}
	})
	t.Run("decryption failure", func(t *testing.T) {
		dir := t.TempDir()
		writeClaudeOrgTestCookies(t, filepath.Join(dir, "Cookies"), claudeOrgTestCookie{host: ".claude.ai", encrypted: []byte("synthetic-only-ciphertext"), updated: 1})
		wantErr := errors.New("synthetic decryption failure")
		decrypt := func([]byte) ([]byte, error) { return nil, wantErr }
		if org, err := resolveClaudeDesktopOrg(context.Background(), dir, decrypt, cache, nil, claudeOrgTestAccount, true); !errors.Is(err, wantErr) || org != "" {
			t.Fatalf("decryption failure allowed cache fallback: org=%q err=%v", org, err)
		}
	})
	t.Run("invalid organization value", func(t *testing.T) {
		dir := t.TempDir()
		writeClaudeOrgTestCookies(t, filepath.Join(dir, "Cookies"), claudeOrgTestCookie{host: ".claude.ai", value: "synthetic-invalid-org", updated: 1})
		if org, err := resolveClaudeDesktopOrg(context.Background(), dir, claudeOrgTestDecrypt, cache, nil, claudeOrgTestAccount, true); !errors.Is(err, errClaudeOrgInvalid) || org != "" {
			t.Fatalf("invalid cookie organization allowed cache fallback: org=%q err=%v", org, err)
		}
	})
	t.Run("canceled context", func(t *testing.T) {
		dir := t.TempDir()
		writeClaudeOrgTestCookies(t, filepath.Join(dir, "Cookies"), claudeOrgTestCookie{host: ".claude.ai", value: claudeOrgTestSecond, updated: 1})
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		if org, err := resolveClaudeDesktopOrg(ctx, dir, claudeOrgTestDecrypt, cache, nil, claudeOrgTestAccount, true); !errors.Is(err, context.Canceled) || org != "" {
			t.Fatalf("cancellation allowed cache fallback: org=%q err=%v", org, err)
		}
	})
}

func TestClaudeDesktopOrgReadableEmptyCookieAllowsSingletonFallback(t *testing.T) {
	dir := t.TempDir()
	writeClaudeOrgTestCookies(t, filepath.Join(dir, "Cookies"))
	cache := map[string]json.RawMessage{claudeOrgTestKey(claudeOrgTestAccount, claudeOrgTestFirst, "user:profile"): claudeOrgTestEntry()}
	got, err := resolveClaudeDesktopOrg(context.Background(), dir, claudeOrgTestDecrypt, cache, nil, claudeOrgTestAccount, true)
	if err != nil || got != claudeOrgTestFirst {
		t.Fatalf("absent cookie in readable database blocked singleton fallback: org=%q err=%v", got, err)
	}
}
