package pipkin

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"reflect"
	"strings"
	"testing"
	"time"
)

const claudeResetTestUsage = `{
  "five_hour": {"utilization": 4, "resets_at": null},
  "cedar_ember": {
    "eligible": true,
    "grants": [
      {"resets_left": 1, "ends_at": "2026-12-11T06:00:00+08:00", "gives_back_weekly": false, "clears": ["five_hour", "seven_day"]},
      {"resets_left": 1, "ends_at": "2027-03-09T07:59:00.123Z", "gives_back_weekly": true, "clears": ["five_hour", "seven_day_overage_included"]},
      {"resets_left": 1, "ends_at": "2027-03-09T07:59:00Z", "gives_back_weekly": true, "clears": ["five_hour", "seven_day_overage_included"]},
      {"resets_left": 2, "ends_at": "2027-01-01T00:00:00Z", "gives_back_weekly": false, "clears": ["five_hour", "seven_day"]},
      {"resets_left": 3, "ends_at": null, "label": "Fable reset"},
      {"resets_left": 9, "ends_at": "2026-11-18T00:00:00Z", "gives_back_weekly": false, "clears": ["five_hour", "seven_day"]},
      {"resets_left": 0, "ends_at": "2027-03-09T07:59:00Z", "gives_back_weekly": true}
    ]
  }
}`

func claudeResetTestReading(t *testing.T) *Reading {
	t.Helper()
	now := time.Date(2026, 11, 18, 0, 0, 0, 0, time.UTC)
	reading, err := parseClaudeUsage([]byte(claudeResetTestUsage), now)
	if err != nil {
		t.Fatal(err)
	}
	return reading
}

