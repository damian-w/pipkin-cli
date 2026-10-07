package pipkin

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net/http"
	"strings"
	"time"
)

func fetchClaudeUsage(ctx context.Context, salt string) (*Reading, error) {
	credential, err := loadClaudeCredential(ctx)
	if err != nil {
		return nil, err
	}
	return fetchClaudeCredentialUsage(ctx, credential, salt)
}

func fetchClaudeCredentialUsage(ctx context.Context, credential claudeCredential, salt string) (*Reading, error) {
	raw, err := requestClaudeUsage(ctx, credential.AccessToken, "usage?cedar_ember=1")
	if err != nil {
		return nil, err
	}
	reading, err := parseClaudeUsage(raw, time.Now())
	if err != nil {
		return nil, err
	}
	reading.Source = credential.source
	reading.Plan = credential.Plan
	identity := credential.identity
	profile, profileErr := requestClaudeUsage(ctx, credential.AccessToken, "profile")
	verified := false
	if profileErr == nil {
		var account struct {
			Account struct {
				UUID string `json:"uuid"`
			} `json:"account"`
			Organization struct {
				UUID string `json:"uuid"`
				Type string `json:"organization_type"`
			} `json:"organization"`
		}
		if json.Unmarshal(profile, &account) == nil && validClaudeUUID(account.Account.UUID) && validClaudeUUID(account.Organization.UUID) {
			actual := strings.ToLower(account.Account.UUID + "|" + account.Organization.UUID)
			if identity != "" && identity != actual {
				return nil, fmt.Errorf("Claude Desktop login no longer matches its active account: %w", errSignedOut)
			}
			identity = actual
			verified = true
			if account.Organization.Type != "" {
				reading.Plan = strings.TrimPrefix(account.Organization.Type, "claude_")
			}
		}
	}
	if credential.source == "claude-desktop" && !verified {
		if profileErr != nil {
			return nil, profileErr
		}
		return nil, errors.New("Claude Desktop account identity could not be verified")
	}
	if credential.source == "claude-desktop" && credential.desktopDir != "" {
		if err := validateClaudeDesktopIdentity(ctx, credential); err != nil {
			return nil, err
		}
	}
	if identity == "" {
		identity = credential.AccessToken
	}
	digest := sha256.Sum256([]byte(salt + identity))
	reading.Account = "a" + hex.EncodeToString(digest[:])[:16]
	return reading, nil
}

func requestClaudeUsage(ctx context.Context, token, endpoint string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "https://api.anthropic.com/api/oauth/"+endpoint, nil)
	if err != nil {
		return nil, errors.New("Claude usage request could not be created")
	}
	req.Header.Set("Authorization", "Bearer "+strings.TrimSpace(token))
	req.Header.Set("Accept", "application/json")
	req.Header.Set("anthropic-beta", "oauth-2025-04-20")
	// This surface header also makes the usage endpoint report reset grants.
	req.Header.Set("User-Agent", "claude-cli/2.1.280 (external, cli)")
	response, err := usageHTTPClient.Do(req)
	if err != nil {
		return nil, offlineError{"Claude usage request failed"}
	}
	defer response.Body.Close()
	if response.StatusCode == http.StatusUnauthorized {
		return nil, fmt.Errorf("Claude sign-in expired; open Claude to renew it: %w", errSignedOut)
	}
	if response.StatusCode == http.StatusForbidden {
		return nil, errors.New("Claude sign-in cannot read subscription usage")
	}
	if response.StatusCode != http.StatusOK {
		return nil, usageResponseError("Claude", response)
	}
	raw, err := readBounded(response.Body, 1<<20)
	if err != nil {
		return nil, errors.New("Claude usage response is unreadable or too large")
	}
	return raw, nil
}

type claudeUsageWindow struct {
	Utilization *float64        `json:"utilization"`
	ResetsAt    json.RawMessage `json:"resets_at"`
}

func claudeWindowValue(raw json.RawMessage, seconds int64, session bool) *Window {
	var value claudeUsageWindow
	if json.Unmarshal(raw, &value) != nil || value.Utilization == nil || math.IsNaN(*value.Utilization) || math.IsInf(*value.Utilization, 0) {
		return nil
	}
	var reset string
	if len(value.ResetsAt) != 0 && json.Unmarshal(value.ResetsAt, &reset) != nil {
		return nil
	}
	// Claude's idle session has exactly zero usage and an explicitly null reset.
	return &Window{Used: usedTenths(*value.Utilization), Reset: claudeReset(reset), Seconds: seconds,
		NotStarted: session && *value.Utilization == 0 && string(value.ResetsAt) == "null"}
}

