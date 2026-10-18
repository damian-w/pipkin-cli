package pipkin

import (
	"os"
	"path/filepath"
	"testing"
)

func TestCodexTransportHome(t *testing.T) {
	for _, value := range []string{"", ".", "relative", "../codex"} {
		t.Setenv("CODEX_HOME", value)
		if _, err := codexTaskHome(); err == nil {
			t.Errorf("accepted nonabsolute home %q", value)
		}
	}
	home := t.TempDir()
	t.Setenv("CODEX_HOME", filepath.Join(home, "data", "..", "codex"))
	if got, err := codexTaskHome(); err != nil || got != filepath.Join(home, "codex") {
		t.Fatalf("absolute home: %q, %v", got, err)
	}
	os.Unsetenv("CODEX_HOME") // t.Setenv restores it afterwards.
	user, err := os.UserHomeDir()
	if err != nil {
		t.Fatal(err)
	}
	if got, err := codexTaskHome(); err != nil || got != filepath.Join(user, ".codex") {
		t.Fatalf("default home: %q, %v", got, err)
	}
}

func TestCodexTransportGUIExecutable(t *testing.T) {
	for _, c := range []struct {
		path, goos string
		want       bool
	}{
		{"/Applications/ChatGPT.app/Contents/MacOS/ChatGPT", "darwin", true},
		{"/Users/test/Applications/Codex.app/Contents/MacOS/Codex", "darwin", true},
		{"/Applications/ChatGPT.app/Contents/Resources/codex", "darwin", false},
		{"/Applications/FakeChatGPT.app/Contents/MacOS/ChatGPT", "darwin", false},
		{"relative/ChatGPT.app/Contents/MacOS/ChatGPT", "darwin", false},
		{"/Applications/../ChatGPT.app/Contents/MacOS/ChatGPT", "darwin", false},
		{`C:\Program Files\WindowsApps\OpenAI.Codex_26.1_x64__2p2nqsd0c76g0\app\ChatGPT.exe`, "windows", true},
		{`C:\Users\test\AppData\Local\Programs\Codex\Codex.exe`, "windows", true},
		{`C:\Program Files\WindowsApps\OpenAI.Codex_26.1_x64__wrong\app\ChatGPT.exe`, "windows", false},
		{`C:\Program Files\WindowsApps\OpenAI.Codex_26.1_x64__2p2nqsd0c76g0\app\codex-cli.exe`, "windows", false},
		{`C:\Program Files\WindowsApps\OpenAI.Codex_26.1_x64__2p2nqsd0c76g0\app\nested\ChatGPT.exe`, "windows", false},
		{`\\remote\Programs\Codex\ChatGPT.exe`, "windows", false},
		{"/usr/bin/codex", "linux", false},
	} {
		if got := codexGUIExecutable(c.path, c.goos); got != c.want {
			t.Errorf("GUI identity %q (%s): %t, want %t", c.path, c.goos, got, c.want)
		}
	}
}
