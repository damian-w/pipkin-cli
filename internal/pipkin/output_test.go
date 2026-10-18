package pipkin

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"math"
	"os"
	"strings"
	"testing"
)

const outputTestNow int64 = 1767225600

func outputTestFloat(value float64) *float64 { return &value }
func outputTestInt(value int64) *int64       { return &value }

func TestOutputProvidersGoldenAndUnchanged(t *testing.T) {
	s := Status{States: map[string]string{"codex": "available", "claude": "available"}, Readings: map[string]*Reading{
		"codex": {Account: "invented-private-account", Source: "invented-private-source", Plan: "sample_plan", Observed: outputTestNow,
			SessionNoCap: true, Weekly: &Window{Used: 250, Reset: outputTestNow + 7*86400},
			Models:  map[string]*Window{"future_model_session": nil, "codex_spark_weekly": {Used: 800, Reset: outputTestNow + 2*86400}},
			Credits: &Credits{Balance: outputTestFloat(123.45)}, ReportsBanked: true, Banked: outputTestInt(0)},
		"claude": {Plan: "sample_team", Observed: outputTestNow - 6*60,
			Session: &Window{Used: 185, Reset: outputTestNow + 36*60}, Weekly: &Window{Used: 400, Reset: outputTestNow + 3*86400 + 5*3600},
			Models:     map[string]*Window{"sonnet": {Used: 0, Reset: outputTestNow + 86400}, "opus": {Used: 100, Reset: outputTestNow + 86400}},
			ExtraUsage: &ExtraUsage{Enabled: true, Used: outputTestFloat(12.5), Limit: outputTestFloat(50), Currency: "USD"}, ReportsBanked: true},
	}}
	before, err := json.Marshal(s)
	if err != nil {
		t.Fatal(err)
	}
	var got bytes.Buffer
	if err := writeProviders(&got, s, outputTestNow, true); err != nil {
		t.Fatal(err)
	}
	want, err := os.ReadFile("testdata/providers_output.txt")
	if err != nil {
		t.Fatal(err)
	}
	if got.String() != string(want) {
		t.Fatalf("provider output differs from golden:\n%s", got.String())
	}
	after, err := json.Marshal(s)
	if err != nil || !bytes.Equal(before, after) {
		t.Fatal("rendering changed the source or JSON")
	}
	got.Reset()
	if err := writeProviders(&got, s, outputTestNow, false); err != nil {
		t.Fatal(err)
	}
	for _, hidden := range []string{"sample_plan", "sample_team", "Credits", "Banked resets", "Extra usage", "Spark", "Opus", "Sonnet", "invented-private"} {
		if strings.Contains(got.String(), hidden) {
			t.Fatalf("compact output included %q", hidden)
		}
	}
	for _, visible := range []string{"Codex\n", "Claude\n", "No session cap", "Last reading 6m ago"} {
		if !strings.Contains(got.String(), visible) {
			t.Fatalf("compact output omitted %q: %s", visible, got.String())
		}
	}
}

func TestOutputFreshnessSignInAndMissingReadings(t *testing.T) {
	for _, c := range []struct {
		name, state, message string
		reading              *Reading
		want, absent         []string
	}{
		{name: "fresh", state: "available", reading: &Reading{Observed: outputTestNow - 299, SessionNoCap: true}, want: []string{"No session cap", "Weekly   Unknown"}, absent: []string{"Last reading", "Last known"}},
		{name: "five minute threshold", state: "available", reading: &Reading{Observed: outputTestNow - 300, SessionNoCap: true}, want: []string{"Last reading 5m ago"}},
		{name: "future source time", state: "available", reading: &Reading{Observed: outputTestNow + 10, SessionNoCap: true}, absent: []string{"ago", "age unknown"}},
		{name: "recent retained", state: "unavailable", message: "Check your connection", reading: &Reading{Observed: outputTestNow - 60, SessionNoCap: true}, want: []string{"Last known reading", "! Check your connection"}, absent: []string{"1m ago"}},
		{name: "old retained", state: "unavailable", reading: &Reading{Observed: outputTestNow - 47*60, SessionNoCap: true}, want: []string{"Last known reading (47m ago)"}},
		{name: "retained unknown age", state: "unavailable", reading: &Reading{SessionNoCap: true}, want: []string{"Last known reading (age unknown)"}},
		{name: "retained skew", state: "unavailable", reading: &Reading{Observed: outputTestNow + 1, SessionNoCap: true}, want: []string{"Last known reading (age unknown)"}},
		{name: "ordinary sign out", state: "signed_out", want: []string{"Not signed in"}, absent: []string{"!", "Session", "Weekly"}},
		{name: "ordinary sign out message", state: "signed_out", message: "not signed in", want: []string{"Not signed in"}, absent: []string{"!", "no existing"}},
		{name: "legacy ordinary sign out", state: "signed_out", message: "no existing sign-in found", want: []string{"Not signed in"}, absent: []string{"!", "no existing"}},
		{name: "expired", state: "signed_out", message: "Sign-in expired; open Codex to renew it", reading: &Reading{SessionNoCap: true}, want: []string{"Not signed in", "! Sign-in expired; open Codex to renew it"}, absent: []string{"Session", "Last known"}},
		{name: "account mismatch", state: "signed_out", message: "The active account changed; sign in again", want: []string{"! The active account changed; sign in again"}},
		{name: "nil reading", state: "available", want: []string{"No reading yet"}},
		{name: "placeholder failed reading", state: "unavailable", message: "Usage is unavailable", reading: &Reading{Account: "private"}, want: []string{"No reading yet", "! Usage is unavailable"}, absent: []string{"Session", "private"}},
		{name: "unfamiliar state", state: "future_state", reading: &Reading{SessionNoCap: true}, want: []string{"Last known reading (age unknown)"}},
		{name: "expired reset keeps allowance", state: "available", reading: &Reading{Session: &Window{Used: 800, Reset: outputTestNow - 1}}, want: []string{"20% left  awaiting refresh"}, absent: []string{"100% left"}},
	} {
		t.Run(c.name, func(t *testing.T) {
			s := Status{States: map[string]string{"codex": c.state}, Readings: map[string]*Reading{"codex": c.reading}, CodexError: c.message}
			var got bytes.Buffer
			if err := writeProviders(&got, s, outputTestNow, false); err != nil {
				t.Fatal(err)
			}
			for _, want := range c.want {
				if !strings.Contains(got.String(), want) {
					t.Fatalf("missing %q in %s", want, got.String())
				}
			}
			for _, absent := range c.absent {
				if strings.Contains(got.String(), absent) {
					t.Fatalf("unexpected %q in %s", absent, got.String())
				}
			}
		})
	}
}

