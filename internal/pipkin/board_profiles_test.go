package pipkin

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
)

func TestBoardCatalogAndLegacyCompatibility(t *testing.T) {
	profiles, err := boardProfiles()
	if err != nil {
		t.Fatal(err)
	}
	if len(profiles) == 0 {
		t.Fatal("empty catalog")
	}
	for _, id := range []string{"bruce-cyd-2432s028-dual-usb", "esp32-2432s028r-dual-usb"} {
		profile, _, err := physicalBoardProfile(id)
		if err != nil || profile.ID != "esp32-2432s028r" {
			t.Fatalf("missing shared qualified profile for %s: %+v %v", id, profile, err)
		}
	}
	if !sameFirmwareBoard("esp32-2432s028r", "esp32-2432s028r-provisional") || sameFirmwareBoard("unknown", "unknown") {
		t.Fatal("incorrect profile alias resolution")
	}
	m := testFirmwareRelease(t, "1.3.0").Manifest
	m.Board = "esp32-2432s028r"
	m.Hardware = "confirmed"
	m.MinimumCLI = "1.4.0"
	if err := validateFirmwareManifest(m, "v1.3.0", "1.4.0"); err != nil {
		t.Fatal(err)
	}
	if err := validateFlashIdentity(testFlashIdentity("1.2.0"), m); err != nil {
		t.Fatal("legacy running firmware cannot migrate", err)
	}
	if err := validateFirmwareManifest(m, "v1.3.0", "1.3.2"); err == nil {
		t.Fatal("accepted old CLI with new profile")
	}
}

func TestPhysicalBoardRequiresExplicitConfirmation(t *testing.T) {
	t.Setenv("PIPKIN_HOME", filepath.Join(t.TempDir(), "pipkin"))
	probe := flashProbe{Chip: "esp32", ChipDescription: "ESP32-D0WD-V3 (revision v3.1)", FlashBytes: 4 << 20, FlashManufacturer: "68", FlashDevice: "4016", MAC: "aa:bb:cc:dd:ee:ff"}
	info, err := inspectBoard(probe, "")
	if err != nil || info.Profile != "" || !slices.Contains(info.Candidates, "bruce-cyd-2432s028-dual-usb") || !slices.Contains(info.Candidates, "esp32-2432s028r-dual-usb") {
		t.Fatalf("electronics auto-confirmed board: %+v %v", info, err)
	}
	if _, err := os.Stat(rememberedBoardsPath()); !os.IsNotExist(err) {
		t.Fatal("inspection wrote association")
	}
	info, err = inspectBoard(probe, "bruce-cyd-2432s028-dual-usb")
	if err != nil || info.Profile != "bruce-cyd-2432s028-dual-usb" {
		t.Fatalf("confirm: %+v %v", info, err)
	}
	if info.DisplayMarking != "TPM408-2.8" || info.DisplayWidth != 240 || info.DisplayHeight != 320 {
		t.Fatalf("missing catalog panel observations: %+v", info)
	}
	info, err = inspectBoard(probe, "")
	if err != nil || info.Profile == "" {
		t.Fatalf("lost saved association: %+v %v", info, err)
	}
	file, err := os.Stat(rememberedBoardsPath())
	if err != nil || runtime.GOOS != "windows" && file.Mode().Perm() != 0600 {
		t.Fatalf("association privacy: %v %v", file, err)
	}
	other := probe
	other.MAC = "11:22:33:44:55:66"
	info, err = inspectBoard(other, "")
	if err != nil || info.Profile != "" {
		t.Fatal("association followed port/electronics instead of MAC")
	}
	changed := probe
	changed.FlashManufacturer = "ef"
	if _, err := inspectBoard(changed, ""); err == nil {
		t.Fatal("changed electronics accepted as remembered board")
	}
	changed = probe
	changed.FlashBytes = 2 << 20
	if _, err := inspectBoard(changed, "esp32-2432s028r-dual-usb"); err == nil {
		t.Fatal("incompatible profile confirmed")
	}
	changed = probe
	changed.Secure = true
	if _, err := inspectBoard(changed, "esp32-2432s028r-dual-usb"); err == nil {
		t.Fatal("secured board confirmed")
	}
}

func TestFlashConfirmedBoardUsesCatalogWithLegacyRelease(t *testing.T) {
	f := newFlashFixture(t)
	if _, err := inspectBoard(f.tool.probe, "bruce-cyd-2432s028-dual-usb"); err != nil {
		t.Fatal(err)
	}
	if err := f.run(context.Background(), flashOptions{}, "no\n"); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(f.output.String(), "Bruce CYD 2432S028") || strings.Contains(f.output.String(), "provisional") || strings.Contains(f.output.String(), "profile is unconfirmed") {
		t.Fatal(f.output.String())
	}
	if len(f.tool.writes) != 0 {
		t.Fatal("declined flash wrote firmware")
	}
}

func TestFlashExplicitProfileOnlyRememberedAfterConfirmation(t *testing.T) {
	for _, answer := range []string{"no\n", "yes\n"} {
		t.Run(strings.TrimSpace(answer), func(t *testing.T) {
			f := newFlashFixture(t)
			if err := f.run(context.Background(), flashOptions{Board: "esp32-2432s028r-dual-usb"}, answer); err != nil {
				t.Fatal(err)
			}
			info, err := inspectBoard(f.tool.probe, "")
			if err != nil {
				t.Fatal(err)
			}
			if (info.Profile != "") != (answer == "yes\n") {
				t.Fatalf("association=%+v answer=%q", info, answer)
			}
		})
	}
}

func TestFlashUnknownBoardRemainsUnconfirmed(t *testing.T) {
	f := newFlashFixture(t)
	f.initial = testFlashIdentity("1.0.0")
	if err := f.run(context.Background(), flashOptions{}, "no\n"); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(f.output.String(), "Physical board profile is unconfirmed") {
		t.Fatal("firmware claim auto-identified physical board", f.output.String())
	}
}
