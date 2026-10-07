package pipkin

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

const testNow = 1800000000

func float(value float64) *float64 { return &value }

func isolate(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	t.Setenv("PIPKIN_HOME", filepath.Join(root, "app"))
	t.Setenv("CLAUDE_CONFIG_DIR", filepath.Join(root, "claude"))
	return root
}

func TestBounds(t *testing.T) {
	if usedTenths(-5) != 0 || usedTenths(250) != 1000 || usedTenths(41.25) != 413 {
		t.Fatal("percentages must be clamped and rounded to tenths")
	}
	if validEpoch(12) != 0 || validEpoch(testNow) != testNow {
		t.Fatal("epochs outside the protocol bounds must be rejected")
	}
}

type fakePort struct {
	incoming []byte
	written  []string
	closed   bool
}

func (p *fakePort) Write(ctx context.Context, data []byte) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	p.written = append(p.written, string(data))
	return nil
}
func (p *fakePort) ReadAvailable() ([]byte, error) {
	data := p.incoming
	p.incoming = nil
	return data, nil
}
func (p *fakePort) Close() error { p.closed = true; return nil }

func TestConnectionSplitsLines(t *testing.T) {
	port := &fakePort{incoming: []byte("boot noise\r\nv=1 kind=identity product=pipkin seq=7 clock_epoch=0 unix=null\nhalf")}
	conn := &connection{port: port}
	lines, _ := conn.lines()
	if len(lines) != 2 || !isIdentity(parseFields(lines[1])) || parseFields(lines[1])["unix"] != "null" {
		t.Fatalf("lines = %q", lines)
	}
	if identity := identify(context.Background(), &connection{port: &fakePort{incoming: []byte(lines[1] + "\n")}},
		time.Second); identity["seq"] != "7" {
		t.Fatalf("identity = %v", identity)
	}
}

func testHelper() (*Helper, *[]string) {
	lines := &[]string{}
	h := &Helper{readings: map[string]*Reading{}, apps: map[string]string{}, sent: map[string]string{},
		due: map[string]time.Time{}, cooldown: map[string]time.Time{}, observedGeneration: map[string]uint64{},
		providerErrors: map[string]string{}, pending: map[string]bool{}, results: make(chan providerResult, len(providers)),
		now: func() time.Time { return time.Unix(testNow, 0).In(time.FixedZone("", 600*60)) }}
	h.write = func(_ context.Context, line string) error { *lines = append(*lines, line); return nil }
	return h, lines
}

func TestFlushSendsOnlyChanges(t *testing.T) {
	h, lines := testHelper()
	h.clockEpoch = 42
	h.apps["codex"] = "available"
	h.readings["codex"] = &Reading{Account: "codex", Observed: testNow, Weekly: &Window{Used: 10}}
	h.flush()
	h.flush()
	if len(*lines) != 2 || !strings.HasSuffix((*lines)[1], "clock_epoch=42\n") {
		t.Fatalf("lines = %q", *lines)
	}
}

func TestUnrequestedIdentityIsIgnored(t *testing.T) {
	h, lines := testHelper()
	h.sequence = 5
	port := &fakePort{incoming: []byte("v=1 kind=identity product=pipkin seq=0 clock_epoch=0 unix=null\n")}
	h.conn = &connection{port: port, name: "COM9"}
	h.handleInput()
	if len(*lines) != 0 {
		t.Fatalf("a duplicate identity reply must not trigger a resync: %q", *lines)
	}
	h.identifyPending, h.identifySequence = true, 3
	port.incoming = []byte("v=1 kind=identity product=pipkin seq=3 clock_epoch=0 unix=1800000000\n")
	h.handleInput()
	if len(*lines) != 0 {
		t.Fatalf("packets sent after the request must not look like a restart: %q", *lines)
	}
	h.identifyPending = true
	port.incoming = []byte("v=1 kind=identity product=pipkin seq=0 clock_epoch=0 unix=null\n")
	h.handleInput()
	if len(*lines) == 0 || !strings.Contains((*lines)[0], "kind=clock") {
		t.Fatalf("a requested reply showing a restart must resync: %q", *lines)
	}
}

func TestNewerVersion(t *testing.T) {
	for _, c := range []struct {
		candidate, current string
		want               bool
	}{{"v0.2.0", "0.1.0", true}, {"v0.1.0", "0.1.0", false}, {"v0.1.0", "0.1.1", false},
		{"v0.1.0", "0.0.0-dev", true}, {"v1.0.0", "0.9.9", true}} {
		if got := newerVersion(c.candidate, c.current); got != c.want {
			t.Errorf("newerVersion(%s, %s) = %v", c.candidate, c.current, got)
		}
	}
}

