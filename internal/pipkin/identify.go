package pipkin

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/url"
	"os"
	"os/signal"
	"runtime"
	"strings"
	"syscall"
)

const identifyHelp = `
Inspect the ESP32 chip, flash, security state and responding Pipkin firmware.
The display temporarily restarts, and a running helper is paused and resumed.
Firmware and board settings are left intact. PCB revision, display and touch
controller cannot be identified reliably from the ESP32 alone.

  --port PORT      Choose a serial port when more than one board is connected.
  --json           Print a complete local report (includes device identifiers).
  --issue          Print a GitHub issue draft with a physical-board checklist.
  --board PROFILE  Confirm a profile after checking the physical board and
                   remember it for this ESP32's MAC address for future flashing.

Review and complete the --issue draft before submitting it. No issue is sent
automatically. The draft omits MAC addresses, USB serial numbers and local paths.
`

type identifyOptions struct {
	Port, Board string
	JSON, Issue bool
}

func parseIdentifyOptions(args []string) (identifyOptions, error) {
	var options identifyOptions
	if err := parseOptions("identify", args, func(flags *flag.FlagSet) {
		flags.StringVar(&options.Port, "port", "", "serial port")
		flags.StringVar(&options.Board, "board", "", "physically confirmed board profile")
		flags.BoolVar(&options.JSON, "json", false, "local JSON report")
		flags.BoolVar(&options.Issue, "issue", false, "GitHub issue draft")
	}, "port", "board"); err != nil {
		return options, err
	}
	if options.JSON && options.Issue {
		return options, errors.New("--json and --issue cannot be combined")
	}
	if !validPortOption(options.Port) {
		return options, errors.New("invalid serial port")
	}
	return options, validateIdentifyBoard(options.Board)
}

// Board inspection has no access to flash reads, writes or erases.
type boardInspector interface {
	Probe(context.Context, string) (flashProbe, error)
	Reset(context.Context, string) error
}

type identifyUSBInfo struct {
	VendorID     string `json:"vendor_id,omitempty"`
	ProductID    string `json:"product_id,omitempty"`
	Manufacturer string `json:"manufacturer,omitempty"`
	Product      string `json:"product,omitempty"`
	SerialNumber string `json:"serial_number,omitempty"`
}

type identifyBoardInfo struct {
	Profile           string   `json:"profile,omitempty"`
	Name              string   `json:"name,omitempty"`
	FirmwareProfile   string   `json:"firmware_profile,omitempty"`
	Status            string   `json:"status"`
	Candidates        []string `json:"candidates,omitempty"`
	PCBMarkings       []string `json:"pcb_markings,omitempty"`
	ModuleMarkings    []string `json:"module_markings,omitempty"`
	USBConnectors     []string `json:"usb_connectors,omitempty"`
	DisplayMarking    string   `json:"display_marking,omitempty"`
	DisplaySizeInches float64  `json:"display_size_inches,omitempty"`
	DisplayWidth      int      `json:"display_width,omitempty"`
	DisplayHeight     int      `json:"display_height,omitempty"`
	DisplayController string   `json:"display_controller,omitempty"`
	TouchController   string   `json:"touch_controller,omitempty"`
}

type identifyActions struct {
	boardActions
	prepare func(context.Context, io.Writer) (boardInspector, error)
	board   func(flashProbe, string) (identifyBoardInfo, error)
	usb     func(string) identifyUSBInfo
}

func defaultIdentifyActions() identifyActions {
	return identifyActions{
		boardActions: defaultBoardActions(),
		prepare: func(ctx context.Context, output io.Writer) (boardInspector, error) {
			return prepareFlashTool(ctx, output)
		},
		board: inspectBoard, usb: readIdentifyUSB,
	}
}

func identifyCommand(args []string) error {
	options, err := parseIdentifyOptions(args)
	if err != nil {
		return err
	}
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	return runIdentify(ctx, options, os.Stdout, os.Stderr, defaultIdentifyActions())
}

type identifyChipInfo struct {
	Family      string `json:"family"`
	Description string `json:"description,omitempty"`
	Features    string `json:"features,omitempty"`
	CrystalMHz  int    `json:"crystal_mhz,omitempty"`
	MAC         string `json:"mac"`
}

type identifyFlashInfo struct {
	Bytes        int64  `json:"bytes"`
	Manufacturer string `json:"manufacturer_id,omitempty"`
	Device       string `json:"device_id,omitempty"`
}

