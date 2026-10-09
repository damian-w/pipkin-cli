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
	"syscall"
	"time"

	"github.com/mattn/go-isatty"
)

type flashOptions struct {
	Port, Version, Board string
	Reinstall            bool
}

// parseOptions parses flag-only arguments. The named string options need a value.
func parseOptions(name string, args []string, define func(*flag.FlagSet), valued ...string) error {
	flags := flag.NewFlagSet(name, flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	define(flags)
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 0 {
		return fmt.Errorf("%s takes options only", name)
	}
	var empty string
	flags.Visit(func(option *flag.Flag) {
		if slices.Contains(valued, option.Name) && option.Value.String() == "" {
			empty = option.Name
		}
	})
	if empty != "" {
		return fmt.Errorf("--%s requires a value", empty)
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
		flags.StringVar(&options.Board, "board", "", "physically confirmed board profile")
		flags.BoolVar(&options.Reinstall, "reinstall", false, "reinstall the same firmware version")
	}, "port", "version", "board"); err != nil {
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
	if err := validateIdentifyBoard(options.Board); err != nil {
		return options, err
	}
	return options, nil
}

type firmwareFlasher interface {
	boardInspector
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
			return publicFirmwareSource().stage(ctx, selected, version, filepath.Join(appDir(), "cache", "firmware"))
		},
		prepare: func(ctx context.Context, output io.Writer) (firmwareFlasher, error) {
			return prepareFlashTool(ctx, output)
		},
	}
}

// boardActions free the display's serial port from the helper for a board operation.
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

// boardSession holds the installation and maintenance locks for one board
// operation. close resets the board and resumes the helper independently of
// the operation's cancelled context.
type boardSession struct {
	boardActions
	installation, maintenance *os.File
	port                      string
	reset                     func(context.Context, string) error // set once ROM probing may reset the board
	restoreHelper             bool
}

func openBoardSession(actions boardActions, purpose string) (*boardSession, error) {
	installation, err := acquireFileLock(installationLockPath())
	if err != nil {
		return nil, fmt.Errorf("could not lock installation for %s: %w", purpose, err)
	}
	maintenance, err := acquireFileLock(maintenanceLockPath())
	if err != nil {
		installation.Close()
		return nil, fmt.Errorf("another board or firmware operation is in progress: %w", err)
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
			*result = errors.Join(*result, fmt.Errorf("could not resume the helper; run pipkin start: %w", err))
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
		return errors.New("flash requires an interactive terminal for confirmation; run pipkin flash in a terminal")
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
		return "", errors.New("no USB serial board found; connect the CYD with a data cable, or use --port PORT")
	case 1:
		return candidates[0], nil
	default:
		var ports []string
		for _, port := range candidates {
			ports = append(ports, terminalText(port))
		}
		return "", fmt.Errorf("multiple USB serial boards found (%s); choose one with --port PORT", strings.Join(ports, ", "))
	}
}

func validateFlashIdentity(identity map[string]string, manifest firmwareManifest) error {
	if identity == nil {
		return nil
	}
	if !isIdentity(identity) || identity["firmware"] == "" || identity["chip"] != manifest.Chip || !sameFirmwareBoard(identity["board"], manifest.Board) || identity["protocol_min"] != "1" || identity["protocol_max"] == "" || (identity["hardware"] != "unconfirmed" && identity["hardware"] != "confirmed") {
		return errors.New("connected Pipkin reports an unsupported or incomplete board identity; no firmware was written")
	}
	return nil
}