// Set UPDATE_FIXTURES=1 to regenerate the firmware's protocol fixture.
func TestProtocolFixture(t *testing.T) {
	h, lines := testHelper()
	banked := int64(0)
	h.readings["codex"] = &Reading{Account: "codex", Observed: testNow, ReportsBanked: true,
		Banked: &banked, SessionNoCap: true, Weekly: &Window{Used: 1000, Reset: testNow + 526000, Seconds: 604800}}
	h.readings["claude"] = &Reading{Account: "claude", Observed: testNow,
		Session: &Window{Used: 235, Reset: testNow + 3600, Seconds: 18000},
		Weekly:  &Window{Used: 412, Reset: testNow + 90000, Seconds: 604800}}
	h.apps["codex"], h.apps["claude"] = "available", "available"
	h.resync(map[string]string{"seq": "0", "clock_epoch": "0", "unix": "null"})
	h.syncClock("1799990000")
	h.flush()
	// Only fresh observations may enter the new clock epoch.
	for _, provider := range providers {
		reading := *h.readings[provider]
		reading.Observed = testNow + 1
		h.results <- providerResult{provider: provider, reading: &reading, generation: h.generation}
	}
	h.pollProviders(context.Background(), h.now())
	h.flush()
	h.readings["claude"].Observed = testNow + 2
	h.readings["claude"].Session = &Window{NotStarted: true, Seconds: 18000}
	h.flush()
	h.readings["claude"].Observed = testNow + 3
	h.readings["claude"].Session = &Window{Used: 10, Reset: testNow + 18000, Seconds: 18000}
	h.flush()
	h.send("kind=host state=disconnected")

	got := strings.Join(*lines, "")
	path := filepath.Join("testdata", "helper_packets.txt")
	if os.Getenv("UPDATE_FIXTURES") == "1" {
		os.WriteFile(path, []byte(got), 0o644)
	}
	want, err := os.ReadFile(path)
	if err != nil || string(want) != got {
		t.Fatalf("helper packets differ from %s (UPDATE_FIXTURES=1 regenerates):\n%s", path, got)
	}
}

func TestOfflineFailureRetriesSoon(t *testing.T) {
	h, _ := testHelper()
	h.pending = map[string]bool{"claude": true}
	h.results = make(chan providerResult, 2)
	now := h.now()
	h.results <- providerResult{provider: "codex", err: offlineError{"Codex usage request failed; check your connection"}}
	h.pollProviders(context.Background(), now)
	if got := h.due["codex"].Sub(now); got != offlineRetry {
		t.Fatalf("offline failure retries after %v, want %v", got, offlineRetry)
	}
	h.pending["codex"] = true
	h.results <- providerResult{provider: "codex", err: errors.New("Codex usage returned HTTP 500")}
	h.pollProviders(context.Background(), now)
	if got := h.due["codex"].Sub(now); got != 15*time.Minute {
		t.Fatalf("provider failure retries after %v, want 15m", got)
	}
}

func cancelledContext() context.Context {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	return ctx
}

func TestRebaseRequiresFreshCollectionIncludingLateResults(t *testing.T) {
	h, lines := testHelper()
	cached := &Reading{Account: "c123", Observed: testNow - 60, Weekly: &Window{Used: 200}}
	h.readings["codex"], h.apps["codex"] = cached, "available"
	h.syncClock("1799990000")
	h.flush()
	if h.generation != 1 || h.readings["codex"] != cached || strings.Contains(strings.Join(*lines, ""), "kind=usage") {
		t.Fatal("rebasing must retain JSON observations without retagging cached quota")
	}
	late := &Reading{Account: "c123", Observed: testNow - 30, Weekly: &Window{Used: 250}}
	h.results <- providerResult{provider: "codex", reading: late, generation: 0}
	h.pollProviders(cancelledContext(), h.now())
	h.flush()
	if h.readings["codex"] != late || !h.due["codex"].Equal(h.now()) || strings.Contains(strings.Join(*lines, ""), "kind=usage") {
		t.Fatal("a result collected before rebase must remain historical and require another read")
	}
	fresh := &Reading{Account: "c123", Observed: testNow, Weekly: &Window{Used: 300}}
	h.results <- providerResult{provider: "codex", reading: fresh, generation: h.generation}
	h.pollProviders(cancelledContext(), h.now())
	h.flush()
	if got := (*lines)[len(*lines)-1]; !strings.Contains(got, "observed=1800000000") || !strings.Contains(got, "clock_epoch=1") {
		t.Fatalf("fresh quota was not sent in the current epoch: %s", got)
	}
	if !h.due["codex"].Equal(h.now().Add(4 * time.Minute)) {
		t.Fatal("a fresh current-generation read must resume ordinary polling")
	}
}

