package pipkin

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

const maxLogBytes = 256 * 1024

func readJSON(path string, value any) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	return json.Unmarshal(data, value)
}

func writeFileAtomic(path string, data []byte, mode os.FileMode) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	file, err := os.CreateTemp(filepath.Dir(path), ".pipkin-*")
	if err != nil {
		return err
	}
	name := file.Name()
	_, writeErr := file.Write(data)
	closeErr := file.Close()
	if writeErr == nil {
		writeErr = closeErr
	}
	if writeErr == nil {
		writeErr = os.Chmod(name, mode)
	}
	if writeErr == nil {
		writeErr = os.Rename(name, path)
	}
	if writeErr != nil {
		os.Remove(name)
	}
	return writeErr
}

func writeJSON(path string, value any) error {
	data, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return err
	}
	return writeFileAtomic(path, append(data, '\n'), 0o600)
}

type Config struct {
	Salt                     string          `json:"salt,omitempty"`
	Port                     string          `json:"port,omitempty"`
	LastPort                 string          `json:"last_port,omitempty"`
	ClaudePreviousStatusLine json.RawMessage `json:"claude_previous_statusline,omitempty"`
}

// Preserve unknown fields and concurrent updates under a separate configuration lock.
func updateConfig(update func(*Config) error) (Config, error) {
	var config Config
	dir, err := resolvedAppDir()
	if err != nil {
		return config, fmt.Errorf("could not locate Pipkin configuration: %w", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	lock, err := waitFileLock(ctx, filepath.Join(dir, "config.lock"))
	if err != nil {
		return config, fmt.Errorf("could not lock Pipkin configuration: %w", err)
	}
	defer lock.Close()
	path := filepath.Join(dir, "config.json")
	fields := make(map[string]json.RawMessage)
	data, err := os.ReadFile(path)
	if err == nil {
		if err = json.Unmarshal(data, &fields); err == nil && fields == nil {
			err = errors.New("configuration must be a JSON object")
		}
		if err == nil {
			err = json.Unmarshal(data, &config)
		}
	}
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return config, fmt.Errorf("could not read Pipkin configuration: %w", err)
	}
	before := config
	// A callback may edit the raw status line in place.
	before.ClaudePreviousStatusLine = bytes.Clone(config.ClaudePreviousStatusLine)
	if update != nil {
		if err := update(&config); err != nil {
			return before, err
		}
	}
	if before.Salt == config.Salt && before.Port == config.Port && before.LastPort == config.LastPort &&
		bytes.Equal(before.ClaudePreviousStatusLine, config.ClaudePreviousStatusLine) {
		return config, nil
	}
	known, err := json.Marshal(config)
	if err != nil {
		return before, err
	}
	var updated map[string]json.RawMessage
	if err := json.Unmarshal(known, &updated); err != nil {
		return before, err
	}
	for _, field := range []string{"salt", "port", "last_port", "claude_previous_statusline"} {
		if value, ok := updated[field]; ok {
			fields[field] = value
		} else {
			delete(fields, field)
		}
	}
	if err := writeJSON(path, fields); err != nil {
		return before, err
	}
	return config, nil
}

func initializedConfig() (Config, error) {
	return updateConfig(func(config *Config) error {
		if config.Salt == "" {
			salt := make([]byte, 16)
			if _, err := rand.Read(salt); err != nil {
				return err
			}
			config.Salt = hex.EncodeToString(salt)
		}
		return nil
	})
}

var errLockBusy = errors.New("another process holds the lock")

// Closing the returned file releases the lock. Keep the lock file in place so
// every process locks the same file even when protected files are replaced.
func acquireFileLock(path string) (*os.File, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, err
	}
	file, err := openLockFile(path)
	if err != nil {
		return nil, err
	}
	locked, err := tryFileLock(file)
	if !locked {
		file.Close()
		if err == nil {
			err = errLockBusy
		}
		return nil, err
	}
	return file, nil
}

func waitFileLock(ctx context.Context, path string) (*os.File, error) {
	for {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		file, err := acquireFileLock(path)
		if !errors.Is(err, errLockBusy) {
			return file, err
		}
		timer := time.NewTimer(25 * time.Millisecond)
		select {
		case <-ctx.Done():
			timer.Stop()
			return nil, ctx.Err()
		case <-timer.C:
		}
	}
}

func logf(format string, args ...any) {
	path := logPath()
	if info, err := os.Stat(path); err == nil && info.Size() > maxLogBytes {
		os.Rename(path, path+".1")
	}
	file, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return
	}
	defer file.Close()
	fmt.Fprintf(file, "%s %s\n", time.Now().Format("2006-01-02 15:04:05"), fmt.Sprintf(format, args...))
}
