package pipkin

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

// Restore the user's status line only when the legacy Pipkin hook is still installed.
func removeLegacyClaudeHook(config *Config) error {
	path, err := filepath.EvalSymlinks(filepath.Join(claudeConfig(), "settings.json"))
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return errors.New("could not locate Claude settings for migration")
	}
	info, err := os.Stat(path)
	if err != nil || !info.Mode().IsRegular() {
		return errors.New("could not read Claude settings for migration")
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return errors.New("could not read Claude settings for migration")
	}
	var settings map[string]json.RawMessage
	if json.Unmarshal(data, &settings) != nil || settings == nil {
		return errors.New("Claude settings are not a valid JSON object; left unchanged")
	}
	var hook struct {
		Type    string `json:"type"`
		Command string `json:"command"`
	}
	if json.Unmarshal(settings["statusLine"], &hook) != nil || hook.Type != "command" ||
		(hook.Command != installedBinary()+" statusline" && hook.Command != `"`+installedBinary()+`" statusline`) {
		return nil
	}
	if len(config.ClaudePreviousStatusLine) == 0 {
		delete(settings, "statusLine")
	} else {
		if !json.Valid(config.ClaudePreviousStatusLine) {
			return errors.New("saved Claude status line is invalid; settings left unchanged")
		}
		settings["statusLine"] = config.ClaudePreviousStatusLine
	}
	updated, err := json.MarshalIndent(settings, "", "  ")
	if err != nil {
		return errors.New("could not encode migrated Claude settings")
	}
	// Avoid overwriting an edit made while this migration was being prepared.
	current, err := os.ReadFile(path)
	if err != nil || !bytes.Equal(current, data) {
		return errors.New("Claude settings changed during migration; retry the upgrade")
	}
	if err := writeFileAtomic(path, append(updated, '\n'), info.Mode().Perm()); err != nil {
		return fmt.Errorf("could not migrate Claude status line: %w", err)
	}
	config.ClaudePreviousStatusLine = nil
	return nil
}