func TestReconnectPreservesObservationButRestartRequiresFreshRead(t *testing.T) {
	for _, epoch := range []uint64{0, 4} {
		h, lines := testHelper()
		h.sequence, h.clockEpoch = 10, epoch
		h.readings["codex"] = &Reading{Account: "c123", Observed: testNow - 60, Weekly: &Window{Used: 200}}
		h.resync(map[string]string{"seq": "10", "clock_epoch": strconv.FormatUint(epoch, 10), "unix": "1800000000"})
		if !strings.Contains(strings.Join(*lines, ""), "observed=1799999940") || h.generation != 0 {
			t.Fatal("same-boot reconnect must preserve the cached observation watermark")
		}
		*lines = nil
		h.resync(map[string]string{"seq": "0", "clock_epoch": "0", "unix": "null"})
		if h.generation != 1 || strings.Contains(strings.Join(*lines, ""), "kind=usage") {
			t.Fatal("device restart must require a fresh read, including an initial zero epoch")
		}
	}
}

func TestOffsetChangeAndShortClockRollback(t *testing.T) {
	h, lines := testHelper()
	h.syncClock("1800000000")
	h.now = func() time.Time { return time.Unix(testNow, 0).In(time.FixedZone("", 480*60)) }
	h.conn = &connection{port: &fakePort{incoming: []byte("v=1 kind=identity product=pipkin seq=1 clock_epoch=0 unix=1800000000\n")}, name: "COM9"}
	h.identifyPending, h.identifySequence = true, 1
	h.handleInput()
	if got := (*lines)[len(*lines)-1]; !strings.Contains(got, "tz=480") || strings.Contains(got, "rebase=1") || h.generation != 0 {
		t.Fatalf("an offset change needs an ordinary clock update: %q", got)
	}
	mono := time.Now()
	h.observeClock(mono.Add(time.Second), mono.Round(0).Add(30*time.Second), mono)
	h.syncClock("1800000030")
	if got := (*lines)[len(*lines)-1]; !strings.Contains(got, "rebase=1") || h.generation != 1 {
		t.Fatalf("a detected 30-second rollback requires a new observation epoch: %q", got)
	}
}

func TestWakeRespectsServerCooldownAndWorkersCaptureInputs(t *testing.T) {
	h, _ := testHelper()
	now := h.now()
	h.pending["claude"] = true
	h.results <- providerResult{provider: "codex", err: &usageRetryError{message: "HTTP 429", RetryAt: now.Add(time.Hour)}}
	h.pollProviders(cancelledContext(), now)
	h.refreshProviders(now)
	calls := make(chan string, 1)
	h.config.Salt = "before"
	h.collect = func(_ context.Context, _, salt string) providerResult {
		calls <- salt
		return providerResult{provider: "codex", reading: &Reading{Account: "c123", Observed: testNow}}
	}
	h.pollProviders(context.Background(), now)
	if h.pending["codex"] {
		t.Fatal("wake must not launch collection during a server cooldown")
	}
	h.pollProviders(context.Background(), now.Add(time.Hour))
	h.config.Salt = "after"
	select {
	case salt := <-calls:
		if salt != "before" {
			t.Fatalf("worker used mutable configuration: %q", salt)
		}
	case <-time.After(time.Second):
		t.Fatal("collection did not resume after the server cooldown")
	}
}

func TestFailedDeliveryStopsFlushAndDoesNotAdvanceClockEpoch(t *testing.T) {
	h, _ := testHelper()
	h.apps["codex"], h.apps["claude"] = "available", "available"
	p := &fakePort{}
	h.conn = &connection{port: p, name: "COM9"}
	h.readings["codex"] = &Reading{Account: "c123", Observed: testNow, Weekly: &Window{Used: 200}}
	attempts := 0
	h.write = func(context.Context, string) error { attempts++; return errors.New("write failed") }
	h.flush()
	if attempts != 1 || len(h.sent) != 0 || h.conn != nil || !p.closed {
		t.Fatal("a failed flush must stop and must not cache undelivered packets")
	}
	h.syncClock("1799990000")
	if h.clockEpoch != 0 || h.generation != 0 {
		t.Fatal("a failed rebase write must not start a new local epoch")
	}
}

