package pipkin

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"
)

const (
	tick            = 2 * time.Second
	heartbeatEvery  = 30 * time.Second
	identifyEvery   = 60 * time.Second
	discoverEvery   = 10 * time.Second
	offlineRetry    = 30 * time.Second
	clockTolerance  = 120
	rebaseThreshold = 300
)

var providers = []string{"codex", "claude"}

type Status struct {
	SchemaVersion int                 `json:"schema_version"`
	States        map[string]string   `json:"states,omitempty"`
	Version       string              `json:"version"`
	PID           int                 `json:"pid"`
	Device        map[string]string   `json:"device"`
	Readings      map[string]*Reading `json:"readings"`
	CodexError    string              `json:"codex_error,omitempty"`
	ClaudeError   string              `json:"claude_error,omitempty"`
	Updated       int64               `json:"updated"`
}

type Helper struct {
	config             Config
	conn               *connection
	device             map[string]string
	sequence           uint64
	clockEpoch         uint64
	generation         uint64
	observedGeneration map[string]uint64
	clockSynced        bool
	clockOffset        int
	rebaseNeeded       bool
	discoverIndex      int
	ctx                context.Context
	readings           map[string]*Reading
	apps               map[string]string
	sent               map[string]string
	now                func() time.Time
	write              func(context.Context, string) error
	collect            func(context.Context, string, string) providerResult

	providerErrors map[string]string
	pending        map[string]bool
	results        chan providerResult
	// Ignore unsolicited replies. Replies cannot include packets sent after identifySequence.
	identifyPending  bool
	identifySequence uint64
	due              map[string]time.Time
	cooldown         map[string]time.Time
	lastStatus       []byte
}

func newHelper() (*Helper, error) {
	config, err := initializedConfig()
	if err != nil {
		return nil, err
	}
	h := &Helper{config: config, now: time.Now, device: map[string]string{},
		readings: map[string]*Reading{}, apps: map[string]string{}, sent: map[string]string{},
		due: map[string]time.Time{}, cooldown: map[string]time.Time{}, observedGeneration: map[string]uint64{},
		providerErrors: map[string]string{}, pending: map[string]bool{}, results: make(chan providerResult, len(providers))}
	h.write = h.writeSerial
	return h, nil
}

func (h *Helper) writeSerial(ctx context.Context, line string) error {
	if h.conn == nil {
		return errors.New("display is disconnected")
	}
	return h.conn.port.Write(ctx, []byte(line))
}

func (h *Helper) context() context.Context {
	if h.ctx != nil {
		return h.ctx
	}
	return context.Background()
}

func (h *Helper) send(fields string) bool {
	h.sequence++
	if err := h.write(h.context(), fmt.Sprintf("v=1 seq=%d %s\n", h.sequence, fields)); err != nil {
		h.disconnect("write failed: " + err.Error())
		return false
	}
	return true
}

func (h *Helper) disconnect(reason string) {
	if h.conn != nil {
		logf("display disconnected: %s", reason)
		h.conn.port.Close()
	}
	h.conn = nil
	h.device = map[string]string{}
	h.sent = map[string]string{}
	h.identifyPending = false
	h.clockSynced = false
}

func (h *Helper) connect(ctx context.Context) {
	ports := candidatePorts()
	if h.config.Port != "" {
		ports = []string{h.config.Port}
	}
	h.connectPorts(ctx, ports, openSerial)
}

// Probe one openable port per pass so unrelated boards cannot stall collection.
// Rotating candidates gives each board a complete identity timeout.
func (h *Helper) connectPorts(ctx context.Context, ports []string, open func(string) (serialPort, error)) {
	for i, name := range ports {
		if name == h.config.LastPort && i > 0 {
			ports[0], ports[i] = ports[i], ports[0]
		}
	}
	for range ports {
		if ctx.Err() != nil {
			return
		}
		name := ports[h.discoverIndex%len(ports)]
		h.discoverIndex++
		port, err := open(name)
		if err != nil {
			h.device = map[string]string{"error": fmt.Sprintf("%s: %v", name, err)}
			continue
		}
		conn := &connection{port: port, name: name}
		identity := identify(ctx, conn, 3500*time.Millisecond)
		if identity == nil || ctx.Err() != nil {
			port.Close()
			return
		}
		h.conn = conn
		h.device = map[string]string{"port": name, "firmware": identity["firmware"]}
		if h.config.LastPort != name {
			config, err := updateConfig(func(config *Config) error {
				config.LastPort = name
				return nil
			})
			if err != nil {
				logf("could not save display port: %v", err)
			} else {
				h.config = config
			}
		}
		h.discoverIndex = 0
		logf("display connected on %s", name)
		h.resync(identity)
		return
	}
}