func validateFlashProbe(probe flashProbe, manifest firmwareManifest) error {
	if probe.Chip != manifest.Chip || probe.FlashBytes != manifest.FlashSize || probe.MAC == "" {
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
	session, err := openBoardSession(actions.boardActions, "flashing")
	if err != nil {
		return err
	}
	var selectedVersion string
	writeStarted, firmwareRunning := false, false
	defer func() {
		session.close(&result)
		if result != nil && writeStarted && !firmwareRunning {
			result = fmt.Errorf("%w; reconnect USB and retry pipkin flash --port %s --version %s --reinstall (there is no automatic rollback)", result, terminalText(session.port), selectedVersion)
		}
	}()
	port, err := selectFlashPort(options.Port, actions.ports())
	if err != nil {
		return err
	}
	session.port = port
	if err := ctx.Err(); err != nil {
		return err
	}
	if _, err := fmt.Fprintln(output, "Checking firmware release…"); err != nil {
		return err
	}
	release, err := actions.load(ctx, options.Version)
	if err != nil {
		return fmt.Errorf("firmware release check failed: %w", err)
	}
	defer release.close()
	selectedVersion = release.Manifest.Version
	// Reject invalid injected or stale metadata before preparing the flasher too.
	if err := validateFirmwareManifest(release.Manifest, release.Tag, version); err != nil {
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
	current, installed := "Not identified as Pipkin", ""
	if identity != nil {
		current = "Pipkin " + terminalText(identity["firmware"])
		installed = strings.TrimPrefix(identity["firmware"], "v")
		if installed == release.Manifest.Version && !options.Reinstall {
			_, err := fmt.Fprintf(output, "Pipkin firmware %s is up to date. Use --reinstall to flash it again.\n", release.Manifest.Version)
			if err == nil && options.Board != "" {
				_, err = fmt.Fprintf(output, "To remember the physical board without flashing, run pipkin identify --board %s.\n", terminalText(options.Board))
			}
			return err
		}
		if newerStableFirmware(identity["firmware"], release.Manifest.Version) && options.Version == "" {
			return errors.New("the board has newer firmware than the latest published release; use --version VERSION to explicitly select a downgrade")
		}
	}
	session.reset = tool.Reset // ROM probing can reset even when it fails midway.
	probe, err := tool.Probe(ctx, port)
	if err != nil {
		return fmt.Errorf("could not check the ESP32 bootloader (close other serial tools, or hold BOOT while connecting): %w", err)
	}
	if err := validateFlashProbe(probe, release.Manifest); err != nil {
		return err
	}
	var boardInfo identifyBoardInfo
	if options.Board != "" {
		p, v, err := physicalBoardProfile(options.Board)
		if err != nil {
			return err
		}
		boardInfo = catalogBoardInfo(p, v, "confirmed for this flash")
	} else {
		boardInfo, err = inspectBoard(probe, "")
		if err != nil {
			return err
		}
	}
	if boardInfo.Profile != "" && !sameFirmwareBoard(boardInfo.FirmwareProfile, release.Manifest.Board) {
		return errors.New("confirmed board profile is incompatible with the selected firmware; no firmware was written")
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
			current = "Pipkin " + terminalText(storedVersion) + " (stored; not responding)"
			installed = strings.TrimPrefix(storedVersion, "v")
			if installed == release.Manifest.Version && !options.Reinstall {
				return fmt.Errorf("Pipkin firmware %s is installed but did not respond; use --reinstall to repair it", storedVersion)
			}
			if newerStableFirmware(storedVersion, release.Manifest.Version) && options.Version == "" {
				return errors.New("stored Pipkin firmware is newer than the latest published release; use --version VERSION to explicitly select a downgrade")
			}
		}
	}
	if updating && !bytes.Equal(existingTable, expectedTable) {
		return errors.New("existing Pipkin uses a different partition layout; this CLI cannot migrate it safely, so no firmware was written; use the source-build guide")
	}
	action, effect := "Install", "Replaces existing board firmware and settings."
	if updating {
		action, effect = "Update", "Keeps the board's selected page."
		if options.Reinstall && installed == release.Manifest.Version {
			action = "Reinstall"
		} else if newerStableFirmware(installed, release.Manifest.Version) {
			action = "Downgrade"
		}
	}
	boardName := "CYD, 2.8-inch touch display (confirm below)"
	if boardInfo.Profile != "" {
		boardName = boardInfo.Name + " (" + boardInfo.Profile + ")"
	}
	plan := fmt.Sprintf("\nDevice:  ESP32, 4 MB — %s\nBoard:   %s\nCurrent: %s\n%s: %s\n", terminalText(port), terminalText(boardName), current, action, "Pipkin "+release.Manifest.Version)
	if boardInfo.Profile == "" {
		plan += "Physical board profile is unconfirmed; inspect the PCB and use pipkin identify --board PROFILE to remember it.\n"
	}
	plan += "\n" + effect + " Keep USB connected until finished.\n"
	if _, err := io.WriteString(output, plan); err != nil {
		return err
	}
	confirmed, err := confirmFlash(ctx, input, output)
	if err != nil {
		return err
	}
	if !confirmed {
		_, err := fmt.Fprintln(output, "Cancelled. No firmware was written.")
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	// A user can unplug or exchange boards while deciding. Recheck the physical
	// device before the first write, rather than trusting its serial-port pathname.
	confirmedProbe, err := tool.Probe(ctx, port)
	if err != nil {
		return err
	}
	if err := validateFlashProbe(confirmedProbe, release.Manifest); err != nil {
		return err
	}
	if confirmedProbe.MAC != probe.MAC {
		return errors.New("the board changed after confirmation; no firmware was written, run pipkin flash again")
	}
	if options.Board != "" {
		if _, err := inspectBoard(confirmedProbe, options.Board); err != nil {
			return err
		}
	}
	var images []flashWriteImage
	for _, image := range release.Manifest.Images {
		if !updating || image.Role == "application" {
			images = append(images, flashWriteImage{Path: filepath.Join(release.Folder, image.File), Offset: image.Offset})
		}
	}
	sort.Slice(images, func(i, j int) bool { return images[i].Offset < images[j].Offset })
	writeStarted = true
	if !updating {
		if err := tool.EraseSettings(ctx, port); err != nil {
			return fmt.Errorf("could not initialize board settings: %w", err)
		}
	}
	if err := tool.Write(ctx, port, images); err != nil {
		return fmt.Errorf("firmware write or verification failed: %w", err)
	}
	if err := tool.Reset(ctx, port); err != nil {
		return fmt.Errorf("firmware written, but board restart failed: %w", err)
	}
	session.reset = nil
	booted, err := actions.identity(ctx, port, 12*time.Second)
	if err != nil {
		return fmt.Errorf("firmware written, but startup could not be checked: %w", err)
	}
	if booted == nil || validateFlashIdentity(booted, release.Manifest) != nil || strings.TrimPrefix(booted["firmware"], "v") != release.Manifest.Version {
		return errors.New("firmware written and verified, but the expected Pipkin version did not respond after restart")
	}
	firmwareRunning = true
	_, err = fmt.Fprintf(output, "Pipkin firmware %s is running.\n", release.Manifest.Version)
	return err
}

func confirmFlash(ctx context.Context, input io.Reader, output io.Writer) (bool, error) {
	if _, err := io.WriteString(output, "Confirm this is a CYD and flash? [y/N] "); err != nil {
		return false, err
	}
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
