package pipkin

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func testFirmwareRelease(t *testing.T, firmwareVersion string) *firmwareRelease {
	t.Helper()
	m := firmwareManifest{SchemaVersion: 1, Product: "pipkin", Version: firmwareVersion,
		SourceRevision: strings.Repeat("a", 40), Board: "esp32-2432s028r-provisional", Hardware: "unconfirmed",
		Chip: "esp32", FlashSize: 4 << 20, FlashMode: "dio", FlashFreq: "40m", ProtocolMin: 1, ProtocolMax: 1,
		MinimumCLI: "1.1.0", Layout: "esp32-single-app-v1"}
	folder := t.TempDir()
	for _, image := range []firmwareImage{
		{Role: "bootloader", File: "bootloader.bin", Offset: 0x1000, Size: 32},
		{Role: "partition-table", File: "partition-table.bin", Offset: 0x8000, Size: 0xc00},
		{Role: "application", File: "pipkin.bin", Offset: 0x10000, Size: 64},
	} {
		data := bytes.Repeat([]byte{byte(len(image.Role))}, int(image.Size))
		digest := sha256.Sum256(data)
		image.SHA256 = hex.EncodeToString(digest[:])
		if err := os.WriteFile(filepath.Join(folder, image.File), data, 0o600); err != nil {
			t.Fatal(err)
		}
		m.Images = append(m.Images, image)
	}
	return &firmwareRelease{Manifest: m, Folder: folder, Tag: "v" + firmwareVersion}
}

func testFlashIdentity(firmwareVersion string) map[string]string {
	return map[string]string{"v": "1", "kind": "identity", "product": "pipkin", "firmware": firmwareVersion,
		"chip": "esp32", "board": "esp32-2432s028r-provisional", "hardware": "unconfirmed",
		"protocol_min": "1", "protocol_max": "1", "seq": "0", "clock_epoch": "0", "unix": "null"}
}

type fakeFirmwareFlasher struct {
	probe                  flashProbe
	table                  []byte
	prefix                 []byte
	probes, resets, erases int
	writes                 [][]flashWriteImage
	writeErr, resetErr     error
	probeErr, readErr      error
	secondMAC              string
	afterWrite             func()
}

func (f *fakeFirmwareFlasher) Probe(context.Context, string) (flashProbe, error) {
	f.probes++
	probe := f.probe
	if f.probes == 2 && f.secondMAC != "" {
		probe.MAC = f.secondMAC
	}
	return probe, f.probeErr
}
func (f *fakeFirmwareFlasher) Read(_ context.Context, _ string, offset, size int64) ([]byte, error) {
	if offset == 0x10000 {
		return f.prefix, f.readErr
	}
	return f.table, f.readErr
}
func (f *fakeFirmwareFlasher) Write(_ context.Context, _ string, images []flashWriteImage) error {
	f.writes = append(f.writes, images)
	if f.afterWrite != nil {
		f.afterWrite()
	}
	return f.writeErr
}
func (f *fakeFirmwareFlasher) Reset(context.Context, string) error {
	f.resets++
	return f.resetErr
}
func (f *fakeFirmwareFlasher) EraseSettings(context.Context, string) error {
	f.erases++
	return nil
}

type flashFixture struct {
	release             *firmwareRelease
	tool                *fakeFirmwareFlasher
	initial, final      map[string]string
	running             bool
	stops, starts       int
	identities          int
	loadErr, prepareErr error
	stopErr, startErr   error
	output              bytes.Buffer
	actions             flashActions
}

