package pipkin

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net/http"
	"strconv"
	"strings"
	"time"
)

const codexUsageURL = "https://chatgpt.com/backend-api/wham/usage"

func fetchCodexUsage(ctx context.Context, salt string) (*Reading, error) {
	auth, err := loadCodexAuth(ctx)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, codexUsageURL, nil)
	if err != nil {
		return nil, errors.New("could not prepare Codex usage request")
	}
	req.Header.Set("Authorization", "Bearer "+auth.AccessToken)
	req.Header.Set("User-Agent", "Pipkin/"+version)
	if auth.AccountID != "" {
		req.Header.Set("ChatGPT-Account-Id", auth.AccountID)
	}
	data, err := requestUsage(req, "Codex", map[int]error{
		http.StatusUnauthorized: fmt.Errorf("Codex sign-in needs refreshing in the Codex app: %w", errSignedOut),
	})
	if err != nil {
		return nil, err
	}
	reading, err := parseCodexUsage(data, auth.AccountID, salt, time.Now().Unix())
	if reading != nil {
		reading.Source = auth.Source
	}
	return reading, err
}

type codexUsageWindow struct {
	UsedPercent        *float64 `json:"used_percent"`
	LimitWindowSeconds int64    `json:"limit_window_seconds"`
	ResetAt            float64  `json:"reset_at"`
	ResetAfterSeconds  *int64   `json:"reset_after_seconds"`
}

type codexUsageLimits struct {
	Primary   *codexUsageWindow `json:"primary_window"`
	Secondary *codexUsageWindow `json:"secondary_window"`
}

func (window *codexUsageWindow) valid() bool {
	return window != nil && window.UsedPercent != nil &&
		!math.IsNaN(*window.UsedPercent) && !math.IsInf(*window.UsedPercent, 0) &&
		window.LimitWindowSeconds >= 0 && window.LimitWindowSeconds <= 366*86400
}

func parseCodexUsage(data []byte, account, salt string, observed int64) (*Reading, error) {
	var payload struct {
		AccountID string           `json:"account_id"`
		PlanType  string           `json:"plan_type"`
		RateLimit codexUsageLimits `json:"rate_limit"`
		Credits   *struct {
			HasCredits *bool           `json:"has_credits"`
			Unlimited  bool            `json:"unlimited"`
			Balance    json.RawMessage `json:"balance"`
		} `json:"credits"`
		ResetCredits *struct {
			AvailableCount *int64 `json:"available_count"`
		} `json:"rate_limit_reset_credits"`
		Additional []struct {
			Name      string           `json:"limit_name"`
			Feature   string           `json:"metered_feature"`
			RateLimit codexUsageLimits `json:"rate_limit"`
		} `json:"additional_rate_limits"`
	}
	if json.Unmarshal(data, &payload) != nil {
		return nil, errors.New("Codex usage response was not valid JSON")
	}
	r := &Reading{Account: "codex", Observed: observed, Plan: payload.PlanType, ReportsBanked: true}
	r.Session, r.Weekly = codexUsageWindows(payload.RateLimit, observed)
	// Only a valid weekly-only response confirms there is no session cap.
	r.SessionNoCap = r.Session == nil && r.Weekly != nil &&
		codexWeeklyOrAbsent(payload.RateLimit.Primary) && codexWeeklyOrAbsent(payload.RateLimit.Secondary)
	if payload.AccountID != "" {
		account = payload.AccountID
	}
	if account != "" {
		r.Account = accountDigest("c", salt, account)
	}
	if credits := payload.Credits; credits != nil {
		r.Credits = &Credits{HasCredits: credits.HasCredits, Unlimited: credits.Unlimited,
			Balance: codexCreditBalance(credits.Balance)}
		if r.Credits.Balance == nil && credits.HasCredits != nil && !*credits.HasCredits && !credits.Unlimited {
			zero := 0.0
			r.Credits.Balance = &zero
		}
	}
	if credits := payload.ResetCredits; credits != nil && credits.AvailableCount != nil && *credits.AvailableCount >= 0 {
		r.Banked = credits.AvailableCount
	}
	for _, extra := range payload.Additional {
		name := extra.Feature
		if name == "" {
			name = extra.Name
		}
		if name == "" {
			continue
		}
		session, weekly := codexUsageWindows(extra.RateLimit, observed)
		if r.Models == nil && (session != nil || weekly != nil) {
			r.Models = make(map[string]*Window)
		}
		if session != nil {
			r.Models[name+"_session"] = session
		}
		if weekly != nil {
			r.Models[name+"_weekly"] = weekly
		}
	}
	if r.Session == nil && r.Weekly == nil && r.Credits == nil && len(r.Models) == 0 && r.Banked == nil {
		return nil, errors.New("Codex did not report usage limits or credits")
	}
	return r, nil
}

func codexUsageWindows(limits codexUsageLimits, observed int64) (session, weekly *Window) {
	for index, item := range []*codexUsageWindow{limits.Primary, limits.Secondary} {
		if !item.valid() {
			continue
		}
		window := &Window{Used: usedTenths(*item.UsedPercent), Reset: validEpoch(item.ResetAt), Seconds: item.LimitWindowSeconds}
		if window.Reset == 0 && item.ResetAfterSeconds != nil && *item.ResetAfterSeconds >= 0 && *item.ResetAfterSeconds < lastEpoch-observed {
			window.Reset = validEpoch(float64(observed + *item.ResetAfterSeconds))
		}
		isWeekly := window.Seconds >= 6*86400 || (window.Seconds == 0 && index == 1)
		if isWeekly && weekly == nil {
			weekly = window
		} else if !isWeekly && session == nil {
			session = window
		}
	}
	return session, weekly
}

func codexWeeklyOrAbsent(window *codexUsageWindow) bool {
	return window == nil || window.valid() && window.LimitWindowSeconds >= 6*86400
}

func codexCreditBalance(raw json.RawMessage) *float64 {
	text := strings.TrimSpace(string(raw))
	if len(text) > 0 && text[0] == '"' {
		if json.Unmarshal(raw, &text) != nil {
			return nil
		}
	}
	value, err := strconv.ParseFloat(text, 64)
	if err != nil || value < 0 || math.IsNaN(value) || math.IsInf(value, 0) {
		return nil
	}
	return &value
}
