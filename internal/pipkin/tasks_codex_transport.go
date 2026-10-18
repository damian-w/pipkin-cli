package pipkin

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
)

func codexTaskHome() (string, error) {
	if value, ok := os.LookupEnv("CODEX_HOME"); ok {
		if value == "" || !filepath.IsAbs(value) {
			return "", errors.New("Codex home must be absolute")
		}
		return filepath.Clean(value), nil
	}
	home, err := os.UserHomeDir()
	if err != nil || !filepath.IsAbs(home) {
		return "", errors.New("Codex home is unavailable")
	}
	return filepath.Join(home, ".codex"), nil
}

// The peer must be a GUI executable, never a separately launched app-server.
func codexGUIExecutable(executable, goos string) bool {
	path := strings.ReplaceAll(executable, `\`, "/")
	for _, part := range strings.Split(path, "/") {
		if part == ".." || part == "." {
			return false
		}
	}
	if goos == "darwin" {
		return strings.HasPrefix(path, "/") && (strings.HasSuffix(path, "/ChatGPT.app/Contents/MacOS/ChatGPT") ||
			strings.HasSuffix(path, "/Codex.app/Contents/MacOS/Codex") ||
			strings.HasSuffix(path, "/Codex.app/Contents/MacOS/ChatGPT"))
	}
	if goos != "windows" || len(path) < 3 || path[1] != ':' || path[2] != '/' {
		return false
	}
	parts := strings.Split(strings.ToLower(path), "/")
	if len(parts) < 3 || (parts[len(parts)-1] != "chatgpt.exe" && parts[len(parts)-1] != "codex.exe") {
		return false
	}
	// Native Windows releases install the GUI beneath its MSIX package.
	for i, part := range parts {
		if strings.HasPrefix(part, "openai.codex_") && strings.HasSuffix(part, "__2p2nqsd0c76g0") &&
			i == len(parts)-3 && parts[i+1] == "app" && i > 0 && parts[i-1] == "windowsapps" {
			return true
		}
	}
	// Also recognize the per-user unpackaged installer location.
	return len(parts) >= 4 && parts[len(parts)-3] == "programs" && parts[len(parts)-2] == "codex"
}
