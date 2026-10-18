package pipkin

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
)

var version string

var errUsage = errors.New("invalid options")

type cliCommand struct {
	name, syntax string
	summary      string // menu entry; empty hides the command
	details      string // extra --help text
	primary      bool
	// parse validates options before the command runs; nil accepts none.
	parse func([]string) error
	run   func([]string) error
	// standalone commands do not need a resolvable installation directory.
	standalone bool
}

func Run(buildVersion, licenseText, thirdPartyNotices string) {
	version = buildVersion
	commands := cliCommands(buildVersion, licenseText, thirdPartyNotices, runtime.GOOS, os.Stdout)
	code, err := dispatch(os.Args[1:], buildVersion, commands, os.Stdout)
	if err != nil {
		fmt.Fprintf(os.Stderr, "pipkin: %v\n", err)
	}
	if code != 0 {
		os.Exit(code)
	}
}

func cliCommands(buildVersion, licenseText, notices, goos string, output io.Writer) []cliCommand {
	plain := func(run func() error) func([]string) error { return func([]string) error { return run() } }
	authorize := ""
	if goos == "darwin" {
		authorize = "Ask macOS again for access to Claude's sign-in"
	}
	printVersion := func([]string) error {
		_, err := fmt.Fprintln(output, buildVersion)
		return err
	}
	printLicense := func([]string) error {
		_, err := fmt.Fprint(output, licenseText, "\n", notices)
		return err
	}
	return []cliCommand{
		{name: "status", syntax: "pipkin status [--json]", summary: "Check Pipkin, your display and your allowance", primary: true,
			details: statusHelp, parse: jsonOption, run: func(args []string) error { return statusCommandTo(args, output) }},
		{name: "usage", syntax: "pipkin usage [--json]", summary: "Show your Claude and Codex allowance right now", primary: true,
			details: usageHelp, parse: jsonOption, run: func(args []string) error { return usageCommandTo(args, output, os.Stderr) }},
		{name: "restart", syntax: "pipkin restart", summary: "Restart Pipkin if the display stops updating", primary: true, run: plain(restartCommand)},
		{name: "update", syntax: "pipkin update", summary: "Update Pipkin on this computer", primary: true, details: updateHelp, run: plain(updateCommand)},
		{name: "flash", syntax: "pipkin flash [options]", summary: "Install or update the display's firmware", primary: true,
			details: flashHelp, parse: func(options []string) error { _, err := parseFlashOptions(options); return err }, run: flashCommand},
		{name: "uninstall", syntax: "pipkin uninstall", summary: "Remove Pipkin from this computer", primary: true, details: uninstallHelp, run: plain(uninstallCommand)},
		{name: "tasks", syntax: "pipkin tasks [--json]", summary: "Show your Claude Code and Codex tasks", details: tasksHelp,
			parse: jsonOption, run: tasksCommand, standalone: true},
		{name: "start", syntax: "pipkin start", summary: "Start Pipkin again after 'pipkin stop'", run: plain(startCommand)},
		{name: "stop", syntax: "pipkin stop", summary: "Pause Pipkin until you next log in to this computer", details: stopHelp, run: plain(stopCommand)},
		{name: "install", syntax: "pipkin install", summary: "Install or repair Pipkin on this computer", details: installHelp, run: plain(installCommand)},
		{name: "authorize", syntax: "pipkin authorize", summary: authorize, details: authorizeHelp, run: plain(authorizeCommand)},
		{name: "identify", syntax: "pipkin identify [--port PORT] [--json | --issue] [--board PROFILE]",
			details: identifyHelp,
			parse:   func(options []string) error { _, err := parseIdentifyOptions(options); return err }, run: identifyCommand},
		{name: "version", syntax: "pipkin version", summary: "Show Pipkin's version", run: printVersion, standalone: true},
		{name: "--version", syntax: "pipkin --version", run: printVersion, standalone: true},
		{name: "-v", syntax: "pipkin -v", run: printVersion, standalone: true},
		{name: "-V", syntax: "pipkin -V", run: printVersion, standalone: true},
		{name: "license", syntax: "pipkin license", summary: "Show the license and third-party notices", run: printLicense, standalone: true},
		{name: "run", syntax: "pipkin run [--home DIR]", parse: startupHome, run: plain(runCommand)},
	}
}

