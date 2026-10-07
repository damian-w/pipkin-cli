package pipkin

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

type fakeBoardInspector struct {
	probe              flashProbe
	probeErr, resetErr error
	probes, resets     int
	onProbe            func(context.Context)
	onReset            func(context.Context)
}

func (tool *fakeBoardInspector) Probe(ctx context.Context, _ string) (flashProbe, error) {
	tool.probes++
	if tool.onProbe != nil {
		tool.onProbe(ctx)
	}
	return tool.probe, tool.probeErr
}

func (tool *fakeBoardInspector) Reset(ctx context.Context, _ string) error {
	tool.resets++
	if tool.onReset != nil {
		tool.onReset(ctx)
	}
	return tool.resetErr
}

type identifyFixture struct {
	tool                             *fakeBoardInspector
	actions                          identifyActions
	identity                         map[string]string
	running                          bool
	stops, starts                    int
	prepareErr, identityErr, stopErr error
	startErr, boardErr               error
	events                           []string
	output, progress                 bytes.Buffer
}

func newIdentifyFixture(t *testing.T) *identifyFixture {
	t.Helper()
	t.Setenv("PIPKIN_HOME", filepath.Join(t.TempDir(), "pipkin"))
	f := &identifyFixture{running: true, identity: testFlashIdentity("1.0.1")}
	f.tool = &fakeBoardInspector{probe: flashProbe{Chip: "esp32", ChipDescription: "ESP32-D0WD-V3 (revision v3.1)",
		FlashBytes: 4 << 20, MAC: "aa:bb:cc:dd:ee:ff", Features: "Wi-Fi, BT, Dual Core, 240MHz",
		CrystalMHz: 40, FlashManufacturer: "ef", FlashDevice: "4016"}}
	f.tool.onProbe = func(context.Context) { f.events = append(f.events, "probe") }
	f.tool.onReset = func(ctx context.Context) {
		f.events = append(f.events, "reset")
		if ctx.Err() != nil {
			t.Fatal("cleanup inherited cancellation")
		}
		if _, bounded := ctx.Deadline(); !bounded {
			t.Fatal("cleanup has no deadline")
		}
	}
	f.actions = identifyActions{
		prepare: func(_ context.Context, progress io.Writer) (boardInspector, error) {
			f.events = append(f.events, "prepare")
			io.WriteString(progress, "tool progress\n")
			return f.tool, f.prepareErr
		},
		ports: func() []string { return []string{"TESTPORT"} },
		identity: func(context.Context, string, time.Duration) (map[string]string, error) {
			f.events = append(f.events, "identity")
			return f.identity, f.identityErr
		},
		running: func() (bool, error) {
			f.events = append(f.events, "running")
			return f.running, nil
		},
		stop: func() error { f.stops++; f.events = append(f.events, "stop"); return f.stopErr },
		start: func() error {
			f.starts++
			f.events = append(f.events, "start")
			maintenance, err := acquireFileLock(maintenanceLockPath())
			if err != nil {
				t.Fatal("helper restoration holds the maintenance lock", err)
			}
			maintenance.Close()
			return f.startErr
		},
		board: func(_ flashProbe, requested string) (identifyBoardInfo, error) {
			f.events = append(f.events, "board")
			return identifyBoardInfo{Profile: requested, Status: "unconfirmed", Candidates: []string{"test-cyd-profile"}}, f.boardErr
		},
		usb: func(string) identifyUSBInfo {
			f.events = append(f.events, "usb")
			return identifyUSBInfo{VendorID: "1a86", ProductID: "7523", Manufacturer: "USB vendor", Product: "USB Serial", SerialNumber: "private-usb-serial"}
		},
	}
	return f
}

func (f *identifyFixture) run(ctx context.Context, options identifyOptions) error {
	return runIdentify(ctx, options, &f.output, &f.progress, f.actions)
}

