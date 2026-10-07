package pipkin

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
)

var version string

const usage = `Pipkin display helper %s

Usage: pipkin <command>

Commands:
  usage       Read current usage now (--json for all fields)
%s  status      Show helper and display status (--json for cached readings)
  start       Start the helper now
  stop        Stop the helper until next sign-in
  restart     Restart the helper now
  update      Install the latest helper release
  flash       Install or update display firmware (asks for confirmation)
  uninstall   Remove the helper and its settings
  install     Install or repair the helper
  version     Print the helper version
  license     Show the license and third-party notices
`

func formatCLIUsage(buildVersion, goos string) string {
	authorize := ""
	if goos == "darwin" {
		authorize = "  authorize   Allow macOS access to Claude's existing sign-in\n"
	}
	return fmt.Sprintf(usage, buildVersion, authorize)
}

func Run(buildVersion, licenseText, thirdPartyNotices string) {
	version = buildVersion
	code, err := dispatch(os.Args[1:], buildVersion, licenseText, thirdPartyNotices, os.Stdout, cliHandlers{
		usage: usageCommand, authorize: authorizeCommand, status: statusCommand,
		statusJSON: statusJSONCommand, start: startCommand, stop: stopCommand,
		restart: restartCommand, update: updateCommand, uninstall: uninstallCommand,
		install: installCommand, run: runCommand, flash: flashCommand,
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "pipkin: %v\n", err)
	}
	if code != 0 {
		os.Exit(code)
	}
}

type cliHandlers struct {
	usage      func([]string) error
	authorize  func() error
	status     func() error
	statusJSON func() error
	start      func() error
	stop       func() error
	restart    func() error
	update     func() error
	uninstall  func() error
	install    func([]string) error
	run        func() error
	flash      func([]string) error
}

func dispatch(args []string, buildVersion, licenseText, notices string, output io.Writer, handlers cliHandlers) (int, error) {
	menu := formatCLIUsage(buildVersion, runtime.GOOS)
	if len(args) == 0 {
		_, err := io.WriteString(output, menu)
		return cliResult(err)
	}
	command := args[0]
	commandUsage := map[string]string{
		"usage": "pipkin usage [--json]", "authorize": "pipkin authorize",
		"status": "pipkin status [--json]", "start": "pipkin start", "stop": "pipkin stop",
		"restart": "pipkin restart",
		"update":  "pipkin update", "uninstall": "pipkin uninstall", "install": "pipkin install",
		"version": "pipkin version", "--version": "pipkin --version", "license": "pipkin license", "run": "pipkin run [--home DIR]",
		"flash": "pipkin flash [--port PORT] [--version VERSION] [--reinstall]",
	}
	if command == "help" || command == "-h" || command == "--help" {
		if len(args) == 1 {
			_, err := io.WriteString(output, menu)
			return cliResult(err)
		}
		if command != "help" || len(args) != 2 {
			return 1, fmt.Errorf("usage: pipkin help [command]")
		}
		command = args[1]
		args = []string{command, "--help"}
	}
	syntax, known := commandUsage[command]
	if !known {
		return 2, fmt.Errorf("unknown command %q\n\n%s", command, menu)
	}
	options := args[1:]
	if len(options) == 1 && (options[0] == "--help" || options[0] == "-h") {
		_, err := fmt.Fprintf(output, "Usage: %s\n", syntax)
		return cliResult(err)
	}
	jsonOption := len(options) == 1 && options[0] == "--json"
	homeOption := command == "run" && len(options) == 2 && options[0] == "--home" && filepath.IsAbs(options[1])
	if command == "flash" {
		if _, err := parseFlashOptions(options); err != nil {
			return 1, fmt.Errorf("%w\nUsage: %s", err, syntax)
		}
	}
	if len(options) != 0 && command != "flash" && !((command == "usage" || command == "status") && jsonOption) && !homeOption {
		return 1, fmt.Errorf("usage: %s", syntax)
	}
	if homeOption {
		previous, present := os.LookupEnv("PIPKIN_HOME")
		if err := os.Setenv("PIPKIN_HOME", options[1]); err != nil {
			return 1, err
		}
		defer func() {
			if present {
				os.Setenv("PIPKIN_HOME", previous)
			} else {
				os.Unsetenv("PIPKIN_HOME")
			}
		}()
	}
	if command != "version" && command != "--version" && command != "license" {
		if _, err := resolvedAppDir(); err != nil {
			return 1, fmt.Errorf("could not resolve the Pipkin installation directory: %w", err)
		}
	}
	var err error
	switch command {
	case "usage":
		err = handlers.usage(options)
	case "authorize":
		err = handlers.authorize()
	case "status":
		if jsonOption {
			err = handlers.statusJSON()
		} else {
			err = handlers.status()
		}
	case "start":
		err = handlers.start()
	case "stop":
		err = handlers.stop()
	case "restart":
		err = handlers.restart()
	case "update":
		err = handlers.update()
	case "flash":
		err = handlers.flash(options)
	case "uninstall":
		err = handlers.uninstall()
	case "install":
		err = handlers.install(options)
	case "version", "--version":
		_, err = fmt.Fprintln(output, buildVersion)
	case "license":
		_, err = fmt.Fprint(output, licenseText, "\n", notices)
	case "run":
		err = handlers.run()
	}
	return cliResult(err)
}

func cliResult(err error) (int, error) {
	if err != nil {
		return 1, err
	}
	return 0, nil
}