func newFlashFixture(t *testing.T) *flashFixture {
	t.Helper()
	t.Setenv("PIPKIN_HOME", filepath.Join(t.TempDir(), "pipkin"))
	oldVersion := version
	version = "1.1.0"
	t.Cleanup(func() { version = oldVersion })
	f := &flashFixture{release: testFirmwareRelease(t, "1.0.1"), running: true, final: testFlashIdentity("1.0.1")}
	_, table := f.release.image("partition-table")
	data, err := os.ReadFile(table)
	if err != nil {
		t.Fatal(err)
	}
	f.tool = &fakeFirmwareFlasher{probe: flashProbe{Chip: "esp32", FlashBytes: 4 << 20, MAC: "aa:bb:cc:dd:ee:ff"}, table: data}
	f.actions = flashActions{
		load:    func(context.Context, string) (*firmwareRelease, error) { return f.release, f.loadErr },
		prepare: func(context.Context, io.Writer) (firmwareFlasher, error) { return f.tool, f.prepareErr },
		ports:   func() []string { return []string{"TESTPORT"} },
		identity: func(context.Context, string, time.Duration) (map[string]string, error) {
			f.identities++
			if f.identities == 1 {
				return f.initial, nil
			}
			return f.final, nil
		},
		running: func() (bool, error) { return f.running, nil },
		stop:    func() error { f.stops++; return f.stopErr },
		start: func() error {
			f.starts++
			// The restored helper must be able to pass its startup lock.
			lock, err := acquireFileLock(maintenanceLockPath())
			if err != nil {
				t.Fatal("helper restoration still holds the flash lock", err)
			}
			lock.Close()
			return f.startErr
		},
	}
	return f
}

func (f *flashFixture) run(ctx context.Context, options flashOptions, answer string) error {
	return runFlash(ctx, options, strings.NewReader(answer), &f.output, f.actions)
}

func TestFlashFirstInstallAndUpdate(t *testing.T) {
	for _, update := range []bool{false, true} {
		t.Run(map[bool]string{false: "first-install", true: "update"}[update], func(t *testing.T) {
			f := newFlashFixture(t)
			if update {
				f.initial = testFlashIdentity("1.0.0")
			}
			if err := f.run(context.Background(), flashOptions{}, "yes\n"); err != nil {
				t.Fatal(err)
			}
			wantImages, wantErase := 3, 1
			if update {
				wantImages, wantErase = 1, 0
			}
			if len(f.tool.writes) != 1 || len(f.tool.writes[0]) != wantImages || f.tool.erases != wantErase || f.tool.resets != 1 || f.stops != 1 || f.starts != 1 {
				t.Fatalf("writes=%v erases=%d resets=%d stops/starts=%d/%d", f.tool.writes, f.tool.erases, f.tool.resets, f.stops, f.starts)
			}
			if update && f.tool.writes[0][0].Offset != 0x10000 {
				t.Fatal("update touched bootloader, partitions or settings")
			}
			if !strings.Contains(f.output.String(), "Confirm this is a CYD") || !strings.Contains(f.output.String(), "1.0.1 is running") {
				t.Fatal(f.output.String())
			}
		})
	}
}

func TestFlashDeclineAndEOFNeverWrite(t *testing.T) {
	for _, input := range []string{"", "y", "yes", "\n", "no\n", "maybe\n", "yes please\n"} {
		t.Run(input, func(t *testing.T) {
			f := newFlashFixture(t)
			if err := f.run(context.Background(), flashOptions{}, input); err != nil {
				t.Fatal(err)
			}
			if len(f.tool.writes) != 0 || f.tool.erases != 0 || f.tool.resets != 1 || f.starts != 1 || !strings.Contains(f.output.String(), "Cancelled") {
				t.Fatal("decline or EOF did not safely restore board/helper")
			}
		})
	}
}

