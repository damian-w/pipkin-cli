package pipkin

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"unsafe"

	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/registry"
)

const runKey = `Software\Microsoft\Windows\CurrentVersion\Run`

func registerAutostart() (string, error) {
	dir, err := resolvedAppDir()
	if err != nil {
		return "", err
	}
	key, _, err := registry.CreateKey(registry.CURRENT_USER, runKey, registry.SET_VALUE)
	if err != nil {
		return "", err
	}
	defer key.Close()
	command := syscall.EscapeArg(serviceBinary()) + " run --home " + syscall.EscapeArg(dir)
	return "a sign-in startup entry", key.SetStringValue("Pipkin", command)
}

func removeAutostart() error {
	key, err := registry.OpenKey(registry.CURRENT_USER, runKey, registry.SET_VALUE)
	if errors.Is(err, registry.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	defer key.Close()
	err = key.DeleteValue("Pipkin")
	if errors.Is(err, registry.ErrNotExist) {
		return nil
	}
	return err
}

func snapshotAutostart() (func() error, error) {
	key, err := registry.OpenKey(registry.CURRENT_USER, runKey, registry.QUERY_VALUE)
	if errors.Is(err, registry.ErrNotExist) {
		return removeAutostart, nil
	}
	if err != nil {
		return nil, err
	}
	value, kind, err := key.GetStringValue("Pipkin")
	key.Close()
	if errors.Is(err, registry.ErrNotExist) {
		return removeAutostart, nil
	}
	if err != nil {
		return nil, err
	}
	return func() error {
		key, _, err := registry.CreateKey(registry.CURRENT_USER, runKey, registry.SET_VALUE)
		if err != nil {
			return err
		}
		defer key.Close()
		if kind == registry.EXPAND_SZ {
			return key.SetExpandStringValue("Pipkin", value)
		}
		return key.SetStringValue("Pipkin", value)
	}, nil
}

func startService() error {
	dir, err := resolvedAppDir()
	if err != nil {
		return err
	}
	return spawnDetached(serviceBinary(), "run", "--home", dir)
}

func stopService() error { return nil }

func userPathContains(folder string) (bool, error) {
	key, err := registry.OpenKey(registry.CURRENT_USER, "Environment", registry.QUERY_VALUE)
	if errors.Is(err, registry.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	defer key.Close()
	value, _, err := key.GetStringValue("Path")
	if errors.Is(err, registry.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	for _, part := range strings.Split(value, ";") {
		if strings.EqualFold(filepath.Clean(part), filepath.Clean(folder)) {
			return true, nil
		}
	}
	return false, nil
}

func updateUserPath(folder string, remove bool) error {
	key, _, err := registry.CreateKey(registry.CURRENT_USER, "Environment",
		registry.QUERY_VALUE|registry.SET_VALUE)
	if err != nil {
		return err
	}
	defer key.Close()
	value, kind, err := key.GetStringValue("Path")
	if errors.Is(err, registry.ErrNotExist) {
		kind = registry.EXPAND_SZ
	} else if err != nil {
		return err
	}
	var parts []string
	for _, part := range strings.Split(value, ";") {
		if part != "" && !strings.EqualFold(filepath.Clean(part), filepath.Clean(folder)) {
			parts = append(parts, part)
		}
	}
	if !remove {
		parts = append(parts, folder)
	}
	joined := strings.Join(parts, ";")
	if kind == registry.EXPAND_SZ {
		err = key.SetExpandStringValue("Path", joined)
	} else {
		err = key.SetStringValue("Path", joined)
	}
	if err != nil {
		return err
	}
	environment, _ := windows.UTF16PtrFromString("Environment")
	result, _, err := windows.NewLazySystemDLL("user32.dll").NewProc("SendMessageTimeoutW").Call(
		0xFFFF, 0x001A, 0, uintptr(unsafe.Pointer(environment)), 2, 5000, 0)
	if result == 0 {
		return fmt.Errorf("PATH was updated but notifying running programs failed: %w", err)
	}
	return nil
}

func serialPermissionHint() string { return "" }

func removeInstallation() error {
	dir, err := resolvedAppDir()
	if err != nil {
		return err
	}
	tombstone, err := stageInstallationRemoval(dir)
	if err != nil || tombstone == "" {
		return err
	}
	// Wait for this executable to exit; LiteralPath avoids environment and wildcard expansion.
	script := fmt.Sprintf("Wait-Process -Id %d -ErrorAction SilentlyContinue; Remove-Item -LiteralPath '%s' -Recurse -Force", os.Getpid(), strings.ReplaceAll(tombstone, "'", "''"))
	command := exec.Command("powershell.exe", "-NoProfile", "-NonInteractive", "-Command", script)
	command.SysProcAttr = &syscall.SysProcAttr{HideWindow: true,
		CreationFlags: windows.DETACHED_PROCESS | windows.CREATE_NO_WINDOW}
	if err := command.Start(); err != nil {
		return errors.Join(err, os.Rename(tombstone, dir))
	}
	return command.Process.Release()
}