func TestClaudeResetGrantDetails(t *testing.T) {
	reading := claudeResetTestReading(t)
	want := []ResetGrant{
		{Type: "full", Remaining: 1, ExpiresAt: 1796940000},
		{Type: "fable", Remaining: 1, ExpiresAt: 1804579140},
		{Type: "fable", Remaining: 1, ExpiresAt: 1804579140},
		{Type: "full", Remaining: 2, ExpiresAt: 1798761600},
		{Type: "unknown", Remaining: 3},
	}
	if !reading.ReportsBanked || reading.Banked == nil || *reading.Banked != 8 || !reflect.DeepEqual(reading.ResetGrants, want) {
		t.Fatalf("reset inventory = %+v, banked = %v", reading.ResetGrants, reading.Banked)
	}
	raw, err := json.Marshal(reading)
	if err != nil {
		t.Fatal(err)
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil {
		t.Fatal(err)
	}
	var grants []map[string]json.RawMessage
	if err := json.Unmarshal(fields["reset_grants"], &grants); err != nil || len(grants) != len(want) {
		t.Fatalf("reset_grants JSON = %s, error = %v", fields["reset_grants"], err)
	}
	for index, grant := range want {
		if string(grants[index]["type"]) != fmt.Sprintf("%q", grant.Type) || string(grants[index]["remaining"]) != fmt.Sprint(grant.Remaining) {
			t.Fatalf("grant JSON = %s", fields["reset_grants"])
		}
		if grant.ExpiresAt == 0 {
			if _, present := grants[index]["expires_at"]; present {
				t.Fatal("unknown expiration became a date")
			}
		} else if string(grants[index]["expires_at"]) != fmt.Sprint(grant.ExpiresAt) {
			t.Fatal("expiration was not serialized in Unix seconds")
		}
	}
}

func TestClaudeResetGrantTypes(t *testing.T) {
	for _, c := range []struct{ name, metadata, want string }{
		{"full", `"gives_back_weekly":false,"clears":["five_hour","seven_day"],"label":"Fable reset"`, "full"},
		{"fable", `"gives_back_weekly":true,"clears":["five_hour","seven_day_overage_included"]`, "fable"},
		{"fable weekly only", `"gives_back_weekly":true,"clears":["seven_day_overage_included"]`, "fable"},
		{"fable takes precedence", `"gives_back_weekly":true,"clears":["five_hour","seven_day"]`, "fable"},
		{"legacy", `"label":"Full reset"`, "unknown"},
		{"missing flag", `"clears":["five_hour","seven_day"]`, "unknown"},
		{"null flag", `"gives_back_weekly":null,"clears":["five_hour","seven_day"]`, "unknown"},
		{"malformed flag", `"gives_back_weekly":"false","clears":["five_hour","seven_day"]`, "unknown"},
		{"missing scope", `"gives_back_weekly":false`, "unknown"},
		{"null scope", `"gives_back_weekly":false,"clears":null`, "unknown"},
		{"malformed scope", `"gives_back_weekly":false,"clears":"seven_day"`, "unknown"},
		{"malformed scope member", `"gives_back_weekly":false,"clears":["five_hour","seven_day",1]`, "unknown"},
		{"session only", `"gives_back_weekly":false,"clears":["five_hour"]`, "unknown"},
		{"weekly only", `"gives_back_weekly":false,"clears":["seven_day"]`, "unknown"},
		{"future scope", `"gives_back_weekly":false,"clears":["five_hour","seven_day_future"]`, "unknown"},
	} {
		t.Run(c.name, func(t *testing.T) {
			raw := `{"cedar_ember":{"eligible":true,"grants":[{"resets_left":1,` + c.metadata + `}]}}`
			reading, err := parseClaudeUsage([]byte(raw), time.Unix(testNow, 0))
			if err != nil || reading.Banked == nil || *reading.Banked != 1 || len(reading.ResetGrants) != 1 {
				t.Fatalf("type metadata changed the legacy balance: %+v, %v", reading, err)
			}
			if reading.ResetGrants[0].Type != c.want {
				t.Fatalf("type = %q, want %q", reading.ResetGrants[0].Type, c.want)
			}
		})
	}
}

func TestClaudeResetGrantBalanceBoundaries(t *testing.T) {
	for _, c := range []struct {
		name, raw string
		banked    *int64
		details   int
		reports   bool
	}{
		{"missing", `null`, nil, 0, false},
		{"unknown eligibility", `{}`, nil, 0, false},
		{"ineligible", `{"eligible":false}`, new(int64(0)), 0, true},
		{"missing inventory", `{"eligible":true}`, nil, 0, true},
		{"null inventory", `{"eligible":true,"grants":null}`, nil, 0, true},
		{"empty inventory", `{"eligible":true,"grants":[]}`, new(int64(0)), 0, true},
		{"zero balance", `{"eligible":true,"grants":[{"resets_left":0}]}`, new(int64(0)), 0, true},
		{"missing count", `{"eligible":true,"grants":[{}]}`, nil, 0, true},
		{"negative count", `{"eligible":true,"grants":[{"resets_left":-1}]}`, nil, 0, true},
		{"unknown date", `{"eligible":true,"grants":[{"resets_left":2}]}`, new(int64(2)), 1, true},
		{"invalid date", `{"eligible":true,"grants":[{"resets_left":2,"ends_at":"invalid"}]}`, nil, 0, true},
		{"out of range date", `{"eligible":true,"grants":[{"resets_left":2,"ends_at":"1970-01-01T00:00:00Z"}]}`, nil, 0, true},
		{"expires now", `{"eligible":true,"grants":[{"resets_left":2,"ends_at":"2026-11-18T00:00:00Z"}]}`, new(int64(0)), 0, true},
		{"already expired", `{"eligible":true,"grants":[{"resets_left":2,"ends_at":"2026-11-17T00:00:00Z"}]}`, new(int64(0)), 0, true},
		{"partial inventory", `{"eligible":true,"grants":[{"resets_left":2},{}]}`, nil, 0, true},
		{"saturated count", `{"eligible":true,"grants":[{"resets_left":9223372036854775807},{"resets_left":2}]}`, new(int64(maxBanked)), 2, true},
	} {
		t.Run(c.name, func(t *testing.T) {
			raw := `{"five_hour":{"utilization":4},"cedar_ember":` + c.raw + `}`
			reading, err := parseClaudeUsage([]byte(raw), time.Date(2026, 11, 18, 0, 0, 0, 0, time.UTC))
			if err != nil || reading.ReportsBanked != c.reports || !reflect.DeepEqual(reading.Banked, c.banked) || len(reading.ResetGrants) != c.details {
				t.Fatalf("inventory = %+v, error = %v", reading, err)
			}
			for _, grant := range reading.ResetGrants {
				if grant.Remaining <= 0 || grant.Remaining > maxBanked {
					t.Fatalf("invalid grant count: %+v", grant)
				}
			}
		})
	}
}

func TestOutputClaudeResetGrants(t *testing.T) {
	reading := claudeResetTestReading(t)
	before, err := json.Marshal(reading)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Unix(reading.Observed, 0).In(time.FixedZone("AWST", 8*3600))
	want := [][2]string{
		{"Full resets", "1  expires 11 Dec 2026, 06:00 GMT+08:00"},
		{"Full resets", "2  expires 1 Jan 2027, 08:00 GMT+08:00"},
		{"Fable resets", "2  expires 9 Mar 2027, 15:59 GMT+08:00"},
		{"Resets (unknown type)", "3  expiry unknown"},
	}
	if got := outputResetGrants(reading.ResetGrants, now); !reflect.DeepEqual(got, want) {
		t.Fatalf("reset rows = %v, want %v", got, want)
	}
	status := Status{States: map[string]string{"claude": "available"}, Readings: map[string]*Reading{"claude": reading}}
	var out bytes.Buffer
	if err := writeProviders(&out, status, reading.Observed, true); err != nil {
		t.Fatal(err)
	}
	for _, label := range []string{"Full resets", "Fable resets", "Resets (unknown type)", "expiry unknown"} {
		if !strings.Contains(out.String(), label) {
			t.Fatalf("missing %q in usage output: %s", label, out.String())
		}
	}
	if strings.Contains(out.String(), "Banked resets") {
		t.Fatal("detailed output duplicated the aggregate reset balance")
	}
	out.Reset()
	if err := writeProviders(&out, status, reading.Observed, false); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out.String(), "resets") || strings.Contains(out.String(), "expiry") {
		t.Fatalf("compact status included grant details: %s", out.String())
	}
	after, err := json.Marshal(reading)
	if err != nil || !bytes.Equal(before, after) {
		t.Fatal("rendering changed the source inventory")
	}
}