type identifySecurityInfo struct {
	SecureBoot     bool `json:"secure_boot"`
	FlashEncrypted bool `json:"flash_encrypted"`
}

type identifyFirmwareInfo struct {
	Responding  bool   `json:"responding"`
	Product     string `json:"product,omitempty"`
	Version     string `json:"version,omitempty"`
	Chip        string `json:"chip,omitempty"`
	Board       string `json:"board,omitempty"`
	Hardware    string `json:"hardware,omitempty"`
	ProtocolMin string `json:"protocol_min,omitempty"`
	ProtocolMax string `json:"protocol_max,omitempty"`
}

type identifyReport struct {
	SchemaVersion int                  `json:"schema_version"`
	CLIVersion    string               `json:"cli_version"`
	HostPlatform  string               `json:"host_platform"`
	Port          string               `json:"port"`
	USB           identifyUSBInfo      `json:"usb"`
	Chip          identifyChipInfo     `json:"chip"`
	Flash         identifyFlashInfo    `json:"flash"`
	Security      identifySecurityInfo `json:"security"`
	Firmware      identifyFirmwareInfo `json:"firmware"`
	Board         identifyBoardInfo    `json:"board"`
}

func buildIdentifyReport(port string, usb identifyUSBInfo, probe flashProbe, identity map[string]string, board identifyBoardInfo) identifyReport {
	report := identifyReport{
		SchemaVersion: 1, CLIVersion: version, HostPlatform: runtime.GOOS + "/" + runtime.GOARCH,
		Port: port, USB: usb,
		Chip: identifyChipInfo{Family: probe.Chip, Description: probe.ChipDescription,
			Features: probe.Features, CrystalMHz: probe.CrystalMHz, MAC: probe.MAC},
		Flash:    identifyFlashInfo{Bytes: probe.FlashBytes, Manufacturer: probe.FlashManufacturer, Device: probe.FlashDevice},
		Security: identifySecurityInfo{SecureBoot: probe.SecureBoot, FlashEncrypted: probe.FlashEncrypted},
		Board:    board,
	}
	if isIdentity(identity) {
		report.Firmware = identifyFirmwareInfo{Responding: true, Product: identity["product"], Version: identity["firmware"],
			Chip: identity["chip"], Board: identity["board"], Hardware: identity["hardware"],
			ProtocolMin: identity["protocol_min"], ProtocolMax: identity["protocol_max"]}
	}
	return report
}

