package pipkin

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

func TestUninstallRejectsWindowsJunctionInCache(t *testing.T) {
	dir, home, work := removalTestInstallation(t)
	outside := t.TempDir()
	marker := filepath.Join(outside, "keep.txt")
	if err := os.WriteFile(marker, []byte("keep"), 0600); err != nil {
		t.Fatal(err)
	}
	cache := filepath.Join(dir, "cache")
	if err := os.Mkdir(cache, 0700); err != nil {
		t.Fatal(err)
	}
	junction := filepath.Join(cache, "linked")
	command := exec.Command("cmd.exe", "/d", "/c", "mklink", "/J", junction, outside)
	if output, err := command.CombinedOutput(); err != nil {
		t.Skipf("test junction unavailable: %v (%s)", err, output)
	}
	if _, err := inspectRemovalTarget(dir, home, work); err == nil {
		t.Fatal("nested junction was accepted for recursive deletion")
	}
	if data, err := os.ReadFile(marker); err != nil || string(data) != "keep" {
		t.Fatal("outside junction target changed")
	}
}
