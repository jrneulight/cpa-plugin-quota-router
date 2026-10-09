package main

import (
	"bytes"
	"fmt"
	"net/http"
	"strings"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

const bedrockFallbackBeta = "claude-code-20250219"

var bedrockModelPrefixes = []string{
	"anthropic.",
	"us.anthropic.",
	"global.anthropic.",
	"eu.anthropic.",
	"apac.anthropic.",
}

var bedrockUnsupportedBetas = map[string]struct{}{
	"redact-thinking-2026-02-12":      {},
	"prompt-caching-scope-2026-01-05": {},
	"advisor-tool-2026-03-01":         {},
	"advanced-tool-use-2025-11-20":    {},
	"fast-mode-2026-02-01":            {},
	"extended-cache-ttl-2025-04-11":   {},
	"cache-diagnosis-2026-04-07":      {},
}

func (r *pluginRuntime) interceptRequest(req pluginapi.RequestInterceptRequest, afterAuth bool) pluginapi.RequestInterceptResponse {
	if r == nil || !afterAuth {
		return pluginapi.RequestInterceptResponse{}
	}
	cfg := r.loadedConfig()
	if !cfg.Enabled {
		return pluginapi.RequestInterceptResponse{}
	}
	if refusal, refused := r.enforceReserve(req, cfg); refused {
		return refusal
	}
	if !cfg.BedrockCompat || !isBedrockClaudeModel(req.Model) {
		return pluginapi.RequestInterceptResponse{}
	}

	body := sanitizeBedrockClaudeBody(req.Body)
	beta := sanitizeBedrockBetaHeader(req.Headers)
	response := pluginapi.RequestInterceptResponse{
		Headers: http.Header{"Anthropic-Beta": []string{beta}},
	}
	if !bytes.Equal(body, req.Body) {
		response.Body = body
	}
	return response
}

func isBedrockClaudeModel(model string) bool {
	model = strings.ToLower(strings.TrimSpace(model))
	for _, prefix := range bedrockModelPrefixes {
		if strings.HasPrefix(model, prefix) {
			return true
		}
	}
	return false
}

func sanitizeBedrockBetaHeader(headers http.Header) string {
	values := make([]string, 0, 8)
	seen := make(map[string]struct{})
	for name, headerValues := range headers {
		if !strings.EqualFold(name, "Anthropic-Beta") {
			continue
		}
		for _, headerValue := range headerValues {
			for _, beta := range strings.Split(headerValue, ",") {
				beta = strings.TrimSpace(beta)
				if beta == "" {
					continue
				}
				key := strings.ToLower(beta)
				if _, unsupported := bedrockUnsupportedBetas[key]; unsupported {
					continue
				}
				if _, duplicate := seen[key]; duplicate {
					continue
				}
				seen[key] = struct{}{}
				values = append(values, beta)
			}
		}
	}
	if len(values) == 0 {
		return bedrockFallbackBeta
	}
	return strings.Join(values, ",")
}

func sanitizeBedrockClaudeBody(body []byte) []byte {
	if len(body) == 0 || !gjson.ValidBytes(body) {
		return body
	}
	updated := bytes.Clone(body)
	for _, path := range []string{"metadata", "speed", "diagnostics"} {
		updated, _ = sjson.DeleteBytes(updated, path)
	}

	if betas := gjson.GetBytes(updated, "betas"); betas.IsArray() {
		filtered := make([]string, 0, len(betas.Array()))
		seen := make(map[string]struct{})
		for _, value := range betas.Array() {
			beta := strings.TrimSpace(value.String())
			key := strings.ToLower(beta)
			if beta == "" {
				continue
			}
			if _, unsupported := bedrockUnsupportedBetas[key]; unsupported {
				continue
			}
			if _, duplicate := seen[key]; duplicate {
				continue
			}
			seen[key] = struct{}{}
			filtered = append(filtered, beta)
		}
		if len(filtered) == 0 {
			updated, _ = sjson.DeleteBytes(updated, "betas")
		} else {
			updated, _ = sjson.SetBytes(updated, "betas", filtered)
		}
	}

	tools := gjson.GetBytes(updated, "tools")
	removedToolNames := make(map[string]struct{})
	if tools.IsArray() {
		items := tools.Array()
		for index := len(items) - 1; index >= 0; index-- {
			toolType := strings.ToLower(strings.TrimSpace(items[index].Get("type").String()))
			toolName := strings.TrimSpace(items[index].Get("name").String())
			if strings.HasPrefix(toolType, "advisor_") || strings.HasPrefix(toolType, "tool_search_tool_") {
				if toolName != "" {
					removedToolNames[strings.ToLower(toolName)] = struct{}{}
				}
				updated, _ = sjson.DeleteBytes(updated, fmt.Sprintf("tools.%d", index))
				continue
			}
			for _, field := range []string{"defer_loading", "input_examples", "allowed_callers"} {
				updated, _ = sjson.DeleteBytes(updated, fmt.Sprintf("tools.%d.%s", index, field))
			}
		}
	}

	choiceName := strings.ToLower(strings.TrimSpace(gjson.GetBytes(updated, "tool_choice.name").String()))
	if _, removed := removedToolNames[choiceName]; removed {
		updated, _ = sjson.DeleteBytes(updated, "tool_choice")
	}
	return updated
}