func TestIdentifyInspectRestoresBoardAndHelperAndKeepsJSONClean(t *testing.T) {
	f := newIdentifyFixture(t)
	if err := f.run(context.Background(), identifyOptions{JSON: true}); err != nil {
		t.Fatal(err)
	}
	var report identifyReport
	if err := json.Unmarshal(f.output.Bytes(), &report); err != nil {
		t.Fatalf("output is not clean JSON: %v\n%s", err, f.output.String())
	}
	if report.SchemaVersion != 1 || report.Chip.MAC != f.tool.probe.MAC || report.USB.SerialNumber != "private-usb-serial" || !report.Firmware.Responding || report.Firmware.Version != "1.0.1" || report.Flash.Bytes != 4<<20 || report.Chip.CrystalMHz != 40 {
		t.Fatalf("incomplete local report: %+v", report)
	}
	if f.tool.probes != 1 || f.tool.resets != 1 || f.stops != 1 || f.starts != 1 {
		t.Fatal("inspection did not restore board/helper")
	}
	if !strings.Contains(f.progress.String(), "tool progress") || strings.Contains(f.output.String(), "tool progress") {
		t.Fatal("inspection progress entered JSON output")
	}
	if want := []string{"prepare", "running", "stop", "identity", "probe", "board", "usb", "reset", "start"}; !reflect.DeepEqual(f.events, want) {
		t.Fatalf("inspection order: %v", f.events)
	}
}

func TestIdentifyNonPipkinBoardAndStoppedHelper(t *testing.T) {
	f := newIdentifyFixture(t)
	f.identity, f.running = nil, false
	if err := f.run(context.Background(), identifyOptions{}); err != nil {
		t.Fatal(err)
	}
	if f.stops != 0 || f.starts != 0 || f.tool.resets != 1 || !strings.Contains(f.output.String(), "No Pipkin identity response") || !strings.Contains(f.output.String(), "chip and flash do not identify the PCB") {
		t.Fatal("non-Pipkin board report or stopped helper changed", f.output.String())
	}
}

func TestIdentifyFailureCleanup(t *testing.T) {
	for _, scenario := range []string{"prepare", "stop", "identity", "probe", "profile", "output", "reset", "start"} {
		t.Run(scenario, func(t *testing.T) {
			f := newIdentifyFixture(t)
			boom := errors.New("injected failure")
			wantReset, wantStart := 1, 1
			switch scenario {
			case "prepare":
				f.prepareErr = boom
				wantReset, wantStart = 0, 0
			case "stop":
				f.stopErr = boom
				wantReset = 0
			case "identity":
				f.identityErr = boom
				wantReset = 0
			case "probe":
				f.tool.probeErr = boom
			case "profile":
				f.boardErr = boom
			case "reset":
				f.tool.resetErr = boom
			case "start":
				f.startErr = boom
			}
			var err error
			if scenario == "output" {
				err = runIdentify(context.Background(), identifyOptions{}, failingCLIWriter{}, &f.progress, f.actions)
				boom = io.ErrClosedPipe
			} else {
				err = f.run(context.Background(), identifyOptions{})
			}
			if !errors.Is(err, boom) || f.tool.resets != wantReset || f.starts != wantStart {
				t.Fatalf("error=%v resets=%d starts=%d", err, f.tool.resets, f.starts)
			}
		})
	}
}

func TestIdentifyCancelledProbeUsesIndependentCleanup(t *testing.T) {
	f := newIdentifyFixture(t)
	ctx, cancel := context.WithCancel(context.Background())
	f.tool.onProbe = func(context.Context) { cancel() }
	if err := f.run(ctx, identifyOptions{}); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	if f.tool.resets != 1 || f.starts != 1 || f.output.Len() != 0 {
		t.Fatal("cancelled probe was not cleaned up")
	}
}

func TestIdentifyCannotRunAlongsideInstallationOrFlash(t *testing.T) {
	for _, lockPath := range []func() string{installationLockPath, maintenanceLockPath} {
		f := newIdentifyFixture(t)
		lock, err := acquireFileLock(lockPath())
		if err != nil {
			t.Fatal(err)
		}
		if err := f.run(context.Background(), identifyOptions{}); err == nil || len(f.events) != 0 {
			t.Fatal("locked inspection touched hardware")
		}
		lock.Close()
	}
}

func TestIdentifyIssueDraftExcludesPrivateIdentifiers(t *testing.T) {
	f := newIdentifyFixture(t)
	if err := f.run(context.Background(), identifyOptions{Issue: true}); err != nil {
		t.Fatal(err)
	}
	text := f.output.String()
	for _, secret := range []string{f.tool.probe.MAC, "private-usb-serial", "TESTPORT", appDir()} {
		if strings.Contains(text, secret) {
			t.Fatalf("public issue draft contains %q", secret)
		}
	}
	for _, required := range []string{"PCB model marking", "PCB revision/date marking", "ESP32 module marking", "USB connector count", "Front and back photos", "Touch type", "Flash manufacturer ID", "4016", "1a86:7523", "test-cyd-profile", "1.0.1", "https://github.com/damian-w/pipkin/issues/new?template=board-support.yml&title="} {
		if !strings.Contains(text, required) {
			t.Fatalf("issue draft omits %q: %s", required, text)
		}
	}
}

