package pipkin

import (
	"fmt"
	"strings"
)

const statusHelp = `
Also shows your Claude Code and Codex tasks. Doesn't contact Claude or Codex.
Running 'pipkin' on its own shows the same view.

  --json  Print the saved status as JSON
`

const usageHelp = `
Includes model limits, credits and extra usage when available.

  --json  Print every allowance field as JSON
`

const stopHelp = `
The display stops updating. Run 'pipkin start' to resume sooner.
`

const uninstallHelp = `
Also removes Pipkin's settings and logs. Your Claude and Codex
sign-ins are kept.
`

const installHelp = `
Installs Pipkin and starts it each time you log in. Run it again to repair
Pipkin; your settings and sign-ins are kept.

On a Mac, if a Keychain prompt appears, enter your Mac login password and
choose Always Allow.
`

const updateHelp = `
Your settings and sign-ins are kept. If newer display firmware is available,
run 'pipkin flash' to install it.
`

const authorizeHelp = `
When the Keychain prompt appears, enter your Mac login password and choose
Always Allow. Pipkin restarts afterwards if it's running.
`

const flashHelp = `
Downloads the latest firmware, checks your display and asks before writing.
Updates keep your settings. With several displays connected, it asks which one
to flash. Keep the USB cable plugged in until it finishes; Ctrl-C is safe until
writing starts.

  --reinstall        Write the same version again to repair a failed update
  --port PORT        Choose a port, e.g. /dev/cu.usbserial-110 or COM9
  --version VERSION  Install a specific firmware version

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