func TestOutputDetailsKeepZeroAndUnknownDistinct(t *testing.T) {
	for _, c := range []struct {
		name    string
		reading *Reading
		want    []string
	}{
		{"reported zeros", &Reading{ReportsBanked: true, Banked: outputTestInt(0), Credits: &Credits{Balance: outputTestFloat(0)}, ExtraUsage: &ExtraUsage{Enabled: true, Used: outputTestFloat(0), Limit: outputTestFloat(0), Currency: "USD"}}, []string{"Credits        0.00", "Extra usage    0.00 of 0.00 USD used", "Banked resets  0"}},
		{"unknown amounts", &Reading{ReportsBanked: true, Credits: &Credits{}, ExtraUsage: &ExtraUsage{Enabled: true}}, []string{"Credits        Unknown", "Extra usage    On; spend unknown", "Banked resets  Unknown", "Session        Unknown", "Weekly         Unknown"}},
		{"nonfinite amounts", &Reading{Credits: &Credits{Balance: outputTestFloat(math.NaN())}, ExtraUsage: &ExtraUsage{Enabled: true, Used: outputTestFloat(math.Inf(1)), Limit: outputTestFloat(25), Currency: "EUR"}}, []string{"Credits      Unknown", "Spend unknown; limit 25.00 EUR"}},
		{"unlimited credits", &Reading{Credits: &Credits{Unlimited: true}}, []string{"Credits  Unlimited"}},
		{"extra off", &Reading{ExtraUsage: &ExtraUsage{}}, []string{"Extra usage  Off"}},
		{"known spend unknown limit", &Reading{ExtraUsage: &ExtraUsage{Enabled: true, Used: outputTestFloat(3.5), Currency: "USD"}}, []string{"3.50 USD used; limit unknown"}},
		{"invalid banked amount", &Reading{ReportsBanked: true, Banked: outputTestInt(-1)}, []string{"Banked resets  Unknown"}},
	} {
		t.Run(c.name, func(t *testing.T) {
			var got bytes.Buffer
			if err := writeProviders(&got, Status{Readings: map[string]*Reading{"codex": c.reading}}, outputTestNow, true); err != nil {
				t.Fatal(err)
			}
			for _, want := range c.want {
				if !strings.Contains(got.String(), want) {
					t.Fatalf("missing %q in %s", want, got.String())
				}
			}
		})
	}
}

func TestOutputSanitizesAndWrapsEveryProviderString(t *testing.T) {
	longLabel := strings.Repeat("z", 150)
	injection := "\x1b[2J\n\r\t\x7f\u009b\u202e模型"
	s := Status{States: map[string]string{"codex": "unavailable"}, CodexError: strings.Repeat("Long actionable message ", 8) + injection,
		Readings: map[string]*Reading{"codex": {Plan: "raw_plan-slug" + injection, Models: map[string]*Window{longLabel + injection: nil}, ExtraUsage: &ExtraUsage{Enabled: true, Used: outputTestFloat(1), Currency: "USD" + injection}}}}
	var got bytes.Buffer
	if err := writeProviders(&got, s, outputTestNow, true); err != nil {
		t.Fatal(err)
	}
	for _, line := range strings.Split(strings.TrimSuffix(got.String(), "\n"), "\n") {
		if len(line) > outputWidth {
			t.Fatalf("line exceeds terminal width: %q", line)
		}
		for _, r := range line {
			if r < 32 || r > 126 {
				t.Fatalf("unsafe terminal character %U", r)
			}
		}
	}
	if !strings.Contains(got.String(), "raw_plan-slug") || strings.Count(strings.ToLower(got.String()), "z") != 150 {
		t.Fatal("wrapping discarded the raw plan or long model label")
	}
	if !strings.Contains(got.String(), "! Long actionable message") || !strings.Contains(got.String(), "1.00 USD") {
		t.Fatal("sanitization hid the error or known spend")
	}
}