func (h *Helper) resync(identity map[string]string) {
	seq, _ := strconv.ParseUint(identity["seq"], 10, 64)
	epoch, _ := strconv.ParseUint(identity["clock_epoch"], 10, 64)
	if seq < h.sequence && epoch == h.clockEpoch {
		h.invalidateObservations()
	}
	if seq > h.sequence {
		h.sequence = seq
	}
	h.setClockEpoch(epoch)
	h.sent = map[string]string{}
	if !h.syncClock(identity["unix"]) || !h.send("kind=host state=awake") {
		return
	}
	h.due["heartbeat"] = h.now().Add(heartbeatEvery)
	h.flush()
}

func (h *Helper) setClockEpoch(epoch uint64) {
	if h.clockEpoch != epoch {
		h.clockEpoch = epoch
		h.invalidateObservations()
	}
}

func (h *Helper) invalidateObservations() {
	h.generation++
	h.refreshProviders(h.now())
}

func (h *Helper) refreshProviders(now time.Time) {
	for _, provider := range providers {
		h.due[provider] = now
	}
}

func (h *Helper) observeClock(now, lastWall, lastMono time.Time) {
	// Wall-clock time advancing faster than monotonic time indicates sleep or a clock change.
	drift := now.Round(0).Sub(lastWall) - now.Sub(lastMono)
	if drift > 20*time.Second || drift < -20*time.Second {
		logf("host clock jump or wake detected")
		if drift < 0 {
			h.rebaseNeeded = true
		}
		h.refreshProviders(now)
		h.due["identify"] = time.Time{}
	}
}

func (h *Helper) offsetChanged(now time.Time) bool {
	_, offset := now.Zone()
	return h.clockSynced && offset/60 != h.clockOffset
}

func (h *Helper) syncClock(deviceUnix string) bool {
	now := h.now()
	_, offset := now.Zone()
	fields := fmt.Sprintf("kind=clock unix=%d tz=%d", now.Unix(), offset/60)
	device, err := strconv.ParseInt(deviceUnix, 10, 64)
	if h.rebaseNeeded || err == nil && abs(device-now.Unix()) > rebaseThreshold {
		if !h.send(fields + " rebase=1") {
			return false
		}
		h.setClockEpoch(h.sequence)
		h.sent = map[string]string{}
	} else if !h.send(fields) {
		return false
	}
	h.clockSynced, h.clockOffset = true, offset/60
	h.rebaseNeeded = false
	h.identifyPending = false
	return true
}

func (h *Helper) flush() {
	for _, provider := range providers {
		if app := h.apps[provider]; app != "" && h.sent[provider+"_app"] != app {
			if !h.send(fmt.Sprintf("kind=app provider=%s state=%s", provider, app)) {
				return
			}
			h.sent[provider+"_app"] = app
		}
		reading := h.readings[provider]
		if reading == nil || reading.Observed == 0 || h.apps[provider] == "signed_out" || h.observedGeneration[provider] != h.generation {
			continue
		}
		fields := fmt.Sprintf("kind=usage provider=%s mode=full %s", provider, usageFields(reading))
		if h.clockEpoch != 0 {
			fields += fmt.Sprintf(" clock_epoch=%d", h.clockEpoch)
		}
		if h.sent[provider] != fields {
			if !h.send(fields) {
				return
			}
			h.sent[provider] = fields
		}
	}
}

func (h *Helper) handleInput() {
	if _, err := os.Stat(h.conn.name); err != nil && strings.HasPrefix(h.conn.name, "/") {
		h.disconnect("device removed")
		return
	}
	lines, err := h.conn.lines()
	if err != nil {
		h.disconnect("read failed: " + err.Error())
		return
	}
	for _, line := range lines {
		fields := parseFields(line)
		if !isIdentity(fields) || !h.identifyPending {
			continue
		}
		h.identifyPending = false
		seq, _ := strconv.ParseUint(fields["seq"], 10, 64)
		epoch, _ := strconv.ParseUint(fields["clock_epoch"], 10, 64)
		device, err := strconv.ParseInt(fields["unix"], 10, 64)
		switch {
		case seq < h.identifySequence || epoch != h.clockEpoch:
			logf("display restarted; resending state")
			h.resync(fields)
		case h.rebaseNeeded || err != nil || abs(device-h.now().Unix()) > clockTolerance || h.offsetChanged(h.now()):
			if h.syncClock(fields["unix"]) {
				h.flush()
			}
		}
	}
}

