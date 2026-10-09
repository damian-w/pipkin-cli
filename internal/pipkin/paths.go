package pipkin

import (
	"os"
	"path/filepath"
	"runtime"
)

func homeDir() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return "."
	}
	return home
}

func envOr(name, fallback string) string {
	if value := os.Getenv(name); value != "" {
		return value
	}
	return fallback
}

func configuredAppDir() string {
	if dir := os.Getenv("PIPKIN_HOME"); dir != "" {
		return dir
	}
	switch runtime.GOOS {
	case "windows":
		return filepath.Join(envOr("LOCALAPPDATA", filepath.Join(homeDir(), "AppData", "Local")), "Pipkin")
	case "darwin":
		return filepath.Join(homeDir(), "Library", "Application Support", "Pipkin")
	default:
		return filepath.Join(envOr("XDG_DATA_HOME", filepath.Join(homeDir(), ".local", "share")), "pipkin")
	}
}

func resolvedAppDir() (string, error) { return filepath.Abs(configuredAppDir()) }

func appDir() string {
	dir, err := resolvedAppDir()
	if err != nil {
		return configuredAppDir()
	}
	return dir
}

func executableName(name string) string {
	if runtime.GOOS == "windows" {
		return name + ".exe"
	}
	return name
}

func installedBinary() string { return filepath.Join(appDir(), "bin", executableName("pipkin")) }

// Windows autostart uses a windowless build to avoid a console.
func serviceBinary() string {
	if runtime.GOOS == "windows" {
		windowless := filepath.Join(appDir(), "bin", "pipkinw.exe")
		if _, err := os.Stat(windowless); err == nil {
			return windowless
		}
	}
	return installedBinary()
}

// Keep these locks beside the installation so uninstall cannot replace them.
func siblingLockPath(name string) string {
	dir := filepath.Clean(appDir())
	return filepath.Join(filepath.Dir(dir), "."+filepath.Base(dir)+"-"+name+".lock")
}

func installationLockPath() string { return siblingLockPath("installation") }

// maintenanceLockPath serializes firmware and board operations with helper startup.
func maintenanceLockPath() string { return siblingLockPath("flash") }

func launcherPath() string { return filepath.Join(homeDir(), ".local", "bin", "pipkin") }

func configPath() string   { return filepath.Join(appDir(), "config.json") }
func statusPath() string   { return filepath.Join(appDir(), "status.json") }
func lockPath() string     { return filepath.Join(appDir(), "helper.lock") }
func pidPath() string      { return filepath.Join(appDir(), "helper.pid") }
func logPath() string      { return filepath.Join(appDir(), "helper.log") }
func claudeConfig() string { return envOr("CLAUDE_CONFIG_DIR", filepath.Join(homeDir(), ".claude")) }
