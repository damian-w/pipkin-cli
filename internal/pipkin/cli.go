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
		authorize = "Allow macOS access to Claude's existing sign-in"
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
		{name: "usage", syntax: "pipkin usage [--json]", summary: "Read current usage now (--json for all fields)",
			parse: jsonOption, run: usageCommand},
		{name: "authorize", syntax: "pipkin authorize", summary: authorize, run: plain(authorizeCommand)},
		{name: "status", syntax: "pipkin status [--json]", summary: "Show helper and display status (--json for cached readings)",
			parse: jsonOption, run: statusCommand},
		{name: "start", syntax: "pipkin start", summary: "Start the helper now", run: plain(startCommand)},
		{name: "stop", syntax: "pipkin stop", summary: "Stop the helper until next sign-in", run: plain(stopCommand)},
		{name: "restart", syntax: "pipkin restart", summary: "Restart the helper now", run: plain(restartCommand)},
		{name: "update", syntax: "pipkin update", summary: "Install the latest helper release", run: plain(updateCommand)},
		{name: "flash", syntax: "pipkin flash [--port PORT] [--version VERSION] [--reinstall] [--board PROFILE]",
			summary: "Install or update display firmware (asks for confirmation)",
			parse:   func(options []string) error { _, err := parseFlashOptions(options); return err }, run: flashCommand},
		{name: "identify", syntax: "pipkin identify [--port PORT] [--json | --issue] [--board PROFILE]",
			summary: "Inspect a connected board and prepare a board support report", details: identifyHelp,
			parse: func(options []string) error { _, err := parseIdentifyOptions(options); return err }, run: identifyCommand},
		{name: "uninstall", syntax: "pipkin uninstall", summary: "Remove the helper and its settings", run: plain(uninstallCommand)},
		{name: "install", syntax: "pipkin install", summary: "Install or repair the helper", run: plain(installCommand)},
		{name: "version", syntax: "pipkin version", summary: "Print the helper version", run: printVersion, standalone: true},
		{name: "--version", syntax: "pipkin --version", run: printVersion, standalone: true},
		{name: "license", syntax: "pipkin license", summary: "Show the license and third-party notices", run: printLicense, standalone: true},
		{name: "run", syntax: "pipkin run [--home DIR]", parse: startupHome, run: plain(runCommand)},
	}
}

func jsonOption(options []string) error {
	if len(options) == 0 || len(options) == 1 && options[0] == "--json" {
		return nil
	}
	return errUsage
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
	fmt.Fprintf(&menu, "Pipkin display helper %s\n\nUsage: pipkin <command>\n\nCommands:\n", buildVersion)
	for _, command := range commands {
		if command.summary != "" {
			fmt.Fprintf(&menu, "  %-11s %s\n", command.name, command.summary)
		}
	}
	return menu.String()
}

func dispatch(args []string, buildVersion string, commands []cliCommand, output io.Writer) (int, error) {
	menu := formatCLIUsage(buildVersion, commands)
	if len(args) == 0 {
		_, err := io.WriteString(output, menu)
		return cliResult(err)
	}
	name := args[0]
	if name == "help" || name == "-h" || name == "--help" {
		if len(args) == 1 {
			_, err := io.WriteString(output, menu)
			return cliResult(err)
		}
		if name != "help" || len(args) != 2 {
			return 1, errors.New("usage: pipkin help [command]")
		}
		name, args = args[1], []string{args[1], "--help"}
	}
	index := slices.IndexFunc(commands, func(command cliCommand) bool { return command.name == name })
	if index < 0 {
		return 2, fmt.Errorf("unknown command %q\n\n%s", name, menu)
	}
	command, options := commands[index], args[1:]
	if len(options) == 1 && (options[0] == "--help" || options[0] == "-h") {
		_, err := fmt.Fprintf(output, "Usage: %s\n%s", command.syntax, command.details)
		return cliResult(err)
	}
	var err error
	if command.parse != nil {
		err = command.parse(options)
	} else if len(options) != 0 {
		err = errUsage
	}
	if errors.Is(err, errUsage) {
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
