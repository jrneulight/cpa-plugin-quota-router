package main

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"math"
	"net/http"
	"strings"
	"time"
)

type usageResult struct {
	WeeklyPercentUsed float64
	ResetAt           time.Time
	ScopedWeekly      map[string]usageWindowResult
	// Windows holds every window the usage endpoint reported, keyed by
	// five_hour, seven_day or a lowercased weekly_scoped model display name.
	// A zero ResetAt means the window has not started, as with an idle
	// five-hour session.
	Windows map[string]usageWindowResult
}

type usageWindowResult struct {
	PercentUsed float64
	ResetAt     time.Time
}

type usageFetcher func(context.Context, string, time.Duration) (usageResult, string)

type httpUsageFetcher struct {
	client   *http.Client
	endpoint string
}

func newHTTPUsageFetcher(endpoint string, transport http.RoundTripper) httpUsageFetcher {
	if transport == nil {
		transport = http.DefaultTransport
	}
	return httpUsageFetcher{
		endpoint: endpoint,
		client: &http.Client{
			Transport: transport,
			CheckRedirect: func(_ *http.Request, _ []*http.Request) error {
				return http.ErrUseLastResponse
			},
		},
	}
}

type usageResponse struct {
	FiveHour *usageWindow `json:"five_hour"`
	SevenDay *usageWindow `json:"seven_day"`
	Limits   []usageLimit `json:"limits"`
}

type usageWindow struct {
	Utilization *float64        `json:"utilization"`
	ResetsAt    json.RawMessage `json:"resets_at"`
}

type usageLimit struct {
	Kind     string          `json:"kind"`
	Percent  *float64        `json:"percent"`
	ResetsAt json.RawMessage `json:"resets_at"`
	Scope    *usageScope     `json:"scope"`
}

type usageScope struct {
	Model *usageScopeModel `json:"model"`
}

type usageScopeModel struct {
	DisplayName string `json:"display_name"`
}

func (f httpUsageFetcher) fetch(ctx context.Context, token string, timeout time.Duration) (usageResult, string) {
	requestCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	req, err := http.NewRequestWithContext(requestCtx, http.MethodGet, f.endpoint, nil)
	if err != nil {
		return usageResult{}, pollErrorHTTP
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("anthropic-beta", anthropicOAuthBeta)

	resp, err := f.client.Do(req)
	if err != nil {
		switch {
		case errors.Is(err, context.Canceled):
			return usageResult{}, pollErrorCancelled
		case errors.Is(err, context.DeadlineExceeded):
			return usageResult{}, pollErrorTimeout
		default:
			return usageResult{}, pollErrorNetwork
		}
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		switch resp.StatusCode {
		case http.StatusUnauthorized:
			return usageResult{}, pollErrorUnauthorized
		case http.StatusForbidden:
			return usageResult{}, pollErrorForbidden
		case http.StatusTooManyRequests:
			return usageResult{}, pollErrorRateLimited
		default:
			if resp.StatusCode >= 500 {
				return usageResult{}, pollErrorServer
			}
			return usageResult{}, pollErrorHTTP
		}
	}

	body, err := io.ReadAll(io.LimitReader(resp.Body, maxUsageResponseBytes+1))
	if err != nil {
		switch {
		case errors.Is(err, context.Canceled):
			return usageResult{}, pollErrorCancelled
		case errors.Is(err, context.DeadlineExceeded):
			return usageResult{}, pollErrorTimeout
		default:
			return usageResult{}, pollErrorRead
		}
	}
	if len(body) > maxUsageResponseBytes {
		return usageResult{}, pollErrorBodyTooLarge
	}
	var payload usageResponse
	if json.Unmarshal(body, &payload) != nil {
		return usageResult{}, pollErrorInvalidJSON
	}
	result := usageResult{
		ScopedWeekly: make(map[string]usageWindowResult),
		Windows:      make(map[string]usageWindowResult),
	}
	if window, ok := parseUsageWindow(payload.FiveHour); ok {
		result.Windows[fiveHourUsageScope] = window
	}
	validSevenDay := false
	if payload.SevenDay != nil {
		percentUsed, validPercent := normalizeWeeklyPercent(payload.SevenDay.Utilization)
		resetAt, validReset := parseResetTime(payload.SevenDay.ResetsAt)
		if validPercent && validReset {
			result.WeeklyPercentUsed = percentUsed
			result.ResetAt = resetAt
			validSevenDay = true
		}
	}
	if window, ok := parseUsageWindow(payload.SevenDay); ok {
		result.Windows[defaultUsageScope] = window
	}
	for _, limit := range payload.Limits {
		if !strings.EqualFold(strings.TrimSpace(limit.Kind), "weekly_scoped") || limit.Scope == nil || limit.Scope.Model == nil {
			continue
		}
		name := strings.ToLower(strings.TrimSpace(limit.Scope.Model.DisplayName))
		if name == "" {
			continue
		}
		percent, validPercent := normalizeWeeklyPercent(limit.Percent)
		if !validPercent {
			continue
		}
		reset, validReset := parseResetTime(limit.ResetsAt)
		if _, seen := result.Windows[name]; !seen {
			result.Windows[name] = usageWindowResult{PercentUsed: percent, ResetAt: reset}
		}
		if !validReset {
			continue
		}
		result.ScopedWeekly[name] = usageWindowResult{PercentUsed: percent, ResetAt: reset}
	}
	if !validSevenDay && len(result.ScopedWeekly) == 0 {
		return usageResult{}, pollErrorInvalidWeekly
	}
	return result, ""
}

func selectUsageWindow(result usageResult, scope string) (usageWindowResult, bool) {
	scope = strings.ToLower(strings.TrimSpace(scope))
	if scope == "" || scope == defaultUsageScope {
		return usageWindowResult{PercentUsed: result.WeeklyPercentUsed, ResetAt: result.ResetAt}, !result.ResetAt.IsZero()
	}
	window, ok := result.ScopedWeekly[scope]
	return window, ok && !window.ResetAt.IsZero()
}

// parseUsageWindow keeps a window whose reset time is missing: Anthropic sends
// a null resets_at for a window that has not started.
func parseUsageWindow(window *usageWindow) (usageWindowResult, bool) {
	if window == nil {
		return usageWindowResult{}, false
	}
	percent, validPercent := normalizeWeeklyPercent(window.Utilization)
	if !validPercent {
		return usageWindowResult{}, false
	}
	reset, _ := parseResetTime(window.ResetsAt)
	return usageWindowResult{PercentUsed: percent, ResetAt: reset}, true
}

// Anthropic reports percentage points: 1.0 means 1%, not 100%.
func normalizeWeeklyPercent(raw *float64) (float64, bool) {
	if raw == nil || math.IsNaN(*raw) || math.IsInf(*raw, 0) || *raw < 0 || *raw > 100 {
		return 0, false
	}
	return *raw, true
}

func parseResetTime(raw json.RawMessage) (time.Time, bool) {
	if len(raw) == 0 || string(raw) == "null" {
		return time.Time{}, false
	}
	var text string
	if json.Unmarshal(raw, &text) != nil {
		return time.Time{}, false
	}
	parsed, err := time.Parse(time.RFC3339Nano, strings.TrimSpace(text))
	return parsed, err == nil
}
