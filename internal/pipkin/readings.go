package pipkin

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"math"
	"strings"
)

const (
	firstEpoch = 1577836800
	lastEpoch  = 4102444800
	maxBanked  = 1000000
)

// Window is one allowance window. NotStarted takes precedence over its percentage.
type Window struct {
	Used       int   `json:"used"`
	Reset      int64 `json:"reset,omitempty"`
	Seconds    int64 `json:"seconds,omitempty"`
	NotStarted bool  `json:"not_started,omitempty"`
}

type Reading struct {
	Source     string             `json:"source,omitempty"`
	Plan       string             `json:"plan,omitempty"`
	Models     map[string]*Window `json:"models,omitempty"`
	ExtraUsage *ExtraUsage        `json:"extra_usage,omitempty"`
	Credits    *Credits           `json:"credits,omitempty"`
	Account    string             `json:"account"`
	Observed   int64              `json:"observed,omitempty"`
	Session    *Window            `json:"session"`
	Weekly     *Window            `json:"weekly"`
	// SessionNoCap confirms the plan has no session window, rather than one not reported.
	SessionNoCap bool `json:"session_no_cap,omitempty"`
	// ReportsBanked distinguishes an unknown balance (sent as null) from a source without one.
	ReportsBanked bool   `json:"reports_banked,omitempty"`
	Banked        *int64 `json:"banked,omitempty"`
}

// accountDigest identifies an account to the display without revealing it.
func accountDigest(prefix, salt, account string) string {
	digest := sha256.Sum256([]byte(salt + account))
	return prefix + hex.EncodeToString(digest[:8])
}

func usedTenths(percent float64) int {
	if math.IsNaN(percent) || math.IsInf(percent, 0) {
		return 0
	}
	return int(math.Max(0, math.Min(1000, math.Round(percent*10))))
}

func validEpoch(value float64) int64 {
	if value < firstEpoch || value > lastEpoch || math.IsNaN(value) {
		return 0
	}
	return int64(value)
}

// usageFields formats a reading for the display protocol. Absent windows mean unknown.
func usageFields(r *Reading) string {
	observed := "null"
	if r.Observed != 0 {
		observed = fmt.Sprint(r.Observed)
	}
	fields := []string{"account=" + r.Account, "observed=" + observed}
	for _, item := range []struct {
		name   string
		window *Window
	}{{"session", r.Session}, {"weekly", r.Weekly}} {
		w := item.window
		if w == nil {
			if item.name == "session" && r.SessionNoCap {
				fields = append(fields, "session=no_cap")
			}
			continue
		}
		id := "current"
		if w.Reset != 0 {
			id = fmt.Sprintf("r%d", w.Reset)
		}
		if w.NotStarted {
			fields = append(fields, fmt.Sprintf("%[1]s=not_started %[1]s_id=%[2]s", item.name, id))
		} else {
			fields = append(fields, fmt.Sprintf("%[1]s=metered %[1]s_id=%[2]s %[1]s_used=%[3]d", item.name, id, w.Used))
		}
		if w.Reset != 0 {
			fields = append(fields, fmt.Sprintf("%s_reset=%d", item.name, w.Reset))
		}
		if w.Seconds != 0 {
			fields = append(fields, fmt.Sprintf("%s_seconds=%d", item.name, w.Seconds))
		}
	}
	if r.ReportsBanked {
		if r.Banked == nil {
			fields = append(fields, "banked=null")
		} else {
			fields = append(fields, fmt.Sprintf("banked=%d", min(*r.Banked, maxBanked)))
		}
	}
	return strings.Join(fields, " ")
}

// Monetary amounts are in the named currency, never raw API cents.
type ExtraUsage struct {
	Enabled   bool     `json:"enabled"`
	Used      *float64 `json:"used"`
	Limit     *float64 `json:"limit"`
	Remaining *float64 `json:"remaining"`
	Currency  string   `json:"currency,omitempty"`
	Reset     int64    `json:"reset,omitempty"`
}

type Credits struct {
	HasCredits *bool    `json:"has_credits"`
	Unlimited  bool     `json:"unlimited"`
	Balance    *float64 `json:"balance"`
}
