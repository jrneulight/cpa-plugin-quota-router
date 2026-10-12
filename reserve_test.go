package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

// reserveConfig is the shape this fork runs in production: a 30% reserve on
// the session, the weekly window and the Fable weekly window of one account.
func reserveConfig(t *testing.T) pluginConfig {
	t.Helper()
	cfg, err := decodeLifecycleConfig(lifecycleYAML(`
protected-accounts: [charles@example.com]
block-on-unknown: true
poll-interval: 2m
near-cutoff-percent-used: 60
near-cutoff-poll-interval: 1m
cutoffs:
  - scope: five_hour
    percent-used: 70
  - scope: seven_day
    percent-used: 70
    models: ["*"]
  - scope: fable
    percent-used: 70
    models: [claude-fable-5, claude-fable-5-1]
`))
	if err != nil {
		t.Fatalf("decode reserve config: %v", err)
	}
	return cfg
}

func lifecycleYAML(yaml string) []byte {
	raw, _ := json.Marshal(lifecycleRequest{ConfigYAML: []byte(strings.TrimSpace(yaml))})
	return raw
}

func reserveRuntime(t *testing.T, now time.Time) *pluginRuntime {
	t.Helper()
	runtime := newTestRuntime(&fakeHost{}, nil, now)
	runtime.cache.reconcile([]physicalClaudeAuth{
		{ID: "dad-auth", AuthIndex: "dad-index", Name: "claude-charles@example.com.json", Email: "charles@example.com"},
		{ID: "jacob-auth", AuthIndex: "jacob-index", Name: "claude-jacob@example.com.json", Email: "jacob@example.com"},
	})
	cfg := reserveConfig(t)
	runtime.config.Store(&cfg)
	return runtime
}

func windows(fiveHour, sevenDay, fable float64, resetAt time.Time) map[string]usageWindowResult {
	return map[string]usageWindowResult{
		fiveHourUsageScope: {PercentUsed: fiveHour, ResetAt: resetAt},
		defaultUsageScope:  {PercentUsed: sevenDay, ResetAt: resetAt},
		"fable":            {PercentUsed: fable, ResetAt: resetAt},
	}
}

func afterAuthRequest(authID, model string) pluginapi.RequestInterceptRequest {
	return pluginapi.RequestInterceptRequest{
		Model:          model,
		RequestedModel: model,
		Metadata:       map[string]any{selectedAuthIDMetadataKey: authID},
	}
}

func TestCutoffsConfigDecodesRulesAndDefaultsModelsToAll(t *testing.T) {
	cfg := reserveConfig(t)
	if len(cfg.Cutoffs) != 3 {
		t.Fatalf("cutoffs = %#v", cfg.Cutoffs)
	}
	if cfg.Cutoffs[0].Scope != fiveHourUsageScope || cfg.Cutoffs[0].Models != nil {
		t.Fatalf("omitted models should cover every model: %#v", cfg.Cutoffs[0])
	}
	if cfg.Cutoffs[1].Models != nil {
		t.Fatalf(`"*" should cover every model: %#v`, cfg.Cutoffs[1])
	}
	if got := cfg.Cutoffs[2].Models; len(got) != 2 || got[0] != "claude-fable-5" {
		t.Fatalf("fable models = %#v", got)
	}
}

func TestCutoffsConfigRejectsInvalidRules(t *testing.T) {
	for name, yaml := range map[string]string{
		"empty list":       "cutoffs: []",
		"missing scope":    "cutoffs: [{percent-used: 70}]",
		"missing percent":  "cutoffs: [{scope: five_hour}]",
		"percent too high": "cutoffs: [{scope: five_hour, percent-used: 101}]",
		"near above cutoff": `near-cutoff-percent-used: 75
cutoffs: [{scope: five_hour, percent-used: 70}]`,
	} {
		if _, err := decodeLifecycleConfig(lifecycleYAML(yaml)); err == nil {
			t.Errorf("%s: config was accepted", name)
		}
	}
}

