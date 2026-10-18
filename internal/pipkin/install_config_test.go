package pipkin

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

func TestInstallRepairsMalformedConfigWithPrivateUniqueBackup(t *testing.T) {
	for _, raw := range []string{"null", "[]", "{broken", `{"salt":"partial","port":23}`} {
		t.Run(raw, func(t *testing.T) {
			t.Setenv("PIPKIN_HOME", t.TempDir())
			if err := os.WriteFile(configPath(), []byte(raw), 0600); err != nil {
				t.Fatal(err)
			}
			if _, err := initializedConfig(); err == nil {
				t.Fatal("normal reads repaired malformed settings")
			}
			config, backup, err := initializedInstallConfig()
			if err != nil || config.Salt == "" || config.Salt == "partial" || backup == "" {
				t.Fatalf("repair did not initialize settings and retain a backup: %v", err)
			}
			data, err := os.ReadFile(backup)
			if err != nil || string(data) != raw {
				t.Fatal("backup did not retain the exact original bytes")
			}
			info, err := os.Stat(backup)
			if err != nil || runtime.GOOS != "windows" && info.Mode().Perm() != 0600 {
				t.Fatal("backup was not private")
			}
			if read, err := initializedConfig(); err != nil || read != config {
				t.Fatal("repaired settings cannot be read normally")
			}
			if err := os.WriteFile(configPath(), []byte(raw), 0600); err != nil {
				t.Fatal(err)
			}
			_, second, err := initializedInstallConfig()
			if err != nil || second == backup {
				t.Fatal("a later repair overwrote an older backup")
			}
			if data, err := os.ReadFile(backup); err != nil || string(data) != raw {
				t.Fatal("a later repair changed the earlier backup")
			}
		})
	}
}

func TestInstallPreservesValidConfigAndUnknownFields(t *testing.T) {
	t.Setenv("PIPKIN_HOME", t.TempDir())
	raw := []byte(`{"salt":"existing","port":"manual","future":{"large":9007199254740993}}`)
	if err := os.WriteFile(configPath(), raw, 0600); err != nil {
		t.Fatal(err)
	}
	config, backup, err := initializedInstallConfig()
	if err != nil || backup != "" || config.Salt != "existing" || config.Port != "manual" {
		t.Fatalf("valid configuration was repaired: %v", err)
	}
	data, err := os.ReadFile(configPath())
	if err != nil || !bytes.Equal(data, raw) {
		t.Fatal("install rewrote valid configuration")
	}
	backups, _ := filepath.Glob(filepath.Join(appDir(), "config.json.backup-*"))
	if len(backups) != 0 {
		t.Fatal("valid configuration acquired unnecessary backups")
	}
}

func TestInstallDoesNotQuarantineUnreadableConfig(t *testing.T) {
	t.Setenv("PIPKIN_HOME", t.TempDir())
	if err := os.Mkdir(configPath(), 0700); err != nil {
		t.Fatal(err)
	}
	if _, backup, err := initializedInstallConfig(); err == nil || backup != "" {
		t.Fatal("an unreadable path was treated as malformed settings")
	}
	if info, err := os.Stat(configPath()); err != nil || !info.IsDir() {
		t.Fatal("unreadable configuration was replaced")
	}
}

func TestInstallInitializationFailureLeavesOriginalConfigUnchanged(t *testing.T) {
	t.Setenv("PIPKIN_HOME", t.TempDir())
	raw := []byte("{broken")
	if err := os.WriteFile(configPath(), raw, 0600); err != nil {
		t.Fatal(err)
	}
	// A failing update after the backup must not replace the original settings.
	want := errors.New("synthetic initialization failure")
	_, backup, err := updateConfigWithRepair(func(*Config) error { return want }, true)
	if !errors.Is(err, want) || backup == "" {
		t.Fatal("repair lost the original callback failure or its backup")
	}
	if data, err := os.ReadFile(configPath()); err != nil || !bytes.Equal(data, raw) {
		t.Fatal("failed initialization replaced the original settings")
	}
}