func TestFlashRejectsIncompatibleAndChangedDevices(t *testing.T) {
	for _, scenario := range []string{"chip", "flash-size", "secure", "identity", "layout", "changed-board", "probe-failed", "read-failed"} {
		t.Run(scenario, func(t *testing.T) {
			f := newFlashFixture(t)
			switch scenario {
			case "chip":
				f.tool.probe.Chip = "esp32s3"
			case "flash-size":
				f.tool.probe.FlashBytes = 2 << 20
			case "secure":
				f.tool.probe.Secure = true
			case "identity":
				f.initial = testFlashIdentity("1.0.0")
				delete(f.initial, "board")
			case "layout":
				f.initial = testFlashIdentity("1.0.0")
				f.tool.table = []byte("different partition layout")
			case "changed-board":
				f.tool.secondMAC = "11:22:33:44:55:66"
			case "probe-failed":
				f.tool.probeErr = errors.New("bootloader unavailable")
			case "read-failed":
				f.tool.readErr = errors.New("read failed")
			}
			if err := f.run(context.Background(), flashOptions{}, "yes\n"); err == nil {
				t.Fatal("unsupported device accepted")
			}
			if len(f.tool.writes) != 0 || f.tool.erases != 0 || f.starts != 1 {
				t.Fatal("device rejection modified flash or failed to restore helper")
			}
		})
	}
}

func TestFlashUpToDateReinstallAndDowngrade(t *testing.T) {
	for _, scenario := range []string{"up-to-date", "reinstall", "newer", "explicit-downgrade"} {
		t.Run(scenario, func(t *testing.T) {
			f := newFlashFixture(t)
			f.initial = testFlashIdentity("1.0.1")
			options := flashOptions{}
			if scenario == "reinstall" {
				options.Reinstall = true
			}
			if scenario == "newer" || scenario == "explicit-downgrade" {
				f.initial["firmware"] = "2.0.0"
			}
			if scenario == "explicit-downgrade" {
				options.Version = "1.0.1"
			}
			err := f.run(context.Background(), options, "y\n")
			if (scenario == "newer") != (err != nil) {
				t.Fatal(err)
			}
			wantWrites := 0
			if scenario == "reinstall" || scenario == "explicit-downgrade" {
				wantWrites = 1
			}
			if len(f.tool.writes) != wantWrites || f.tool.erases != 0 || f.starts != 1 {
				t.Fatal("version selection or restoration failed")
			}
		})
	}
}

func TestFlashStoredPipkinFirmwarePreservesSettings(t *testing.T) {
	for _, scenario := range []string{"recover-update", "recover-reinstall", "recover-reinstall-v-prefix", "same-needs-reinstall", "same-v-prefix-needs-reinstall", "unsupported-layout", "newer-stored"} {
		t.Run(scenario, func(t *testing.T) {
			f := newFlashFixture(t)
			storedVersion := "1.0.0"
			options := flashOptions{}
			if scenario == "recover-reinstall" || scenario == "same-needs-reinstall" {
				storedVersion = "1.0.1"
				options.Reinstall = scenario == "recover-reinstall"
			}
			if scenario == "recover-reinstall-v-prefix" || scenario == "same-v-prefix-needs-reinstall" {
				storedVersion = "v1.0.1"
				options.Reinstall = scenario == "recover-reinstall-v-prefix"
			}
			if scenario == "unsupported-layout" {
				f.tool.table = []byte("different layout")
			}
			if scenario == "newer-stored" {
				storedVersion = "2.0.0"
			}
			f.tool.prefix = buildTestESPImage(storedVersion, strings.Repeat("a", 40))
			err := f.run(context.Background(), options, "yes\n")
			wantWrite := scenario == "recover-update" || scenario == "recover-reinstall" || scenario == "recover-reinstall-v-prefix"
			if wantWrite != (err == nil) || f.tool.erases != 0 || f.starts != 1 {
				t.Fatalf("stored firmware recovery: err=%v erases=%d starts=%d", err, f.tool.erases, f.starts)
			}
			if wantWrite {
				if len(f.tool.writes) != 1 || len(f.tool.writes[0]) != 1 || f.tool.writes[0][0].Offset != 0x10000 || !strings.Contains(f.output.String(), "stored; not responding") {
					t.Fatal("nonresponding kit was treated as a new board", f.output.String())
				}
				if options.Reinstall && !strings.Contains(f.output.String(), "Reinstall: Pipkin 1.0.1") {
					t.Fatal("stored firmware reinstall was labelled as an update", f.output.String())
				}
			} else if len(f.tool.writes) != 0 {
				t.Fatal("rejected stored firmware was written")
			}
		})
	}
}

