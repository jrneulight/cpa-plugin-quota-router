package main

import (
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
	"gopkg.in/yaml.v3"
)

type lifecycleRequest struct {
	ConfigYAML []byte `json:"config_yaml"`
}

type rawPluginConfig struct {
	Enabled           *bool            `yaml:"enabled"`
	ProtectedModels   *[]string        `yaml:"protected-models"`
	ProtectedAccounts *[]string        `yaml:"protected-accounts"`
	UsageScope        string           `yaml:"usage-scope"`
	BlockOnUnknown    *bool            `yaml:"block-on-unknown"`
	CutoffPercentUsed *float64         `yaml:"cutoff-percent-used"`
	NearCutoffPercent *float64         `yaml:"near-cutoff-percent-used"`
	PollInterval      string           `yaml:"poll-interval"`
	NearPollInterval  string           `yaml:"near-cutoff-poll-interval"`
	RequestTimeout    string           `yaml:"request-timeout"`
	BedrockCompat     *bool            `yaml:"bedrock-compat"`
	Cutoffs           *[]rawCutoffRule `yaml:"cutoffs"`
	UsageSource       string           `yaml:"usage-source"`
	SeatPacerFile     string           `yaml:"seat-pacer-file"`
}

type rawCutoffRule struct {
	Scope       string   `yaml:"scope"`
	PercentUsed *float64 `yaml:"percent-used"`
	Models      []string `yaml:"models"`
}

type pluginConfig struct {
	Enabled           bool
	ProtectedModels   []string
	ProtectedAccounts []string
	UsageScope        string
	BlockOnUnknown    bool
	CutoffPercentUsed float64
	NearCutoffPercent float64
	PollInterval      time.Duration
	NearPollInterval  time.Duration
	RequestTimeout    time.Duration
	BedrockCompat     bool
	// Cutoffs, when set, replace the single usage-scope rule with one rule per
	// usage window. Protected accounts are refused once any rule covering the
	// requested model reaches its percent used.
	Cutoffs []cutoffRule
	// UsageSource is usageSourceEndpoint, which polls Anthropic's usage
	// endpoint, or usageSourceSeatPacerFile, which reads the readings the
	// claude-seat-pacer plugin saves. SeatPacerFile is that file's path, with
	// a leading ~/ already expanded.
	UsageSource   string
	SeatPacerFile string
}

type cutoffRule struct {
	Scope       string
	PercentUsed float64
	// Models is nil when the rule covers every model.
	Models []string
}

type registration struct {
	SchemaVersion uint32                   `json:"schema_version"`
	Metadata      pluginapi.Metadata       `json:"metadata"`
	Capabilities  registrationCapabilities `json:"capabilities"`
}

type registrationCapabilities struct {
	Scheduler          bool `json:"scheduler"`
	RequestInterceptor bool `json:"request_interceptor"`
	ManagementAPI      bool `json:"management_api"`
}

type managementRegistrationResponse struct {
	Routes []managementRoute `json:"routes"`
}

type managementRoute struct {
	Method string `json:"method"`
	Path   string `json:"path"`
}

func defaultPluginConfig() pluginConfig {
	return pluginConfig{
		Enabled:           true,
		ProtectedModels:   []string{defaultProtectedModel},
		UsageScope:        defaultUsageScope,
		CutoffPercentUsed: defaultCutoffPercentUsed,
		NearCutoffPercent: disabledNearCutoffPercent,
		PollInterval:      defaultPollInterval,
		NearPollInterval:  defaultNearPollInterval,
		RequestTimeout:    defaultRequestTimeout,
		UsageSource:       usageSourceEndpoint,
		SeatPacerFile:     defaultSeatPacerFile,
	}
}

