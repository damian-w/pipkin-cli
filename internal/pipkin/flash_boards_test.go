package pipkin

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

var testLargeBoard, _ = boardByID("es3c40p")

// withChipID retargets a test image to another chip, keeping its digest valid.
func withChipID(image []byte, chipID uint16) []byte {
	image = append([]byte(nil), image...)
	binary.LittleEndian.PutUint16(image[12:14], chipID)
	body := image[:len(image)-sha256.Size]
	digest := sha256.Sum256(body)
	return append(body, digest[:]...)
}

// testTwoBoardRelease is a staged release with the CYD's and the 4-inch board's images.
func testTwoBoardRelease(t *testing.T, firmwareVersion string) *firmwareRelease {
	t.Helper()
	release := testFirmwareRelease(t, firmwareVersion)
	large := release.Manifest
	large.Board = testLargeBoard.ID
	large.Images = nil
	for _, image := range release.Manifest.Images {
		data, err := os.ReadFile(filepath.Join(release.Folder, image.File))
		if err != nil {
			t.Fatal(err)
		}
		image.File = testLargeBoard.imageFile(image.Role)
		if image.Role == "bootloader" {
			image.Offset = testLargeBoard.BootloaderOffset
		}
		if err := os.WriteFile(filepath.Join(release.Folder, image.File), data, 0o600); err != nil {
			t.Fatal(err)
		}
		large.Images = append(large.Images, image)
	}
	if err := os.WriteFile(filepath.Join(release.Folder, testLargeBoard.demoFile()), []byte("demo reel"), 0o600); err != nil {
		t.Fatal(err)
	}
	release.boards = map[string]firmwareManifest{firmwareBoard: release.Manifest, testLargeBoard.ID: large}
	return release
}

// newLargeFlashFixture is a flash fixture for the 4-inch board with the two-board release.
func newLargeFlashFixture(t *testing.T) *flashFixture {
	t.Helper()
	f := newFlashFixture(t)
	f.release = testTwoBoardRelease(t, "2.0.1")
	f.tool.probe = flashProbe{Chip: "esp32s3", FlashBytes: 16 << 20, MAC: "7c:e8:b1:b1:6e:b8"}
	return f
}

func testLargeIdentity(firmwareVersion string) map[string]string {
	identity := testFlashIdentity(firmwareVersion)
	identity["board"] = testLargeBoard.ID
	return identity
}

func TestDisplayBoardsHaveDistinctReleaseAssets(t *testing.T) {
	seen := map[string]bool{}
	for _, board := range displayBoards {
		for _, name := range []string{board.manifestFile(), board.demoFile(), board.imageFile("bootloader"), board.imageFile("partition-table"), board.imageFile("application")} {
			if seen[name] {
				t.Fatal("two boards share", name)
			}
			seen[name] = true
		}
		if found, ok := boardByChip(strings.ToUpper(board.ChipName)); !ok || found.ID != board.ID {
			t.Fatal("chip name does not find its board", board.ChipName)
		}
	}
	// CLIs from before the 4-inch board read these names, so the CYD keeps them.
	cyd, _ := boardByID(firmwareBoard)
	if cyd.manifestFile() != "firmware.json" || cyd.imageFile("application") != "pipkin.bin" || cyd.demoFile() != demoFirmwareFile {
		t.Fatal("the CYD's release asset names changed")
	}
}