func TestIdentifyReportDoesNotCopyUnexpectedIdentityFields(t *testing.T) {
	identity := testFlashIdentity("1.0.1")
	identity["credential"] = "private-credential"
	report := buildIdentifyReport("TESTPORT", identifyUSBInfo{}, flashProbe{}, identity, identifyBoardInfo{})
	encoded, err := json.Marshal(report)
	if err != nil || bytes.Contains(encoded, []byte("private-credential")) {
		t.Fatalf("unexpected firmware field escaped: %s %v", encoded, err)
	}
}

func TestIdentifyValidatesRequestedProfileBeforeHardware(t *testing.T) {
	f := newIdentifyFixture(t)
	f.actions.validateBoard = func(string) error { return errors.New("unknown profile") }
	if err := f.run(context.Background(), identifyOptions{Board: "missing-profile"}); err == nil || len(f.events) != 0 {
		t.Fatal("invalid profile touched hardware")
	}
}

func TestIdentifyCatalogObservationsAreLabeledAndIncompleteFieldsOmitted(t *testing.T) {
	board := identifyBoardInfo{
		Profile: "known-profile", Name: "Known CYD", Status: "confirmed locally",
		PCBMarkings: []string{"Bruce CYD 2432S028"}, ModuleMarkings: []string{"ESP-32S"}, USBConnectors: []string{"USB-C", "micro-USB"},
		DisplayMarking: "TPM408-2.8", DisplaySizeInches: 2.8, DisplayWidth: 240, DisplayHeight: 320,
		DisplayController: "ILI9341-compatible", TouchController: "XPT2046-compatible",
	}
	report := buildIdentifyReport("TESTPORT", identifyUSBInfo{}, flashProbe{}, nil, board)
	for _, write := range []func(io.Writer, identifyReport) error{writeIdentifySummary, writeIdentifyIssue} {
		var output bytes.Buffer
		if err := write(&output, report); err != nil {
			t.Fatal(err)
		}
		for _, wanted := range []string{"Catalog PCB markings", "Bruce CYD 2432S028", "Catalog module markings", "ESP-32S", "Catalog USB connectors", "USB-C, micro-USB", "Catalog display", "TPM408-2.8, 2.8-inch, native 240×320 pixels", "ILI9341-compatible", "XPT2046-compatible", "not measured by this probe"} {
			if !strings.Contains(output.String(), wanted) {
				t.Fatalf("catalog report omits %q: %s", wanted, output.String())
			}
		}
	}
	if fields := identifyCatalogFields(identifyBoardInfo{}); len(fields) != 0 {
		t.Fatalf("empty catalog emits values: %+v", fields)
	}
	if fields := identifyCatalogFields(identifyBoardInfo{DisplayMarking: "panel", DisplayWidth: 240}); len(fields) != 1 || fields[0].Value != "panel" {
		t.Fatalf("incomplete dimensions emit zero: %+v", fields)
	}
	encoded, err := json.Marshal(identifyBoardInfo{})
	if err != nil || bytes.Contains(encoded, []byte("display_width")) || bytes.Contains(encoded, []byte("display_size_inches")) {
		t.Fatalf("empty JSON catalog emits display values: %s %v", encoded, err)
	}
}

func TestIdentifyOptions(t *testing.T) {
	for _, args := range [][]string{{"--json", "--issue"}, {"--port="}, {"--board="}, {"--board", "MixedCase"}, {"--board", "../board"}, {"--port", "\x1b[31m"}, {"--port", " space"}, {"--unknown"}, {"extra"}} {
		if _, err := parseIdentifyOptions(args); err == nil {
			t.Fatalf("accepted %v", args)
		}
	}
	options, err := parseIdentifyOptions([]string{"--port", "TESTPORT", "--json", "--board", "test-cyd-profile"})
	if err != nil || options.Port != "TESTPORT" || !options.JSON || options.Board != "test-cyd-profile" {
		t.Fatalf("options=%+v error=%v", options, err)
	}
}