// Network requests run outside the serial loop so heartbeats keep flowing.
func (h *Helper) pollProviders(ctx context.Context, now time.Time) {
	for {
		select {
		case result := <-h.results:
			h.pending[result.provider] = false
			h.due[result.provider] = now.Add(4 * time.Minute)
			delete(h.cooldown, result.provider)
			if result.err != nil {
				h.due[result.provider] = now.Add(15 * time.Minute)
				var retry *usageRetryError
				if errors.As(result.err, &retry) {
					h.cooldown[result.provider] = retry.RetryAt
				}
				if errors.As(result.err, new(offlineError)) {
					h.due[result.provider] = now.Add(offlineRetry)
				}
				if errors.Is(result.err, errSignedOut) {
					h.readings[result.provider] = &Reading{Account: result.provider}
					delete(h.sent, result.provider)
					delete(h.observedGeneration, result.provider)
				}
			}
			h.apps[result.provider] = providerState(result.err)
			h.providerErrors[result.provider] = providerMessage(result.err)
			if result.err == nil {
				h.readings[result.provider] = result.reading
				h.observedGeneration[result.provider] = result.generation
			}
			if result.generation != h.generation && result.err == nil {
				// Keep the snapshot for JSON, but re-observe before sending a new epoch.
				h.due[result.provider] = now
			}
			if h.cooldown[result.provider].After(h.due[result.provider]) {
				h.due[result.provider] = h.cooldown[result.provider]
			}
		default:
			if ctx.Err() != nil {
				return
			}
			for _, provider := range providers {
				if !h.pending[provider] && !now.Before(h.due[provider]) && !now.Before(h.cooldown[provider]) {
					h.pending[provider] = true
					generation, salt := h.generation, h.config.Salt
					collect := h.collect
					if collect == nil {
						collect = collectProvider
					}
					go func() {
						result := collect(ctx, provider, salt)
						result.generation = generation
						select {
						case h.results <- result:
						case <-ctx.Done():
						}
					}()
				}
			}
			return
		}
	}
}

func (h *Helper) writeStatus() {
	status := Status{SchemaVersion: 1, States: h.apps, Version: version, PID: os.Getpid(), Device: h.device, Readings: h.readings,
		CodexError: h.providerErrors["codex"], ClaudeError: h.providerErrors["claude"]}
	current, _ := json.Marshal(status)
	if bytes.Equal(current, h.lastStatus) {
		return
	}
	status.Updated = h.now().Unix()
	if err := writeJSON(statusPath(), status); err != nil {
		logf("could not save usage snapshot: %v", err)
		return
	}
	h.lastStatus = current
}

func (h *Helper) run(ctx context.Context) {
	logf("helper %s started", version)
	h.ctx = ctx
	defer func() {
		// Provider work has stopped; allow one bounded final host report.
		shutdown, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		h.ctx = shutdown
		if h.conn != nil {
			h.send("kind=host state=disconnected")
			if h.conn != nil {
				h.conn.port.Close()
				h.conn = nil
			}
		}
		logf("helper stopped")
	}()
	lastMono := h.now()
	lastWall := lastMono.Round(0)
	ticker := time.NewTicker(tick)
	defer ticker.Stop()
	for ctx.Err() == nil {
		now := h.now()
		h.observeClock(now, lastWall, lastMono)
		lastWall, lastMono = now.Round(0), now
		if h.conn == nil && now.After(h.due["discover"]) {
			h.connect(ctx)
			now = h.now()
			h.due["discover"] = now.Add(discoverEvery)
		}
		if ctx.Err() != nil {
			return
		}
		if h.conn != nil {
			if h.offsetChanged(now) || h.rebaseNeeded {
				h.syncClock("")
			}
		}
		if h.conn != nil {
			h.handleInput()
		}
		h.pollProviders(ctx, now)
		if h.conn != nil {
			if now.After(h.due["heartbeat"]) {
				h.due["heartbeat"] = now.Add(heartbeatEvery)
				h.send("kind=host state=awake")
			}
			if h.conn != nil && now.After(h.due["identify"]) {
				h.due["identify"] = now.Add(identifyEvery)
				if err := h.conn.port.Write(ctx, []byte("v=1 kind=identify\n")); err != nil {
					h.disconnect("write failed: " + err.Error())
				} else {
					h.identifyPending = true
					h.identifySequence = h.sequence
				}
			}
			if h.conn != nil {
				h.flush()
			}
		}
		h.writeStatus()
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

func runCommand() error {
	return runCommandWithMaintenanceRelease((*os.File).Close)
}

func runCommandWithMaintenanceRelease(release func(*os.File) error) error {
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	// A service manager may restart the helper while flash owns the serial port.
	// Serialize startup with maintenance until its validated PID is published.
	maintenance, err := waitFileLock(ctx, maintenanceLockPath())
	if err != nil {
		return err
	}
	maintenanceHeld := true
	defer func() {
		if maintenanceHeld {
			maintenance.Close()
		}
	}()
	lock, err := lockInstance()
	if err != nil {
		return err
	}
	defer lock.Close()
	// Clear a crashed helper's PID under the instance lock before readiness checks.
	if err := removePID(); err != nil {
		return fmt.Errorf("could not remove stale helper PID: %w", err)
	}
	helper, err := newHelper()
	if err != nil {
		return err
	}
	if err := publishPID(); err != nil {
		return err
	}
	defer func() {
		if err := removePID(); err != nil {
			logf("could not remove helper PID: %v", err)
		}
	}()
	// A close error still ends this startup attempt. Remove its published PID
	// before releasing the instance lock, including that error path.
	err = release(maintenance)
	maintenanceHeld = false
	if err != nil {
		return fmt.Errorf("could not release helper startup maintenance lock: %w", err)
	}
	helper.run(ctx)
	return nil
}

func abs(value int64) int64 {
	if value < 0 {
		return -value
	}
	return value
}