func TestFlash4InchBoardInstallAndUpdate(t *testing.T) {
	for _, update := range []bool{false, true} {
		t.Run(map[bool]string{false: "first-install", true: "update"}[update], func(t *testing.T) {
			f := newLargeFlashFixture(t)
			f.final = testLargeIdentity("2.0.1")
			if update {
				f.initial = testLargeIdentity("2.0.0")
			}
			if err := f.run(context.Background(), flashOptions{}, "yes\n"); err != nil {
				t.Fatal(err, f.output.String())
			}
			if len(f.tool.writes) != 1 {
				t.Fatal(f.tool.writes)
			}
			written := map[int64]string{}
			for _, image := range f.tool.writes[0] {
				written[image.Offset] = filepath.Base(image.Path)
			}
			want := map[int64]string{0x10000: "pipkin-es3c40p.bin"}
			if !update {
				want[0] = "bootloader-es3c40p.bin"
				want[0x8000] = "partition-table-es3c40p.bin"
			}
			if len(written) != len(want) {
				t.Fatal(written)
			}
			for offset, name := range want {
				if written[offset] != name {
					t.Fatalf("wrote %v, want %v", written, want)
				}
			}
			output := f.output.String()
			if update && !strings.Contains(output, "Ready to update your 4-inch display from Pipkin 2.0.0 to 2.0.1.") ||
				!update && (!strings.Contains(output, "Board: 4-inch ES3C40P touch display") || !strings.Contains(output, "Confirm this is an ES3C40P")) {
				t.Fatal(output)
			}
		})
	}
}

func TestFlash4InchBoardChecks(t *testing.T) {
	for _, scenario := range []string{"old-release", "identity-chip-mismatch", "flash-size", "secure"} {
		t.Run(scenario, func(t *testing.T) {
			f := newLargeFlashFixture(t)
			want := "does not match"
			switch scenario {
			case "old-release":
				f.release = testFirmwareRelease(t, "2.0.1")
				want = "doesn't support the 4-inch ES3C40P display"
			case "identity-chip-mismatch":
				f.initial = testLargeIdentity("2.0.0")
				f.tool.probe.Chip = "esp32"
			case "flash-size":
				f.tool.probe.FlashBytes = 4 << 20
			case "secure":
				f.tool.probe.Secure = true
				want = "secure boot"
			}
			err := f.run(context.Background(), flashOptions{}, "yes\n")
			if err == nil || !strings.Contains(err.Error(), want) {
				t.Fatalf("got %v, want %q", err, want)
			}
			if len(f.tool.writes) != 0 || f.tool.erases != 0 || f.starts != 1 {
				t.Fatal("a rejected board was written or the helper was not restored")
			}
		})
	}
	// A CYD with the 4-inch board's identity, or the reverse, never gets the other's images.
	f := newLargeFlashFixture(t)
	f.initial = testFlashIdentity("2.0.0")
	if err := f.run(context.Background(), flashOptions{}, "yes\n"); err == nil || len(f.tool.writes) != 0 {
		t.Fatal("an ESP32-S3 reporting the CYD's board was flashed", err)
	}
}

func TestFlash4InchDemoReel(t *testing.T) {
	f := newLargeFlashFixture(t)
	f.initial = testLargeIdentity("2.0.1")
	f.final = testLargeIdentity("2.0.1-demo")
	if err := f.run(context.Background(), flashOptions{Demo: true}, "y\n"); err != nil {
		t.Fatal(err)
	}
	if images := f.tool.writes[0]; len(images) != 1 || filepath.Base(images[0].Path) != "pipkin-demo-es3c40p.bin" {
		t.Fatal("the 4-inch demo reel was not written", images)
	}
	f = newLargeFlashFixture(t)
	os.Remove(filepath.Join(f.release.Folder, testLargeBoard.demoFile()))
	if err := f.run(context.Background(), flashOptions{Demo: true}, "y\n"); err == nil || len(f.tool.writes) != 0 || f.tool.erases != 0 {
		t.Fatal("a missing 4-inch demo reel was not caught before writing", err)
	}
}

