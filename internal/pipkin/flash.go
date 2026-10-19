package pipkin

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/mattn/go-isatty"
)

type flashOptions struct {
	Port, Version string
	Reinstall     bool
}

// parseOptions parses flag-only arguments. The named string options need a value.
func parseOptions(name string, args []string, define func(*flag.FlagSet), valued ...string) error {
	flags := flag.NewFlagSet(name, flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	define(flags)
	for i := 0; i < len(args); i++ {
		arg := args[i]
		if !strings.HasPrefix(arg, "-") || arg == "-" || arg == "--" {
			return invalidCLIOption(arg)
		}
		name, value, assigned := strings.Cut(strings.TrimLeft(arg, "-"), "=")
		if flags.Lookup(name) == nil {
			return invalidCLIOption(arg)
		}
		if slices.Contains(valued, name) {
			if assigned {
				if value == "" {
					return fmt.Errorf("--%s needs a value", name)
				}
			} else {
				if i+1 == len(args) || strings.HasPrefix(args[i+1], "-") || args[i+1] == "" {
					return fmt.Errorf("--%s needs a value", name)
				}
				i++
			}
		}
	}
	if err := flags.Parse(args); err != nil {
		return errors.New(terminalText(strings.ReplaceAll(err.Error(), "flag -", "option --")))
	}
	return nil
}

func validPortOption(port string) bool {
	return port == "" || len(port) <= 256 && !strings.HasPrefix(port, "-") && strings.TrimSpace(port) == port && terminalText(port) == port
}

func parseFlashOptions(args []string) (flashOptions, error) {
	var options flashOptions
	if err := parseOptions("flash", args, func(flags *flag.FlagSet) {
		flags.StringVar(&options.Port, "port", "", "serial port")
		flags.StringVar(&options.Version, "version", "", "stable firmware version")
		flags.BoolVar(&options.Reinstall, "reinstall", false, "reinstall the same firmware version")
	}, "port", "version"); err != nil {
		return options, err
	}
	if !validPortOption(options.Port) {
		return options, errors.New("invalid serial port")
	}
	if options.Version != "" {
		if _, valid := stableVersionNumbers(options.Version); !valid {
			return options, errors.New("firmware version must be a stable version such as 1.0.0")
		}
		options.Version = strings.TrimPrefix(options.Version, "v")
	}
	return options, nil
}

type firmwareFlasher interface {
	Probe(context.Context, string) (flashProbe, error)
	Reset(context.Context, string) error
	Read(context.Context, string, int64, int64) ([]byte, error)
	Write(context.Context, string, []flashWriteImage) error
	EraseSettings(context.Context, string) error
}

type flashActions struct {
	boardActions
	load    func(context.Context, string) (*firmwareRelease, error)
	prepare func(context.Context, io.Writer) (firmwareFlasher, error)
}

func defaultFlashActions() flashActions {
	return flashActions{
		boardActions: defaultBoardActions(),
		load: func(ctx context.Context, selected string) (*firmwareRelease, error) {
			return publicFirmwareSource().stage(ctx, selected, filepath.Join(appDir(), "cache", "firmware"))
		},
		prepare: func(ctx context.Context, output io.Writer) (firmwareFlasher, error) {
			return prepareFlashTool(ctx, output)
		},
	}
}

// boardActions free the display's serial port from the helper while flashing.
type boardActions struct {
	ports    func() []string
	identity func(context.Context, string, time.Duration) (map[string]string, error)
	running  func() (bool, error)
	stop     func() error
	start    func() error
}

func defaultBoardActions() boardActions {
	return boardActions{ports: candidatePorts, identity: readFlashIdentity, running: helperRunning, stop: stopHelper, start: startHelper}
}

// boardSession holds the installation and maintenance locks while flashing.
// close resets the board and resumes the helper independently of the
// operation's cancelled context.
type boardSession struct {
	boardActions
	installation, maintenance *os.File
	port                      string
	reset                     func(context.Context, string) error // set once ROM probing may reset the board
	restoreHelper             bool
}

func openBoardSession(actions boardActions) (*boardSession, error) {
	installation, err := acquireFileLock(installationLockPath())
	if err != nil {
		return nil, commandLockError("use", err)
	}
	maintenance, err := acquireFileLock(maintenanceLockPath())
	if err != nil {
		installation.Close()
		return nil, commandLockError("use", err)
	}
	return &boardSession{boardActions: actions, installation: installation, maintenance: maintenance}, nil
}

// claimPort pauses a running helper, then reads the identity of any responding Pipkin.
func (s *boardSession) claimPort(ctx context.Context) (map[string]string, error) {
	running, err := s.running()
	if err != nil {
		return nil, err
	}
	if running {
		s.restoreHelper = true // A failed stop may already have interrupted service.
		if err := s.stop(); err != nil {
			return nil, fmt.Errorf("could not release the display connection: %w", err)
		}
	}
	identity, err := s.identity(ctx, s.port, 3500*time.Millisecond)
	if err != nil {
		return nil, fmt.Errorf("could not inspect %s (check serial permissions and close other serial tools): %w", terminalText(s.port), err)
	}
	return identity, nil
}

// bootModeFailure explains how to hold the board in its bootloader by hand.
func (s *boardSession) bootModeFailure() error {
	s.reset = nil // esptool's last reset already started the board's firmware.
	var help bytes.Buffer
	lines := lineOutput{w: &help}
	lines.line("", "Hold the BOOT button on the back of the display and run the same command again.")
	lines.line("", `Keep holding BOOT until "Writing firmware..." appears.`)
	return fmt.Errorf("%w, so no firmware was written.\n%s", errFlashBootMode, strings.TrimSuffix(help.String(), "\n"))
}

func (s *boardSession) close(result *error) {
	if s.reset != nil {
		cleanup, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		if err := s.reset(cleanup, s.port); err != nil {
			*result = errors.Join(*result, fmt.Errorf("could not restart the board; reconnect its USB cable: %w", err))
		}
		cancel()
	}
	// runCommand waits for this lock at startup, so release it before resuming the helper.
	if err := s.maintenance.Close(); err != nil {
		*result = errors.Join(*result, err)
	}
	if s.restoreHelper {
		if err := s.start(); err != nil {
			*result = errors.Join(*result, fmt.Errorf("couldn't restart Pipkin; run 'pipkin start': %w", err))
		}
	}
	s.installation.Close()
}

func flashCommand(args []string) error {
	options, err := parseFlashOptions(args)
	if err != nil {
		return err
	}
	if !isatty.IsTerminal(os.Stdin.Fd()) && !isatty.IsCygwinTerminal(os.Stdin.Fd()) {
		return errors.New("flash needs a terminal to ask for confirmation; run 'pipkin flash' in a terminal window")
	}
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	return runFlash(ctx, options, os.Stdin, os.Stdout, defaultFlashActions())
}

func readFlashIdentity(ctx context.Context, name string, timeout time.Duration) (map[string]string, error) {
	port, err := openSerial(name)
	if err != nil {
		return nil, err
	}
	defer port.Close()
	return identify(ctx, &connection{port: port, name: name}, timeout), ctx.Err()
}

func selectFlashPort(explicit string, candidates []string) (string, error) {
	if explicit != "" {
		return explicit, nil
	}
	switch len(candidates) {
	case 0:
		return "", errors.New("no display found; connect it with a USB data cable, or choose a port with --port PORT")
	case 1:
		return candidates[0], nil
	default:
		var ports []string
		for _, port := range candidates {
			ports = append(ports, terminalText(port))
		}
		var message bytes.Buffer
		_ = writeOutputLine(&message, "", "Several USB serial boards are connected ("+strings.Join(ports, ", ")+").")
		_ = writeOutputLine(&message, "", "Unplug the others, or run 'pipkin flash --port PORT' to choose one.")
		return "", errors.New(strings.TrimSuffix(message.String(), "\n"))
	}
}

func validateFlashIdentity(identity map[string]string, manifest firmwareManifest) error {
	if identity == nil {
		return nil
	}
	if !isIdentity(identity) || identity["firmware"] == "" || identity["board"] != manifest.Board {
		return errors.New("connected Pipkin reports an unsupported or incomplete board identity; no firmware was written")
	}
	return nil
}

func validateFlashProbe(probe flashProbe) error {
	if probe.Chip != "esp32" || probe.FlashBytes != baselineFlashBytes || probe.MAC == "" {
		return errors.New("connected board does not match the required ESP32 with 4 MB flash; no firmware was written")
	}
	if probe.Secure {
		return errors.New("secure boot or flash encryption is enabled; this board cannot use the standard firmware images")
	}
	return nil
}

// runFlash keeps service restoration independent of the operation's cancelled
// context. No ROM write or erase is reachable without explicit confirmation.
func runFlash(ctx context.Context, options flashOptions, input io.Reader, output io.Writer, actions flashActions) (result error) {
	protectedOutput := &flashOutput{w: output}
	output = protectedOutput
	var session *boardSession
	var selectedVersion string
	var finishNotice func()
	writeStarted, written, firmwareRunning := false, false, false
	defer func() {
		cancelledBeforeWrite := !writeStarted && ctx.Err() != nil && errors.Is(result, ctx.Err())
		if cancelledBeforeWrite {
			result = flashCancellationRemainder(result, ctx.Err())
		}
		if session != nil {
			session.close(&result)
		}
		if finishNotice != nil {
			finishNotice()
		}
		if cancelledBeforeWrite {
			result = errors.Join(result, writeOutputLine(output, "", "Cancelled. No firmware was written."))
		}
		if outputErr := protectedOutput.failure(); outputErr != nil && !errors.Is(result, outputErr) {
			result = errors.Join(result, outputErr)
		}
		if result != nil && writeStarted && !firmwareRunning {
			result = &flashWriteFailure{cause: result, written: written,
				command: commandHint("flash", "--port", session.port, "--version", selectedVersion, "--reinstall")}
		}
	}()
	if err := ctx.Err(); err != nil {
		return err
	}
	var err error
	session, err = openBoardSession(actions.boardActions)
	if err != nil {
		return err
	}
	port, err := selectFlashPort(options.Port, actions.ports())
	if err != nil {
		return err
	}
	session.port = port
	if err := writeOutputLine(output, "", "Checking the firmware release..."); err != nil {
		return err
	}
	release, err := actions.load(ctx, options.Version)
	if err != nil {
		return fmt.Errorf("firmware release check failed: %w", err)
	}
	defer release.close()
	selectedVersion = release.Manifest.Version
	// Reject invalid injected or stale metadata before preparing the flasher too.
	if err := validateFirmwareManifest(release.Manifest, release.Tag); err != nil {
		return err
	}
	tool, err := actions.prepare(ctx, output)
	if err != nil {
		return fmt.Errorf("could not prepare the firmware flasher: %w", err)
	}
	identity, err := session.claimPort(ctx)
	if err != nil {
		return err
	}
	if err := validateFlashIdentity(identity, release.Manifest); err != nil {
		return err
	}
	installed := ""
	if identity != nil {
		installed = strings.TrimPrefix(identity["firmware"], "v")
		if installed == release.Manifest.Version && !options.Reinstall {
			return writeOutputLine(output, "", "Pipkin firmware "+release.Manifest.Version+" is up to date. Use --reinstall to flash it again.")
		}
		if newerVersion(identity["firmware"], release.Manifest.Version) && options.Version == "" {
			return fmt.Errorf("your display has newer firmware than the latest release; to downgrade, run 'pipkin flash --version %s'", release.Manifest.Version)
		}
	}
	if err := writeOutputLine(output, "", "Checking your display. The screen may go blank for a moment."); err != nil {
		return err
	}
	session.reset = tool.Reset // ROM probing can reset even when it fails midway.
	probe, err := tool.Probe(ctx, port)
	if errors.Is(err, errFlashBootMode) {
		return session.bootModeFailure()
	}
	if err != nil {
		return fmt.Errorf("could not check the ESP32 bootloader (close other serial tools, or hold BOOT while connecting): %w", err)
	}
	if err := validateFlashProbe(probe); err != nil {
		return err
	}
	partition, partitionPath := release.image("partition-table")
	expectedTable, err := os.ReadFile(partitionPath)
	if err != nil {
		return err
	}
	existingTable, err := tool.Read(ctx, port, partition.Offset, partition.Size)
	if err != nil {
		return fmt.Errorf("could not check the existing partition layout: %w", err)
	}
	updating := identity != nil
	stored := false
	if !updating {
		// A board in the ROM loader, or with an interrupted app, cannot answer
		// identify. Inspect its app description before treating it as a new board
		// and clearing a kit owner's settings. This is stored identity only.
		prefix, err := tool.Read(ctx, port, applicationOffset, 1024)
		if err != nil {
			return fmt.Errorf("could not inspect existing firmware: %w", err)
		}
		var storedVersion string
		storedVersion, updating = readPipkinFirmwareVersion(prefix)
		if updating {
			stored = true
			installed = strings.TrimPrefix(storedVersion, "v")
			if installed == release.Manifest.Version && !options.Reinstall {
				return fmt.Errorf("Pipkin firmware %s is installed but not responding; run 'pipkin flash --reinstall' to repair it", storedVersion)
			}
			if newerVersion(storedVersion, release.Manifest.Version) && options.Version == "" {
				return fmt.Errorf("your display has newer firmware than the latest release; to downgrade, run 'pipkin flash --version %s'", release.Manifest.Version)
			}
		}
	}
	if updating && !bytes.Equal(existingTable, expectedTable) {
		return errors.New("existing Pipkin uses a different partition layout; this CLI cannot migrate it safely, so no firmware was written; use the source-build guide")
	}
	action := "Install"
	if updating {
		action = "Update"
		if options.Reinstall && installed == release.Manifest.Version {
			action = "Reinstall"
		} else if newerVersion(installed, release.Manifest.Version) {
			action = "Downgrade"
		}
	}
	prompt, err := writeFlashPlan(output, flashPlan{action: action, installed: installed, selected: release.Manifest.Version,
		port: port, stored: stored})
	if err != nil {
		return err
	}
	confirmed, err := confirmFlashPrompt(ctx, input, output, prompt)
	if err != nil {
		return err
	}
	if !confirmed {
		return writeOutputLine(output, "", "Cancelled. No firmware was written.")
	}
	// A user can unplug or exchange boards while deciding. Recheck the physical
	// device before the first write, rather than trusting its serial-port pathname.
	confirmedProbe, err := tool.Probe(ctx, port)
	if errors.Is(err, errFlashBootMode) {
		return session.bootModeFailure()
	}
	if err != nil {
		return err
	}
	if err := validateFlashProbe(confirmedProbe); err != nil {
		return err
	}
	if confirmedProbe.MAC != probe.MAC {
		return errors.New("the board changed after confirmation; no firmware was written, run pipkin flash again")
	}
	var images []flashWriteImage
	for _, image := range release.Manifest.Images {
		if !updating || image.Role == "application" {
			images = append(images, flashWriteImage{Path: filepath.Join(release.Folder, image.File), Offset: image.Offset})
		}
	}
	sort.Slice(images, func(i, j int) bool { return images[i].Offset < images[j].Offset })
	if err := writeOutputLine(output, "", "This takes about a minute. Keep the USB cable plugged in until it finishes."); err != nil {
		return err
	}
	// Last chance to cancel. After this, the write, verification and boot check
	// finish under the flashing tool's own timeouts.
	if err := ctx.Err(); err != nil {
		return err
	}
	writeCtx := context.WithoutCancel(ctx)
	writeStarted = true
	finishNotice = flashCancellationNotice(ctx, output)
	if !updating {
		if err := tool.EraseSettings(writeCtx, port); err != nil {
			return fmt.Errorf("could not initialize board settings: %w", err)
		}
	}
	if err := tool.Write(writeCtx, port, images); err != nil {
		return fmt.Errorf("firmware write or verification failed: %w", err)
	}
	written = true
	if err := tool.Reset(writeCtx, port); err != nil {
		return fmt.Errorf("firmware written, but board restart failed: %w", err)
	}
	session.reset = nil
	booted, err := actions.identity(writeCtx, port, 12*time.Second)
	if err != nil {
		return fmt.Errorf("firmware written, but startup could not be checked: %w", err)
	}
	if booted == nil || validateFlashIdentity(booted, release.Manifest) != nil || strings.TrimPrefix(booted["firmware"], "v") != release.Manifest.Version {
		return errors.New("firmware written and verified, but the expected Pipkin version did not respond after restart")
	}
	firmwareRunning = true
	finishNotice()
	return writeOutputLine(output, "", "Pipkin firmware "+release.Manifest.Version+" is running.")
}

// flashCancellationRemainder drops the cancellation but keeps other failures.
func flashCancellationRemainder(err, cancelled error) error {
	if err == nil || !errors.Is(err, cancelled) {
		return err
	}
	switch wrapped := err.(type) {
	case interface{ Unwrap() []error }:
		var remaining []error
		for _, child := range wrapped.Unwrap() {
			if child = flashCancellationRemainder(child, cancelled); child != nil {
				remaining = append(remaining, child)
			}
		}
		return errors.Join(remaining...)
	case interface{ Unwrap() error }:
		return flashCancellationRemainder(wrapped.Unwrap(), cancelled)
	default:
		return nil
	}
}

func confirmFlashPrompt(ctx context.Context, input io.Reader, output io.Writer, prompt string) (confirmed bool, result error) {
	if err := ctx.Err(); err != nil {
		return false, err
	}
	n, err := io.WriteString(output, prompt)
	if err != nil {
		return false, err
	}
	if n != len(prompt) {
		return false, io.ErrShortWrite
	}
	defer func() { result = errors.Join(result, writeOutputLine(output, "", "")) }()
	type answer struct {
		line string
		err  error
	}
	answers := make(chan answer, 1)
	go func() {
		// An overlong line is rejected, not partially interpreted as consent.
		reader := bufio.NewReaderSize(input, 64)
		line, err := reader.ReadSlice('\n')
		answers <- answer{string(line), err}
	}()
	select {
	case <-ctx.Done():
		return false, ctx.Err()
	case response := <-answers:
		if errors.Is(response.err, io.EOF) {
			return false, nil
		}
		if response.err != nil {
			return false, response.err
		}
		switch strings.ToLower(strings.TrimSpace(response.line)) {
		case "y", "yes":
			return true, nil
		default:
			return false, nil
		}
	}
}

// flashOutput serializes the Ctrl-C notice with tool output and remembers the
// first write failure without interrupting the board.
type flashOutput struct {
	mu  sync.Mutex
	w   io.Writer
	err error
}

func (out *flashOutput) Write(data []byte) (int, error) {
	out.mu.Lock()
	defer out.mu.Unlock()
	if out.err != nil {
		return 0, out.err
	}
	n, err := out.w.Write(data)
	if err == nil && n != len(data) {
		err = io.ErrShortWrite
	}
	out.err = err
	return n, err
}

func (out *flashOutput) failure() error {
	out.mu.Lock()
	defer out.mu.Unlock()
	return out.err
}

func flashCancellationNotice(ctx context.Context, output io.Writer) func() {
	var once sync.Once
	notice := func() {
		once.Do(func() {
			_ = writeOutputLine(output, "", "Finishing the firmware write. Keep the cable plugged in.")
		})
	}
	stop := context.AfterFunc(ctx, notice)
	var finishOnce sync.Once
	return func() {
		finishOnce.Do(func() {
			if !stop() || ctx.Err() != nil {
				// Waits for a notice already being written.
				notice()
			}
		})
	}
}

type flashPlan struct {
	action, installed, selected, port string
	stored                            bool
}

func writeFlashPlan(output io.Writer, plan flashPlan) (string, error) {
	var heading, prompt string
	switch plan.action {
	case "Update", "Downgrade":
		heading = fmt.Sprintf("Ready to %s your display from Pipkin %s to %s.", strings.ToLower(plan.action), plan.installed, plan.selected)
		prompt = plan.action + " now? [y/N] "
	case "Reinstall":
		heading = "Ready to reinstall Pipkin " + plan.selected + " on your display."
		prompt = "Reinstall now? [y/N] "
	default:
		heading = "Ready to install Pipkin " + plan.selected + "."
		prompt = "Confirm this is a CYD and install Pipkin? [y/N] "
	}
	lines := lineOutput{w: output}
	lines.line("", "")
	lines.line("", heading)
	if plan.stored {
		state := "installed but not responding"
		if numbers, ok := stableVersionNumbers(plan.installed); ok && numbers[0] != protocolVersion {
			state = "too old for this version of Pipkin"
		}
		lines.line("", "Current: Pipkin "+plan.installed+" ("+state+")")
	}
	if plan.action != "Install" {
		lines.line("", "Your settings are kept.")
		return prompt, lines.err
	}
	lines.line("", "Board: CYD, 2.8-inch touch display")
	lines.line("", "Port: "+plan.port)
	lines.line("", "This replaces the board's current firmware and settings.")
	return prompt, lines.err
}

type flashWriteFailure struct {
	cause   error
	written bool
	command string
}

func (failure *flashWriteFailure) Unwrap() error { return failure.cause }

func (failure *flashWriteFailure) Error() string {
	headline := "The firmware write didn't finish."
	if failure.written {
		headline = "The firmware was written, but Pipkin couldn't confirm it started."
	}
	var text bytes.Buffer
	lines := lineOutput{w: &text}
	lines.line("", headline)
	lines.line("", "The display may stay blank until the firmware is reinstalled.")
	lines.line("", "")
	lines.line("", "To fix it:")
	lines.line("  ", "1. Unplug the display, wait a few seconds, and plug it back in.")
	lines.line("  ", "2. Run this command:")
	// Keep the command on one line so it can be pasted.
	lines.printf("     %s\n\n", failure.command)
	lines.line("Details: ", failure.cause.Error())
	return strings.TrimSuffix(text.String(), "\n")
}