func TestCancelledDiscoveryRotatesAndShutdownUsesBoundedContext(t *testing.T) {
	isolate(t)
	h, lines := testHelper()
	opened := []string{}
	open := func(name string) (serialPort, error) {
		opened = append(opened, name)
		return &fakePort{}, nil
	}
	h.connectPorts(cancelledContext(), []string{"COM1", "COM2"}, open)
	if len(opened) != 0 {
		t.Fatal("canceled discovery must not open a device")
	}
	for range 2 {
		ctx, cancel := context.WithTimeout(context.Background(), time.Millisecond)
		h.connectPorts(ctx, []string{"COM1", "COM2"}, open)
		cancel()
	}
	if len(opened) != 2 || opened[0] != "COM1" || opened[1] != "COM2" {
		t.Fatalf("unresponsive candidates must rotate: %v", opened)
	}
	h.conn = &connection{port: &fakePort{}, name: "COM3"}
	h.write = func(ctx context.Context, line string) error {
		if ctx.Err() != nil {
			t.Fatal("shutdown host report must use a live context")
		}
		if _, bounded := ctx.Deadline(); !bounded {
			t.Fatal("shutdown host report must have a deadline")
		}
		*lines = append(*lines, line)
		return nil
	}
	h.run(cancelledContext())
	if len(*lines) != 1 || !strings.Contains((*lines)[0], "state=disconnected") || h.conn != nil {
		t.Fatal("shutdown must send one final report and close the connection")
	}
}

func TestRunCommandClearsStalePIDBeforeConfigurationWait(t *testing.T) {
	isolate(t)
	if err := os.MkdirAll(appDir(), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(pidPath(), []byte("123"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(configPath(), []byte("{broken"), 0o600); err != nil {
		t.Fatal(err)
	}
	configLock, err := acquireFileLock(filepath.Join(appDir(), "config.lock"))
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- runCommand() }()
	finished := false
	defer func() {
		configLock.Close()
		if !finished {
			select {
			case <-done:
			case <-time.After(time.Second):
				t.Error("configuration-blocked startup did not stop")
			}
		}
	}()
	deadline := time.Now().Add(time.Second)
	for {
		_, err := os.Stat(pidPath())
		if errors.Is(err, os.ErrNotExist) {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("startup retained the stale PID while waiting for configuration")
		}
		time.Sleep(time.Millisecond)
	}
	select {
	case err := <-done:
		finished = true
		t.Fatalf("startup did not wait for configuration: %v", err)
	default:
	}
	if _, err := runningPID(); !errors.Is(err, errHelperStarting) {
		t.Fatalf("readiness should report startup while configuration is locked: %v", err)
	}
	if err := configLock.Close(); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		finished = true
		if err == nil || !strings.Contains(err.Error(), "configuration") {
			t.Fatalf("invalid configuration must stop startup: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("startup did not finish after configuration was released")
	}
	if _, err := os.Stat(pidPath()); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("failed startup must not publish or retain a PID")
	}
	if running, err := helperRunning(); err != nil || running {
		t.Fatalf("failed startup retained the instance lock: running=%t, err=%v", running, err)
	}
}

func TestRunCommandReportsStalePIDRemovalFailure(t *testing.T) {
	isolate(t)
	if err := os.MkdirAll(pidPath(), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(pidPath(), "keep"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := runCommand(); err == nil || !strings.Contains(err.Error(), "stale helper PID") {
		t.Fatalf("stale PID removal failure must stop startup: %v", err)
	}
	if running, err := helperRunning(); err != nil || running {
		t.Fatalf("failed PID cleanup retained the instance lock: running=%t, err=%v", running, err)
	}
}

func TestRunCommandRemovesPublishedPIDWhenMaintenanceCloseFails(t *testing.T) {
	isolate(t)
	closeFailure := errors.New("injected maintenance close failure")
	releases := 0
	err := runCommandWithMaintenanceRelease(func(file *os.File) error {
		releases++
		if _, err := readPIDRecord(); err != nil {
			t.Fatalf("startup did not publish PID before releasing maintenance: %v", err)
		}
		if err := file.Close(); err != nil {
			t.Fatal(err)
		}
		return closeFailure
	})
	if !errors.Is(err, closeFailure) || releases != 1 {
		t.Fatalf("maintenance release = %d, %v", releases, err)
	}
	if _, err := os.Stat(pidPath()); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("failed startup retained its published PID: %v", err)
	}
	if running, err := helperRunning(); err != nil || running {
		t.Fatalf("failed startup retained the instance lock: %t, %v", running, err)
	}
	maintenance, err := acquireFileLock(maintenanceLockPath())
	if err != nil {
		t.Fatalf("failed startup retained the maintenance lock: %v", err)
	}
	maintenance.Close()
}
