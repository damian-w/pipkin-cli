package pipkin

import (
	"encoding/xml"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func isolateDarwinService(t *testing.T) {
	t.Helper()
	t.Setenv("HOME", t.TempDir())
	t.Setenv("PIPKIN_HOME", filepath.Join(t.TempDir(), "Pipkin & <home>"))
	original := serviceCommand
	t.Cleanup(func() { serviceCommand = original })
}

func TestDarwinAutostartPersistsHomeAsXMLArgument(t *testing.T) {
	isolateDarwinService(t)
	if _, err := registerAutostart(); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(launchAgentPath())
	if err != nil {
		t.Fatal(err)
	}
	var plist struct {
		Dict struct {
			Arguments struct {
				Strings []string `xml:"string"`
			} `xml:"array"`
		} `xml:"dict"`
	}
	if err := xml.Unmarshal(data, &plist); err != nil {
		t.Fatal(err)
	}
	args := plist.Dict.Arguments.Strings
	if len(args) != 4 || args[0] != serviceBinary() || args[1] != "run" || args[2] != "--home" || args[3] != appDir() {
		t.Fatalf("startup arguments = %q", args)
	}
}

func TestDarwinStartAcceptsSuccessfulKickstart(t *testing.T) {
	isolateDarwinService(t)
	if _, err := registerAutostart(); err != nil {
		t.Fatal(err)
	}
	bootstrapErr, kickstartErr := errors.New("already loaded"), errors.New("kickstart failed")
	calls := 0
	serviceCommand = func(_ string, args ...string) error {
		calls++
		if args[0] == "bootstrap" {
			return bootstrapErr
		}
		return nil
	}
	if err := startService(); err != nil || calls != 2 {
		t.Fatalf("successful kickstart = %v, calls=%d", err, calls)
	}
	serviceCommand = func(_ string, args ...string) error {
		if args[0] == "bootstrap" {
			return bootstrapErr
		}
		return kickstartErr
	}
	if err := startService(); !errors.Is(err, bootstrapErr) || !errors.Is(err, kickstartErr) {
		t.Fatalf("double failure = %v", err)
	}
}

func TestDarwinStopAcceptsAlreadyUnloadedService(t *testing.T) {
	isolateDarwinService(t)
	if _, err := registerAutostart(); err != nil {
		t.Fatal(err)
	}
	serviceCommand = func(string, ...string) error { return errors.New("bootout: No such process") }
	if err := stopService(); err != nil {
		t.Fatal(err)
	}
	wanted := errors.New("permission denied")
	serviceCommand = func(string, ...string) error { return wanted }
	if err := stopService(); !errors.Is(err, wanted) {
		t.Fatalf("stop failure = %v", err)
	}
}
