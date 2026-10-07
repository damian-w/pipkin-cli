package pipkin

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"net/url"
	"os"
	"os/user"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"time"

	_ "modernc.org/sqlite"
)

type claudeCredential struct {
	AccessToken      string   `json:"accessToken"`
	ExpiresAt        float64  `json:"expiresAt"`
	Plan             string   `json:"subscriptionType"`
	Scopes           []string `json:"scopes"`
	source, identity string
	desktopDir       string
}

type claudeDesktopConfig struct {
	TokenCacheV2 json.RawMessage `json:"oauth:tokenCacheV2"`
	TokenCache   json.RawMessage `json:"oauth:tokenCache"`
	AccountUUID  string          `json:"lastKnownAccountUuid"`
}

func claudeCachePresent(raw json.RawMessage) bool {
	raw = bytes.TrimSpace(raw)
	return len(raw) > 0 && !bytes.Equal(raw, []byte("null"))
}

func (config claudeDesktopConfig) signedIn() bool {
	return claudeCachePresent(config.TokenCacheV2) || claudeCachePresent(config.TokenCache)
}

func readClaudeDesktopConfig(dir string) (claudeDesktopConfig, error) {
	var config claudeDesktopConfig
	raw, err := readClaudeFile(filepath.Join(dir, "config.json"))
	if errors.Is(err, os.ErrNotExist) {
		return config, err
	}
	if err != nil {
		return config, errors.New("Claude Desktop credentials could not be read")
	}
	if json.Unmarshal(raw, &config) != nil {
		return config, errors.New("Claude Desktop configuration is invalid")
	}
	return config, nil
}

func claudeDesktopDirs() []string {
	switch runtime.GOOS {
	case "darwin":
		return []string{filepath.Join(homeDir(), "Library", "Application Support", "Claude")}
	case "windows":
		dirs := []string{filepath.Join(envOr("APPDATA", filepath.Join(homeDir(), "AppData", "Roaming")), "Claude")}
		packages, _ := filepath.Glob(filepath.Join(envOr("LOCALAPPDATA", filepath.Join(homeDir(), "AppData", "Local")), "Packages", "Claude_*", "LocalCache", "Roaming", "Claude"))
		return append(dirs, packages...)
	default:
		return []string{filepath.Join(envOr("XDG_CONFIG_HOME", filepath.Join(homeDir(), ".config")), "Claude"), filepath.Join(envOr("XDG_CONFIG_HOME", filepath.Join(homeDir(), ".config")), "claude")}
	}
}

func claudeDesktopSignedIn() bool {
	for _, dir := range claudeDesktopDirs() {
		config, err := readClaudeDesktopConfig(dir)
		if err == nil && config.signedIn() {
			return true
		}
	}
	return false
}

func readClaudeFile(path string) ([]byte, error) {
	return readCredentialFile(path, 4<<20, "Claude credential file is unreadable or too large")
}

func loadClaudeCredential(ctx context.Context) (claudeCredential, error) {
	var zero claudeCredential
	// Desktop owns its rotating token. Borrow it read-only; never refresh or save it.
	for _, dir := range claudeDesktopDirs() {
		config, err := readClaudeDesktopConfig(dir)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return zero, err
		}
		if !config.signedIn() {
			continue
		}
		return loadClaudeDesktopCredential(ctx, dir, config)
	}
	if runtime.GOOS == "darwin" {
		service := "Claude Code-credentials"
		if custom := os.Getenv("CLAUDE_CONFIG_DIR"); custom != "" {
			sum := sha256.Sum256([]byte(custom))
			service += "-" + hex.EncodeToString(sum[:])[:8]
		}
		if current, err := user.Current(); err == nil {
			raw, err := readGenericPassword(ctx, service, current.Username)
			if err == nil {
				return parseClaudeCodeCredential(raw, "claude-keychain")
			}
			if !errors.Is(err, errCredentialMissing) {
				return zero, err
			}
		}
	}
	raw, err := readClaudeFile(filepath.Join(claudeConfig(), ".credentials.json"))
	if errors.Is(err, os.ErrNotExist) {
		return zero, fmt.Errorf("Claude: %w", errSignedOut)
	}
	if err != nil {
		return zero, errors.New("Claude credentials could not be read")
	}
	return parseClaudeCodeCredential(raw, "claude-auth-file")
}