// multiDisplayFixture connects a CYD running Pipkin 2.0.0, a 4-inch board running
// 2.0.0 and a board without Pipkin.
func multiDisplayFixture(t *testing.T) (*flashFixture, *[]string) {
	f := newFlashFixture(t)
	f.release = testTwoBoardRelease(t, "2.0.1")
	identities := map[string]map[string]string{
		"/dev/cu.usbserial-1": testFlashIdentity("2.0.0"),
		"/dev/cu.usbmodem-2":  testLargeIdentity("2.0.0"),
		"/dev/cu.usbserial-3": nil,
	}
	var mu sync.Mutex
	var asked []string
	f.actions.ports = func() []string { return []string{"/dev/cu.usbserial-1", "/dev/cu.usbmodem-2", "/dev/cu.usbserial-3"} }
	f.actions.identity = func(_ context.Context, port string, _ time.Duration) (map[string]string, error) {
		mu.Lock()
		defer mu.Unlock()
		identity := identities[port]
		asked = append(asked, port)
		if len(f.tool.writes) > 0 && identity != nil {
			// After flashing, the chosen display answers with the new version.
			identity = map[string]string{}
			for key, value := range identities[port] {
				identity[key] = value
			}
			identity["firmware"] = "2.0.1"
		}
		return identity, nil
	}
	return f, &asked
}

func TestFlashAsksWhichOfSeveralDisplays(t *testing.T) {
	f, asked := multiDisplayFixture(t)
	f.tool.probe = flashProbe{Chip: "esp32s3", FlashBytes: 16 << 20, MAC: "7c:e8:b1:b1:6e:b8"}
	if err := f.run(context.Background(), flashOptions{}, "2\nyes\n"); err != nil {
		t.Fatal(err, f.output.String())
	}
	output := f.output.String()
	for _, want := range []string{
		"  1. 2.8-inch CYD, Pipkin 2.0.0 (/dev/cu.usbserial-1)",
		"  2. 4-inch ES3C40P, Pipkin 2.0.0 (/dev/cu.usbmodem-2)",
		"  3. No Pipkin running; its board is checked next (/dev/cu.usbserial-3)",
		"Which display should Pipkin flash? [1-3, Enter cancels]",
		"Ready to update your 4-inch display from Pipkin 2.0.0 to 2.0.1.",
	} {
		if !strings.Contains(output, want) {
			t.Fatalf("missing %q:\n%s", want, output)
		}
	}
	if strings.Index(output, "Which display") > strings.Index(output, "Checking the firmware release") {
		t.Fatal("the release was checked before the display was chosen")
	}
	// Each display is asked once, then only the chosen one, before checking it and after flashing.
	if len(*asked) != 5 || (*asked)[3] != "/dev/cu.usbmodem-2" || (*asked)[4] != "/dev/cu.usbmodem-2" || f.stops != 1 || f.starts != 1 {
		t.Fatal(*asked, f.stops, f.starts)
	}
	if images := f.tool.writes[0]; len(images) != 1 || filepath.Base(images[0].Path) != "pipkin-es3c40p.bin" {
		t.Fatal(images)
	}
}

func TestFlashPickerCancelsWithoutWriting(t *testing.T) {
	for _, answer := range []string{"", "\n", "0\n", "4\n", "two\n", "1.5\n"} {
		t.Run(answer, func(t *testing.T) {
			f, _ := multiDisplayFixture(t)
			loaded := false
			load := f.actions.load
			f.actions.load = func(ctx context.Context, version string, demo bool) (*firmwareRelease, error) {
				loaded = true
				return load(ctx, version, demo)
			}
			if err := f.run(context.Background(), flashOptions{}, answer); err != nil {
				t.Fatal(err)
			}
			if loaded || len(f.tool.writes) != 0 || f.tool.erases != 0 || f.tool.probes != 0 || f.starts != 1 || !strings.Contains(f.output.String(), "Cancelled. No firmware was written.") {
				t.Fatal("cancelling the picker changed something", f.output.String())
			}
		})
	}
	// --port skips the picker.
	f, asked := multiDisplayFixture(t)
	if err := f.run(context.Background(), flashOptions{Port: "/dev/cu.usbserial-1"}, "yes\n"); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(f.output.String(), "Which display") || (*asked)[0] != "/dev/cu.usbserial-1" {
		t.Fatal(f.output.String(), *asked)
	}
}