func jsonOption(options []string) error {
	if len(options) == 0 || len(options) == 1 && options[0] == "--json" {
		return nil
	}
	bad := options[0]
	if bad == "--json" && len(options) > 1 {
		bad = options[1]
		if bad == "--json" {
			return &cliOptionError{"--json can only be used once"}
		}
	}
	return invalidCLIOption(bad)
}

// Service managers start the helper with the installation directory it was registered for.
func startupHome(options []string) error {
	if len(options) == 0 {
		return nil
	}
	if len(options) != 2 || options[0] != "--home" || !filepath.IsAbs(options[1]) {
		return errUsage
	}
	return os.Setenv("PIPKIN_HOME", options[1])
}

func formatCLIUsage(buildVersion string, commands []cliCommand) string {
	var menu strings.Builder
	fmt.Fprintf(&menu, "Pipkin %s shows your Claude and Codex allowance on your desk display.\n\n", buildVersion)
	var more []string
	for _, command := range commands {
		if command.summary != "" {
			if command.primary {
				fmt.Fprintf(&menu, "  %-11s %s\n", command.name, command.summary)
			} else {
				more = append(more, command.name)
			}
		}
	}
	if len(more) > 0 {
		fmt.Fprintf(&menu, "\nMore: %s\n", strings.Join(more, ", "))
	}
	menu.WriteString("Run 'pipkin help <command>' for details.\nGuide: github.com/damian-w/pipkin-cli\n")
	return menu.String()
}

func dispatch(args []string, buildVersion string, commands []cliCommand, output io.Writer) (int, error) {
	menu := formatCLIUsage(buildVersion, commands)
	if len(args) == 0 {
		if dir, err := resolvedAppDir(); err == nil {
			if installed, err := pathExists(filepath.Join(dir, "bin", executableName("pipkin"))); err == nil && installed {
				if index := slices.IndexFunc(commands, func(command cliCommand) bool { return command.name == "status" }); index >= 0 {
					if err := commands[index].run(nil); err != nil {
						return cliResult(err)
					}
					_, err := io.WriteString(output, "\nRun 'pipkin help' to see all commands.\n")
					return cliResult(err)
				}
			}
		}
		_, err := io.WriteString(output, menu)
		return cliResult(err)
	}
	name := args[0]
	if name == "help" || name == "-h" || name == "--help" {
		if len(args) == 1 {
			_, err := io.WriteString(output, menu)
			return cliResult(err)
		}
		if len(args) != 2 {
			return 1, errors.New("usage: pipkin help [command]")
		}
		name, args = args[1], []string{args[1], "--help"}
		if name == "help" || name == "-h" || name == "--help" {
			_, err := io.WriteString(output, menu)
			return cliResult(err)
		}
	}
	index := slices.IndexFunc(commands, func(command cliCommand) bool { return command.name == name })
	if index < 0 {
		if suggestion := suggestCLICommand(name, commands); suggestion != "" {
			return 2, fmt.Errorf("unknown command %q. Did you mean 'pipkin %s'?\nRun 'pipkin help' to see all commands.", terminalText(name), suggestion)
		}
		return 2, fmt.Errorf("unknown command %q\n\n%s", terminalText(name), menu)
	}
	command, options := commands[index], args[1:]
	if slices.Contains(options, "--help") || slices.Contains(options, "-h") {
		heading := ""
		if command.summary != "" {
			heading = command.summary + ".\n\n"
		}
		_, err := fmt.Fprintf(output, "%sUsage: %s\n%s", heading, command.syntax, command.details)
		return cliResult(err)
	}
	var err error
	if command.parse != nil {
		err = command.parse(options)
	} else if len(options) != 0 {
		err = invalidCLIOption(options[0])
	}
	var optionErr *cliOptionError
	if errors.As(err, &optionErr) {
		return 1, fmt.Errorf("%s for 'pipkin %s'\nUsage: %s", optionErr.Error(), command.name, command.syntax)
	} else if errors.Is(err, errUsage) {
		return 1, fmt.Errorf("usage: %s", command.syntax)
	} else if err != nil {
		return 1, fmt.Errorf("%w\nUsage: %s", err, command.syntax)
	}
	if !command.standalone {
		if _, err := resolvedAppDir(); err != nil {
			return 1, fmt.Errorf("could not resolve the Pipkin installation directory: %w", err)
		}
	}
	return cliResult(command.run(options))
}

func cliResult(err error) (int, error) {
	if err != nil {
		return 1, err
	}
	return 0, nil
}