func TestOutputDurationsAndWindows(t *testing.T) {
	for _, c := range []struct {
		seconds int64
		want    string
	}{{0, "0s"}, {30, "30s"}, {36 * 60, "36m"}, {3600, "1h"}, {3600 + 30*60, "1h 30m"}, {86400 + 39*60, "1d"}, {7 * 86400, "7d"}, {3*86400 + 5*3600, "3d 5h"}} {
		if got := outputDuration(c.seconds); got != c.want {
			t.Fatalf("duration %d = %q, want %q", c.seconds, got, c.want)
		}
	}
	if got := outputWindow(&Window{NotStarted: true, Used: 500, Reset: outputTestNow - 1}, outputTestNow); got != "Starts with your first message" {
		t.Fatal(got)
	}
	for _, used := range []int{-1, 1001} {
		if got := outputWindow(&Window{Used: used}, outputTestNow); got != "Unknown" {
			t.Fatal(got)
		}
	}
	for _, c := range []struct {
		used int
		want string
	}{{175, "82% left"}, {165, "84% left"}} {
		if got := outputWindow(&Window{Used: c.used}, outputTestNow); got != c.want {
			t.Fatalf("percentage rounding for used=%d: got %q, want %q", c.used, got, c.want)
		}
	}
}

func TestOutputLegacyCachedProviderMessages(t *testing.T) {
	for _, c := range []struct {
		name, provider, state, message, want string
		calm                                 bool
	}{
		{"ordinary Claude", "claude", "signed_out", "Claude: no existing sign-in found", "Not signed in", true},
		{"absent Codex app sign-in", "codex", "signed_out", "Codex app sign-in is absent: no existing sign-in found", "Not signed in", true},
		{"absent Codex keyring sign-in", "codex", "signed_out", "Codex keyring sign-in is absent: no existing sign-in found", "Not signed in", true},
		{"missing Codex sign-in", "codex", "signed_out", "no readable Codex app sign-in found: no existing sign-in found", "Not signed in", true},
		{"expired Claude", "claude", "signed_out", "Claude: Claude sign-in expired; open Claude to renew it: no existing sign-in found", "! Claude sign-in expired; open Claude to renew it", false},
		{"mismatched Claude account", "claude", "signed_out", "Claude: Claude Desktop login no longer matches its active account: no existing sign-in found", "! Claude Desktop login no longer matches its active account", false},
		{"in-memory Codex sign-in", "codex", "signed_out", "Codex uses an in-memory sign-in: no existing sign-in found", "! Codex uses an in-memory sign-in", false},
		{"unavailable Codex", "codex", "unavailable", "Codex: Can't read usage; check your connection", "! Can't read usage; check your connection", false},
	} {
		t.Run(c.name, func(t *testing.T) {
			s := Status{States: map[string]string{c.provider: c.state}}
			if c.provider == "codex" {
				s.CodexError = c.message
			} else {
				s.ClaudeError = c.message
			}
			before, _ := json.Marshal(s)
			var got bytes.Buffer
			if err := writeProviders(&got, s, outputTestNow, false); err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(got.String(), c.want) || strings.Contains(got.String(), "no existing sign-in found") || strings.Contains(got.String(), "! Codex:") || strings.Contains(got.String(), "! Claude:") {
				t.Fatalf("legacy message was not normalized: %s", got.String())
			}
			if c.calm && strings.Contains(got.String(), "!") {
				t.Fatalf("ordinary sign-out gained an error marker: %s", got.String())
			}
			after, _ := json.Marshal(s)
			if !bytes.Equal(before, after) {
				t.Fatal("text normalization changed cached JSON")
			}
		})
	}
}

type outputTestWriter struct {
	writes int
	short  bool
	err    error
}

func (w *outputTestWriter) Write(value []byte) (int, error) {
	if w.writes > 0 {
		w.writes--
		return len(value), nil
	}
	if w.short {
		return len(value) - 1, nil
	}
	return 0, w.err
}

func TestOutputWriterFailuresPropagate(t *testing.T) {
	want := errors.New("fixture writer failed")
	for _, c := range []struct {
		writer *outputTestWriter
		want   error
	}{{&outputTestWriter{err: want}, want}, {&outputTestWriter{writes: 2, err: want}, want}, {&outputTestWriter{short: true}, io.ErrShortWrite}} {
		if err := writeProviders(c.writer, Status{Readings: map[string]*Reading{"codex": {SessionNoCap: true}}}, outputTestNow, true); !errors.Is(err, c.want) {
			t.Fatalf("writer error = %v, want %v", err, c.want)
		}
	}
}