func TestReserveRefusesSelectedProtectedAccountAtAnyCutoff(t *testing.T) {
	now := time.Date(2026, 10, 9, 12, 0, 0, 0, time.UTC)
	reset := now.Add(2 * time.Hour)
	for name, tc := range map[string]struct {
		windows map[string]usageWindowResult
		model   string
		refused bool
		scope   string
	}{
		"all below cutoffs":      {windows(69, 69, 69, reset), "claude-opus-5-5", false, ""},
		"session at cutoff":      {windows(70, 10, 10, reset), "claude-opus-5-5", true, fiveHourUsageScope},
		"weekly at cutoff":       {windows(10, 70, 10, reset), "claude-haiku-5-5", true, defaultUsageScope},
		"fable cutoff on fable":  {windows(10, 10, 85, reset), "claude-fable-5-1", true, "fable"},
		"fable cutoff on opus":   {windows(10, 10, 85, reset), "claude-opus-5-5", false, ""},
		"missing fable reading":  {map[string]usageWindowResult{fiveHourUsageScope: {PercentUsed: 1}, defaultUsageScope: {PercentUsed: 1}}, "claude-fable-5", true, "fable"},
		"idle session, no reset": {map[string]usageWindowResult{fiveHourUsageScope: {PercentUsed: 0}, defaultUsageScope: {PercentUsed: 1, ResetAt: reset}, "fable": {PercentUsed: 1, ResetAt: reset}}, "claude-fable-5", false, ""},
	} {
		t.Run(name, func(t *testing.T) {
			runtime := reserveRuntime(t, now)
			runtime.cache.recordWindows("dad-auth", tc.windows, now)
			response := runtime.interceptRequest(afterAuthRequest("dad-auth", tc.model), true)
			if response.Terminate != tc.refused {
				t.Fatalf("terminate = %v, want %v (body %s)", response.Terminate, tc.refused, response.ResponseBody)
			}
			if !tc.refused {
				return
			}
			if response.StatusCode != http.StatusTooManyRequests {
				t.Fatalf("status = %d", response.StatusCode)
			}
			var body struct {
				Type  string `json:"type"`
				Error struct {
					Type    string `json:"type"`
					Message string `json:"message"`
					Code    string `json:"code"`
				} `json:"error"`
			}
			if err := json.Unmarshal(response.ResponseBody, &body); err != nil || body.Type != "error" || body.Error.Type != "rate_limit_error" || body.Error.Code != reserveErrorCode {
				t.Fatalf("body = %s (%v)", response.ResponseBody, err)
			}
			if !strings.Contains(body.Error.Message, tc.scope) {
				t.Fatalf("message %q does not name scope %q", body.Error.Message, tc.scope)
			}
		})
	}
}

func TestReserveSetsRetryAfterUntilTheWindowResets(t *testing.T) {
	now := time.Date(2026, 10, 9, 12, 0, 0, 0, time.UTC)
	runtime := reserveRuntime(t, now)
	runtime.cache.recordWindows("dad-auth", windows(90, 10, 10, now.Add(90*time.Second)), now)
	response := runtime.interceptRequest(afterAuthRequest("dad-auth", "claude-opus-5-5"), true)
	if got := response.ResponseHeaders.Get("Retry-After"); got != "90" {
		t.Fatalf("Retry-After = %q, want 90", got)
	}
}

func TestReserveLeavesOtherAccountsAndResetWindowsAlone(t *testing.T) {
	now := time.Date(2026, 10, 9, 12, 0, 0, 0, time.UTC)
	runtime := reserveRuntime(t, now)
	runtime.cache.recordWindows("jacob-auth", windows(99, 99, 99, now.Add(time.Hour)), now)
	if response := runtime.interceptRequest(afterAuthRequest("jacob-auth", "claude-opus-5-5"), true); response.Terminate {
		t.Fatal("an unprotected account was refused")
	}
	runtime.cache.recordWindows("dad-auth", windows(99, 10, 10, now.Add(-time.Minute)), now.Add(-2*time.Minute))
	if response := runtime.interceptRequest(afterAuthRequest("dad-auth", "claude-opus-5-5"), true); response.Terminate {
		t.Fatalf("a session window past its reset still refused: %s", response.ResponseBody)
	}
	if response := runtime.interceptRequest(afterAuthRequest("unknown-auth", "claude-opus-5-5"), true); response.Terminate {
		t.Fatal("a credential the cache does not know (another provider) was refused")
	}
	if response := runtime.interceptRequest(afterAuthRequest("dad-auth", "claude-opus-5-5"), false); response.Terminate {
		t.Fatal("the before-auth hook refused a request before any account was chosen")
	}
}

func TestReserveFindsTheAccountByIndexWhenTheIDIsAbsent(t *testing.T) {
	now := time.Date(2026, 10, 9, 12, 0, 0, 0, time.UTC)
	runtime := reserveRuntime(t, now)
	runtime.cache.recordWindows("dad-auth", windows(80, 10, 10, now.Add(time.Hour)), now)
	request := afterAuthRequest("", "claude-opus-5-5")
	request.Metadata = map[string]any{selectedAuthIndexMetadataKey: "dad-index"}
	if response := runtime.interceptRequest(request, true); !response.Terminate {
		t.Fatal("selected_auth_index did not identify the protected account")
	}
}