func loadClaudeDesktopCredential(ctx context.Context, dir string, config claudeDesktopConfig) (claudeCredential, error) {
	if err := ctx.Err(); err != nil {
		return claudeCredential{}, err
	}
	account := config.AccountUUID
	if !validClaudeUUID(account) {
		return claudeCredential{}, errors.New("Claude Desktop's active account is unavailable; open Claude Desktop")
	}
	decrypt, err := claudeDecryptor(ctx, dir)
	if err != nil {
		return claudeCredential{}, err
	}
	caches, err := decodeClaudeDesktopCaches(config, decrypt)
	if err != nil {
		return claudeCredential{}, err
	}
	org, err := resolveClaudeDesktopOrg(ctx, dir, decrypt, caches[0], caches[1], account, runtime.GOOS == "windows")
	if err != nil {
		return claudeCredential{}, err
	}
	credential, err := selectClaudeDesktopToken(caches[0], caches[1], account, org, time.Now())
	credential.source = "claude-desktop"
	credential.identity = strings.ToLower(account) + "|" + org
	if runtime.GOOS == "windows" {
		credential.desktopDir = dir
	}
	return credential, err
}

func decodeClaudeDesktopCaches(config claudeDesktopConfig, decrypt func([]byte) ([]byte, error)) ([2]map[string]json.RawMessage, error) {
	var caches [2]map[string]json.RawMessage
	for i, raw := range []json.RawMessage{config.TokenCacheV2, config.TokenCache} {
		if !claudeCachePresent(raw) {
			continue
		}
		var encoded string
		if json.Unmarshal(raw, &encoded) != nil {
			return caches, errors.New("Claude Desktop token cache is invalid")
		}
		encrypted, err := base64.StdEncoding.DecodeString(encoded)
		if err != nil {
			return caches, errors.New("Claude Desktop token cache is invalid")
		}
		clear, err := decrypt(encrypted)
		if err != nil {
			return caches, err
		}
		if json.Unmarshal(clear, &caches[i]) != nil {
			return caches, errors.New("Claude Desktop token cache is invalid")
		}
	}
	return caches, nil
}

// Profile verifies the token's owner, not GUI selection. Recheck persisted
// Desktop state before publishing Windows readings, including inferred orgs.
func validateClaudeDesktopIdentity(ctx context.Context, credential claudeCredential) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	config, err := readClaudeDesktopConfig(credential.desktopDir)
	if err != nil || !config.signedIn() {
		return fmt.Errorf("Claude Desktop sign-in changed while reading usage; retry: %w", errSignedOut)
	}
	account, _, _ := strings.Cut(credential.identity, "|")
	if !strings.EqualFold(config.AccountUUID, account) {
		return fmt.Errorf("Claude Desktop account changed while reading usage; retry: %w", errSignedOut)
	}
	current, err := loadClaudeDesktopCredential(ctx, credential.desktopDir, config)
	if err != nil {
		return err
	}
	if current.identity != credential.identity {
		return fmt.Errorf("Claude Desktop organization changed while reading usage; retry: %w", errSignedOut)
	}
	return nil
}

func parseClaudeCodeCredential(raw []byte, source string) (claudeCredential, error) {
	var file struct {
		OAuth claudeCredential `json:"claudeAiOauth"`
	}
	if json.Unmarshal(raw, &file) != nil || strings.TrimSpace(file.OAuth.AccessToken) == "" {
		return claudeCredential{}, fmt.Errorf("Claude: %w", errSignedOut)
	}
	c := file.OAuth
	if c.ExpiresAt > 0 && c.ExpiresAt <= float64(time.Now().UnixMilli()) {
		return c, fmt.Errorf("Claude sign-in expired; open Claude to renew it: %w", errSignedOut)
	}
	if len(c.Scopes) > 0 && !slices.Contains(c.Scopes, "user:profile") {
		return c, errors.New("Claude sign-in cannot read subscription usage")
	}
	c.source = source
	return c, nil
}