func claudeReset(value string) int64 {
	parsed, err := time.Parse(time.RFC3339Nano, value)
	if err != nil {
		return 0
	}
	return validEpoch(float64(parsed.Unix()))
}

func parseClaudeUsage(raw []byte, now time.Time) (*Reading, error) {
	var payload map[string]json.RawMessage
	if json.Unmarshal(raw, &payload) != nil {
		return nil, errors.New("Claude usage response is invalid")
	}
	r := &Reading{Account: "claude", Observed: now.Unix(), Models: make(map[string]*Window)}
	r.Session = claudeWindowValue(payload["five_hour"], 5*3600, true)
	r.Weekly = claudeWindowValue(payload["seven_day"], 7*86400, false)
	for key, value := range payload {
		if strings.HasPrefix(key, "seven_day_") {
			if window := claudeWindowValue(value, 7*86400, false); window != nil {
				r.Models[strings.TrimPrefix(key, "seven_day_")] = window
			}
		}
	}
	var limits []struct {
		Kind     string   `json:"kind"`
		Percent  *float64 `json:"percent"`
		ResetsAt string   `json:"resets_at"`
		Scope    struct {
			Model struct {
				DisplayName string `json:"display_name"`
			} `json:"model"`
		} `json:"scope"`
	}
	if json.Unmarshal(payload["limits"], &limits) == nil {
		for _, limit := range limits {
			name := strings.ToLower(strings.TrimSpace(limit.Scope.Model.DisplayName))
			if limit.Kind == "weekly_scoped" && limit.Percent != nil && len(name) > 0 && len(name) <= 80 && !strings.ContainsAny(name, "\r\n\t") {
				r.Models[name] = &Window{Used: usedTenths(*limit.Percent), Reset: claudeReset(limit.ResetsAt), Seconds: 7 * 86400}
			}
		}
	}
	var extra *struct {
		Enabled bool     `json:"is_enabled"`
		Used    *float64 `json:"used_credits"`
		Limit   *float64 `json:"monthly_limit"`
		Reset   string   `json:"resets_at"`
	}
	if json.Unmarshal(payload["extra_usage"], &extra) == nil && extra != nil {
		r.ExtraUsage = &ExtraUsage{Enabled: extra.Enabled, Currency: "USD", Reset: claudeReset(extra.Reset)}
		if extra.Used != nil && *extra.Used >= 0 {
			amount := *extra.Used / 100
			r.ExtraUsage.Used = &amount
		}
		if extra.Limit != nil && *extra.Limit > 0 {
			// Anthropic uses zero for uncapped extra usage, not a zero-dollar cap.
			amount := *extra.Limit / 100
			r.ExtraUsage.Limit = &amount
		}
		if r.ExtraUsage.Used != nil && r.ExtraUsage.Limit != nil {
			amount := math.Max(0, *r.ExtraUsage.Limit-*r.ExtraUsage.Used)
			r.ExtraUsage.Remaining = &amount
		}
	}
	var resets *struct {
		Eligible *bool `json:"eligible"`
		Grants   *[]struct {
			Left   *int64 `json:"resets_left"`
			EndsAt string `json:"ends_at"`
		} `json:"grants"`
	}
	if json.Unmarshal(payload["cedar_ember"], &resets) == nil && resets != nil && resets.Eligible != nil {
		r.ReportsBanked = true
		var count int64
		known := !*resets.Eligible || resets.Grants != nil
		if *resets.Eligible && resets.Grants != nil {
			for _, grant := range *resets.Grants {
				expiry := claudeReset(grant.EndsAt)
				if grant.Left == nil || *grant.Left < 0 || grant.EndsAt != "" && expiry == 0 {
					known = false
					continue
				}
				if grant.Left != nil && *grant.Left > 0 && (expiry == 0 || expiry > now.Unix()) {
					count = min(maxBanked, count+min(*grant.Left, maxBanked))
				}
			}
		}
		if known {
			r.Banked = &count
		}
	}
	if r.Session == nil && r.Weekly == nil && len(r.Models) == 0 && r.ExtraUsage == nil && !r.ReportsBanked {
		return nil, errors.New("Claude usage response has no recognized limits")
	}
	return r, nil
}