func TestReserveIsStrictAboutStaleOrMissingReadings(t *testing.T) {
	now := time.Date(2026, 10, 9, 12, 0, 0, 0, time.UTC)
	runtime := reserveRuntime(t, now)
	if response := runtime.interceptRequest(afterAuthRequest("dad-auth", "claude-opus-5-5"), true); !response.Terminate {
		t.Fatal("an account never read was allowed under block-on-unknown")
	}
	cfg := runtime.loadedConfig()
	runtime.cache.recordWindows("dad-auth", windows(1, 1, 1, now.Add(time.Hour)), now.Add(-cfg.reserveStaleAfter()-time.Second))
	if response := runtime.interceptRequest(afterAuthRequest("dad-auth", "claude-opus-5-5"), true); !response.Terminate {
		t.Fatal("a stale reading was trusted under block-on-unknown")
	}
	cfg.BlockOnUnknown = false
	runtime.config.Store(&cfg)
	if response := runtime.interceptRequest(afterAuthRequest("dad-auth", "claude-opus-5-5"), true); response.Terminate {
		t.Fatal("a stale reading refused without block-on-unknown")
	}
}

func TestReserveRunsBeforeBedrockCompat(t *testing.T) {
	now := time.Date(2026, 10, 9, 12, 0, 0, 0, time.UTC)
	runtime := reserveRuntime(t, now)
	cfg := runtime.loadedConfig()
	cfg.BedrockCompat = true
	runtime.config.Store(&cfg)
	runtime.cache.recordWindows("dad-auth", windows(10, 10, 10, now.Add(time.Hour)), now)
	request := afterAuthRequest("bedrock-auth", "us.anthropic.claude-opus-5-5")
	request.Headers = http.Header{"Anthropic-Beta": []string{"fast-mode-2026-02-01"}}
	request.Body = []byte(`{"model":"us.anthropic.claude-opus-5-5"}`)
	if response := runtime.interceptRequest(request, true); response.Terminate || len(response.Headers) == 0 {
		t.Fatalf("bedrock compat stopped applying: %#v", response)
	}
}

func TestSchedulerPickWithCutoffsSkipsRefusedProtectedAccount(t *testing.T) {
	now := time.Date(2026, 10, 9, 12, 0, 0, 0, time.UTC)
	runtime := reserveRuntime(t, now)
	runtime.cache.recordWindows("dad-auth", windows(10, 10, 10, now.Add(time.Hour)), now)
	response, decisionError := runtime.pick(claudeModelRequest("claude-opus-5-5", candidate("dad-auth", -50)))
	if decisionError != nil || response.Handled {
		t.Fatalf("an account under its cutoffs should leave routing to the host: %#v %#v", response, decisionError)
	}
	runtime.cache.recordWindows("dad-auth", windows(75, 10, 10, now.Add(time.Hour)), now)
	response, decisionError = runtime.pick(claudeModelRequest("claude-opus-5-5", candidate("dad-auth", -50)))
	if decisionError == nil || decisionError.Code != reserveErrorCode {
		t.Fatalf("the only candidate past its reserve should be refused: %#v %#v", response, decisionError)
	}
	response, decisionError = runtime.pick(claudeModelRequest("claude-opus-5-5", candidate("dad-auth", -50), candidate("jacob-auth", -50)))
	if decisionError != nil || !response.Handled || response.AuthID != "jacob-auth" {
		t.Fatalf("an eligible account should be picked over a refused one: %#v %#v", response, decisionError)
	}
}

func TestFetchReadsFiveHourAndEveryWindow(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{
			"five_hour":{"utilization":42,"resets_at":"2026-10-09T16:00:00Z"},
			"seven_day":{"utilization":12,"resets_at":"2026-10-13T22:00:00Z"},
			"limits":[{"kind":"weekly_scoped","percent":3,"resets_at":"2026-10-13T22:00:00Z","scope":{"model":{"display_name":"Fable"}}}]
		}`))
	}))
	defer server.Close()
	result, category := newHTTPUsageFetcher(server.URL, server.Client().Transport).fetch(context.Background(), "token", time.Second)
	if category != "" {
		t.Fatalf("category = %q", category)
	}
	for scope, want := range map[string]float64{fiveHourUsageScope: 42, defaultUsageScope: 12, "fable": 3} {
		if got, ok := result.Windows[scope]; !ok || got.PercentUsed != want {
			t.Errorf("%s = %#v, want %v", scope, got, want)
		}
	}
}

func TestFetchKeepsAnIdleFiveHourWindowWithoutAReset(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"five_hour":{"utilization":0,"resets_at":null},"seven_day":{"utilization":1,"resets_at":"2026-10-13T22:00:00Z"}}`))
	}))
	defer server.Close()
	result, _ := newHTTPUsageFetcher(server.URL, server.Client().Transport).fetch(context.Background(), "token", time.Second)
	window, ok := result.Windows[fiveHourUsageScope]
	if !ok || window.PercentUsed != 0 || !window.ResetAt.IsZero() {
		t.Fatalf("idle five-hour window = %#v, %v", window, ok)
	}
}