func decodeLifecycleConfig(raw []byte) (pluginConfig, error) {
	var request lifecycleRequest
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &request); err != nil {
			return pluginConfig{}, fmt.Errorf("decode lifecycle request: %w", err)
		}
	}
	cfg := defaultPluginConfig()
	if len(request.ConfigYAML) == 0 {
		return cfg, nil
	}
	var decoded rawPluginConfig
	if err := yaml.Unmarshal(request.ConfigYAML, &decoded); err != nil {
		return pluginConfig{}, fmt.Errorf("decode plugin config: %w", err)
	}
	if decoded.Enabled != nil {
		cfg.Enabled = *decoded.Enabled
	}
	if decoded.BedrockCompat != nil {
		cfg.BedrockCompat = *decoded.BedrockCompat
	}
	if decoded.ProtectedModels != nil {
		protectedModels, err := normalizeProtectedModels(*decoded.ProtectedModels)
		if err != nil {
			return pluginConfig{}, err
		}
		cfg.ProtectedModels = protectedModels
	}
	if decoded.ProtectedAccounts != nil {
		protectedAccounts, err := normalizeProtectedAccounts(*decoded.ProtectedAccounts)
		if err != nil {
			return pluginConfig{}, err
		}
		cfg.ProtectedAccounts = protectedAccounts
	}
	if value := strings.TrimSpace(decoded.UsageScope); value != "" {
		cfg.UsageScope = strings.ToLower(value)
	}
	if decoded.BlockOnUnknown != nil {
		cfg.BlockOnUnknown = *decoded.BlockOnUnknown
	}
	if decoded.CutoffPercentUsed != nil {
		cfg.CutoffPercentUsed = *decoded.CutoffPercentUsed
	}
	if decoded.NearCutoffPercent != nil {
		cfg.NearCutoffPercent = *decoded.NearCutoffPercent
	}
	if value := strings.TrimSpace(decoded.PollInterval); value != "" {
		interval, err := time.ParseDuration(value)
		if err != nil {
			return pluginConfig{}, fmt.Errorf("poll-interval must be a Go duration")
		}
		cfg.PollInterval = interval
	}
	if value := strings.TrimSpace(decoded.NearPollInterval); value != "" {
		interval, err := time.ParseDuration(value)
		if err != nil {
			return pluginConfig{}, fmt.Errorf("near-cutoff-poll-interval must be a Go duration")
		}
		cfg.NearPollInterval = interval
	}
	if value := strings.TrimSpace(decoded.RequestTimeout); value != "" {
		timeout, err := time.ParseDuration(value)
		if err != nil {
			return pluginConfig{}, fmt.Errorf("request-timeout must be a Go duration")
		}
		cfg.RequestTimeout = timeout
	}
	if math.IsNaN(cfg.CutoffPercentUsed) || math.IsInf(cfg.CutoffPercentUsed, 0) || cfg.CutoffPercentUsed < 0 || cfg.CutoffPercentUsed > 100 {
		return pluginConfig{}, fmt.Errorf("cutoff-percent-used must be between 0 and 100")
	}
	if cfg.PollInterval <= 0 {
		return pluginConfig{}, fmt.Errorf("poll-interval must be greater than zero")
	}
	if decoded.Cutoffs != nil {
		cutoffs, err := normalizeCutoffRules(*decoded.Cutoffs)
		if err != nil {
			return pluginConfig{}, err
		}
		cfg.Cutoffs = cutoffs
	}
	if cfg.NearCutoffPercent != disabledNearCutoffPercent {
		if math.IsNaN(cfg.NearCutoffPercent) || math.IsInf(cfg.NearCutoffPercent, 0) || cfg.NearCutoffPercent < 0 {
			return pluginConfig{}, fmt.Errorf("near-cutoff-percent-used must be between 0 and 100")
		}
		if len(cfg.Cutoffs) == 0 && cfg.NearCutoffPercent >= cfg.CutoffPercentUsed {
			return pluginConfig{}, fmt.Errorf("near-cutoff-percent-used must be below cutoff-percent-used")
		}
		for _, rule := range cfg.Cutoffs {
			if cfg.NearCutoffPercent >= rule.PercentUsed {
				return pluginConfig{}, fmt.Errorf("near-cutoff-percent-used must be below every cutoffs percent-used")
			}
		}
		if cfg.NearPollInterval <= 0 || cfg.NearPollInterval > cfg.PollInterval {
			return pluginConfig{}, fmt.Errorf("near-cutoff-poll-interval must be greater than zero and no longer than poll-interval")
		}
	}
	if cfg.RequestTimeout <= 0 {
		return pluginConfig{}, fmt.Errorf("request-timeout must be greater than zero")
	}
	if cfg.UsageScope == "" {
		return pluginConfig{}, fmt.Errorf("usage-scope must not be empty")
	}
	if value := strings.TrimSpace(decoded.UsageSource); value != "" {
		cfg.UsageSource = strings.ToLower(value)
	}
	if value := strings.TrimSpace(decoded.SeatPacerFile); value != "" {
		cfg.SeatPacerFile = value
	}
	switch cfg.UsageSource {
	case usageSourceEndpoint:
	case usageSourceSeatPacerFile:
		if len(cfg.Cutoffs) == 0 {
			return pluginConfig{}, fmt.Errorf("usage-source seat-pacer-file requires cutoffs")
		}
		path, err := expandHomePath(cfg.SeatPacerFile)
		if err != nil {
			return pluginConfig{}, err
		}
		cfg.SeatPacerFile = path
	default:
		return pluginConfig{}, fmt.Errorf("usage-source must be %s or %s", usageSourceEndpoint, usageSourceSeatPacerFile)
	}
	return cfg, nil
}

