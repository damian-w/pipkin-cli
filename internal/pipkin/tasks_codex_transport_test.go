package pipkin

import (
	"context"
	"errors"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"
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

func TestCodexTransportCancellationSurvivesClearedDeadline(t *testing.T) {
	client, server := net.Pipe()
	defer server.Close()
	ctx, cancel := context.WithCancel(context.Background())
	conn, err := boundCodexIPCConn(ctx, client)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if err := conn.SetDeadline(time.Time{}); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { _, err := conn.Read(make([]byte, 1)); done <- err }()
	cancel()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("cancelled read succeeded")
		}
	case <-time.After(time.Second):
		t.Fatal("cancelled IPC read remained blocked")
	}
}

func TestCodexTransportExpiredContextClosesConnection(t *testing.T) {
	client, server := net.Pipe()
	defer server.Close()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if conn, err := boundCodexIPCConn(ctx, client); !errors.Is(err, context.Canceled) || conn != nil {
		t.Fatalf("cancelled connection: %v, %v", conn, err)
	}
	if _, err := server.Write([]byte{1}); err == nil {
		t.Fatal("cancelled connection was not closed")
	}
}

func TestCodexTransportDeadline(t *testing.T) {
	client, server := net.Pipe()
	defer server.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Millisecond)
	defer cancel()
	conn, err := boundCodexIPCConn(ctx, client)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	started := time.Now()
	_, err = conn.Read(make([]byte, 1))
	if err == nil || time.Since(started) > time.Second {
		t.Fatalf("deadline read: %v after %v", err, time.Since(started))
	}
}

func TestCodexTransportUnsetHomeUsesUserHome(t *testing.T) {
	prior, set := os.LookupEnv("CODEX_HOME")
	if err := os.Unsetenv("CODEX_HOME"); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if set {
			os.Setenv("CODEX_HOME", prior)
		} else {
			os.Unsetenv("CODEX_HOME")
		}
	})
	home, err := os.UserHomeDir()
	if err != nil {
		t.Fatal(err)
	}
	if got, err := codexTaskHome(); err != nil || got != filepath.Join(home, ".codex") {
		t.Fatalf("default home: %q, %v", got, err)
	}
}
