package pipkin

import (
	"bytes"
	"errors"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func removalTestInstallation(t *testing.T) (dir, home, work string) {
	t.Helper()
	root := t.TempDir()
	dir, home, work = filepath.Join(root, "Pipkin"), filepath.Join(root, "home"), filepath.Join(root, "work")
	for _, path := range []string{filepath.Join(dir, "bin"), home, work} {
		if err := os.MkdirAll(path, 0700); err != nil {
			t.Fatal(err)
		}
	}
	removalTestMarkers(t, dir)
	return dir, home, work
}

func removalTestMarkers(t *testing.T, dir string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(dir, "bin"), 0700); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{filepath.Join(dir, "config.json"), filepath.Join(dir, "bin", executableName("pipkin"))} {
		if err := os.WriteFile(path, []byte("synthetic damaged installation; never execute"), 0600); err != nil {
			t.Fatal(err)
		}
	}
}

func TestUninstallRejectsUnsafeTargetsBeforeAnyAction(t *testing.T) {
	for _, kind := range []string{"relative", "root", "home", "work", "ancestor", "missing config", "missing binary", "marker directory", "unrelated root file", "unrelated bin file"} {
		t.Run(kind, func(t *testing.T) {
			dir, home, work := removalTestInstallation(t)
			switch kind {
			case "relative":
				dir = filepath.Join("relative", "Pipkin")
			case "root":
				dir = filepath.VolumeName(dir) + string(filepath.Separator)
			case "home":
				dir = home
				removalTestMarkers(t, dir)
			case "work":
				dir = work
				removalTestMarkers(t, dir)
			case "ancestor":
				dir = filepath.Dir(home)
				removalTestMarkers(t, dir)
			case "missing config":
				if err := os.Remove(filepath.Join(dir, "config.json")); err != nil {
					t.Fatal(err)
				}
			case "missing binary":
				if err := os.Remove(filepath.Join(dir, "bin", executableName("pipkin"))); err != nil {
					t.Fatal(err)
				}
			case "marker directory":
				if err := os.Remove(filepath.Join(dir, "config.json")); err != nil {
					t.Fatal(err)
				}
				if err := os.Mkdir(filepath.Join(dir, "config.json"), 0700); err != nil {
					t.Fatal(err)
				}
			case "unrelated root file":
				if err := os.WriteFile(filepath.Join(dir, "personal-notes.txt"), []byte("keep"), 0600); err != nil {
					t.Fatal(err)
				}
			case "unrelated bin file":
				if err := os.WriteFile(filepath.Join(dir, "bin", "unrelated-command"), []byte("keep"), 0600); err != nil {
					t.Fatal(err)
				}
			}
			calls := 0
			action := func() error { calls++; return nil }
			deps := uninstallDependencies{target: func() (removalTarget, error) { return inspectRemovalTarget(dir, home, work) },
				lock: func() (io.Closer, error) { calls++; return removalTestCloser{}, nil },
				stop: action, startup: action, launcher: action, remove: func(removalTarget) error { return action() }}
			if err := runUninstallCommand(io.Discard, deps); err == nil || calls != 0 {
				t.Fatalf("unsafe target caused side effects: %v, calls=%d", err, calls)
			}
		})
	}
}

func TestUninstallRejectsLinkedTargetsAndMarkers(t *testing.T) {
	for _, kind := range []string{"target", "target trailing slash", "bin", "config", "binary", "parent aliases home", "nested cache"} {
		t.Run(kind, func(t *testing.T) {
			dir, home, work := removalTestInstallation(t)
			outside := t.TempDir()
			path, destination := filepath.Join(filepath.Dir(dir), "linked"), dir
			switch kind {
			case "bin":
				path, destination = filepath.Join(dir, "bin"), outside
				if err := os.RemoveAll(path); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(outside, executableName("pipkin")), []byte("keep"), 0600); err != nil {
					t.Fatal(err)
				}
			case "config", "binary":
				path, destination = filepath.Join(dir, "config.json"), filepath.Join(outside, "keep")
				if kind == "binary" {
					path = filepath.Join(dir, "bin", executableName("pipkin"))
				}
				if err := os.Remove(path); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(destination, []byte("keep"), 0600); err != nil {
					t.Fatal(err)
				}
			case "parent aliases home":
				removalTestMarkers(t, home)
				destination = filepath.Dir(home)
			case "nested cache":
				path, destination = filepath.Join(dir, "cache", "firmware", "linked"), outside
				if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
					t.Fatal(err)
				}
			}
			if err := os.Symlink(destination, path); err != nil {
				t.Skip("symlinks unavailable for this test user")
			}
			if strings.HasPrefix(kind, "target") {
				dir = path
			}
			if kind == "target trailing slash" {
				dir += string(filepath.Separator)
			}
			if kind == "parent aliases home" {
				dir = filepath.Join(path, filepath.Base(home))
			}
			if _, err := inspectRemovalTarget(dir, home, work); err == nil {
				t.Fatal("unsafe linked target or marker accepted")
			}
			if _, err := os.Stat(destination); err != nil {
				t.Fatal("linked destination changed")
			}
		})
	}
}

