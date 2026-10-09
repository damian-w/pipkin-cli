package pipkin

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestConfigUpdatesPreserveOtherFields(t *testing.T) {
	t.Setenv("PIPKIN_HOME", t.TempDir())
	original := []byte(`{"salt":"existing-identity","port":"manual","last_port":"old","future":{"large":9007199254740993}}`)
	if err := os.MkdirAll(appDir(), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(configPath(), original, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := initializedConfig(); err != nil {
		t.Fatal(err)
	}
	if data, _ := os.ReadFile(configPath()); !bytes.Equal(data, original) {
		t.Fatal("reading an initialized configuration must leave its bytes unchanged")
	}
	if _, err := updateConfig(func(config *Config) error {
		config.Port = "new manual"
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := updateConfig(func(config *Config) error {
		config.LastPort = "new detected"
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	var fields map[string]json.RawMessage
	if err := readJSON(configPath(), &fields); err != nil {
		t.Fatal(err)
	}
	var future struct{ Large json.Number }
	if err := json.Unmarshal(fields["future"], &future); err != nil || future.Large != "9007199254740993" {
		t.Fatal("unknown configuration fields must be preserved without rounding")
	}
	config, err := initializedConfig()
	if err != nil || config.Salt != "existing-identity" || config.Port != "new manual" || config.LastPort != "new detected" {
		t.Fatal("individual updates must retain the existing identity and unrelated settings")
	}
}

func TestConfigCallbackFailureLeavesFileUnchanged(t *testing.T) {
	t.Setenv("PIPKIN_HOME", t.TempDir())
	if _, err := initializedConfig(); err != nil {
		t.Fatal(err)
	}
	original, err := os.ReadFile(configPath())
	if err != nil {
		t.Fatal(err)
	}
	failure := errors.New("update failed")
	if _, err := updateConfig(func(config *Config) error {
		config.Salt = "replacement"
		return failure
	}); !errors.Is(err, failure) {
		t.Fatal("failed callback must propagate its error")
	}
	if data, _ := os.ReadFile(configPath()); !bytes.Equal(data, original) {
		t.Fatal("failed callback must leave the configuration unchanged")
	}
}

func TestConfigRejectsInvalidObjects(t *testing.T) {
	t.Setenv("PIPKIN_HOME", t.TempDir())
	if err := os.MkdirAll(appDir(), 0o755); err != nil {
		t.Fatal(err)
	}
	for _, invalid := range []string{"null", "[]", "{broken", `{"salt":23}`} {
		if err := os.WriteFile(configPath(), []byte(invalid), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := initializedConfig(); err == nil {
			t.Fatal("invalid configuration must report an error")
		}
		if data, _ := os.ReadFile(configPath()); string(data) != invalid {
			t.Fatal("invalid configuration must be left unchanged")
		}
	}
}

func TestConfigProcess(t *testing.T) {
	if os.Getenv("PIPKIN_CONFIG_TEST_PROCESS") == "1" {
		config, err := initializedConfig()
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		if field := os.Getenv("PIPKIN_CONFIG_TEST_UPDATE"); field != "" {
			config, err = updateConfig(func(current *Config) error {
				switch field {
				case "port":
					current.Port = "manual"
				case "last_port":
					current.LastPort = "detected"
				}
				return nil
			})
			if err != nil {
				fmt.Fprintln(os.Stderr, err)
				os.Exit(1)
			}
		}
		fmt.Println(config.Salt)
		os.Exit(0)
	}
	t.Setenv("PIPKIN_HOME", t.TempDir())
	const count = 8
	var results [count]string
	var failures [count]error
	var group sync.WaitGroup
	start := make(chan struct{})
	for i := range count {
		group.Go(func() {
			<-start
			ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
			defer cancel()
			command := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestConfigProcess$")
			command.Env = append(os.Environ(), "PIPKIN_CONFIG_TEST_PROCESS=1")
			out, err := command.CombinedOutput()
			results[i], failures[i] = strings.TrimSpace(string(out)), err
		})
	}
	close(start)
	group.Wait()
	for i := range count {
		if failures[i] != nil {
			t.Fatalf("configuration process failed: %v: %s", failures[i], results[i])
		}
		if len(results[i]) != 32 || results[i] != results[0] {
			t.Fatal("concurrent first runs must share one installation identity")
		}
	}
	config, err := initializedConfig()
	if err != nil || config.Salt != results[0] {
		t.Fatal("concurrent initialization must return the persisted identity")
	}
}

func TestConfigConcurrentUpdates(t *testing.T) {
	t.Setenv("PIPKIN_HOME", t.TempDir())
	var group sync.WaitGroup
	var failures [2]error
	for i, field := range []string{"port", "last_port"} {
		group.Go(func() {
			ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
			defer cancel()
			command := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestConfigProcess$")
			command.Env = append(os.Environ(), "PIPKIN_CONFIG_TEST_PROCESS=1", "PIPKIN_CONFIG_TEST_UPDATE="+field)
			out, err := command.CombinedOutput()
			if err != nil {
				failures[i] = fmt.Errorf("%w: %s", err, out)
			}
		})
	}
	group.Wait()
	for _, err := range failures {
		if err != nil {
			t.Fatal(err)
		}
	}
	config, err := initializedConfig()
	if err != nil || config.Port != "manual" || config.LastPort != "detected" {
		t.Fatal("concurrent processes must retain both field updates")
	}
}

func TestConfigLockContentionAndCancellation(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.lock")
	lock, err := acquireFileLock(path)
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Close()
	if other, err := acquireFileLock(path); !errors.Is(err, errLockBusy) {
		if other != nil {
			other.Close()
		}
		t.Fatalf("contended lock must report contention: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if _, err := waitFileLock(ctx, path); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("waiting for a lock must honor cancellation: %v", err)
	}
	if err := lock.Close(); err != nil {
		t.Fatal(err)
	}
	other, err := acquireFileLock(path)
	if err != nil {
		t.Fatal("closing a lock must release it:", err)
	}
	other.Close()
}

func TestRelativeAppDirectoryIsResolved(t *testing.T) {
	t.Setenv("PIPKIN_HOME", filepath.Join("relative", "Pipkin Home"))
	dir, err := resolvedAppDir()
	if err != nil || !filepath.IsAbs(dir) || dir != appDir() {
		t.Fatal("the helper needs an absolute app directory for detached startup")
	}
	wd, err := os.Getwd()
	if err != nil || dir != filepath.Join(wd, "relative", "Pipkin Home") {
		t.Fatal("relative overrides must resolve against the current directory")
	}
}
