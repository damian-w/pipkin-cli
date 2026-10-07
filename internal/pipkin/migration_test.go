package pipkin

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func TestLegacyClaudeHookMigration(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", dir)
	t.Setenv("PIPKIN_HOME", filepath.Join(dir, "Pipkin Home"))
	path := filepath.Join(dir, "settings.json")
	previous := json.RawMessage(`{"type":"command","command":"my-existing-status-command","padding":2}`)
	for _, quoted := range []bool{false, true} {
		for _, restore := range []bool{false, true} {
			command := installedBinary() + " statusline"
			if quoted {
				command = `"` + installedBinary() + `" statusline`
			}
			hook, _ := json.Marshal(map[string]string{"type": "command", "command": command})
			input := append([]byte(`{"permissions":{"allow":["Read"],"deny":[]},"largeNumber":9007199254740993,"statusLine":`), hook...)
			input = append(input, '}')
			if err := os.WriteFile(path, input, 0640); err != nil {
				t.Fatal(err)
			}
			before, err := os.Stat(path)
			if err != nil {
				t.Fatal(err)
			}
			config := Config{}
			if restore {
				config.ClaudePreviousStatusLine = previous
			}
			if err := removeLegacyClaudeHook(&config); err != nil {
				t.Fatal(err)
			}
			data, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			var settings map[string]json.RawMessage
			if err := json.Unmarshal(data, &settings); err != nil {
				t.Fatal(err)
			}
			compact := func(raw []byte) []byte {
				var out bytes.Buffer
				if err := json.Compact(&out, raw); err != nil {
					t.Fatal(err)
				}
				return out.Bytes()
			}
			if string(settings["largeNumber"]) != "9007199254740993" || string(compact(settings["permissions"])) != `{"allow":["Read"],"deny":[]}` {
				t.Fatal("migration changed unrelated settings")
			}
			if restore {
				if !bytes.Equal(compact(settings["statusLine"]), previous) {
					t.Fatal("previous status line was not restored exactly")
				}
			} else if _, ok := settings["statusLine"]; ok {
				t.Fatal("legacy status line should have been removed")
			}
			after, err := os.Stat(path)
			if err != nil || after.Mode().Perm() != before.Mode().Perm() {
				t.Fatal("settings permissions changed")
			}
			if config.ClaudePreviousStatusLine != nil {
				t.Fatal("successful migration should clear the saved status line")
			}
		}
	}
}

func TestLegacyClaudeMigrationLeavesOtherSettingsUntouched(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", dir)
	t.Setenv("PIPKIN_HOME", filepath.Join(dir, "Pipkin Home"))
	path := filepath.Join(dir, "settings.json")
	config := Config{ClaudePreviousStatusLine: json.RawMessage(`{"command":"saved-command"}`)}
	if err := removeLegacyClaudeHook(&config); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatal("absent settings must not be created")
	}
	for _, command := range []string{"", "/some/other/pipkin statusline", installedBinary() + " statusline --custom", "echo pipkin statusline", installedBinary() + " statusline; echo other"} {
		hook, _ := json.Marshal(map[string]string{"type": "command", "command": command})
		data := append([]byte(`{"statusLine":`), hook...)
		data = append(data, '}')
		if err := os.WriteFile(path, data, 0600); err != nil {
			t.Fatal(err)
		}
		if err := removeLegacyClaudeHook(&config); err != nil {
			t.Fatal(err)
		}
		after, _ := os.ReadFile(path)
		if !bytes.Equal(after, data) || len(config.ClaudePreviousStatusLine) == 0 {
			t.Fatal("unrelated hook or saved status line changed")
		}
	}
	for _, data := range []string{`{"other":true}`, `{broken-json`, `null`, `[]`, `{"other":true} trailing`} {
		if err := os.WriteFile(path, []byte(data), 0600); err != nil {
			t.Fatal(err)
		}
		err := removeLegacyClaudeHook(&config)
		if data != `{"other":true}` && err == nil {
			t.Fatal("invalid settings must report an error")
		}
		after, _ := os.ReadFile(path)
		if string(after) != data {
			t.Fatal("absent hook or invalid settings were modified")
		}
	}
	hook, _ := json.Marshal(map[string]string{"type": "command", "command": installedBinary() + " statusline"})
	data := append(append([]byte(`{"statusLine":`), hook...), '}')
	if err := os.WriteFile(path, data, 0600); err != nil {
		t.Fatal(err)
	}
	config.ClaudePreviousStatusLine = json.RawMessage(`{invalid-saved-value`)
	if err := removeLegacyClaudeHook(&config); err == nil {
		t.Fatal("invalid saved status line must fail before rewriting settings")
	}
	if after, _ := os.ReadFile(path); !bytes.Equal(after, data) || string(config.ClaudePreviousStatusLine) != `{invalid-saved-value` {
		t.Fatal("failed migration must retain settings and saved status line")
	}
}

func TestLegacyClaudeMigrationUpdatesCurrentConfig(t *testing.T) {
	t.Setenv("PIPKIN_HOME", t.TempDir())
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	if _, err := initializedConfig(); err != nil {
		t.Fatal(err)
	}
	previous := json.RawMessage(`{"type":"command","command":"custom status","padding":4}`)
	if _, err := updateConfig(func(config *Config) error {
		config.ClaudePreviousStatusLine = previous
		config.Port = "manual"
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	// A helper update between installation preparation and migration must survive.
	if _, err := updateConfig(func(config *Config) error {
		config.LastPort = "detected"
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	hook, _ := json.Marshal(map[string]string{"type": "command", "command": installedBinary() + " statusline"})
	path := filepath.Join(claudeConfig(), "settings.json")
	if err := os.WriteFile(path, append(append([]byte(`{"statusLine":`), hook...), '}'), 0o600); err != nil {
		t.Fatal(err)
	}
	config, err := updateConfig(removeLegacyClaudeHook)
	if err != nil || config.Port != "manual" || config.LastPort != "detected" || config.ClaudePreviousStatusLine != nil {
		t.Fatal("migration must update the latest configuration without replacing unrelated fields")
	}
	var settings map[string]json.RawMessage
	if err := readJSON(path, &settings); err != nil {
		t.Fatal(err)
	}
	var restored bytes.Buffer
	if err := json.Compact(&restored, settings["statusLine"]); err != nil || !bytes.Equal(restored.Bytes(), previous) {
		t.Fatal("migration must restore the saved command from the latest configuration")
	}
	loaded, err := initializedConfig()
	if err != nil || loaded.Port != "manual" || loaded.LastPort != "detected" || loaded.ClaudePreviousStatusLine != nil {
		t.Fatal("migration must persist only its intended configuration change")
	}
}
