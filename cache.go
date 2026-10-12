package main

import (
	"sort"
	"strings"
	"sync"
	"time"
)

type cutoffStatusResponse struct {
	Enabled                bool                  `json:"enabled"`
	ProtectedModels        []string              `json:"protected_models"`
	CutoffPercentUsed      float64               `json:"cutoff_percent_used"`
	NearCutoffPercentUsed  float64               `json:"near_cutoff_percent_used"`
	PollInterval           string                `json:"poll_interval"`
	NearCutoffPollInterval string                `json:"near_cutoff_poll_interval"`
	ProtectedAccounts      []string              `json:"protected_accounts,omitempty"`
	UsageScope             string                `json:"usage_scope"`
	BlockOnUnknown         bool                  `json:"block_on_unknown"`
	Cutoffs                []cutoffStatusRule    `json:"cutoffs,omitempty"`
	Accounts               []cutoffAccountStatus `json:"accounts"`
}

type cutoffAccountStatus struct {
	ID                string   `json:"id"`
	AuthIndex         string   `json:"auth_index,omitempty"`
	Name              string   `json:"name,omitempty"`
	Email             string   `json:"email,omitempty"`
	Protected         bool     `json:"protected"`
	Known             bool     `json:"known"`
	Fresh             bool     `json:"fresh"`
	Blocked           bool     `json:"blocked"`
	WeeklyPercentUsed *float64 `json:"weekly_percent_used,omitempty"`
	UsageScope        string   `json:"usage_scope,omitempty"`
	RefreshInterval   string   `json:"refresh_interval,omitempty"`
	SampledAt         string   `json:"sampled_at,omitempty"`
	ResetAt           string   `json:"reset_at,omitempty"`
	LastErrorCategory string   `json:"last_error_category,omitempty"`
	// Windows, ReserveRefused and ReserveScope report cutoffs state. The
	// verdict is the all-models one; a model-specific rule can refuse more.
	Windows          map[string]windowStatus `json:"windows,omitempty"`
	WindowsSampledAt string                  `json:"windows_sampled_at,omitempty"`
	ReserveRefused   bool                    `json:"reserve_refused,omitempty"`
	ReserveScope     string                  `json:"reserve_scope,omitempty"`
}

type quotaSample struct {
	AuthIndex         string
	Name              string
	Email             string
	Account           string
	Identity          string
	UsageScope        string
	HasSample         bool
	WeeklyPercentUsed float64
	SampledAt         time.Time
	LastAttemptAt     time.Time
	ResetAt           time.Time
	LastErrorCategory string
	// Windows and WindowsSampledAt carry the latest reading of every usage
	// window, used by cutoffs rules.
	Windows          map[string]usageWindowResult
	WindowsSampledAt time.Time
}

func (s quotaSample) known(now time.Time) bool {
	return s.HasSample && (s.ResetAt.IsZero() || now.Before(s.ResetAt))
}

func (s quotaSample) blocked(now time.Time, cutoff float64, scope ...string) bool {
	if len(scope) > 0 && !strings.EqualFold(s.UsageScope, scope[0]) {
		return false
	}
	return s.known(now) && s.WeeklyPercentUsed >= cutoff
}

func (s quotaSample) fresh(now time.Time, maximumAge time.Duration, scope string) bool {
	return s.known(now) && strings.EqualFold(s.UsageScope, scope) && !s.SampledAt.IsZero() && !now.After(s.SampledAt.Add(maximumAge))
}

type quotaCache struct {
	mu      sync.Mutex
	samples map[string]quotaSample
}

func (c *quotaCache) empty() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.samples) == 0
}

func (c *quotaCache) reconcile(auths []physicalClaudeAuth) {
	keep := make(map[string]struct{}, len(auths))
	c.mu.Lock()
	for _, auth := range auths {
		if strings.TrimSpace(auth.ID) == "" {
			continue
		}
		keep[auth.ID] = struct{}{}
		sample := c.samples[auth.ID]
		if sample.Identity != "" && auth.Identity != "" && sample.Identity != auth.Identity {
			sample = quotaSample{}
		}
		sample.AuthIndex = auth.AuthIndex
		sample.Name = strings.TrimSpace(auth.Name)
		sample.Email = strings.ToLower(strings.TrimSpace(auth.Email))
		sample.Account = strings.ToLower(strings.TrimSpace(auth.Account))
		sample.Identity = auth.Identity
		c.samples[auth.ID] = sample
	}
	for authID := range c.samples {
		if _, ok := keep[authID]; !ok {
			delete(c.samples, authID)
		}
	}
	c.mu.Unlock()
}

