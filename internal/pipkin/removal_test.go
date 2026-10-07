package pipkin

import (
	"os"
	"path/filepath"
	"testing"
)

func TestDelayedRemovalCannotDeleteFreshInstallation(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "Pipkin")
	if err := os.Mkdir(dir, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "old"), nil, 0600); err != nil {
		t.Fatal(err)
	}
	tombstone, err := stageInstallationRemoval(dir)
	if err != nil {
		t.Fatal(err)
	}
	if tombstone == dir || filepath.Dir(tombstone) != filepath.Dir(dir) {
		t.Fatalf("unsafe removal destination %q", tombstone)
	}
	if err := os.Mkdir(dir, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "new"), nil, 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.RemoveAll(tombstone); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, "new")); err != nil {
		t.Fatal("delayed removal deleted new installation")
	}
}

func TestStagedRemovalCanRestoreInstallationBeforeCleanup(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "Pipkin")
	if err := os.Mkdir(dir, 0700); err != nil {
		t.Fatal(err)
	}
	tombstone, err := stageInstallationRemoval(dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(tombstone, dir); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(dir); err != nil {
		t.Fatal("failed to restore staged installation")
	}
}
