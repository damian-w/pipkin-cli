package pipkin

import (
	"errors"
	"fmt"
	"html"
	"os"
	"path/filepath"
	"strings"
)

const serviceLabel = "pipkin.helper"

func launchAgentPath() string {
	return filepath.Join(homeDir(), "Library", "LaunchAgents", serviceLabel+".plist")
}

func launchDomain() string { return fmt.Sprintf("gui/%d", os.Getuid()) }

func registerAutostart() (string, error) {
	dir, err := resolvedAppDir()
	if err != nil {
		return "", err
	}
	plist := `<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0"><dict>
<key>Label</key><string>` + serviceLabel + `</string>
<key>ProgramArguments</key><array><string>` + html.EscapeString(serviceBinary()) + `</string><string>run</string><string>--home</string><string>` + html.EscapeString(dir) + `</string></array>
<key>RunAtLoad</key><true/>
<key>KeepAlive</key><dict><key>SuccessfulExit</key><false/></dict>
<key>ProcessType</key><string>Background</string>
<key>LowPriorityIO</key><true/>
<key>Nice</key><integer>10</integer>
</dict></plist>
`
	return "a launchd login agent", writeFileAtomic(launchAgentPath(), []byte(plist), 0o644)
}

func removeAutostart() error { return removeIfExists(launchAgentPath()) }

func snapshotAutostart() (func() error, error) {
	state, err := captureStartupFile(launchAgentPath())
	if err != nil {
		return nil, err
	}
	return state.restore, nil
}

func startService() error {
	exists, err := pathExists(launchAgentPath())
	if err != nil {
		return err
	}
	if !exists {
		return startDetachedHelper()
	}
	if err := serviceCommand("launchctl", "bootstrap", launchDomain(), launchAgentPath()); err != nil {
		if retryErr := serviceCommand("launchctl", "kickstart", launchDomain()+"/"+serviceLabel); retryErr != nil {
			return errors.Join(err, retryErr)
		}
	}
	return nil
}

func stopService() error {
	exists, err := pathExists(launchAgentPath())
	if err != nil || !exists {
		return err
	}
	err = serviceCommand("launchctl", "bootout", launchDomain()+"/"+serviceLabel)
	if err != nil && (strings.Contains(err.Error(), "No such process") || strings.Contains(err.Error(), "Could not find service")) {
		return nil
	}
	return err
}

func updateUserPath(string, bool) error { return nil }

func userPathContains(string) (bool, error) { return false, nil }

func serialPermissionHint() string { return "" }

func removeInstallation() error { return os.RemoveAll(appDir()) }