func TestFirmwareReleaseStagesEveryBoard(t *testing.T) {
	f := newFirmwareHTTPFixture(t)
	var cyd firmwareManifest
	json.Unmarshal(f.files["firmware.json"], &cyd)
	large := cyd
	large.Board = testLargeBoard.ID
	large.Images = nil
	for _, image := range cyd.Images {
		data := f.files[image.File]
		if image.Role != "partition-table" {
			data = withChipID(data, testLargeBoard.ChipID)
		}
		image.File = testLargeBoard.imageFile(image.Role)
		if image.Role == "bootloader" {
			image.Offset = testLargeBoard.BootloaderOffset
		}
		digest := sha256.Sum256(data)
		image.Size, image.SHA256 = int64(len(data)), hex.EncodeToString(digest[:])
		f.files[image.File] = data
		large.Images = append(large.Images, image)
	}
	f.files[testLargeBoard.manifestFile()], _ = json.Marshal(large)
	f.refreshChecksums()
	assets := f.metadata["assets"].([]map[string]string)
	for _, name := range []string{testLargeBoard.manifestFile(), "bootloader-es3c40p.bin", "partition-table-es3c40p.bin", "pipkin-es3c40p.bin"} {
		assets = append(assets, map[string]string{"name": name, "browser_download_url": f.server.URL + "/download/v2.0.0/" + name})
	}
	f.metadata["assets"] = assets
	release, err := f.source.stage(context.Background(), "", t.TempDir(), false)
	if err != nil {
		t.Fatal(err)
	}
	defer release.close()
	if release.Manifest.Board != firmwareBoard || len(release.boards) != 2 {
		t.Fatal(release.Manifest.Board, release.boards)
	}
	if err := release.selectBoard(testLargeBoard); err != nil || release.Manifest.Board != testLargeBoard.ID {
		t.Fatal(err)
	}
	if image, path := release.image("bootloader"); image.Offset != 0 || filepath.Base(path) != "bootloader-es3c40p.bin" {
		t.Fatal(image, path)
	}
	for _, image := range release.Manifest.Images {
		if got, err := os.ReadFile(filepath.Join(release.Folder, image.File)); err != nil || string(got) != string(f.files[image.File]) {
			t.Fatal("4-inch image was not staged", image.File, err)
		}
	}

	// A release whose only demo reel is the 4-inch one still stages it.
	reel := withChipID(buildTestESPImage("2.0.0-demo", large.SourceRevision), testLargeBoard.ChipID)
	f.files[testLargeBoard.demoFile()] = reel
	f.refreshChecksums()
	f.metadata["assets"] = append(f.metadata["assets"].([]map[string]string), map[string]string{
		"name": testLargeBoard.demoFile(), "browser_download_url": f.server.URL + "/download/v2.0.0/" + testLargeBoard.demoFile()})
	demo, err := f.source.stage(context.Background(), "", t.TempDir(), true)
	if err != nil {
		t.Fatal("4-inch-only demo reel was refused:", err)
	}
	got, err := os.ReadFile(filepath.Join(demo.Folder, testLargeBoard.demoFile()))
	demo.close()
	if err != nil || string(got) != string(reel) {
		t.Fatal("4-inch demo reel was not staged", err)
	}

	// A 4-inch manifest whose images are for the classic ESP32 is rejected.
	f.files["pipkin-es3c40p.bin"] = withChipID(f.files["pipkin-es3c40p.bin"], 0)
	digest := sha256.Sum256(f.files["pipkin-es3c40p.bin"])
	large.Images[2].SHA256 = hex.EncodeToString(digest[:])
	f.files[testLargeBoard.manifestFile()], _ = json.Marshal(large)
	f.refreshChecksums()
	parent := t.TempDir()
	if release, err := f.source.stage(context.Background(), "", parent, false); err == nil {
		release.close()
		t.Fatal("4-inch manifest with ESP32 images accepted")
	}
	if entries, _ := os.ReadDir(parent); len(entries) != 0 {
		t.Fatal("rejected release left staged images behind")
	}
}
