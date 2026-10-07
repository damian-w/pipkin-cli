package pipkin

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestFlashProbeRequiresCompleteClassicESP32(t *testing.T) {
	for _, description := range []string{"ESP32-D0WDQ6 (revision v1.0)", "ESP32-D0WD-V3 (revision v3.1)", "ESP32-D0WD (revision v1.1)"} {
		data := "Connected to ESP32 on /dev/cu.usbserial-test:\nChip type: " + description + "\nMAC: AA:BB:CC:DD:EE:FF\nDetected flash size: 4MB\n"
		probe, err := parseFlashProbe([]byte(data))
		if err != nil || probe.Chip != "esp32" || probe.FlashBytes != 4<<20 || probe.MAC != "aa:bb:cc:dd:ee:ff" {
			t.Fatalf("%s: %+v, %v", description, probe, err)
		}
		for _, malformed := range []string{
			strings.Replace(data, "Connected to ESP32 on", "Connected to ESP32-S3 on", 1),
			strings.Replace(data, "Detected flash size: 4MB\n", "", 1),
			strings.Replace(data, "Detected flash size: 4MB", "Detected flash size: Unknown", 1),
			data + "MAC: aa:bb:cc:dd:ee:ff\n",
			data + "Detected flash size: 8MB\n",
			strings.Replace(data, "Connected to ESP32 on", "Chip is ESP32 on", 1),
		} {
			if _, err := parseFlashProbe([]byte(malformed)); err == nil {
				t.Fatalf("accepted incomplete/ambiguous device: %q", malformed)
			}
		}
	}
}

func TestFlashSecurityRegisterParserFailsClosed(t *testing.T) {
	value, err := parseToolRegister([]byte("0x3ff5a018 = 0x00000030\n"), 0x3ff5a018)
	if err != nil || value != 0x30 {
		t.Fatalf("value = %#x, %v", value, err)
	}
	for _, data := range []string{"", "0x3ff5a000 = 0x00000000\n", "0x3ff5a018 = 0x30\n", "0x3ff5a018 = 0x00000000\n0x3ff5a018 = 0x00000030\n"} {
		if _, err := parseToolRegister([]byte(data), 0x3ff5a018); err == nil {
			t.Fatalf("accepted %q", data)
		}
	}
}

func TestFlashProbeElectronicsDetails(t *testing.T) {
	data := "Connected to ESP32 on /dev/cu.usbserial-test:\nChip type: ESP32-D0WD-V3 (revision v3.1)\nFeatures: Wi-Fi, BT, Dual Core + LP Core, 240MHz\nCrystal frequency: 40MHz\nMAC: AA:BB:CC:DD:EE:FF\nManufacturer: 68\nDevice: 4016\nDetected flash size: 4MB\n"
	probe, err := parseFlashProbe([]byte(data))
	if err != nil {
		t.Fatal(err)
	}
	if probe.ChipDescription != "ESP32-D0WD-V3 (revision v3.1)" || probe.Features == "" || probe.CrystalMHz != 40 || probe.FlashManufacturer != "68" || probe.FlashDevice != "4016" {
		t.Fatalf("missing electronics details: %+v", probe)
	}
	for _, malformed := range []string{data + "Manufacturer: ef\n", strings.Replace(data, "Crystal frequency: 40MHz", "Crystal frequency: 400MHz", 1), strings.Replace(data, "Device: 4016", "Device: ???", 1)} {
		if _, err := parseFlashProbe([]byte(malformed)); err == nil {
			t.Fatalf("accepted malformed details: %q", malformed)
		}
	}
}

func testToolArchive(t *testing.T, platform, suffix string, files map[string][]byte, unsafe bool) (string, flashToolAsset) {
	t.Helper()
	filename := filepath.Join(t.TempDir(), "tool"+suffix)
	file, err := os.Create(filename)
	if err != nil {
		t.Fatal(err)
	}
	root := "esptool-" + platform + "/"
	if suffix == ".zip" {
		writer := zip.NewWriter(file)
		for name, data := range files {
			entry := &zip.FileHeader{Name: root + name}
			entry.SetMode(0o700)
			if unsafe {
				entry.Name = name
			}
			content, err := writer.CreateHeader(entry)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := content.Write(data); err != nil {
				t.Fatal(err)
			}
		}
		if err := writer.Close(); err != nil {
			t.Fatal(err)
		}
	} else {
		compressed := gzip.NewWriter(file)
		writer := tar.NewWriter(compressed)
		for name, data := range files {
			entry := &tar.Header{Name: root + name, Mode: 0o700, Size: int64(len(data)), Typeflag: tar.TypeReg}
			if unsafe {
				entry.Name = name
			}
			if err := writer.WriteHeader(entry); err != nil {
				t.Fatal(err)
			}
			if _, err := writer.Write(data); err != nil {
				t.Fatal(err)
			}
		}
		if err := writer.Close(); err != nil {
			t.Fatal(err)
		}
		if err := compressed.Close(); err != nil {
			t.Fatal(err)
		}
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filename)
	if err != nil {
		t.Fatal(err)
	}
	hash := sha256.Sum256(data)
	return filename, flashToolAsset{Platform: platform, Archive: "tool" + suffix, SHA256: hex.EncodeToString(hash[:])}
}