// expandHomePath resolves a leading ~/ against the user's home directory. A
// relative path is left as it is, so it resolves against the host process's
// working directory.
func expandHomePath(path string) (string, error) {
	if path != "~" && !strings.HasPrefix(path, "~/") {
		return path, nil
	}
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return "", fmt.Errorf("seat-pacer-file: cannot resolve ~ without a home directory")
	}
	return filepath.Join(home, strings.TrimPrefix(path, "~")), nil
}

func (cfg pluginConfig) refreshInterval(sample quotaSample) time.Duration {
	if cfg.NearCutoffPercent != disabledNearCutoffPercent &&
		sample.HasSample && strings.EqualFold(sample.UsageScope, cfg.UsageScope) &&
		sample.WeeklyPercentUsed >= cfg.NearCutoffPercent {
		return cfg.NearPollInterval
	}
	return cfg.PollInterval
}

func normalizeCutoffRules(raw []rawCutoffRule) ([]cutoffRule, error) {
	if len(raw) == 0 {
		return nil, fmt.Errorf("cutoffs must contain at least one rule")
	}
	rules := make([]cutoffRule, 0, len(raw))
	for _, item := range raw {
		scope := strings.ToLower(strings.TrimSpace(item.Scope))
		if scope == "" {
			return nil, fmt.Errorf("cutoffs scope must not be empty")
		}
		if item.PercentUsed == nil {
			return nil, fmt.Errorf("cutoffs %s needs percent-used", scope)
		}
		percent := *item.PercentUsed
		if math.IsNaN(percent) || math.IsInf(percent, 0) || percent < 0 || percent > 100 {
			return nil, fmt.Errorf("cutoffs %s percent-used must be between 0 and 100", scope)
		}
		var models []string
		allModels := len(item.Models) == 0
		for _, model := range item.Models {
			if strings.TrimSpace(model) == allModelsSelector {
				allModels = true
			}
		}
		if !allModels {
			normalized, err := normalizeProtectedModels(item.Models)
			if err != nil {
				return nil, fmt.Errorf("cutoffs %s models: %w", scope, err)
			}
			models = normalized
		}
		rules = append(rules, cutoffRule{Scope: scope, PercentUsed: percent, Models: models})
	}
	return rules, nil
}

func (rule cutoffRule) coversModel(models ...string) bool {
	if rule.Models == nil {
		return true
	}
	for _, model := range models {
		if isProtectedModel(model, rule.Models) {
			return true
		}
	}
	return false
}

func normalizeProtectedAccounts(accounts []string) ([]string, error) {
	normalized := make([]string, 0, len(accounts))
	seen := make(map[string]struct{}, len(accounts))
	for _, account := range accounts {
		account = strings.ToLower(strings.TrimSpace(account))
		if account == "" {
			return nil, fmt.Errorf("protected-accounts must not contain empty selectors")
		}
		if _, exists := seen[account]; exists {
			continue
		}
		seen[account] = struct{}{}
		normalized = append(normalized, account)
	}
	sort.Strings(normalized)
	return normalized, nil
}

func normalizeProtectedModels(models []string) ([]string, error) {
	if len(models) == 0 {
		return nil, fmt.Errorf("protected-models must contain at least one model")
	}
	normalized := make([]string, 0, len(models))
	seen := make(map[string]struct{}, len(models))
	for _, model := range models {
		model = strings.ToLower(strings.TrimSpace(model))
		if model == "" {
			return nil, fmt.Errorf("protected-models must not contain empty model names")
		}
		if _, exists := seen[model]; exists {
			continue
		}
		seen[model] = struct{}{}
		normalized = append(normalized, model)
	}
	sort.Strings(normalized)
	return normalized, nil
}