func TestPollRecordsWindowsWhenCutoffsAreSet(t *testing.T) {
	now := time.Date(2026, 10, 9, 12, 0, 0, 0, time.UTC)
	host := &fakeHost{
		entries:  []pluginapi.HostAuthFileEntry{physicalEntry("dad-auth", "dad-index")},
		authJSON: map[string]json.RawMessage{"dad-index": credentialJSON("dad-token")},
	}
	host.entries[0].Email = "charles@example.com"
	fetcher := &fakeFetcher{replies: map[string][]fetchReply{
		"dad-token": {{result: usageResult{Windows: windows(71, 20, 5, now.Add(time.Hour))}}},
	}}
	runtime := newTestRuntime(host, fetcher.fetch, now)
	cfg := reserveConfig(t)
	runtime.pollOnce(context.Background(), cfg)
	sample := runtime.cache.snapshot("dad-auth")
	if sample.Windows[fiveHourUsageScope].PercentUsed != 71 || !sample.WindowsSampledAt.Equal(now) {
		t.Fatalf("windows were not recorded: %#v", sample)
	}
	if strings.Contains(host.logText(), pollErrorInvalidScope) {
		t.Fatal("a missing legacy usage-scope window was reported as a failure while cutoffs are set")
	}
}

func TestReserveWorkerRefreshesOnItsOwnInterval(t *testing.T) {
	host := &fakeHost{
		entries:  []pluginapi.HostAuthFileEntry{physicalEntry("dad-auth", "dad-index")},
		authJSON: map[string]json.RawMessage{"dad-index": credentialJSON("dad-token")},
	}
	host.entries[0].Email = "charles@example.com"
	reply := fetchReply{result: usageResult{Windows: windows(1, 1, 1, time.Now().Add(time.Hour))}}
	fetcher := &fakeFetcher{replies: map[string][]fetchReply{"dad-token": {reply}}}
	runtime := newPluginRuntime(host, fetcher.fetch, time.Now)
	cfg := reserveConfig(t)
	cfg.NearCutoffPercent = disabledNearCutoffPercent
	cfg.PollInterval = 20 * time.Millisecond
	runtime.applyConfig(cfg)
	defer runtime.shutdown()
	waitFor(t, func() bool { return fetcher.callCount() >= 3 })
}

func TestReserveRefreshIntervalTightensNearACutoff(t *testing.T) {
	now := time.Date(2026, 10, 9, 12, 0, 0, 0, time.UTC)
	runtime := reserveRuntime(t, now)
	cfg := runtime.loadedConfig()
	runtime.cache.recordWindows("dad-auth", windows(30, 30, 30, now.Add(time.Hour)), now)
	if got := runtime.reserveRefreshInterval(cfg); got != 2*time.Minute {
		t.Fatalf("interval below the near threshold = %s", got)
	}
	runtime.cache.recordWindows("dad-auth", windows(61, 30, 30, now.Add(time.Hour)), now)
	if got := runtime.reserveRefreshInterval(cfg); got != time.Minute {
		t.Fatalf("interval near a cutoff = %s", got)
	}
	cfg.Cutoffs = nil
	if got := runtime.reserveRefreshInterval(cfg); got != 0 {
		t.Fatalf("legacy config should keep the worker idle, interval = %s", got)
	}
}

func TestManagementStatusReportsCutoffsAndWindows(t *testing.T) {
	now := time.Date(2026, 10, 9, 12, 0, 0, 0, time.UTC)
	runtime := reserveRuntime(t, now)
	runtime.cache.recordWindows("dad-auth", windows(72, 10, 10, now.Add(time.Hour)), now)
	response := runtime.handleManagement(pluginapi.ManagementRequest{Method: http.MethodGet, Path: managementStatusFullPath})
	var status cutoffStatusResponse
	if err := json.Unmarshal(response.Body, &status); err != nil {
		t.Fatal(err)
	}
	if len(status.Cutoffs) != 3 || status.Cutoffs[0].Models[0] != allModelsSelector {
		t.Fatalf("cutoffs = %#v", status.Cutoffs)
	}
	for _, account := range status.Accounts {
		if account.ID != "dad-auth" {
			continue
		}
		if !account.ReserveRefused || account.ReserveScope != fiveHourUsageScope || account.Windows[fiveHourUsageScope].PercentUsed != 72 {
			t.Fatalf("dad status = %#v", account)
		}
		return
	}
	t.Fatal("protected account missing from status")
}
