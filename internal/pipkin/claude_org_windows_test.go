//go:build windows

package pipkin

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"golang.org/x/sys/windows"
)

func TestClaudeDesktopOrgWindowsNativeErrorClassification(t *testing.T) {
	for _, c := range []struct {
		name string
		err  error
		want error
	}{
		{"sharing violation", windows.ERROR_SHARING_VIOLATION, errClaudeOrgUnreadable},
		{"wrapped sharing violation", fmt.Errorf("synthetic wrapper: %w", windows.ERROR_SHARING_VIOLATION), errClaudeOrgUnreadable},
		{"access denied", windows.ERROR_ACCESS_DENIED, errClaudeOrgUnreadable},
		{"wrapped access denied", &os.PathError{Op: "open", Path: "synthetic-private-location", Err: windows.ERROR_ACCESS_DENIED}, errClaudeOrgUnreadable},
		{"other native failure", windows.ERROR_INVALID_PARAMETER, errClaudeOrgStorage},
		{"byte range lock remains strict", windows.ERROR_LOCK_VIOLATION, errClaudeOrgStorage},
		{"unknown error remains strict", errors.New("synthetic-private-location"), errClaudeOrgStorage},
	} {
		t.Run(c.name, func(t *testing.T) {
			got := claudeCookieReadError(c.err)
			if !errors.Is(got, c.want) {
				t.Fatalf("native error was classified as %v, want %v", got, c.want)
			}
			if strings.Contains(got.Error(), "synthetic-private-location") {
				t.Fatal("native error leaked its file path")
			}
		})
	}
}

func lockClaudeOrgTestCookies(t *testing.T, path string) {
	t.Helper()
	name, err := windows.UTF16PtrFromString(path)
	if err != nil {
		t.Fatal(err)
	}
	handle, err := windows.CreateFile(name, windows.GENERIC_READ, 0, nil, windows.OPEN_EXISTING, windows.FILE_ATTRIBUTE_NORMAL, 0)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := windows.CloseHandle(handle); err != nil {
			t.Error(err)
		}
	})
	file, err := os.Open(path)
	if file != nil {
		file.Close()
	}
	if !errors.Is(err, windows.ERROR_SHARING_VIOLATION) {
		t.Fatalf("fixture did not reproduce Windows cookie sharing denial: %v", err)
	}
}

func TestClaudeDesktopOrgWindowsLockedCookie(t *testing.T) {
	for _, c := range []struct {
		name          string
		allowFallback bool
		secondOrg     bool
		want          string
	}{
		{name: "singleton succeeds", allowFallback: true, want: claudeOrgTestFirst},
		{name: "disabled fallback refuses lock"},
		{name: "multiple organizations refuse lock", allowFallback: true, secondOrg: true},
	} {
		t.Run(c.name, func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, "Network", "Cookies")
			writeClaudeOrgTestCookies(t, path, claudeOrgTestCookie{host: ".claude.ai", value: claudeOrgTestSecond, updated: 1})
			lockClaudeOrgTestCookies(t, path)
			cache := map[string]json.RawMessage{claudeOrgTestKey(claudeOrgTestAccount, claudeOrgTestFirst, "user:profile"): claudeOrgTestEntry()}
			if c.secondOrg {
				cache[claudeOrgTestKey(claudeOrgTestAccount, claudeOrgTestSecond, "user:profile")] = claudeOrgTestEntry()
			}
			got, err := resolveClaudeDesktopOrg(context.Background(), dir, claudeOrgTestDecrypt, cache, nil, claudeOrgTestAccount, c.allowFallback)
			if c.want == "" {
				if err == nil || got != "" {
					t.Fatalf("locked cookie unexpectedly resolved: org=%q err=%v", got, err)
				}
				return
			}
			if err != nil || got != c.want {
				t.Fatalf("locked-cookie singleton fallback failed: org=%q err=%v", got, err)
			}
		})
	}
}

func TestClaudeDesktopOrgWindowsReadableCookieWinsOverLockedCandidate(t *testing.T) {
	dir := t.TempDir()
	rootCookie := filepath.Join(dir, "Cookies")
	writeClaudeOrgTestCookies(t, rootCookie, claudeOrgTestCookie{host: ".claude.ai", value: claudeOrgTestFirst, updated: 1})
	lockClaudeOrgTestCookies(t, rootCookie)
	writeClaudeOrgTestCookies(t, filepath.Join(dir, "Network", "Cookies"), claudeOrgTestCookie{host: ".claude.ai", value: claudeOrgTestSecond, updated: 1})
	cache := map[string]json.RawMessage{claudeOrgTestKey(claudeOrgTestAccount, claudeOrgTestFirst, "user:profile"): claudeOrgTestEntry()}
	got, err := resolveClaudeDesktopOrg(context.Background(), dir, claudeOrgTestDecrypt, cache, nil, claudeOrgTestAccount, true)
	if err != nil || got != claudeOrgTestSecond {
		t.Fatalf("readable candidate lost precedence to locked cookie cache fallback: org=%q err=%v", got, err)
	}
}

func TestClaudeDesktopOrgWindowsStrictCandidateBlocksLockedFallback(t *testing.T) {
	cache := map[string]json.RawMessage{claudeOrgTestKey(claudeOrgTestAccount, claudeOrgTestFirst, "user:profile"): claudeOrgTestEntry()}
	for _, lockedRelative := range []string{"Cookies", filepath.Join("Network", "Cookies")} {
		for _, c := range []struct {
			name  string
			write func(*testing.T, string)
			want  error
		}{
			{
				name: "corrupt database",
				write: func(t *testing.T, path string) {
					if err := os.WriteFile(path, []byte("synthetic corrupt database"), 0600); err != nil {
						t.Fatal(err)
					}
				},
				want: errClaudeOrgStorage,
			},
			{
				name: "unsupported schema",
				write: func(t *testing.T, path string) {
					db, err := sql.Open("sqlite", path)
					if err != nil {
						t.Fatal(err)
					}
					defer db.Close()
					if _, err := db.Exec("CREATE TABLE cookies (name TEXT)"); err != nil {
						t.Fatal(err)
					}
				},
				want: errClaudeOrgStorage,
			},
			{
				name: "invalid organization",
				write: func(t *testing.T, path string) {
					writeClaudeOrgTestCookies(t, path, claudeOrgTestCookie{host: ".claude.ai", value: "synthetic-invalid-org", updated: 1})
				},
				want: errClaudeOrgInvalid,
			},
		} {
			t.Run(lockedRelative+"/"+c.name, func(t *testing.T) {
				dir := t.TempDir()
				locked := filepath.Join(dir, lockedRelative)
				writeClaudeOrgTestCookies(t, locked, claudeOrgTestCookie{host: ".claude.ai", value: claudeOrgTestFirst, updated: 1})
				lockClaudeOrgTestCookies(t, locked)
				strictRelative := filepath.Join("Network", "Cookies")
				if lockedRelative != "Cookies" {
					strictRelative = "Cookies"
				}
				strict := filepath.Join(dir, strictRelative)
				if err := os.MkdirAll(filepath.Dir(strict), 0700); err != nil {
					t.Fatal(err)
				}
				c.write(t, strict)
				org, err := resolveClaudeDesktopOrg(context.Background(), dir, claudeOrgTestDecrypt, cache, nil, claudeOrgTestAccount, true)
				if org != "" || !errors.Is(err, c.want) {
					t.Fatalf("strict cookie failure lost precedence to locked-cookie fallback: org=%q err=%v, want %v", org, err, c.want)
				}
			})
		}
	}
}