func TestUninstallProtectsCaseAliasesByDirectoryIdentity(t *testing.T) {
	dir, home, work := removalTestInstallation(t)
	alias := filepath.Join(filepath.Dir(dir), strings.ToLower(filepath.Base(dir)))
	actual, err := os.Stat(dir)
	if err != nil {
		t.Fatal(err)
	}
	alternate, err := os.Stat(alias)
	if err != nil || !os.SameFile(actual, alternate) {
		t.Skip("test filesystem is case-sensitive")
	}
	for _, protected := range []struct{ home, work string }{{dir, work}, {home, dir}} {
		if _, err := inspectRemovalTarget(alias, protected.home, protected.work); err == nil {
			t.Fatal("a differently cased path bypassed protected directory identity")
		}
	}
}

func TestUninstallStagesOnlyVerifiedInstallationAndPreservesReplacement(t *testing.T) {
	dir, home, work := removalTestInstallation(t)
	for _, name := range []string{"cache", "tools"} {
		if err := os.Mkdir(filepath.Join(dir, name), 0700); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(dir, "config.json.backup-fixture"), []byte("original"), 0600); err != nil {
		t.Fatal(err)
	}
	target, err := inspectRemovalTarget(dir, home, work)
	if err != nil {
		t.Fatal(err)
	}
	tombstone, err := stageVerifiedInstallationRemoval(target)
	if err != nil || tombstone == "" {
		t.Fatalf("verified staging failed: %v", err)
	}
	removalTestMarkers(t, dir)
	if err := os.RemoveAll(tombstone); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, "config.json")); err != nil {
		t.Fatal("new installation was deleted")
	}
}

func TestUninstallRejectsChangedDirectoryOrNewUnrelatedFiles(t *testing.T) {
	for _, change := range []string{"identity", "unrelated file"} {
		t.Run(change, func(t *testing.T) {
			dir, home, work := removalTestInstallation(t)
			target, err := inspectRemovalTarget(dir, home, work)
			if err != nil {
				t.Fatal(err)
			}
			if change == "identity" {
				if err := os.Rename(dir, dir+"-original"); err != nil {
					t.Fatal(err)
				}
				removalTestMarkers(t, dir)
			} else if err := os.WriteFile(filepath.Join(dir, "personal-notes.txt"), []byte("keep"), 0600); err != nil {
				t.Fatal(err)
			}
			if staged, err := stageVerifiedInstallationRemoval(target); err == nil || staged != "" {
				t.Fatal("changed target was staged")
			}
			if _, err := os.Stat(dir); err != nil {
				t.Fatal("changed directory was removed")
			}
		})
	}
}

type removalTestCloser struct{ close func() }

func (c removalTestCloser) Close() error {
	if c.close != nil {
		c.close()
	}
	return nil
}

func TestUninstallFlowOrdersActionsAndStopsOnFailure(t *testing.T) {
	for _, failure := range []string{"", "lock", "stop", "startup", "launcher", "remove"} {
		t.Run(failure, func(t *testing.T) {
			dir, home, work := removalTestInstallation(t)
			var calls []string
			want := errors.New("synthetic uninstall failure")
			action := func(name string) func() error {
				return func() error {
					calls = append(calls, name)
					if failure == name {
						return want
					}
					return nil
				}
			}
			deps := uninstallDependencies{goos: "darwin", target: func() (removalTarget, error) {
				calls = append(calls, "verify")
				return inspectRemovalTarget(dir, home, work)
			},
				lock: func() (io.Closer, error) {
					if err := action("lock")(); err != nil {
						return nil, err
					}
					return removalTestCloser{close: func() { calls = append(calls, "unlock") }}, nil
				},
				stop: action("stop"), startup: action("startup"), launcher: action("launcher"), remove: func(removalTarget) error { return action("remove")() }}
			var out bytes.Buffer
			err := runUninstallCommand(&out, deps)
			expected := []string{"verify", "lock", "stop", "startup", "launcher", "remove"}
			if failure != "" {
				for i, name := range expected {
					if name == failure {
						expected = expected[:i+1]
						break
					}
				}
				if !errors.Is(err, want) || out.Len() != 0 {
					t.Fatalf("failure hidden or success reported: %v, %q", err, out.String())
				}
			} else if err != nil || !strings.Contains(out.String(), "was removed") {
				t.Fatalf("success summary: %v %q", err, out.String())
			}
			if failure != "lock" {
				expected = append(expected, "unlock")
			}
			if !reflect.DeepEqual(calls, expected) {
				t.Fatalf("actions=%v, want=%v", calls, expected)
			}
		})
	}
}

func TestUninstallWithoutFolderStillRemovesStartupAndLauncher(t *testing.T) {
	dir, home, work := removalTestInstallation(t)
	if err := os.RemoveAll(dir); err != nil {
		t.Fatal(err)
	}
	var calls []string
	action := func(name string) func() error { return func() error { calls = append(calls, name); return nil } }
	deps := uninstallDependencies{goos: "darwin", target: func() (removalTarget, error) { return inspectRemovalTarget(dir, home, work) },
		lock: func() (io.Closer, error) { return removalTestCloser{close: func() {}}, nil },
		stop: action("stop"), startup: action("startup"), launcher: action("launcher"), remove: removeInstallation}
	var out bytes.Buffer
	if err := runUninstallCommand(&out, deps); err != nil || !reflect.DeepEqual(calls, []string{"stop", "startup", "launcher"}) {
		t.Fatalf("missing folder blocked cleanup: %v %v", err, calls)
	}
	if _, err := os.Lstat(dir); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("uninstall recreated the missing folder")
	}
}