func TestFlashOrdersFirstInstallImagesBeforeErasingSettings(t *testing.T) {
	f := newFlashFixture(t)
	images := f.release.Manifest.Images
	images[0], images[2] = images[2], images[0]
	if err := f.run(context.Background(), flashOptions{}, "yes\n"); err != nil {
		t.Fatal(err)
	}
	if len(f.tool.writes) != 1 || len(f.tool.writes[0]) != 3 {
		t.Fatal("missing first-install images")
	}
	for i, offset := range []int64{0x1000, 0x8000, 0x10000} {
		if f.tool.writes[0][i].Offset != offset {
			t.Fatal("image ordering would fail after settings were erased")
		}
	}
}

func TestFlashFailureRestoresHelperAndReportsRecovery(t *testing.T) {
	for _, scenario := range []string{"download", "tool", "stop", "write", "wrong-version", "no-boot", "restore"} {
		t.Run(scenario, func(t *testing.T) {
			f := newFlashFixture(t)
			boom := errors.New("simulated failure")
			switch scenario {
			case "download":
				f.loadErr = boom
			case "tool":
				f.prepareErr = boom
			case "stop":
				f.stopErr = boom
			case "write":
				f.tool.writeErr = boom
			case "wrong-version":
				f.final["firmware"] = "1.0.0"
			case "no-boot":
				f.final = nil
			case "restore":
				f.startErr = boom
			}
			err := f.run(context.Background(), flashOptions{}, "y\n")
			if err == nil {
				t.Fatal("failure reported success")
			}
			if scenario == "download" || scenario == "tool" {
				if f.stops != 0 || f.starts != 0 || f.tool.erases != 0 {
					t.Fatal("preparation failure disturbed helper/board")
				}
			} else if f.starts != 1 {
				t.Fatal("helper not restored on failure")
			}
			if len(f.tool.writes) != 0 && scenario != "restore" && !strings.Contains(err.Error(), "no automatic rollback") {
				t.Fatal("write failure did not explain recovery", err)
			}
			if scenario == "restore" && strings.Contains(err.Error(), "retry pipkin flash") {
				t.Fatal("helper failure asked to reflash verified running firmware", err)
			}
		})
	}
}

func TestFlashCancellationRestoresStoppedHelper(t *testing.T) {
	f := newFlashFixture(t)
	ctx, cancel := context.WithCancel(context.Background())
	f.tool.afterWrite = cancel
	f.tool.writeErr = context.Canceled
	if err := f.run(ctx, flashOptions{}, "y\n"); !errors.Is(err, context.Canceled) || f.starts != 1 || f.tool.resets != 1 {
		t.Fatal("cancelled write was not recovered", err)
	}
	f = newFlashFixture(t)
	f.running = false
	if err := f.run(context.Background(), flashOptions{}, "n\n"); err != nil || f.stops != 0 || f.starts != 0 {
		t.Fatal("a stopped helper was incorrectly started", err)
	}
}

func TestFlashConfirmationBoundaries(t *testing.T) {
	for _, answer := range []string{"y\n", "YES\r\n", "  yes \n"} {
		if yes, err := confirmFlash(context.Background(), strings.NewReader(answer), io.Discard); !yes || err != nil {
			t.Fatal(answer, yes, err)
		}
	}
	if yes, err := confirmFlash(context.Background(), strings.NewReader(strings.Repeat("y", 1024)+"\n"), io.Discard); yes || err == nil {
		t.Fatal("overlong answer accepted")
	}
	ctx, cancel := context.WithCancel(context.Background())
	reader, writer := io.Pipe()
	defer reader.Close()
	defer writer.Close()
	cancel()
	if yes, err := confirmFlash(ctx, reader, io.Discard); yes || !errors.Is(err, context.Canceled) {
		t.Fatal("cancelled confirmation accepted", yes, err)
	}
}