func (c *quotaCache) claimRefresh(authID string, now time.Time, cutoff float64, minimumAge time.Duration, usageScope ...string) bool {
	if strings.TrimSpace(authID) == "" {
		return false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	sample := c.samples[authID]
	if sample.blocked(now, cutoff, usageScope...) {
		return false
	}
	if len(usageScope) > 0 && !strings.EqualFold(sample.UsageScope, usageScope[0]) {
		sample.LastAttemptAt = now
		c.samples[authID] = sample
		return true
	}
	lastCheck := sample.SampledAt
	if sample.LastAttemptAt.After(lastCheck) {
		lastCheck = sample.LastAttemptAt
	}
	if !lastCheck.IsZero() && now.Before(lastCheck.Add(minimumAge)) {
		return false
	}
	sample.LastAttemptAt = now
	c.samples[authID] = sample
	return true
}

func (c *quotaCache) recordAttempt(authID string, attemptedAt time.Time) {
	if strings.TrimSpace(authID) == "" {
		return
	}
	c.mu.Lock()
	sample := c.samples[authID]
	sample.LastAttemptAt = attemptedAt
	c.samples[authID] = sample
	c.mu.Unlock()
}

func (c *quotaCache) recordSuccess(authID string, percentUsed float64, resetAt, sampledAt time.Time) {
	c.recordSuccessScope(authID, defaultUsageScope, percentUsed, resetAt, sampledAt)
}

func (c *quotaCache) recordSuccessScope(authID, usageScope string, percentUsed float64, resetAt, sampledAt time.Time) {
	if strings.TrimSpace(authID) == "" {
		return
	}
	c.mu.Lock()
	sample := c.samples[authID]
	sample.HasSample = true
	sample.WeeklyPercentUsed = percentUsed
	sample.UsageScope = strings.ToLower(strings.TrimSpace(usageScope))
	sample.SampledAt = sampledAt
	sample.LastAttemptAt = sampledAt
	sample.ResetAt = resetAt
	sample.LastErrorCategory = ""
	c.samples[authID] = sample
	c.mu.Unlock()
}

func (c *quotaCache) recordWindows(authID string, windows map[string]usageWindowResult, sampledAt time.Time) {
	if strings.TrimSpace(authID) == "" {
		return
	}
	copied := make(map[string]usageWindowResult, len(windows))
	for scope, window := range windows {
		copied[scope] = window
	}
	c.mu.Lock()
	sample := c.samples[authID]
	sample.Windows = copied
	sample.WindowsSampledAt = sampledAt
	sample.LastAttemptAt = sampledAt
	sample.LastErrorCategory = ""
	c.samples[authID] = sample
	c.mu.Unlock()
}

func (c *quotaCache) samplesSnapshot() map[string]quotaSample {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make(map[string]quotaSample, len(c.samples))
	for authID, sample := range c.samples {
		out[authID] = sample
	}
	return out
}

func (c *quotaCache) recordFailure(authID, category string) {
	if strings.TrimSpace(authID) == "" {
		return
	}
	c.mu.Lock()
	sample := c.samples[authID]
	sample.LastErrorCategory = category
	c.samples[authID] = sample
	c.mu.Unlock()
}

func (c *quotaCache) isBlocked(authID string, now time.Time, cutoff float64) bool {
	c.mu.Lock()
	sample := c.samples[authID]
	c.mu.Unlock()
	return sample.blocked(now, cutoff)
}

func (c *quotaCache) snapshot(authID string) quotaSample {
	c.mu.Lock()
	sample := c.samples[authID]
	c.mu.Unlock()
	return sample
}

func (c *quotaCache) statuses(now time.Time, cfg pluginConfig) []cutoffAccountStatus {
	c.mu.Lock()
	defer c.mu.Unlock()

	accounts := make([]cutoffAccountStatus, 0, len(c.samples))
	for authID, sample := range c.samples {
		known := sample.known(now) && strings.EqualFold(sample.UsageScope, cfg.UsageScope)
		protected := matchesProtectedAccount(physicalClaudeAuth{ID: authID, AuthIndex: sample.AuthIndex, Name: sample.Name, Email: sample.Email, Account: sample.Account}, cfg.ProtectedAccounts)
		refreshInterval := cfg.refreshInterval(sample)
		fresh := sample.fresh(now, refreshInterval, cfg.UsageScope)
		account := cutoffAccountStatus{
			ID:                authID,
			AuthIndex:         sample.AuthIndex,
			Name:              sample.Name,
			Email:             sample.Email,
			Protected:         protected,
			Known:             known,
			Fresh:             fresh,
			Blocked:           sample.blocked(now, cfg.CutoffPercentUsed, cfg.UsageScope) || (protected && cfg.BlockOnUnknown && !fresh),
			UsageScope:        sample.UsageScope,
			RefreshInterval:   refreshInterval.String(),
			LastErrorCategory: sample.LastErrorCategory,
		}
		if len(cfg.Cutoffs) > 0 {
			account.Windows = windowStatuses(sample.Windows)
			if !sample.WindowsSampledAt.IsZero() {
				account.WindowsSampledAt = sample.WindowsSampledAt.UTC().Format(time.RFC3339Nano)
			}
			if protected {
				verdict := cfg.reserveVerdictForAnyModel(sample, now)
				account.ReserveRefused = verdict.Refused
				account.ReserveScope = verdict.Scope
				account.Blocked = verdict.Refused
			}
		}
		if known {
			percentUsed := sample.WeeklyPercentUsed
			account.WeeklyPercentUsed = &percentUsed
			if !sample.SampledAt.IsZero() {
				account.SampledAt = sample.SampledAt.UTC().Format(time.RFC3339Nano)
			}
			account.ResetAt = sample.ResetAt.UTC().Format(time.RFC3339Nano)
		}
		accounts = append(accounts, account)
	}
	sort.Slice(accounts, func(i, j int) bool { return accounts[i].ID < accounts[j].ID })
	return accounts
}
