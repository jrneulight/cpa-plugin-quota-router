package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
	"strings"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

type physicalClaudeAuth struct {
	ID        string
	AuthIndex string
	Name      string
	Email     string
	Account   string
	Identity  string
}

func physicalClaudeAuths(entries []pluginapi.HostAuthFileEntry) []physicalClaudeAuth {
	auths := make([]physicalClaudeAuth, 0, len(entries))
	for _, entry := range entries {
		provider := strings.ToLower(strings.TrimSpace(entry.Provider))
		if provider == "" {
			provider = strings.ToLower(strings.TrimSpace(entry.Type))
		}
		if provider != "claude" || entry.Disabled || strings.EqualFold(strings.TrimSpace(entry.Status), "disabled") || entry.RuntimeOnly || strings.TrimSpace(entry.Path) == "" {
			continue
		}
		id := strings.TrimSpace(entry.ID)
		if id == "" || id != entry.ID || strings.TrimSpace(entry.AuthIndex) == "" {
			continue
		}
		auths = append(auths, physicalClaudeAuth{
			ID:        entry.ID,
			AuthIndex: entry.AuthIndex,
			Name:      strings.TrimSpace(entry.Name),
			Email:     strings.ToLower(strings.TrimSpace(entry.Email)),
			Account:   strings.ToLower(strings.TrimSpace(entry.Account)),
			Identity:  physicalAuthIdentity(entry),
		})
	}
	sort.Slice(auths, func(i, j int) bool { return auths[i].ID < auths[j].ID })
	return auths
}

func matchesProtectedAccount(auth physicalClaudeAuth, selectors []string) bool {
	if len(selectors) == 0 {
		return true
	}
	values := []string{auth.ID, auth.AuthIndex, auth.Name, auth.Email, auth.Account}
	for _, selector := range selectors {
		for _, value := range values {
			if strings.EqualFold(strings.TrimSpace(selector), strings.TrimSpace(value)) {
				return true
			}
		}
	}
	return false
}

func candidateMatchesProtectedAccount(candidate *pluginapi.SchedulerAuthCandidate, sample quotaSample, selectors []string) bool {
	if candidate == nil {
		return false
	}
	auth := physicalClaudeAuth{
		ID: candidate.ID, AuthIndex: sample.AuthIndex, Name: sample.Name,
		Email: sample.Email, Account: sample.Account,
	}
	if matchesProtectedAccount(auth, selectors) {
		return true
	}
	for _, selector := range selectors {
		for _, key := range []string{"email", "auth_index", "name", "file_name", "account"} {
			if strings.EqualFold(strings.TrimSpace(selector), strings.TrimSpace(candidate.Attributes[key])) {
				return true
			}
			if value, ok := candidate.Metadata[key].(string); ok && strings.EqualFold(strings.TrimSpace(selector), strings.TrimSpace(value)) {
				return true
			}
		}
	}
	return false
}

func physicalAuthIdentity(entry pluginapi.HostAuthFileEntry) string {
	return fmt.Sprintf("index:%q|path:%q|account:%q|email:%q|file:%d:%d",
		entry.AuthIndex,
		entry.Path,
		strings.TrimSpace(entry.Account),
		strings.ToLower(strings.TrimSpace(entry.Email)),
		entry.Size,
		entry.ModTime.UnixNano(),
	)
}

func (r *pluginRuntime) pick(req pluginapi.SchedulerPickRequest) (pluginapi.SchedulerPickResponse, *envelopeError) {
	cfg := r.loadedConfig()
	if !cfg.Enabled || !isClaudeRequest(req) || !isProtectedModel(req.Model, cfg.ProtectedModels) {
		return pluginapi.SchedulerPickResponse{Handled: false}, nil
	}
	now := r.now()
	var selected *pluginapi.SchedulerAuthCandidate
	claudeCandidates, blockedCandidates := 0, 0
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
		protected := candidateMatchesProtectedAccount(candidate, sample, cfg.ProtectedAccounts)
		blocked := false
		if protected {
			fresh := sample.fresh(now, cfg.PollInterval, cfg.UsageScope)
			if !fresh && len(cfg.ProtectedAccounts) > 0 {
				r.queueCandidateRefresh(candidate.ID, cfg, now)
			}
			blocked = sample.blocked(now, cfg.CutoffPercentUsed, cfg.UsageScope) || (cfg.BlockOnUnknown && !fresh)
		}
		if blocked {
			blockedCandidates++
			continue
		}
		if selected == nil || candidate.Priority > selected.Priority ||
			(candidate.Priority == selected.Priority && candidate.ID < selected.ID) {
			selected = candidate
		}
	}
	if blockedCandidates == 0 && len(cfg.ProtectedAccounts) > 0 {
		return pluginapi.SchedulerPickResponse{Handled: false}, nil
	}
	if selected != nil {
		if len(cfg.ProtectedAccounts) == 0 {
			r.queueCandidateRefresh(selected.ID, cfg, now)
		}
		return pluginapi.SchedulerPickResponse{AuthID: selected.ID, Handled: true}, nil
	}
	if claudeCandidates > 0 && blockedCandidates == claudeCandidates {
		return pluginapi.SchedulerPickResponse{}, &envelopeError{Code: exhaustedErrorCode, Message: exhaustedErrorCode}
	}
	return pluginapi.SchedulerPickResponse{Handled: false}, nil
}

func isClaudeRequest(req pluginapi.SchedulerPickRequest) bool {
	if strings.EqualFold(strings.TrimSpace(req.Provider), "claude") {
		return true
	}
	if strings.TrimSpace(req.Provider) != "" || len(req.Providers) != 1 {
		return false
	}
	return strings.EqualFold(strings.TrimSpace(req.Providers[0]), "claude")
}

func isProtectedModel(model string, protectedModels []string) bool {
	model = strings.TrimSpace(model)
	if model == "" {
		return false
	}
	for _, protectedModel := range protectedModels {
		if strings.EqualFold(model, protectedModel) {
			return true
		}
	}
	return false
}

func (r *pluginRuntime) handleManagement(req pluginapi.ManagementRequest) pluginapi.ManagementResponse {
	if !strings.EqualFold(strings.TrimSpace(req.Method), http.MethodGet) || strings.TrimRight(strings.TrimSpace(req.Path), "/") != managementStatusFullPath {
		body, _ := json.Marshal(map[string]string{"error": "not found"})
		return pluginapi.ManagementResponse{
			StatusCode: http.StatusNotFound,
			Headers:    http.Header{"Content-Type": []string{"application/json; charset=utf-8"}},
			Body:       body,
		}
	}
	cfg := r.loadedConfig()
	body, _ := json.Marshal(cutoffStatusResponse{
		Enabled:           cfg.Enabled,
		CutoffPercentUsed: cfg.CutoffPercentUsed,
		ProtectedModels:   cfg.ProtectedModels,
		ProtectedAccounts: cfg.ProtectedAccounts,
		UsageScope:        cfg.UsageScope,
		BlockOnUnknown:    cfg.BlockOnUnknown,
		Accounts:          r.cache.statuses(r.now(), cfg),
	})
	return pluginapi.ManagementResponse{
		StatusCode: http.StatusOK,
		Headers:    http.Header{"Content-Type": []string{"application/json; charset=utf-8"}},
		Body:       body,
	}
}
