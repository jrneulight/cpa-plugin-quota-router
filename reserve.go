package main

import (
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

// Metadata keys CLIProxyAPI sets once it has chosen a credential
// (cliproxyexecutor.SelectedAuthMetadataKey and SelectedAuthIndexMetadataKey).
const (
	selectedAuthIDMetadataKey    = "selected_auth_id"
	selectedAuthIndexMetadataKey = "selected_auth_index"
)

// reserveStaleAfter is how old a cutoffs reading may grow before a protected
// account with block-on-unknown counts as unknown. Three poll intervals leaves
// room for one failed poll without refusing traffic.
func (cfg pluginConfig) reserveStaleAfter() time.Duration {
	return 3*cfg.PollInterval + cfg.RequestTimeout
}

// reserveVerdict says whether a protected account must be refused for a model,
// and which rule refused it.
type reserveVerdict struct {
	Refused     bool
	Scope       string
	PercentUsed float64
	Cutoff      float64
	Unknown     bool
	ResetAt     time.Time
}

func (cfg pluginConfig) reserveVerdict(sample quotaSample, now time.Time, models ...string) reserveVerdict {
	stale := sample.WindowsSampledAt.IsZero() || now.After(sample.WindowsSampledAt.Add(cfg.reserveStaleAfter()))
	for _, rule := range cfg.Cutoffs {
		if !rule.coversModel(models...) {
			continue
		}
		window, ok := sample.Windows[rule.Scope]
		if stale || !ok {
			if cfg.BlockOnUnknown {
				return reserveVerdict{Refused: true, Scope: rule.Scope, Cutoff: rule.PercentUsed, Unknown: true}
			}
			continue
		}
		if !window.ResetAt.IsZero() && !now.Before(window.ResetAt) {
			// The window has reset since the reading; it starts again from zero.
			continue
		}
		if window.PercentUsed >= rule.PercentUsed {
			return reserveVerdict{Refused: true, Scope: rule.Scope, PercentUsed: window.PercentUsed, Cutoff: rule.PercentUsed, ResetAt: window.ResetAt}
		}
	}
	return reserveVerdict{}
}

func (cfg pluginConfig) cutoffsCoverModel(models ...string) bool {
	for _, rule := range cfg.Cutoffs {
		if rule.coversModel(models...) {
			return true
		}
	}
	return false
}

func sampleIsProtected(authID string, sample quotaSample, selectors []string) bool {
	return matchesProtectedAccount(physicalClaudeAuth{
		ID: authID, AuthIndex: sample.AuthIndex, Name: sample.Name, Email: sample.Email, Account: sample.Account,
	}, selectors)
}

// enforceReserve refuses a request CLIProxyAPI has already routed to a
// protected account past its cutoffs. It is the guard that holds when another
// plugin is the active scheduler, since the host consults only one.
func (r *pluginRuntime) enforceReserve(req pluginapi.RequestInterceptRequest, cfg pluginConfig) (pluginapi.RequestInterceptResponse, bool) {
	if len(cfg.Cutoffs) == 0 || !cfg.cutoffsCoverModel(req.RequestedModel, req.Model) {
		return pluginapi.RequestInterceptResponse{}, false
	}
	authID, sample, found := r.selectedClaudeSample(req.Metadata)
	if !found || !sampleIsProtected(authID, sample, cfg.ProtectedAccounts) {
		return pluginapi.RequestInterceptResponse{}, false
	}
	now := r.now()
	verdict := cfg.reserveVerdict(sample, now, req.RequestedModel, req.Model)
	if !verdict.Refused {
		return pluginapi.RequestInterceptResponse{}, false
	}
	if verdict.Unknown {
		r.queueReserveRefresh()
	}
	r.log("info", "quota router reserve refused request", map[string]any{
		"auth_id":      authID,
		"usage_scope":  verdict.Scope,
		"percent_used": verdict.PercentUsed,
		"cutoff":       verdict.Cutoff,
		"unknown":      verdict.Unknown,
	})
	return reserveRefusal(verdict, now), true
}

func (r *pluginRuntime) selectedClaudeSample(metadata map[string]any) (string, quotaSample, bool) {
	authID, _ := metadata[selectedAuthIDMetadataKey].(string)
	authID = strings.TrimSpace(authID)
	authIndex, _ := metadata[selectedAuthIndexMetadataKey].(string)
	authIndex = strings.TrimSpace(authIndex)
	if authID != "" {
		r.cache.mu.Lock()
		sample, ok := r.cache.samples[authID]
		r.cache.mu.Unlock()
		if ok {
			return authID, sample, true
		}
	}
	if authIndex == "" {
		return "", quotaSample{}, false
	}
	for id, sample := range r.cache.samplesSnapshot() {
		if sample.AuthIndex == authIndex {
			return id, sample, true
		}
	}
	return "", quotaSample{}, false
}

func reserveRefusal(verdict reserveVerdict, now time.Time) pluginapi.RequestInterceptResponse {
	message := fmt.Sprintf("quota-router: account reserve reached on %s (%.0f%% used, cutoff %.0f%%)", verdict.Scope, verdict.PercentUsed, verdict.Cutoff)
	if verdict.Unknown {
		message = fmt.Sprintf("quota-router: account reserve cannot be confirmed on %s (no fresh usage reading)", verdict.Scope)
	}
	body, _ := json.Marshal(map[string]any{
		"type":  "error",
		"error": map[string]string{"type": "rate_limit_error", "message": message, "code": reserveErrorCode},
	})
	headers := http.Header{"Content-Type": []string{"application/json"}}
	if !verdict.ResetAt.IsZero() && verdict.ResetAt.After(now) {
		seconds := int(math.Ceil(verdict.ResetAt.Sub(now).Seconds()))
		headers.Set("Retry-After", strconv.Itoa(seconds))
	}
	return pluginapi.RequestInterceptResponse{
		Terminate:       true,
		StatusCode:      http.StatusTooManyRequests,
		ResponseHeaders: headers,
		ResponseBody:    body,
	}
}

// pickWithCutoffs mirrors the single-scope pick: it stays out of the way while
// no protected candidate is refused, so the host's own routing decides.
func (r *pluginRuntime) pickWithCutoffs(req pluginapi.SchedulerPickRequest, cfg pluginConfig) (pluginapi.SchedulerPickResponse, *envelopeError) {
	if !cfg.cutoffsCoverModel(req.Model) {
		return pluginapi.SchedulerPickResponse{Handled: false}, nil
	}
	now := r.now()
	var selected *pluginapi.SchedulerAuthCandidate
	claudeCandidates, refused := 0, 0
	for i := range req.Candidates {
		candidate := &req.Candidates[i]
		provider := strings.ToLower(strings.TrimSpace(candidate.Provider))
		if provider != "" && provider != "claude" {
			continue
		}
		if candidate.ID == "" || strings.TrimSpace(candidate.ID) != candidate.ID {
			continue
		}
		claudeCandidates++
		sample := r.cache.snapshot(candidate.ID)
		if candidateMatchesProtectedAccount(candidate, sample, cfg.ProtectedAccounts) && cfg.reserveVerdict(sample, now, req.Model).Refused {
			refused++
			continue
		}
		if selected == nil || candidate.Priority > selected.Priority ||
			(candidate.Priority == selected.Priority && candidate.ID < selected.ID) {
			selected = candidate
		}
	}
	if refused == 0 {
		return pluginapi.SchedulerPickResponse{Handled: false}, nil
	}
	if selected != nil {
		return pluginapi.SchedulerPickResponse{AuthID: selected.ID, Handled: true}, nil
	}
	if claudeCandidates > 0 && refused == claudeCandidates {
		return pluginapi.SchedulerPickResponse{}, &envelopeError{Code: reserveErrorCode, Message: reserveErrorCode}
	}
	return pluginapi.SchedulerPickResponse{Handled: false}, nil
}

// reserveRefreshInterval is how often the worker re-reads protected accounts
// while cutoffs are set. Without a scheduler pick to prompt it, the worker
// would otherwise never notice usage climbing toward a cutoff.
func (r *pluginRuntime) reserveRefreshInterval(cfg pluginConfig) time.Duration {
	if len(cfg.Cutoffs) == 0 {
		return 0
	}
	if cfg.NearCutoffPercent == disabledNearCutoffPercent {
		return cfg.PollInterval
	}
	for authID, sample := range r.cache.samplesSnapshot() {
		if !sampleIsProtected(authID, sample, cfg.ProtectedAccounts) {
			continue
		}
		for _, rule := range cfg.Cutoffs {
			if window, ok := sample.Windows[rule.Scope]; ok && window.PercentUsed >= cfg.NearCutoffPercent {
				return cfg.NearPollInterval
			}
		}
	}
	return cfg.PollInterval
}

func (r *pluginRuntime) queueReserveRefresh() {
	r.lifecycleMu.Lock()
	defer r.lifecycleMu.Unlock()
	r.queueAllRefreshLocked()
}

type cutoffStatusRule struct {
	Scope       string   `json:"scope"`
	PercentUsed float64  `json:"percent_used"`
	Models      []string `json:"models"`
}

type windowStatus struct {
	PercentUsed float64 `json:"percent_used"`
	ResetAt     string  `json:"reset_at,omitempty"`
}

func cutoffStatusRules(rules []cutoffRule) []cutoffStatusRule {
	out := make([]cutoffStatusRule, 0, len(rules))
	for _, rule := range rules {
		models := rule.Models
		if models == nil {
			models = []string{allModelsSelector}
		}
		out = append(out, cutoffStatusRule{Scope: rule.Scope, PercentUsed: rule.PercentUsed, Models: models})
	}
	return out
}

func windowStatuses(windows map[string]usageWindowResult) map[string]windowStatus {
	if len(windows) == 0 {
		return nil
	}
	scopes := make([]string, 0, len(windows))
	for scope := range windows {
		scopes = append(scopes, scope)
	}
	sort.Strings(scopes)
	out := make(map[string]windowStatus, len(windows))
	for _, scope := range scopes {
		window := windows[scope]
		status := windowStatus{PercentUsed: window.PercentUsed}
		if !window.ResetAt.IsZero() {
			status.ResetAt = window.ResetAt.UTC().Format(time.RFC3339Nano)
		}
		out[scope] = status
	}
	return out
}

// reserveVerdictForAnyModel is the verdict for a request no rule's model list
// narrows: every rule applies, as if the model were covered by all of them.
func (cfg pluginConfig) reserveVerdictForAnyModel(sample quotaSample, now time.Time) reserveVerdict {
	all := cfg
	all.Cutoffs = make([]cutoffRule, len(cfg.Cutoffs))
	for i, rule := range cfg.Cutoffs {
		rule.Models = nil
		all.Cutoffs[i] = rule
	}
	return all.reserveVerdict(sample, now)
}
