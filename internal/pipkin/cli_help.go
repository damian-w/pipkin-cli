package pipkin

import (
	"fmt"
	"strings"
)

const statusHelp = `
Shows saved allowance readings, task summaries and the last known display connection.
Does not contact Claude or Codex or probe the display.
Running 'pipkin' without a command shows this view when installed.
Task counts are observed only; partial or unavailable coverage is stated.

  --json  Print the cached snapshot, including local task observations
`

const usageHelp = `
Checks your existing Claude and Codex sign-ins for current subscription usage.
Does not change your sign-ins or update the display's saved readings.

  --json  Print all available allowance fields

For repeated checks without provider requests, use 'pipkin status --json'.
`

const stopHelp = `
The display stops updating. Run 'pipkin start' to resume sooner.
`

const uninstallHelp = `
Stops Pipkin and removes automatic startup, settings, remembered boards and logs.
Your Claude and Codex sign-ins are kept.
Requires a verified installation in its own folder. Unsafe paths, directory links
and unrelated files are refused before Pipkin is stopped. Run it from outside
Pipkin's installation folder; a PIPKIN_HOME override must be absolute.
`

const installHelp = `
Installs Pipkin and starts it automatically when you log in to this computer.
Existing sign-ins are kept. On macOS, a Keychain dialog may ask for your Mac
login password; choose Always Allow.

Malformed settings are backed up before repair. Valid settings are kept.
On macOS and Linux, PATH fixes are printed for you to paste; shell startup
files are not edited automatically.
`

const updateHelp = `
Checks GitHub for a newer Pipkin release and verifies downloads before replacing
the app. Your settings and sign-ins are kept.

May suggest newer display firmware using its saved version. To update the
display itself, run 'pipkin flash'.
`

const authorizeHelp = `
On macOS, retries access to Claude's saved sign-in. A Keychain dialog may ask
for your Mac login password; choose Always Allow for background access.
No dialog is needed when an accessible Claude Code sign-in can be used.
A running Pipkin restarts after successful authorization.
`

const flashHelp = `
Downloads the latest firmware, checks the connected display, shows what it
will do and only continues if you answer yes. Updates keep your settings.
Keep the USB cable plugged in until it finishes.
Ctrl-C cancels before writing. Once writing starts, wait for verification and
the display restart to finish.

  --reinstall        Write the same version again to repair a failed update
  --port PORT        Choose a port, e.g. /dev/cu.usbserial-110 or COM9
  --version VERSION  Install a specific firmware version, not a CLI version
  --board PROFILE    For board builders; see 'pipkin help identify'

Guide: github.com/damian-w/pipkin-cli/blob/main/docs/firmware.md
`

type cliOptionError struct{ message string }

func (e *cliOptionError) Error() string { return e.message }
func (e *cliOptionError) Unwrap() error { return errUsage }

func invalidCLIOption(value string) error {
	label := "unexpected argument"
	if strings.HasPrefix(value, "-") {
		label = "unknown option"
	}
	return &cliOptionError{fmt.Sprintf("%s %q", label, terminalText(value))}
}

func suggestCLICommand(name string, commands []cliCommand) string {
	name = strings.ToLower(name)
	if len(name) > 64 {
		return ""
	}
	prefix, matches := "", 0
	for _, command := range commands {
		if command.summary != "" && name != "" && strings.HasPrefix(command.name, name) {
			prefix, matches = command.name, matches+1
		}
	}
	if matches == 1 {
		return prefix
	}
	if matches > 1 {
		return ""
	}
	best, score, tied := "", 3, false
	for _, command := range commands {
		if command.summary == "" {
			continue
		}
		distance := cliEditDistance(name, command.name)
		if distance < score {
			best, score, tied = command.name, distance, false
		} else if distance == score {
			tied = true
		}
	}
	if tied {
		return ""
	}
	return best
}

func cliEditDistance(a, b string) int {
	row := make([]int, len(b)+1)
	for i := range row {
		row[i] = i
	}
	for i := range len(a) {
		previous := row[0]
		row[0] = i + 1
		for j := range len(b) {
			cost := 0
			if a[i] != b[j] {
				cost = 1
			}
			next := min(row[j+1]+1, row[j]+1, previous+cost)
			previous, row[j+1] = row[j+1], next
		}
	}
	return row[len(b)]
}
