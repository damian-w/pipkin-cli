package pipkin

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

func TestStartupFileRestorePreservesPriorAbsenceAndContent(t *testing.T) {
	path := filepath.Join(t.TempDir(), "startup")
	absent, err := captureStartupFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := writeFileAtomic(path, []byte("new definition"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := absent.restore(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("prior absence not restored: %v", err)
	}
	if err := os.WriteFile(path, []byte("original definition"), 0600); err != nil {
		t.Fatal(err)
	}
	saved, err := captureStartupFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := writeFileAtomic(path, []byte("replacement"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := saved.restore(); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil || string(data) != "original definition" {
		t.Fatalf("restored content = %q, %v", data, err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if runtime.GOOS != "windows" && info.Mode().Perm() != 0600 {
		t.Fatalf("restored mode = %v", info.Mode())
	}
}

func TestStartupFileRestorePreservesSymlink(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlink creation can require administrator privileges")
	}
	dir := t.TempDir()
	path := filepath.Join(dir, "startup")
	if err := os.Symlink("original-target", path); err != nil {
		t.Fatal(err)
	}
	saved, err := captureStartupFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := writeFileAtomic(path, []byte("replacement"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := saved.restore(); err != nil {
		t.Fatal(err)
	}
	if target, err := os.Readlink(path); err != nil || target != "original-target" {
		t.Fatalf("restored link = %q, %v", target, err)
	}
}