func managementRegistration() managementRegistrationResponse {
	return managementRegistrationResponse{Routes: []managementRoute{{
		Method: http.MethodGet,
		Path:   managementStatusRoute,
	}}}
}

func pluginRegistration() registration {
	return registration{
		SchemaVersion: pluginabi.SchemaVersion,
		Metadata: pluginapi.Metadata{
			Name:             pluginName,
			Version:          pluginVersion,
			Author:           "Smarty Pants Inc; scoped-account fork by jrneulight",
			GitHubRepository: "https://github.com/jrneulight/cpa-plugin-quota-router",
			ConfigFields: []pluginapi.ConfigField{
				{
					Name:        "protected-models",
					Type:        pluginapi.ConfigFieldTypeArray,
					Description: "Exact Claude model IDs whose routing is paused at the weekly cutoff. Default: [claude-fable-5].",
				},
				{
					Name:        "protected-accounts",
					Type:        pluginapi.ConfigFieldTypeArray,
					Description: "Exact auth ID, auth index, file name, email, or account selectors governed by the cutoff. Empty protects all Claude OAuth accounts.",
				},
				{
					Name:        "usage-scope",
					Type:        pluginapi.ConfigFieldTypeString,
					Description: "Anthropic weekly usage scope: seven_day or an exact weekly_scoped model display name such as fable. Default: seven_day.",
				},
				{
					Name:        "block-on-unknown",
					Type:        pluginapi.ConfigFieldTypeBoolean,
					Description: "Treat missing, expired, or stale quota data as blocked for protected accounts. Default: false.",
				},
				{
					Name:        "cutoff-percent-used",
					Type:        pluginapi.ConfigFieldTypeNumber,
					Description: "Blocks a protected Claude auth when the selected usage scope reaches this percent used. Default: 50.",
				},
				{
					Name:        "poll-interval",
					Type:        pluginapi.ConfigFieldTypeString,
					Description: "Minimum cached-usage age below the near-cutoff threshold. Default: 5m.",
				},
				{
					Name:        "near-cutoff-percent-used",
					Type:        pluginapi.ConfigFieldTypeNumber,
					Description: "Optional utilization threshold that switches to the shorter near-cutoff polling interval.",
				},
				{
					Name:        "near-cutoff-poll-interval",
					Type:        pluginapi.ConfigFieldTypeString,
					Description: "Minimum cached-usage age at or above near-cutoff-percent-used. Default: 1m.",
				},
				{
					Name:        "request-timeout",
					Type:        pluginapi.ConfigFieldTypeString,
					Description: "Anthropic usage request timeout as a Go duration. Default: 10s.",
				},
				{
					Name:        "cutoffs",
					Type:        pluginapi.ConfigFieldTypeArray,
					Description: "Optional per-window rules ({scope, percent-used, models}) that replace usage-scope and cutoff-percent-used. scope is five_hour, seven_day or a weekly_scoped model name such as fable; models defaults to every model (\"*\"). Protected accounts are refused, before routing and after selection, once any rule covering the model is reached; with block-on-unknown a missing or stale reading also refuses.",
				},
				{
					Name:        "usage-source",
					Type:        pluginapi.ConfigFieldTypeString,
					Description: "Where protected-account usage readings come from: endpoint polls Anthropic's usage endpoint; seat-pacer-file reads the readings the claude-seat-pacer plugin saves and makes no request to Anthropic (requires cutoffs). Default: endpoint.",
				},
				{
					Name:        "seat-pacer-file",
					Type:        pluginapi.ConfigFieldTypeString,
					Description: "Path of claude-seat-pacer's history.json, read when usage-source is seat-pacer-file. A leading ~/ is the user's home; a relative path resolves against CLIProxyAPI's working directory. Default: " + defaultSeatPacerFile + ".",
				},
				{
					Name:        "bedrock-compat",
					Type:        pluginapi.ConfigFieldTypeBoolean,
					Description: "Sanitize Claude Code-only metadata, beta flags, and unsupported server tools when the selected upstream is Amazon Bedrock. Default: false.",
				},
			},
		},
		Capabilities: registrationCapabilities{Scheduler: true, RequestInterceptor: true, ManagementAPI: true},
	}
}