func TestFlashToolArchiveExtraction(t *testing.T) {
	for _, suffix := range []string{".tar.gz", ".zip"} {
		t.Run(suffix, func(t *testing.T) {
			platform, executable := "linux-amd64", "esptool"
			if suffix == ".zip" {
				platform, executable = "windows-amd64", "esptool.exe"
			}
			archive, asset := testToolArchive(t, platform, suffix, map[string][]byte{executable: []byte("tool"), "LICENSE": []byte("GPL-2.0-or-later"), "README.md": []byte("upstream"), "espefuse": []byte("not used")}, false)
			if err := verifyToolArchive(archive, asset.SHA256); err != nil {
				t.Fatal(err)
			}
			dir := t.TempDir()
			if err := extractFlashTool(context.Background(), archive, dir, asset); err != nil {
				t.Fatal(err)
			}
			for _, name := range []string{executable, "LICENSE", "README.md"} {
				if _, err := os.Stat(filepath.Join(dir, name)); err != nil {
					t.Fatal(err)
				}
			}
			if _, err := os.Stat(filepath.Join(dir, "espefuse")); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("unexpected provisioning tool: %v", err)
			}
			if !sameToolFiles(dir, dir, asset) {
				t.Fatal("cache did not match")
			}
			if err := os.WriteFile(archive, []byte("changed"), 0o600); err != nil {
				t.Fatal(err)
			}
			if err := verifyToolArchive(archive, asset.SHA256); err == nil {
				t.Fatal("accepted changed archive")
			}
		})
	}
}

func TestFlashToolCacheChecksExecutablePermissionsOnUnix(t *testing.T) {
	asset := flashToolAsset{Platform: "linux-amd64"}
	source, cached := t.TempDir(), t.TempDir()
	for _, directory := range []string{source, cached} {
		for name, data := range map[string]string{"esptool": "tool", "LICENSE": "GPL-2.0-or-later", "README.md": "upstream"} {
			if err := os.WriteFile(filepath.Join(directory, name), []byte(data), 0o700); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := os.Chmod(filepath.Join(cached, "esptool"), 0o600); err != nil {
		t.Fatal(err)
	}
	if got, want := sameToolFiles(source, cached, asset), runtime.GOOS == "windows"; got != want {
		t.Fatalf("cache with no Unix executable bit matches = %v, want %v on %s", got, want, runtime.GOOS)
	}
	if err := os.Chmod(filepath.Join(cached, "esptool"), 0o700); err != nil {
		t.Fatal(err)
	}
	if !sameToolFiles(source, cached, asset) {
		t.Fatal("cache with executable permissions did not match")
	}
}

func TestFlashToolArchiveRejectsUnsafePathsAndMissingLicense(t *testing.T) {
	for _, suffix := range []string{".tar.gz", ".zip"} {
		for _, name := range []string{"../esptool", "/esptool", "esptool-linux-amd64/../../outside", "esptool-linux-amd64/..\\outside", "C:/esptool", "esptool-linux-amd64/./esptool", "esptool-linux-amd64/../esptool-linux-amd64"} {
			archive, asset := testToolArchive(t, "linux-amd64", suffix, map[string][]byte{name: []byte("unsafe")}, true)
			if err := extractFlashTool(context.Background(), archive, t.TempDir(), asset); err == nil {
				t.Fatalf("accepted %s %q", suffix, name)
			}
		}
		archive, asset := testToolArchive(t, "linux-amd64", suffix, map[string][]byte{"esptool": []byte("binary"), "README.md": []byte("readme")}, false)
		if err := extractFlashTool(context.Background(), archive, t.TempDir(), asset); err == nil || !strings.Contains(err.Error(), "LICENSE") {
			t.Fatalf("missing licence accepted: %v", err)
		}
	}
}

func TestFlashToolArchiveRejectsLinksDuplicatesAndOversize(t *testing.T) {
	for _, scenario := range []string{"link", "duplicate", "oversize"} {
		filename := filepath.Join(t.TempDir(), "tool.tar.gz")
		file, err := os.Create(filename)
		if err != nil {
			t.Fatal(err)
		}
		compressed := gzip.NewWriter(file)
		writer := tar.NewWriter(compressed)
		header := &tar.Header{Name: "esptool-linux-amd64/esptool", Mode: 0o700, Typeflag: tar.TypeReg}
		if scenario == "link" {
			header.Typeflag = tar.TypeSymlink
			header.Linkname = "/outside"
		}
		if scenario == "oversize" {
			header.Size = maxToolFileBytes + 1
		}
		if err := writer.WriteHeader(header); err != nil {
			t.Fatal(err)
		}
		if scenario == "duplicate" {
			if err := writer.WriteHeader(header); err != nil {
				t.Fatal(err)
			}
		}
		// The oversized fixture is deliberately truncated after its header.
		_ = writer.Close()
		_ = compressed.Close()
		_ = file.Close()
		asset := flashToolAsset{Platform: "linux-amd64", Archive: "tool.tar.gz"}
		if err := extractFlashTool(context.Background(), filename, t.TempDir(), asset); err == nil {
			t.Fatalf("accepted %s archive", scenario)
		}
	}
}

func TestFlashToolDownloadVerifiesBeforeInstalling(t *testing.T) {
	payload := []byte("official archive fixture")
	hash := sha256.Sum256(payload)
	for _, corrupt := range []bool{false, true} {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if corrupt {
				w.Write([]byte("changed"))
			} else {
				w.Write(payload)
			}
		}))
		destination := filepath.Join(t.TempDir(), "tool.tar.gz")
		err := downloadToolArchive(context.Background(), server.Client(), server.URL, destination, hex.EncodeToString(hash[:]))
		server.Close()
		if corrupt {
			if err == nil {
				t.Fatal("accepted checksum failure")
			}
			if _, err := os.Stat(destination); !errors.Is(err, os.ErrNotExist) {
				t.Fatal("installed unchecked archive")
			}
		} else if err != nil {
			t.Fatal(err)
		}
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.Header().Set("Content-Length", "100663297") }))
	defer server.Close()
	if err := downloadToolArchive(context.Background(), server.Client(), server.URL, filepath.Join(t.TempDir(), "large"), "unused"); err == nil {
		t.Fatal("accepted oversized archive")
	}
}