func runIdentify(ctx context.Context, options identifyOptions, output, progress io.Writer, actions identifyActions) (result error) {
	session, err := openBoardSession(actions.boardActions, "board inspection")
	if err != nil {
		return err
	}
	defer session.close(&result)
	port, err := selectFlashPort(options.Port, actions.ports())
	if err != nil {
		return err
	}
	session.port = port
	if err := ctx.Err(); err != nil {
		return err
	}
	if _, err := fmt.Fprintln(progress, "Inspecting the board; the display will temporarily restart…"); err != nil {
		return err
	}
	tool, err := actions.prepare(ctx, progress)
	if err != nil {
		return fmt.Errorf("could not prepare the board inspection tool: %w", err)
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	identity, err := session.claimPort(ctx)
	if err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	session.reset = tool.Reset // A failed or interrupted ROM probe may still reset.
	probe, err := tool.Probe(ctx, port)
	if err != nil {
		return fmt.Errorf("could not inspect the ESP32 bootloader (close other serial tools, or hold BOOT while connecting): %w", err)
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	board, err := actions.board(probe, options.Board)
	if err != nil {
		return err
	}
	report := buildIdentifyReport(port, actions.usb(port), probe, identity, board)
	if options.JSON {
		encoder := json.NewEncoder(output)
		encoder.SetIndent("", "  ")
		return encoder.Encode(report)
	}
	if options.Issue {
		return writeIdentifyIssue(output, report)
	}
	return writeIdentifySummary(output, report)
}

func identifyValue(value string) string {
	if value == "" {
		return "not reported"
	}
	return terminalText(value)
}

func writeIdentifySummary(output io.Writer, report identifyReport) error {
	var text strings.Builder
	fmt.Fprintf(&text, "Port:           %s\n", identifyValue(report.Port))
	if report.USB.VendorID != "" || report.USB.ProductID != "" {
		fmt.Fprintf(&text, "USB:            %s:%s (%s)\n", identifyValue(report.USB.VendorID), identifyValue(report.USB.ProductID), identifyValue(report.USB.Product))
	}
	description := report.Chip.Description
	if description == "" {
		description = report.Chip.Family
	}
	fmt.Fprintf(&text, "Chip:           %s\nFeatures:       %s\nCrystal:        %s\n", identifyValue(description), identifyValue(report.Chip.Features), identifyCrystal(report.Chip.CrystalMHz))
	fmt.Fprintf(&text, "Flash:          %d bytes (%d MB)\nFlash ID:       %s:%s\nMAC:            %s\n", report.Flash.Bytes, report.Flash.Bytes/(1<<20), identifyValue(report.Flash.Manufacturer), identifyValue(report.Flash.Device), identifyValue(report.Chip.MAC))
	fmt.Fprintf(&text, "Secure boot:    %t\nFlash encrypted: %t\n", report.Security.SecureBoot, report.Security.FlashEncrypted)
	if report.Firmware.Responding {
		fmt.Fprintf(&text, "Firmware:       Pipkin %s\nFirmware board: %s (%s)\nProtocol:       %s–%s\n", identifyValue(report.Firmware.Version), identifyValue(report.Firmware.Board), identifyValue(report.Firmware.Hardware), identifyValue(report.Firmware.ProtocolMin), identifyValue(report.Firmware.ProtocolMax))
	} else {
		fmt.Fprintln(&text, "Firmware:       No Pipkin identity response")
	}
	if report.Board.Profile != "" {
		fmt.Fprintf(&text, "Board profile:  %s (%s)\n", identifyValue(report.Board.Profile), identifyValue(report.Board.Status))
		fmt.Fprintf(&text, "Board:          %s\nFirmware profile: %s\n", identifyValue(report.Board.Name), identifyValue(report.Board.FirmwareProfile))
	} else {
		fmt.Fprintln(&text, "Board profile:  Unconfirmed; chip and flash do not identify the PCB")
	}
	if fields := identifyCatalogFields(report.Board); len(fields) != 0 {
		fmt.Fprintln(&text, "Catalog observations (from the stored profile; not measured by this probe):")
		for _, field := range fields {
			fmt.Fprintf(&text, "  %s: %s\n", field.Name, identifyValue(field.Value))
		}
	}
	if len(report.Board.Candidates) != 0 {
		fmt.Fprintln(&text, "Compatible candidates (check the physical board before confirming):")
		for _, candidate := range report.Board.Candidates {
			fmt.Fprintf(&text, "  %s\n", identifyValue(candidate))
			if _, variant, err := physicalBoardProfile(candidate); err == nil {
				fmt.Fprintf(&text, "    %s\n", identifyValue(variant.Name))
				fmt.Fprintf(&text, "    PCB: %s; USB: %s\n", identifyValue(strings.Join(variant.PCBMarkings, ", ")), identifyValue(strings.Join(variant.USBConnectors, ", ")))
			}
		}
	}
	fmt.Fprint(&text, "\nUse pipkin identify --board PROFILE after checking the PCB to remember its profile.\nUse pipkin identify --issue for a report and physical-board checklist to submit on GitHub.\n")
	_, err := io.WriteString(output, text.String())
	return err
}

func identifyCrystal(mhz int) string {
	if mhz == 0 {
		return "not reported"
	}
	return fmt.Sprintf("%d MHz", mhz)
}

type identifyCatalogField struct{ Name, Value string }

func identifyCatalogFields(board identifyBoardInfo) []identifyCatalogField {
	var fields []identifyCatalogField
	add := func(name, value string) {
		if value != "" {
			fields = append(fields, identifyCatalogField{Name: name, Value: value})
		}
	}
	add("Catalog PCB markings", strings.Join(board.PCBMarkings, ", "))
	add("Catalog module markings", strings.Join(board.ModuleMarkings, ", "))
	add("Catalog USB connectors", strings.Join(board.USBConnectors, ", "))
	var display []string
	if board.DisplayMarking != "" {
		display = append(display, board.DisplayMarking)
	}
	if board.DisplaySizeInches > 0 {
		display = append(display, fmt.Sprintf("%g-inch", board.DisplaySizeInches))
	}
	if board.DisplayWidth > 0 && board.DisplayHeight > 0 {
		display = append(display, fmt.Sprintf("native %d×%d pixels", board.DisplayWidth, board.DisplayHeight))
	}
	add("Catalog display", strings.Join(display, ", "))
	add("Catalog display controller", board.DisplayController)
	add("Catalog touch controller", board.TouchController)
	return fields
}

// The issue format deliberately selects public electronics metadata. The local
// JSON report is never serialized into an issue, so host/device identifiers and
// future identity fields cannot enter the public draft by accident.
func writeIdentifyIssue(output io.Writer, report identifyReport) error {
	var text strings.Builder
	fmt.Fprint(&text, "# CYD board profile request\n\nPlease complete the physical observations below and review this draft before submitting it.\n\n## Inspected electronics\n\n| Field | Value |\n| --- | --- |\n")
	row := func(name, value string) {
		value = strings.NewReplacer("|", "\\|", "`", "\\`", "<", "&lt;", ">", "&gt;").Replace(identifyValue(value))
		fmt.Fprintf(&text, "| %s | %s |\n", name, value)
	}
	row("Pipkin CLI", report.CLIVersion)
	row("Host platform", report.HostPlatform)
	row("ESP32 family", report.Chip.Family)
	row("Chip description", report.Chip.Description)
	row("Chip features", report.Chip.Features)
	row("Crystal", identifyCrystal(report.Chip.CrystalMHz))
	row("Flash capacity", fmt.Sprintf("%d bytes", report.Flash.Bytes))
	row("Flash manufacturer ID", report.Flash.Manufacturer)
	row("Flash device ID", report.Flash.Device)
	row("Secure boot", fmt.Sprint(report.Security.SecureBoot))
	row("Flash encryption", fmt.Sprint(report.Security.FlashEncrypted))
	if report.USB.VendorID != "" || report.USB.ProductID != "" {
		row("USB vendor/product ID", report.USB.VendorID+":"+report.USB.ProductID)
		row("USB manufacturer", report.USB.Manufacturer)
		row("USB product", report.USB.Product)
	}
	row("Confirmed physical profile", report.Board.Profile)
	row("Profile status", report.Board.Status)
	row("Firmware compatibility profile", report.Board.FirmwareProfile)
	if fields := identifyCatalogFields(report.Board); len(fields) != 0 {
		row("Catalog observation source", "Stored board profile; not measured by this probe")
		for _, field := range fields {
			row(field.Name, field.Value)
		}
	}
	if len(report.Board.Candidates) != 0 {
		row("Compatible candidates; physical confirmation required", strings.Join(report.Board.Candidates, ", "))
	}
	fmt.Fprint(&text, "\n## Responding firmware\n\n")
	if report.Firmware.Responding {
		fmt.Fprint(&text, "| Field | Value |\n| --- | --- |\n")
		row("Product", report.Firmware.Product)
		row("Version", report.Firmware.Version)
		row("Firmware board", report.Firmware.Board)
		row("Firmware hardware status", report.Firmware.Hardware)
		row("Protocol minimum/maximum", report.Firmware.ProtocolMin+"/"+report.Firmware.ProtocolMax)
	} else {
		fmt.Fprint(&text, "No Pipkin identity response. This does not identify other installed firmware.\n")
	}
	fmt.Fprint(&text, `
## Physical board observations

- [ ] PCB model marking (copy exactly):
- [ ] PCB revision/date marking (copy exactly, or state none):
- [ ] ESP32 module marking (copy exactly):
- [ ] USB connector count and types (USB-C / Micro-USB / other):
- [ ] Front and back photos attached; cover any unique serial labels:
- [ ] Display size, resolution and any display/controller markings:
- [ ] Touch type and controller markings, if known:
- [ ] Existing firmware displays correctly (colors, orientation, backlight):
- [ ] Touch works and coordinates match the display, if tested:
- [ ] Purchase/product link, if available:

The ESP32 probe does not establish PCB revision or display/touch wiring. A
maintainer can use the observations and photos to add or verify a profile.
This draft omits MAC addresses, USB serial numbers, local ports and paths.
`)
	issueURL := "https://github.com/damian-w/pipkin/issues/new?" + url.Values{
		"template": {"board-support.yml"},
		"title":    {"Board profile request: CYD (add PCB marking)"},
	}.Encode()
	fmt.Fprintf(&text, "\nOpen [a new GitHub issue](%s), paste this draft and complete the checklist.\n", issueURL)
	_, err := io.WriteString(output, text.String())
	return err
}
