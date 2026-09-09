package main

import (
	"net/http"
	"strings"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
	"github.com/tidwall/gjson"
)

func TestBedrockCompatRunsOnlyAfterBedrockAuthSelection(t *testing.T) {
	runtime := newPluginRuntime(nil, nil, nil)
	cfg := defaultPluginConfig()
	cfg.BedrockCompat = true
	runtime.config.Store(&cfg)
	req := pluginapi.RequestInterceptRequest{
		Model:   "us.anthropic.claude-fable-5",
		Headers: http.Header{"Anthropic-Beta": {"prompt-caching-scope-2026-01-05"}},
		Body:    []byte(`{"metadata":{"user_id":"{}"},"messages":[]}`),
	}

	if got := runtime.interceptRequest(req, false); len(got.Body) != 0 || len(got.Headers) != 0 {
		t.Fatalf("before-auth response = %#v, want no changes", got)
	}
	native := req
	native.Model = "claude-fable-5"
	if got := runtime.interceptRequest(native, true); len(got.Body) != 0 || len(got.Headers) != 0 {
		t.Fatalf("native Claude response = %#v, want no changes", got)
	}

	got := runtime.interceptRequest(req, true)
	if gjson.GetBytes(got.Body, "metadata").Exists() {
		t.Fatalf("metadata survived Bedrock sanitization: %s", got.Body)
	}
	if beta := got.Headers.Get("Anthropic-Beta"); beta != bedrockFallbackBeta {
		t.Fatalf("Anthropic-Beta = %q, want %q", beta, bedrockFallbackBeta)
	}
}

func TestSanitizeBedrockClaudeBodyRemovesUnsupportedFeatures(t *testing.T) {
	body := []byte(`{
		"model":"us.anthropic.claude-fable-5",
		"metadata":{"user_id":"{\"session_id\":\"session\"}"},
		"speed":"fast",
		"diagnostics":{"enabled":true},
		"betas":["effort-2025-11-24","advisor-tool-2026-03-01","fast-mode-2026-02-01"],
		"tools":[
			{"type":"advisor_20260301","name":"advisor"},
			{"type":"tool_search_tool_bm25_20251119","name":"tool_search"},
			{"name":"Read","description":"read","input_schema":{"type":"object"},"defer_loading":true,"input_examples":[],"allowed_callers":["code_execution_20250825"]}
		],
		"tool_choice":{"type":"tool","name":"advisor"},
		"messages":[]
	}`)

	got := sanitizeBedrockClaudeBody(body)
	for _, path := range []string{"metadata", "speed", "diagnostics", "tool_choice"} {
		if gjson.GetBytes(got, path).Exists() {
			t.Errorf("%s survived Bedrock sanitization: %s", path, got)
		}
	}
	if count := len(gjson.GetBytes(got, "tools").Array()); count != 1 {
		t.Fatalf("tools count = %d, want 1: %s", count, got)
	}
	for _, path := range []string{"tools.0.defer_loading", "tools.0.input_examples", "tools.0.allowed_callers"} {
		if gjson.GetBytes(got, path).Exists() {
			t.Errorf("%s survived Bedrock sanitization: %s", path, got)
		}
	}
	if betas := gjson.GetBytes(got, "betas").Array(); len(betas) != 1 || betas[0].String() != "effort-2025-11-24" {
		t.Fatalf("betas = %s, want only effort", gjson.GetBytes(got, "betas").Raw)
	}
}

func TestSanitizeBedrockBetaHeaderPreservesSupportedValuesInOrder(t *testing.T) {
	headers := http.Header{
		"anthropic-beta": {
			"interleaved-thinking-2025-05-14,prompt-caching-scope-2026-01-05",
			"effort-2025-11-24,interleaved-thinking-2025-05-14,advisor-tool-2026-03-01",
		},
	}
	got := sanitizeBedrockBetaHeader(headers)
	want := "interleaved-thinking-2025-05-14,effort-2025-11-24"
	if got != want {
		t.Fatalf("sanitized betas = %q, want %q", got, want)
	}
	for unsupported := range bedrockUnsupportedBetas {
		if strings.Contains(got, unsupported) {
			t.Fatalf("unsupported beta %q survived: %q", unsupported, got)
		}
	}
}