var (
	errClaudeOrgMissing    = errors.New("Claude Desktop's active organization is unavailable; open Claude Desktop")
	errClaudeOrgUnreadable = errors.New("Claude Desktop's active organization cannot be read")
	errClaudeOrgInvalid    = errors.New("Claude Desktop's active organization cookie is invalid")
	errClaudeOrgStorage    = errors.New("Claude Desktop's cookie database could not be read or queried")
	errClaudeOrgAmbiguous  = errors.New("Claude Desktop's active organization is unavailable: multiple organizations are cached and its cookie cannot be read")
)

func resolveClaudeDesktopOrg(ctx context.Context, dir string, decrypt func([]byte) ([]byte, error), v2, v1 map[string]json.RawMessage, account string, allowCacheFallback bool) (string, error) {
	org, err := claudeActiveOrg(ctx, dir, decrypt)
	if err == nil || !allowCacheFallback || (!errors.Is(err, errClaudeOrgMissing) && !errors.Is(err, errClaudeOrgUnreadable)) {
		return org, err
	}
	if err := ctx.Err(); err != nil {
		return "", err
	}
	// Count every represented org, including expired entries and tombstones.
	// Token validity, scopes and cache ordering do not identify GUI selection.
	orgs := make(map[string]bool)
	for _, cache := range []map[string]json.RawMessage{v2, v1} {
		for _, entry := range normalizedClaudeCache(cache, account) {
			orgs[entry.key.org] = true
		}
	}
	if len(orgs) > 1 {
		return "", errClaudeOrgAmbiguous
	}
	for inferred := range orgs {
		return inferred, nil
	}
	return "", err
}

func claudeActiveOrg(ctx context.Context, dir string, decrypt func([]byte) ([]byte, error)) (string, error) {
	var unavailable, invalid error
	record := func(err error) {
		if errors.Is(err, errClaudeOrgUnreadable) {
			if unavailable == nil {
				unavailable = err
			}
		} else if invalid == nil {
			invalid = err
		}
	}
	for _, relative := range []string{"Cookies", filepath.Join("Network", "Cookies")} {
		if err := ctx.Err(); err != nil {
			return "", err
		}
		path := filepath.Join(dir, relative)
		info, err := os.Stat(path)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			record(claudeCookieReadError(err))
			continue
		}
		if info.IsDir() {
			record(errClaudeOrgStorage)
			continue
		}
		// Preserve native sharing/access errors before SQLite reduces them to
		// a generic cannot-open code. This does not override the owner's lock.
		file, err := os.Open(path)
		if err != nil {
			record(claudeCookieReadError(err))
			continue
		}
		file.Close()
		uriPath := filepath.ToSlash(path)
		if len(uriPath) > 1 && uriPath[1] == ':' {
			uriPath = "/" + uriPath
		}
		address := url.URL{Scheme: "file", Path: uriPath, RawQuery: "mode=ro"}
		db, err := sql.Open("sqlite", address.String())
		if err != nil {
			record(errClaudeOrgStorage)
			continue
		}
		rows, err := db.QueryContext(ctx, "SELECT host_key,value,encrypted_value FROM cookies WHERE name='lastActiveOrg' AND host_key IN ('.claude.ai','claude.ai') ORDER BY last_update_utc DESC")
		if err != nil {
			db.Close()
			if ctx.Err() != nil {
				return "", ctx.Err()
			}
			record(errClaudeOrgStorage)
			continue
		}
		for rows.Next() {
			var host, value string
			var encrypted []byte
			if rows.Scan(&host, &value, &encrypted) != nil {
				record(errClaudeOrgStorage)
				continue
			}
			if value == "" {
				clear, err := decrypt(encrypted)
				if err != nil {
					rows.Close()
					db.Close()
					return "", err
				}
				hash := sha256.Sum256([]byte(host))
				if bytes.HasPrefix(clear, hash[:]) {
					clear = clear[len(hash):]
				}
				value = string(clear)
			}
			if validClaudeUUID(value) {
				rows.Close()
				db.Close()
				if err := ctx.Err(); err != nil {
					return "", err
				}
				return strings.ToLower(value), nil
			}
			record(errClaudeOrgInvalid)
		}
		rowErr := rows.Err()
		rows.Close()
		db.Close()
		if ctx.Err() != nil {
			return "", ctx.Err()
		}
		if rowErr != nil {
			record(errClaudeOrgStorage)
		}
	}
	if invalid != nil {
		return "", invalid
	}
	if unavailable != nil {
		return "", unavailable
	}
	return "", errClaudeOrgMissing
}

