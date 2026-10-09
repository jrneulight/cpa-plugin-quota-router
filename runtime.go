package main

import (
	"context"
	"encoding/json"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

type hostClient interface {
	listAuth() ([]pluginapi.HostAuthFileEntry, error)
	getAuth(authIndex string) (json.RawMessage, error)
	log(level, message string, fields map[string]any)
}

type claudeCredential struct {
	Type        string `json:"type"`
	AccessToken string `json:"access_token"`
}

type pluginRuntime struct {
	lifecycleMu sync.Mutex
	refreshMu   sync.Mutex
	config      atomic.Pointer[pluginConfig]
	cache       quotaCache
	host        hostClient
	fetch       usageFetcher
	now         func() time.Time
	wake        chan struct{}
	cancel      context.CancelFunc
	done        chan struct{}
	pendingAll  bool
	pendingIDs  map[string]struct{}
	inFlightAll bool
	inFlightIDs map[string]struct{}
}

func newPluginRuntime(host hostClient, fetch usageFetcher, now func() time.Time) *pluginRuntime {
	if now == nil {
		now = time.Now
	}
	runtime := &pluginRuntime{
		cache: quotaCache{samples: make(map[string]quotaSample)},
		host:  host,
		fetch: fetch,
		now:   now,
	}
	cfg := defaultPluginConfig()
	runtime.config.Store(&cfg)
	return runtime
}

func (r *pluginRuntime) applyConfig(cfg pluginConfig) {
	r.lifecycleMu.Lock()
	defer r.lifecycleMu.Unlock()

	previous := r.loadedConfig()
	r.config.Store(&cfg)
	if !cfg.Enabled {
		r.stopLocked()
		r.cache.reconcile(nil)
		return
	}
	if r.cancel == nil {
		ctx, cancel := context.WithCancel(context.Background())
		wake := make(chan struct{}, 1)
		done := make(chan struct{})
		r.wake, r.cancel, r.done = wake, cancel, done
		go r.refreshLoop(ctx, wake, done)
		r.queueAllRefreshLocked()
		r.log("info", "quota router refresh worker started", map[string]any{
			"cutoff_percent_used": cfg.CutoffPercentUsed,
			"protected_models":    cfg.ProtectedModels,
			"protected_accounts":  cfg.ProtectedAccounts,
			"usage_scope":         cfg.UsageScope,
			"block_on_unknown":    cfg.BlockOnUnknown,
			"minimum_refresh_age": cfg.PollInterval.String(),
			"near_cutoff_percent": cfg.NearCutoffPercent,
			"near_refresh_age":    cfg.NearPollInterval.String(),
			"request_timeout":     cfg.RequestTimeout.String(),
		})
		return
	}
	if r.cache.empty() || quotaSelectionChanged(previous, cfg) {
		r.queueAllRefreshLocked()
	}
	r.log("info", "quota router configuration reloaded", map[string]any{
		"cutoff_percent_used": cfg.CutoffPercentUsed,
		"protected_models":    cfg.ProtectedModels,
		"protected_accounts":  cfg.ProtectedAccounts,
		"usage_scope":         cfg.UsageScope,
		"block_on_unknown":    cfg.BlockOnUnknown,
		"minimum_refresh_age": cfg.PollInterval.String(),
		"near_cutoff_percent": cfg.NearCutoffPercent,
		"near_refresh_age":    cfg.NearPollInterval.String(),
		"request_timeout":     cfg.RequestTimeout.String(),
	})
}

func quotaSelectionChanged(previous, current pluginConfig) bool {
	if !strings.EqualFold(previous.UsageScope, current.UsageScope) || len(previous.ProtectedAccounts) != len(current.ProtectedAccounts) {
		return true
	}
	for i := range previous.ProtectedAccounts {
		if !strings.EqualFold(previous.ProtectedAccounts[i], current.ProtectedAccounts[i]) {
			return true
		}
	}
	return false
}

func (r *pluginRuntime) shutdown() {
	if r == nil {
		return
	}
	r.lifecycleMu.Lock()
	defer r.lifecycleMu.Unlock()
	r.stopLocked()
}

func (r *pluginRuntime) stopLocked() {
	if r.cancel == nil {
		return
	}
	cancel, done := r.cancel, r.done
	cancel()
	if done != nil {
		<-done
	}
	r.wake, r.cancel, r.done = nil, nil, nil
	r.refreshMu.Lock()
	r.pendingAll, r.inFlightAll = false, false
	clear(r.pendingIDs)
	clear(r.inFlightIDs)
	r.refreshMu.Unlock()
	r.log("info", "quota router refresh worker stopped", nil)
}

func (r *pluginRuntime) loadedConfig() pluginConfig {
	if r == nil {
		return defaultPluginConfig()
	}
	if cfg := r.config.Load(); cfg != nil {
		return *cfg
	}
	return defaultPluginConfig()
}

func (r *pluginRuntime) queueAllRefreshLocked() {
	if r.wake == nil {
		return
	}
	r.refreshMu.Lock()
	r.pendingAll = true
	clear(r.pendingIDs)
	wake := r.wake
	r.refreshMu.Unlock()
	select {
	case wake <- struct{}{}:
	default:
	}
}

func (r *pluginRuntime) queueCandidateRefresh(authID string, cfg pluginConfig, now time.Time) {
	authID = strings.TrimSpace(authID)
	if authID == "" {
		return
	}
	r.lifecycleMu.Lock()
	defer r.lifecycleMu.Unlock()
	if r.wake == nil || r.cancel == nil || !r.loadedConfig().Enabled {
		return
	}
	r.refreshMu.Lock()
	if r.pendingAll || r.inFlightAll {
		r.refreshMu.Unlock()
		return
	}
	if _, exists := r.pendingIDs[authID]; exists {
		r.refreshMu.Unlock()
		return
	}
	if _, exists := r.inFlightIDs[authID]; exists {
		r.refreshMu.Unlock()
		return
	}
	minimumAge := cfg.refreshInterval(r.cache.snapshot(authID))
	if !r.cache.claimRefresh(authID, now, cfg.CutoffPercentUsed, minimumAge, cfg.UsageScope) {
		r.refreshMu.Unlock()
		return
	}
	if r.pendingIDs == nil {
		r.pendingIDs = make(map[string]struct{})
	}
	r.pendingIDs[authID] = struct{}{}
	wake := r.wake
	r.refreshMu.Unlock()
	select {
	case wake <- struct{}{}:
	default:
	}
}

func (r *pluginRuntime) refreshLoop(ctx context.Context, wake <-chan struct{}, done chan<- struct{}) {
	defer close(done)
	for {
		select {
		case <-ctx.Done():
			return
		case <-wake:
		}
		for {
			all, authIDs := r.takePendingRefresh()
			if !all && len(authIDs) == 0 {
				break
			}
			cfg := r.loadedConfig()
			if cfg.Enabled {
				r.refreshAuths(ctx, cfg, all, authIDs)
			}
			r.finishRefresh(all, authIDs)
			if ctx.Err() != nil {
				return
			}
		}
	}
}

func (r *pluginRuntime) takePendingRefresh() (bool, map[string]struct{}) {
	r.refreshMu.Lock()
	defer r.refreshMu.Unlock()
	if r.pendingAll {
		r.pendingAll = false
		clear(r.pendingIDs)
		r.inFlightAll = true
		return true, nil
	}
	if len(r.pendingIDs) == 0 {
		return false, nil
	}
	authIDs := r.pendingIDs
	r.pendingIDs = make(map[string]struct{})
	if r.inFlightIDs == nil {
		r.inFlightIDs = make(map[string]struct{}, len(authIDs))
	}
	for authID := range authIDs {
		r.inFlightIDs[authID] = struct{}{}
	}
	return false, authIDs
}

func (r *pluginRuntime) finishRefresh(all bool, authIDs map[string]struct{}) {
	r.refreshMu.Lock()
	defer r.refreshMu.Unlock()
	if all {
		r.inFlightAll = false
		return
	}
	for authID := range authIDs {
		delete(r.inFlightIDs, authID)
	}
}

func (r *pluginRuntime) pollOnce(ctx context.Context, cfg pluginConfig) {
	r.refreshAuths(ctx, cfg, true, nil)
}

func (r *pluginRuntime) refreshAuths(ctx context.Context, cfg pluginConfig, all bool, authIDs map[string]struct{}) {
	if r == nil || r.host == nil || r.fetch == nil || ctx.Err() != nil {
		return
	}
	entries, err := r.host.listAuth()
	if err != nil {
		r.log("warn", "quota router auth discovery failed", map[string]any{"category": "auth_list"})
		return
	}
	auths := physicalClaudeAuths(entries)
	r.cache.reconcile(auths)
	for _, auth := range auths {
		if ctx.Err() != nil {
			return
		}
		if !all {
			if _, selected := authIDs[auth.ID]; !selected {
				continue
			}
		}
		if !matchesProtectedAccount(auth, cfg.ProtectedAccounts) {
			continue
		}
		r.pollAuth(ctx, auth, cfg)
	}
}

func (r *pluginRuntime) pollAuth(ctx context.Context, auth physicalClaudeAuth, cfg pluginConfig) {
	r.cache.recordAttempt(auth.ID, r.now())
	rawAuth, err := r.host.getAuth(auth.AuthIndex)
	if err != nil {
		r.recordPollFailure(auth.ID, pollErrorAuthGet)
		return
	}
	var credential claudeCredential
	if json.Unmarshal(rawAuth, &credential) != nil {
		r.recordPollFailure(auth.ID, pollErrorAuthGet)
		return
	}
	token := strings.TrimSpace(credential.AccessToken)
	if !strings.EqualFold(strings.TrimSpace(credential.Type), "claude") || token == "" {
		r.recordPollFailure(auth.ID, pollErrorMissingToken)
		return
	}
	result, category := r.fetch(ctx, token, cfg.RequestTimeout)
	if category != "" {
		if category != pollErrorCancelled || ctx.Err() == nil {
			r.recordPollFailure(auth.ID, category)
		}
		return
	}
	window, ok := selectUsageWindow(result, cfg.UsageScope)
	if !ok {
		r.recordPollFailure(auth.ID, pollErrorInvalidScope)
		return
	}
	r.cache.recordSuccessScope(auth.ID, cfg.UsageScope, window.PercentUsed, window.ResetAt, r.now())
	r.log("debug", "quota router quota refreshed", map[string]any{
		"auth_id":             auth.ID,
		"usage_scope":         cfg.UsageScope,
		"weekly_percent_used": window.PercentUsed,
		"blocked":             window.PercentUsed >= cfg.CutoffPercentUsed,
	})
}

func (r *pluginRuntime) recordPollFailure(authID, category string) {
	r.cache.recordFailure(authID, category)
	r.log("warn", "quota router quota refresh failed", map[string]any{
		"auth_id":  authID,
		"category": category,
	})
}

func (r *pluginRuntime) log(level, message string, fields map[string]any) {
	if r != nil && r.host != nil {
		r.host.log(level, message, fields)
	}
}
