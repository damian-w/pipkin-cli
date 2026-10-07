package pipkin

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
)

func configHome() string { return envOr("XDG_CONFIG_HOME", filepath.Join(homeDir(), ".config")) }

func systemdUnitPath() string {
	return filepath.Join(configHome(), "systemd", "user", "pipkin.service")
}

func desktopEntryPath() string { return filepath.Join(configHome(), "autostart", "pipkin.desktop") }

func hasSystemd() bool {
	return serviceCommand("systemctl", "--user", "show-environment") == nil
}

func systemdArgument(value string) string {
	return `"` + strings.NewReplacer(`\`, `\\`, `"`, `\"`, `%`, `%%`, `$`, `$$`, "\n", `\n`, "\r", `\r`, "\t", `\t`).Replace(value) + `"`
}

func desktopArgument(value string) string {
	// Desktop entries decode escapes twice: once for the string, once for Exec.
	quoted := `"` + strings.NewReplacer(`\`, `\\`, `"`, `\"`, "`", "\\`", `$`, `\$`, `%`, `%%`).Replace(value) + `"`
	return strings.NewReplacer(`\`, `\\`, "\n", `\n`, "\r", `\r`, "\t", `\t`).Replace(quoted)
}

func registerAutostart() (string, error) {
	dir, err := resolvedAppDir()
	if err != nil {
		return "", err
	}
	if hasSystemd() {
		// env avoids systemd's restrictions on characters in the executable token.
		command := "/usr/bin/env -- " + systemdArgument(serviceBinary()) + " run --home " + systemdArgument(dir)
		unit := fmt.Sprintf("[Unit]\nDescription=Pipkin display helper\n\n[Service]\nExecStart=%s\n"+
			"Restart=on-failure\nRestartSec=10\nNice=10\n\n[Install]\nWantedBy=default.target\n", command)
		if err := writeFileAtomic(systemdUnitPath(), []byte(unit), 0o644); err != nil {
			return "", err
		}
		if err := serviceCommand("systemctl", "--user", "daemon-reload"); err != nil {
			return "", err
		}
		if err := serviceCommand("systemctl", "--user", "enable", "pipkin.service"); err != nil {
			return "", err
		}
		if err := removeIfExists(desktopEntryPath()); err != nil {
			return "", err
		}
		return "a systemd user service", nil
	}
	command := desktopArgument(serviceBinary()) + " run --home " + desktopArgument(dir)
	entry := "[Desktop Entry]\nType=Application\nName=Pipkin helper\nNoDisplay=true\nExec=" + command + "\n"
	return "a desktop autostart entry", writeFileAtomic(desktopEntryPath(), []byte(entry), 0o644)
}

func snapshotAutostart() (func() error, error) {
	unit, err := captureStartupFile(systemdUnitPath())
	if err != nil {
		return nil, err
	}
	desktop, err := captureStartupFile(desktopEntryPath())
	if err != nil {
		return nil, err
	}
	managed := hasSystemd()
	enabled := false
	if managed && unit.present {
		err := serviceCommand("systemctl", "--user", "is-enabled", "pipkin.service")
		if err == nil {
			enabled = true
		} else {
			var exit *exec.ExitError
			if !errors.As(err, &exit) || exit.ExitCode() != 1 {
				return nil, err
			}
		}
	}
	return func() error {
		var disableErr error
		if managed && !enabled {
			if current, err := pathExists(systemdUnitPath()); err != nil {
				disableErr = err
			} else if current {
				disableErr = serviceCommand("systemctl", "--user", "disable", "pipkin.service")
			}
		}
		filesErr := errors.Join(unit.restore(), desktop.restore())
		var reloadErr, enableErr error
		if managed {
			reloadErr = serviceCommand("systemctl", "--user", "daemon-reload")
			if enabled && filesErr == nil {
				enableErr = serviceCommand("systemctl", "--user", "enable", "pipkin.service")
			}
		}
		return errors.Join(disableErr, filesErr, reloadErr, enableErr)
	}, nil
}

func removeAutostart() error {
	exists, err := pathExists(systemdUnitPath())
	if err != nil {
		return err
	}
	if exists {
		if hasSystemd() {
			if err := serviceCommand("systemctl", "--user", "disable", "pipkin.service"); err != nil {
				return err
			}
		}
		if err := removeIfExists(systemdUnitPath()); err != nil {
			return err
		}
		if hasSystemd() {
			if err := serviceCommand("systemctl", "--user", "daemon-reload"); err != nil {
				return err
			}
		}
	}
	return removeIfExists(desktopEntryPath())
}

func startService() error {
	exists, err := pathExists(systemdUnitPath())
	if err != nil {
		return err
	}
	if exists && hasSystemd() {
		return serviceCommand("systemctl", "--user", "start", "pipkin.service")
	}
	return startDetachedHelper()
}

func stopService() error {
	exists, err := pathExists(systemdUnitPath())
	if err != nil {
		return err
	}
	if exists && hasSystemd() {
		return serviceCommand("systemctl", "--user", "stop", "pipkin.service")
	}
	return nil
}

func updateUserPath(string, bool) error { return nil }

func userPathContains(string) (bool, error) { return false, nil }

func serialPermissionHint() string {
	current, err := user.Current()
	if err != nil {
		return ""
	}
	groups, _ := current.GroupIds()
	for _, name := range []string{"dialout", "uucp"} {
		group, err := user.LookupGroup(name)
		if err != nil {
			continue
		}
		if slices.Contains(groups, group.Gid) || strconv.Itoa(os.Getgid()) == group.Gid {
			return ""
		}
		return fmt.Sprintf("Serial access needs the %q group: sudo usermod -aG %s $USER, then sign "+
			"out and back in.", name, name)
	}
	return ""
}

func removeInstallation() error { return os.RemoveAll(appDir()) }