func TestFlashToolPinnedPlatforms(t *testing.T) {
	if len(flashToolAssets) != 6 {
		t.Fatal("changed supported platform matrix")
	}
	for _, asset := range flashToolAssets {
		if hash, err := hex.DecodeString(asset.SHA256); err != nil || len(hash) != sha256.Size {
			t.Fatalf("invalid pin for %+v", asset)
		}
	}
	if flashToolAssets["windows/arm64"] != flashToolAssets["windows/amd64"] {
		t.Fatal("Windows ARM must use the pinned upstream amd64 helper")
	}
}

func TestFlashToolSubprocessBackend(t *testing.T) {
	dir := t.TempDir()
	binary := filepath.Join(dir, "fake-esptool")
	if runtime.GOOS == "windows" {
		binary += ".exe"
	}
	command := exec.Command("go", "build", "-o", binary, "./testdata/esptool")
	if data, err := command.CombinedOutput(); err != nil {
		t.Fatalf("fixture build failed: %v\n%s", err, data)
	}
	log := filepath.Join(dir, "calls")
	t.Setenv("PIPKIN_TOOL_TEST_LOG", log)
	tool := &flashTool{path: binary, output: io.Discard}
	probe, err := tool.Probe(context.Background(), "COM7")
	if err != nil || probe.Secure || probe.FlashBytes != baselineFlashBytes {
		t.Fatalf("probe = %+v, %v", probe, err)
	}
	t.Setenv("PIPKIN_TOOL_TEST_SECURE", "yes")
	probe, err = tool.Probe(context.Background(), "COM7")
	if err != nil || !probe.Secure {
		t.Fatalf("secure probe = %+v, %v", probe, err)
	}
	t.Setenv("PIPKIN_TOOL_TEST_SECURE", "")
	data, err := tool.Read(context.Background(), "COM7", 0x8000, 0x1000)
	if err != nil || len(data) != 0x1000 {
		t.Fatalf("read = %d, %v", len(data), err)
	}
	t.Setenv("PIPKIN_TOOL_TEST_SHORT_READ", "yes")
	if _, err := tool.Read(context.Background(), "COM7", 0x8000, 0x1000); err == nil {
		t.Fatal("accepted incomplete device read")
	}
	t.Setenv("PIPKIN_TOOL_TEST_SHORT_READ", "")
	image := filepath.Join(dir, "firmware.bin")
	if err := os.WriteFile(image, []byte("firmware"), 0o600); err != nil {
		t.Fatal(err)
	}
	images := []flashWriteImage{{Path: image, Offset: 0x10000}}
	if err := tool.Write(context.Background(), "COM7", images); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PIPKIN_TOOL_TEST_FAIL", "verify-flash")
	if err := tool.Write(context.Background(), "COM7", images); err == nil {
		t.Fatal("reported success when verification failed")
	}
	t.Setenv("PIPKIN_TOOL_TEST_FAIL", "")
	if err := tool.EraseSettings(context.Background(), "COM7"); err != nil {
		t.Fatal(err)
	}
	if err := tool.Reset(context.Background(), "COM7"); err != nil {
		t.Fatal(err)
	}
	calls, err := os.ReadFile(log)
	if err != nil {
		t.Fatal(err)
	}
	for _, line := range strings.Split(strings.TrimSpace(string(calls)), "\n") {
		if strings.Contains(line, "--force") || strings.Contains(line, "erase-flash") || strings.Contains(line, "--erase-all") {
			t.Fatalf("unsafe flash args: %s", line)
		}
		if !strings.Contains(line, "--after|no-reset") && !strings.Contains(line, "--after|hard-reset|--connect-attempts|2|--verbose|--no-stub|--chip|esp32|read-mac") {
			t.Fatalf("reset changed bootloader unexpectedly: %s", line)
		}
	}
	if !strings.Contains(string(calls), "erase-region|0x9000|0x6000") {
		t.Fatal("settings erase lost its bounded region")
	}
	if !strings.Contains(string(calls), "--before|default-reset|--after|hard-reset") {
		t.Fatal("post-flash reset must release DTR through the standard reset sequence before booting the application")
	}
	t.Setenv("ESPTOOL_CHIP", "esp8266")
	t.Setenv("ESPTOOL_CFGFILE", "untrusted")
	data, err = tool.run(context.Background(), time.Second, "environment")
	if err != nil || !strings.Contains(string(data), "chip= color=1 config=[esptool]") {
		t.Fatalf("inherited config leaked into flash: %s, %v", data, err)
	}
	marker := filepath.Join(dir, "survived")
	start := time.Now()
	_, err = tool.run(context.Background(), 200*time.Millisecond, "hang", marker)
	if !errors.Is(err, context.DeadlineExceeded) || time.Since(start) > 8*time.Second {
		t.Fatalf("cancellation was not bounded: %v", err)
	}
	time.Sleep(time.Second)
	if _, err := os.Stat(marker); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("serial-owning child survived cancellation: %v", err)
	}
	// Exercise the actual cache/download path using the same native subprocess.
	// A changed extracted executable is restored only from the verified archive.
	binaryData, err := os.ReadFile(binary)
	if err != nil {
		t.Fatal(err)
	}
	platform, name, suffix := "linux-amd64", "esptool", ".tar.gz"
	if runtime.GOOS == "windows" {
		platform, name, suffix = "windows-amd64", "esptool.exe", ".zip"
	}
	archive, asset := testToolArchive(t, platform, suffix, map[string][]byte{name: binaryData, "LICENSE": []byte("GPL-2.0-or-later"), "README.md": []byte("upstream")}, false)
	archiveData, err := os.ReadFile(archive)
	if err != nil {
		t.Fatal(err)
	}
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { requests.Add(1); w.Write(archiveData) }))
	defer server.Close()
	cache := t.TempDir()
	cached, err := prepareFlashToolAsset(context.Background(), io.Discard, cache, asset, server.Client(), server.URL)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(cached.path, []byte("changed executable"), 0o700); err != nil {
		t.Fatal(err)
	}
	if _, err := prepareFlashToolAsset(context.Background(), io.Discard, cache, asset, server.Client(), server.URL); err != nil {
		t.Fatalf("could not repair changed executable: %v", err)
	}
	if requests.Load() != 1 {
		t.Fatalf("re-downloaded a verified cached archive: %d requests", requests.Load())
	}
	if err := os.WriteFile(filepath.Join(cache, asset.Archive), []byte("changed archive"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := prepareFlashToolAsset(context.Background(), io.Discard, cache, asset, server.Client(), server.URL); err == nil {
		t.Fatal("executed helper after cached archive tampering")
	}
}

// Opt-in network/platform smoke, run in CI. It never opens a serial port.
func TestFlashToolOfficialSmoke(t *testing.T) {
	if os.Getenv("PIPKIN_TOOL_SMOKE") != "1" {
		t.Skip("official helper smoke is opt-in")
	}
	t.Setenv("PIPKIN_HOME", t.TempDir())
	ctx, cancel := context.WithTimeout(context.Background(), 6*time.Minute)
	defer cancel()
	tool, err := prepareFlashTool(ctx, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	data, err := tool.run(ctx, 30*time.Second, "--help")
	if err != nil || !bytes.Contains(data, []byte("write-flash")) || !bytes.Contains(data, []byte("read-flash")) {
		t.Fatalf("official tool help failed: %v\n%s", err, data)
	}
	if _, err := prepareFlashTool(ctx, io.Discard); err != nil {
		t.Fatalf("cached tool preparation failed: %v", err)
	}
}