func TestFlashCLIOptionsAndAmbiguousPorts(t *testing.T) {
	for _, args := range [][]string{{"--yes"}, {"--port"}, {"--port", ""}, {"--version", ""}, {"--version", "main"}, {"--version", "1.0.0-beta"}, {"--port", "-oops"}, {"--port", "COM1\n"}, {"--version", strings.Repeat("9", 32) + ".0.0"}, {"extra"}} {
		if _, err := parseFlashOptions(args); err == nil {
			t.Fatal("invalid flash options accepted", args)
		}
	}
	options, err := parseFlashOptions([]string{"--port", "COM3", "--version", "v1.0.0", "--reinstall"})
	if err != nil || options.Port != "COM3" || options.Version != "1.0.0" || !options.Reinstall {
		t.Fatal(options, err)
	}
	for _, candidates := range [][]string{nil, {"one", "two"}} {
		if _, err := selectFlashPort("", candidates); err == nil {
			t.Fatal("ambiguous/missing port accepted")
		}
	}
	if port, err := selectFlashPort("chosen", []string{"one", "two"}); err != nil || port != "chosen" {
		t.Fatal(port, err)
	}
	var argsSeen []string
	code, err := dispatch([]string{"flash", "--port", "COM4"}, "1.1.0", "", "", io.Discard,
		cliHandlers{flash: func(args []string) error { argsSeen = args; return nil }})
	if code != 0 || err != nil || len(argsSeen) != 2 {
		t.Fatal("flash route failed", code, err, argsSeen)
	}
	for _, args := range [][]string{{"flash", "--help"}, {"help", "flash"}} {
		calls := 0
		var output bytes.Buffer
		if code, err := dispatch(args, "1.1.0", "", "", &output, recordingHandlers(&calls)); code != 0 || err != nil || calls != 0 || !strings.Contains(output.String(), "--reinstall") {
			t.Fatal("flash help has side effects", code, err)
		}
	}
}

func TestFirmwareManifestRejectsUnsafeContracts(t *testing.T) {
	base := testFirmwareRelease(t, "1.0.0").Manifest
	for _, change := range []func(*firmwareManifest){
		func(m *firmwareManifest) { m.SchemaVersion = 2 },
		func(m *firmwareManifest) { m.Version = "1.0.1" },
		func(m *firmwareManifest) { m.SourceRevision = "unknown" },
		func(m *firmwareManifest) { m.MinimumCLI = "2.0.0" },
		func(m *firmwareManifest) { m.MinimumCLI = strings.Repeat("9", 32) + ".0.0" },
		func(m *firmwareManifest) { m.Board = "esp32-s3" },
		func(m *firmwareManifest) { m.ProtocolMin = 2 },
		func(m *firmwareManifest) { m.FlashMode = "qio" },
		func(m *firmwareManifest) { m.Images[0].Offset = 0x9000 },
		func(m *firmwareManifest) { m.Images[0].File = "../bootloader.bin" },
		func(m *firmwareManifest) { m.Images[0].Size = 1 << 20 },
		func(m *firmwareManifest) { m.Images[1].Size = 4096 },
		func(m *firmwareManifest) { m.Images[2].SHA256 = "bad" },
		func(m *firmwareManifest) { m.Images[1].Role = "bootloader" },
	} {
		data, _ := json.Marshal(base)
		var manifest firmwareManifest
		json.Unmarshal(data, &manifest)
		change(&manifest)
		if err := validateFirmwareManifest(manifest, "v1.0.0", "1.1.0"); err == nil {
			t.Fatalf("unsafe contract accepted: %+v", manifest)
		}
	}
	if err := validateFirmwareManifest(base, "v1.0.0", "1.1.0"); err != nil {
		t.Fatal(err)
	}
}