func TestOutputResetGrantExpirationAndUnknowns(t *testing.T) {
	now := time.Date(2026, 11, 18, 0, 0, 0, 0, time.FixedZone("", 5*3600+45*60))
	for _, c := range []struct {
		name   string
		grants []ResetGrant
		want   [][2]string
	}{
		{"expires now", []ResetGrant{{Type: "full", Remaining: 1, ExpiresAt: now.Unix()}}, [][2]string{{"Full resets", "1  expired 18 Nov 2026, 00:00 GMT+05:45"}}},
		{"unknown type and invalid date", []ResetGrant{{Type: "future\nkind", Remaining: 1, ExpiresAt: 1}}, [][2]string{{"Resets (unknown type)", "1  expiry unknown"}}},
		{"negative count", []ResetGrant{{Type: "fable", Remaining: -1}}, [][2]string{{"Fable resets", "Unknown  expiry unknown"}}},
		{"saturated group", []ResetGrant{{Type: "full", Remaining: 9223372036854775807}, {Type: "full", Remaining: 2}}, [][2]string{{"Full resets", "1000000  expiry unknown"}}},
		{"unknown date sorts last", []ResetGrant{{Type: "full", Remaining: 2}, {Type: "full", Remaining: 1, ExpiresAt: now.Unix()}}, [][2]string{{"Full resets", "1  expired 18 Nov 2026, 00:00 GMT+05:45"}, {"Full resets", "2  expiry unknown"}}},
	} {
		t.Run(c.name, func(t *testing.T) {
			if got := outputResetGrants(c.grants, now); !reflect.DeepEqual(got, c.want) {
				t.Fatalf("reset rows = %v, want %v", got, c.want)
			}
		})
	}
}

