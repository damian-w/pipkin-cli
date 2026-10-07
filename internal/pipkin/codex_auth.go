package pipkin

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/BurntSushi/toml"
)

type codexAuth struct {
	AccessToken string `json:"access_token"`
	AccountID   string `json:"account_id"`
	IDToken     string `json:"id_token"`
	Source      string `json:"-"`
}

func loadCodexAuth(ctx context.Context) (codexAuth, error) {
	for _, home := range codexAuthHomes() {
		// Explicit keyring configuration wins over a potentially stale auth file.
		mode, err := codexAuthStoreMode(home)
		if err != nil {
			return codexAuth{}, err
		}
		if mode == "ephemeral" {
			return codexAuth{}, fmt.Errorf("Codex uses an in-memory sign-in: %w", errSignedOut)
		}
		if mode == "keyring" || mode == "auto" {
			auth, err := codexKeyringAuth(ctx, home)
			if err == nil {
				return auth, nil
			}
			if !errors.Is(err, errCredentialMissing) {
				return codexAuth{}, err
			}
			if mode == "keyring" {
				return codexAuth{}, fmt.Errorf("Codex keyring sign-in is absent: %w", errSignedOut)
			}
		}
		data, err := readCodexAuthFile(filepath.Join(home, "auth.json"))
		if err == nil {
			auth, err := parseCodexAuth(data)
			auth.Source = "codex_auth_file"
			return auth, err
		}
		if !errors.Is(err, os.ErrNotExist) {
			return codexAuth{}, errors.New("Codex credential file is not readable")
		}
		if _, err := os.Stat(filepath.Join(home, "config.toml")); !errors.Is(err, os.ErrNotExist) {
			// A configured home is selected even after sign-out removes auth.json.
			return codexAuth{}, fmt.Errorf("Codex app sign-in is absent: %w", errSignedOut)
		}
	}
	return codexAuth{}, fmt.Errorf("no readable Codex app sign-in found: %w", errSignedOut)
}

func codexAuthHomes() []string {
	if custom := os.Getenv("CODEX_HOME"); custom != "" {
		return []string{custom}
	}
	return []string{filepath.Join(homeDir(), ".codex"), filepath.Join(homeDir(), ".config", "codex")}
}

func codexKeyringAuth(ctx context.Context, home string) (codexAuth, error) {
	data, err := readGenericPassword(ctx, "Codex Auth", codexStoreKey(home))
	if err != nil {
		return codexAuth{}, fmt.Errorf("Codex credential store: %w", err)
	}
	auth, err := parseCodexAuth(data)
	auth.Source = "codex_keyring"
	return auth, err
}

func codexStoreKey(home string) string {
	if canonical, err := filepath.EvalSymlinks(home); err == nil {
		if absolute, err := filepath.Abs(canonical); err == nil {
			home = absolute
			// Rust canonicalize uses Windows extended-length paths for the key.
			if runtime.GOOS == "windows" && !strings.HasPrefix(home, `\\?\`) {
				if strings.HasPrefix(home, `\\`) {
					home = `\\?\UNC\` + strings.TrimPrefix(home, `\\`)
				} else {
					home = `\\?\` + home
				}
			}
		}
	}
	sum := sha256.Sum256([]byte(home))
	return "cli|" + hex.EncodeToString(sum[:8])
}

func readCodexAuthFile(path string) ([]byte, error) {
	return readCredentialFile(path, 1<<20, "Codex credential file could not be read")
}

func parseCodexAuth(data []byte) (codexAuth, error) {
	var document struct {
		Tokens   codexAuth `json:"tokens"`
		APIKey   string    `json:"OPENAI_API_KEY"`
		AuthMode string    `json:"auth_mode"`
	}
	// Older Keychain exports encode the JSON bytes as hexadecimal.
	if !json.Valid(data) {
		if decoded, err := hex.DecodeString(strings.TrimSpace(string(data))); err == nil {
			data = decoded
		}
	}
	if json.Unmarshal(data, &document) != nil {
		return codexAuth{}, errors.New("Codex sign-in file is malformed")
	}
	if (document.AuthMode != "" && document.AuthMode != "chatgpt" && document.AuthMode != "chatgptAuthTokens") || (document.AuthMode == "" && document.APIKey != "") {
		return codexAuth{}, fmt.Errorf("Codex does not use a subscription sign-in: %w", errSignedOut)
	}
	if strings.TrimSpace(document.Tokens.AccessToken) == "" {
		return codexAuth{}, errors.New("Codex sign-in has no subscription access token")
	}
	auth := document.Tokens
	if strings.ContainsAny(auth.AccessToken+auth.AccountID, "\r\n") {
		return codexAuth{}, errors.New("Codex sign-in is malformed")
	}
	if auth.AccountID == "" {
		for _, token := range []string{auth.IDToken, auth.AccessToken} {
			parts := strings.Split(token, ".")
			if len(parts) != 3 {
				continue
			}
			data, err := base64.RawURLEncoding.DecodeString(strings.TrimRight(parts[1], "="))
			if err != nil {
				continue
			}
			var claims struct {
				Auth struct {
					AccountID string `json:"chatgpt_account_id"`
				} `json:"https://api.openai.com/auth"`
			}
			if json.Unmarshal(data, &claims) == nil && claims.Auth.AccountID != "" && !strings.ContainsAny(claims.Auth.AccountID, "\r\n") {
				auth.AccountID = claims.Auth.AccountID
				break
			}
		}
	}
	return auth, nil
}

func codexAuthStoreMode(home string) (string, error) {
	data, err := readCodexAuthFile(filepath.Join(home, "config.toml"))
	if errors.Is(err, os.ErrNotExist) {
		return "file", nil
	}
	if err != nil {
		return "", errors.New("Codex configuration is not readable")
	}
	var config struct {
		Mode     *string `toml:"cli_auth_credentials_store"`
		Features struct {
			SecretAuthStorage bool `toml:"secret_auth_storage"`
		} `toml:"features"`
	}
	if _, err := toml.Decode(string(data), &config); err != nil {
		return "", errors.New("Codex configuration is malformed")
	}
	mode := "file"
	if config.Mode != nil {
		mode = *config.Mode
	}
	if config.Features.SecretAuthStorage && (mode == "keyring" || mode == "auto") {
		return "", errors.New("Codex encrypted secret storage is not supported yet")
	}
	switch mode {
	case "file", "keyring", "auto", "ephemeral":
		return mode, nil
	default:
		return "", errors.New("Codex credential storage mode is not supported")
	}
}