func validClaudeUUID(s string) bool {
	if len(s) != 36 {
		return false
	}
	for i, c := range s {
		if i == 8 || i == 13 || i == 18 || i == 23 {
			if c != '-' {
				return false
			}
			continue
		}
		if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f' || c >= 'A' && c <= 'F') {
			return false
		}
	}
	return true
}

type claudeCacheKey struct {
	client, org string
	scopes      []string
}

type claudeCacheEntry struct {
	key   claudeCacheKey
	value json.RawMessage
}

func parseClaudeCacheKey(text, account string) (claudeCacheKey, bool) {
	if strings.HasPrefix(text, "acct:") {
		owner, remainder, ok := strings.Cut(text[5:], "|")
		if !ok || !validClaudeUUID(account) || !strings.EqualFold(owner, account) {
			return claudeCacheKey{}, false
		}
		text = remainder
	}
	prefix, scopeText, ok := strings.Cut(text, ":https://api.anthropic.com:")
	client, org, valid := strings.Cut(prefix, ":")
	if !ok || !valid || !validClaudeUUID(client) || !validClaudeUUID(org) {
		return claudeCacheKey{}, false
	}
	scopes := strings.Fields(scopeText)
	slices.Sort(scopes)
	return claudeCacheKey{strings.ToLower(client), strings.ToLower(org), slices.Compact(scopes)}, true
}

// Scoped entries and V2 tombstones override older aliases, even if the old token is valid.
func normalizedClaudeCache(cache map[string]json.RawMessage, account string) map[string]claudeCacheEntry {
	normalized := make(map[string]claudeCacheEntry)
	keys := slices.Sorted(maps.Keys(cache))
	for _, scoped := range []bool{false, true} {
		for _, key := range keys {
			if strings.HasPrefix(key, "acct:") != scoped {
				continue
			}
			parsed, ok := parseClaudeCacheKey(key, account)
			if !ok {
				continue
			}
			canonical := parsed.client + ":" + parsed.org + ":https://api.anthropic.com:" + strings.Join(parsed.scopes, " ")
			normalized[canonical] = claudeCacheEntry{parsed, cache[key]}
		}
	}
	return normalized
}

func selectClaudeDesktopToken(v2, v1 map[string]json.RawMessage, account, org string, now time.Time) (claudeCredential, error) {
	newest := normalizedClaudeCache(v2, account)
	older := normalizedClaudeCache(v1, account)
	for key := range newest {
		delete(older, key)
	}
	stale := false
	for _, cache := range []map[string]claudeCacheEntry{newest, older} {
		var best claudeCredential
		bestRank := -1
		for _, canonical := range slices.Sorted(maps.Keys(cache)) {
			cached := cache[canonical]
			key := cached.key
			if !strings.EqualFold(key.org, org) || !slices.Contains(key.scopes, "user:profile") {
				continue
			}
			var entry struct {
				Token     string  `json:"token"`
				ExpiresAt float64 `json:"expiresAt"`
				Plan      string  `json:"subscriptionType"`
			}
			if json.Unmarshal(cached.value, &entry) != nil || strings.TrimSpace(entry.Token) == "" {
				continue
			}
			if entry.ExpiresAt <= float64(now.Add(2*time.Minute).UnixMilli()) {
				stale = true
				continue
			}
			rank := len(key.scopes)
			if slices.Contains(key.scopes, "user:inference") {
				rank += 100
				if key.client == "9d1c250a-e61b-44d9-88ed-5944d1962f5e" {
					rank += 1000
				}
			}
			if rank > bestRank || rank == bestRank && entry.ExpiresAt > best.ExpiresAt {
				best = claudeCredential{AccessToken: entry.Token, ExpiresAt: entry.ExpiresAt, Plan: entry.Plan, Scopes: key.scopes}
				bestRank = rank
			}
		}
		if bestRank >= 0 {
			return best, nil
		}
	}
	if stale {
		return claudeCredential{}, fmt.Errorf("Claude Desktop sign-in expired; open Claude Desktop to renew it: %w", errSignedOut)
	}
	return claudeCredential{}, fmt.Errorf("Claude Desktop has no usage-capable login for the active account: %w", errSignedOut)
}
