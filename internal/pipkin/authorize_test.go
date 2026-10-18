package pipkin

import (
	"bytes"
	"context"
	"errors"
	"io"
	"strings"
	"testing"
)

func TestAuthorizeOnlyExplainsPromptWhenDesktopNeedsIt(t *testing.T) {
	for _, prompt := range []bool{false, true} {
		var out bytes.Buffer
		deps := authorizeDependencies{authorize: func(_ context.Context, notice func() error) error {
			if prompt {
				return notice()
			}
			return nil
		}, running: func() (int, error) { return 0, nil }}
		if err := runAuthorize(context.Background(), "darwin", false, &out, deps); err != nil {
			t.Fatal(err)
		}
		if strings.Contains(out.String(), "Mac login password") != prompt || !strings.Contains(out.String(), "sign-in is ready") || strings.Contains(out.String(), "No Claude login") {
			t.Fatalf("misleading authorization output: %s", out.String())
		}
	}
}

func TestAuthorizeStandaloneRestartsRunningHelperButNestedDoesNot(t *testing.T) {
	for _, c := range []struct {
		nested  bool
		pid     int
		restart bool
	}{{false, 42, true}, {false, 0, false}, {true, 42, false}} {
		checks, restarts := 0, 0
		deps := authorizeDependencies{authorize: func(context.Context, func() error) error { return nil },
			running: func() (int, error) { checks++; return c.pid, nil },
			restart: func() error { restarts++; return nil }}
		if err := runAuthorize(context.Background(), "darwin", c.nested, io.Discard, deps); err != nil {
			t.Fatal(err)
		}
		if (restarts == 1) != c.restart || c.nested && checks != 0 {
			t.Fatal("authorization changed the helper at the wrong lifecycle stage")
		}
	}
}

func TestAuthorizeFailuresKeepCauseAndAvoidRestart(t *testing.T) {
	want := errors.New("synthetic permission failure")
	for _, phase := range []string{"permission", "output", "running", "restart", "cancelled"} {
		calls := 0
		deps := authorizeDependencies{authorize: func(context.Context, func() error) error { return nil },
			running: func() (int, error) { return 42, nil }, restart: func() error { calls++; return nil }}
		ctx := context.Background()
		var out io.Writer = io.Discard
		expected := want
		switch phase {
		case "permission":
			deps.authorize = func(context.Context, func() error) error { return want }
		case "output":
			out, expected = failingCLIWriter{}, io.ErrClosedPipe
		case "running":
			deps.running = func() (int, error) { return 0, want }
		case "restart":
			deps.restart = func() error { calls++; return want }
		case "cancelled":
			var cancel context.CancelFunc
			ctx, cancel = context.WithCancel(ctx)
			cancel()
			expected = context.Canceled
		}
		if err := runAuthorize(ctx, "darwin", false, out, deps); !errors.Is(err, expected) {
			t.Fatalf("%s lost its cause: %v", phase, err)
		}
		if calls != 0 && phase != "restart" {
			t.Fatal("failed authorization restarted the helper")
		}
	}
	if err := runAuthorize(context.Background(), "windows", false, io.Discard, authorizeDependencies{}); err != nil {
		t.Fatal("non-macOS authorization changed behaviour")
	}
}