func TestClaudeResetGrantsSurviveUsageAndCachedStatus(t *testing.T) {
	isolate(t)
	reading := claudeResetTestReading(t)
	var out bytes.Buffer
	if err := runUsage(context.Background(), true, &out, &bytes.Buffer{}, usageDependencies{
		now:    func() time.Time { return time.Unix(reading.Observed, 0) },
		config: func() (Config, error) { return Config{}, nil },
		collect: func(_ context.Context, provider, _ string) providerResult {
			if provider == "claude" {
				return providerResult{provider: provider, reading: reading}
			}
			return providerResult{provider: provider, reading: &Reading{Account: provider}, err: errSignedOut}
		},
	}); err != nil {
		t.Fatal(err)
	}
	want, err := json.Marshal(reading)
	if err != nil {
		t.Fatal(err)
	}
	var fresh Status
	if err := json.Unmarshal(out.Bytes(), &fresh); err != nil || fresh.SchemaVersion != 1 {
		t.Fatalf("fresh JSON lost reset details: %s, %v", out.String(), err)
	}
	if got, err := json.Marshal(fresh.Readings["claude"]); err != nil || !bytes.Equal(got, want) {
		t.Fatalf("fresh reading = %s, want %s, error = %v", got, want, err)
	}
	h, _ := testHelper()
	h.readings, h.apps = fresh.Readings, fresh.States
	h.writeStatus()
	before, err := os.ReadFile(statusPath())
	if err != nil {
		t.Fatal(err)
	}
	out.Reset()
	if err := statusJSONTo(&out); err != nil {
		t.Fatal(err)
	}
	var cached Status
	if err := json.Unmarshal(out.Bytes(), &cached); err != nil || cached.SchemaVersion != 1 {
		t.Fatalf("cached JSON lost reset details: %s, %v", out.String(), err)
	}
	if got, err := json.Marshal(cached.Readings["claude"]); err != nil || !bytes.Equal(got, want) {
		t.Fatalf("cached reading = %s, want %s, error = %v", got, want, err)
	}
	if after, err := os.ReadFile(statusPath()); err != nil || !bytes.Equal(before, after) {
		t.Fatal("reading cached reset details changed the saved snapshot")
	}
}

func TestClaudeResetGrantsPreserveLegacyUSBPackets(t *testing.T) {
	reading := claudeResetTestReading(t)
	legacy, err := parseClaudeUsage([]byte(`{"five_hour":{"utilization":4,"resets_at":null},"cedar_ember":{"eligible":true,"grants":[{"resets_left":8}]}}`), time.Unix(reading.Observed, 0))
	if err != nil {
		t.Fatal(err)
	}
	// The pre-feature inventory has no grant details on the shared Reading.
	legacy.ResetGrants = nil
	var packets [2]string
	for index, sample := range []*Reading{legacy, reading} {
		h, lines := testHelper()
		h.readings["claude"], h.apps["claude"] = sample, "available"
		h.resync(map[string]string{"seq": "0", "clock_epoch": "0", "unix": "null", "firmware": "2.0.0"})
		packets[index] = strings.Join(*lines, "")
	}
	if packets[0] != packets[1] || !strings.Contains(packets[1], "banked=8\n") {
		t.Fatalf("legacy packets changed:\n%s\nwith grant details:\n%s", packets[0], packets[1])
	}
}
