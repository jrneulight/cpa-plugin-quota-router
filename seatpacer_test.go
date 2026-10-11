package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

const (
	charlesAuthID = "claude-charles@example.com.json"
	jacobAuthID   = "claude-jacob@example.com.json"
	fixturePath   = "testdata/seat-pacer-history.json"
)

// fixtureObservedAt is charles's observed_at in the fixture file.
var fixtureObservedAt = time.Date(2026, 10, 11, 20, 29, 48, 288000, time.UTC)

// seatPacerConfig is reserveConfig reading Seat Pacer's file at path instead
// of polling Anthropic.
func seatPacerConfig(t *testing.T, path string) pluginConfig {
	t.Helper()
	cfg, err := decodeLifecycleConfig(lifecycleYAML(fmt.Sprintf(`
protected-accounts: [charles@example.com, jacob@example.com]
block-on-unknown: true
poll-interval: 2m
usage-source: seat-pacer-file
seat-pacer-file: %q
cutoffs:
  - scope: five_hour
    percent-used: 70
  - scope: seven_day
    percent-used: 70
  - scope: fable
    percent-used: 70
    models: [claude-fable-5]
`, path)))
	if err != nil {
		t.Fatalf("decode seat pacer config: %v", err)
	}
	return cfg
}

// seatPacerHost lists charles and jacob, and would hand out tokens if asked:
// a test fails when anything asks.
func seatPacerHost() *fakeHost {
	charles := physicalEntry(charlesAuthID, "charles-index")
	charles.Name = charlesAuthID
	charles.Email = "charles@example.com"
	jacob := physicalEntry(jacobAuthID, "jacob-index")
	jacob.Name = jacobAuthID
	jacob.Email = "jacob@example.com"
	return &fakeHost{
		entries: []pluginapi.HostAuthFileEntry{charles, jacob},
		authJSON: map[string]json.RawMessage{
			"charles-index": credentialJSON("charles-token"),
			"jacob-index":   credentialJSON("jacob-token"),
		},
	}
}

func assertNoAnthropicContact(t *testing.T, host *fakeHost, fetcher *fakeFetcher) {
	t.Helper()
	if calls := fetcher.callCount(); calls != 0 {
		t.Fatalf("usage endpoint fetched %d times in seat-pacer-file mode", calls)
	}
	if _, gets := host.counts(); gets != 0 {
		t.Fatalf("getAuth read %d credentials in seat-pacer-file mode", gets)
	}
}

func TestSeatPacerFileRecordsWindowsAtObservedAt(t *testing.T) {
	now := fixtureObservedAt.Add(time.Minute)
	host := seatPacerHost()
	fetcher := &fakeFetcher{}
	runtime := newTestRuntime(host, fetcher.fetch, now)
	runtime.pollOnce(context.Background(), seatPacerConfig(t, fixturePath))
	assertNoAnthropicContact(t, host, fetcher)

	reset := time.Date(2026, 10, 13, 21, 59, 59, 0, time.UTC)
	tests := []struct {
		name    string
		authID  string
		scope   string
		percent float64
		reset   time.Time
	}{
		{"idle five-hour window keeps a zero reset", charlesAuthID, fiveHourUsageScope, 0, time.Time{}},
		{"fraction becomes percent points", charlesAuthID, defaultUsageScope, 2, reset},
		{"scope is the lowercased model name, first reading wins", charlesAuthID, "fable", 29, reset},
		{"usage past the cap reads as 100", charlesAuthID, "opus", 100, reset},
		{"a refusing window reads as 100", charlesAuthID, "sonnet", 100, reset},
		{"a header-sourced reading counts", jacobAuthID, defaultUsageScope, 75, reset},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			window, ok := runtime.cache.snapshot(tt.authID).Windows[tt.scope]
			if !ok || window.PercentUsed != tt.percent || !window.ResetAt.Equal(tt.reset) {
				t.Fatalf("%s %s = %#v, %v; want %v reset %v", tt.authID, tt.scope, window, ok, tt.percent, tt.reset)
			}
		})
	}

	charles := runtime.cache.snapshot(charlesAuthID)
	if !charles.WindowsSampledAt.Equal(fixtureObservedAt) {
		t.Fatalf("windows sampled at %v, want Seat Pacer's observed_at %v", charles.WindowsSampledAt, fixtureObservedAt)
	}
	if len(charles.Windows) != 5 {
		t.Fatalf("an unknown window kind was recorded: %#v", charles.Windows)
	}
	if charles.LastErrorCategory != "" {
		t.Fatalf("a good reading left error %q", charles.LastErrorCategory)
	}
	jacob := runtime.cache.snapshot(jacobAuthID)
	if !jacob.WindowsSampledAt.Equal(time.Date(2026, 10, 11, 20, 25, 0, 0, time.UTC)) {
		t.Fatalf("jacob sampled at %v", jacob.WindowsSampledAt)
	}
	if verdict := seatPacerConfig(t, fixturePath).reserveVerdictForAnyModel(jacob, now); !verdict.Refused || verdict.Scope != defaultUsageScope || verdict.Unknown {
		t.Fatalf("jacob at 75%% of the week should be refused on seven_day: %#v", verdict)
	}
}

// writeSeatPacerReading writes a version 1 file holding one reading for
// charles, observed at observedAt, with every window at percent.
func writeSeatPacerReading(t *testing.T, path string, observedAt time.Time, percent float64, resetAt time.Time) {
	t.Helper()
	fraction := percent / 100
	body := map[string]any{
		"version": 1,
		"seats":   map[string]any{},
		"readings": map[string]any{
			charlesAuthID: map[string]any{
				"observed_at": observedAt,
				"source":      "usage-endpoint",
				"windows": []map[string]any{
					{"kind": "five_hour", "scope": nil, "utilization": fraction, "resets_at": resetAt, "duration": int64(5 * time.Hour)},
					{"kind": "seven_day", "scope": nil, "utilization": fraction, "resets_at": resetAt, "duration": int64(7 * 24 * time.Hour)},
					{"kind": "weekly_scoped", "scope": "Fable", "utilization": fraction, "resets_at": resetAt, "duration": int64(7 * 24 * time.Hour)},
				},
			},
		},
	}
	raw, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestSeatPacerStalenessFollowsObservedAt(t *testing.T) {
	now := time.Date(2026, 10, 11, 21, 0, 0, 0, time.UTC)
	staleAfter := seatPacerConfig(t, "unused").reserveStaleAfter()
	tests := []struct {
		name        string
		age         time.Duration
		percent     float64
		wantRefused bool
		wantUnknown bool
	}{
		{"fresh reading under the cutoff passes", time.Minute, 10, false, false},
		{"reading at the edge of staleness still passes", staleAfter, 10, false, false},
		{"reading older than reserveStaleAfter refuses as unknown", staleAfter + time.Second, 10, true, true},
		{"fresh reading over the cutoff refuses on usage", time.Minute, 80, true, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "history.json")
			writeSeatPacerReading(t, path, now.Add(-tt.age), tt.percent, now.Add(24*time.Hour))
			host := seatPacerHost()
			fetcher := &fakeFetcher{}
			runtime := newTestRuntime(host, fetcher.fetch, now)
			cfg := seatPacerConfig(t, path)
			runtime.config.Store(&cfg)
			runtime.pollOnce(context.Background(), cfg)
			assertNoAnthropicContact(t, host, fetcher)

			response, refused := runtime.enforceReserve(afterAuthRequest(charlesAuthID, "claude-fable-5"), cfg)
			if refused != tt.wantRefused {
				t.Fatalf("refused = %v, want %v (%#v)", refused, tt.wantRefused, response)
			}
			if refused && strings.Contains(string(response.ResponseBody), "cannot be confirmed") != tt.wantUnknown {
				t.Fatalf("unknown = %v, body %s", !tt.wantUnknown, response.ResponseBody)
			}
			pick, pickErr := runtime.pick(claudeModelRequest("claude-fable-5", candidate(charlesAuthID, 1)))
			if (pickErr != nil) != tt.wantRefused {
				t.Fatalf("pick = %#v, %#v; want refused %v", pick, pickErr, tt.wantRefused)
			}
		})
	}
}

func TestSeatPacerFileFailuresRefuseUnderBlockOnUnknown(t *testing.T) {
	now := time.Date(2026, 10, 11, 21, 0, 0, 0, time.UTC)
	const marker = "FILE-CONTENT-MARKER"
	tests := []struct {
		name     string
		write    func(t *testing.T, path string)
		category string
	}{
		{"missing file", func(*testing.T, string) {}, pollErrorSeatPacerFileMissing},
		{"unreadable path", func(t *testing.T, path string) {
			if err := os.Mkdir(path, 0o700); err != nil {
				t.Fatal(err)
			}
		}, pollErrorSeatPacerFileUnreadable},
		{"bad JSON", func(t *testing.T, path string) {
			mustWrite(t, path, `{"version":1,"readings":{"`+marker)
		}, pollErrorSeatPacerFileInvalid},
		{"no version", func(t *testing.T, path string) {
			mustWrite(t, path, `{"readings":{}}`)
		}, pollErrorSeatPacerFileInvalid},
		{"unknown version", func(t *testing.T, path string) {
			mustWrite(t, path, `{"version":2,"readings":{}}`)
		}, pollErrorSeatPacerFileVersion},
		{"over the size cap", func(t *testing.T, path string) {
			mustWrite(t, path, `{"version":1,"pad":"`+strings.Repeat("x", maxSeatPacerFileBytes)+`"}`)
		}, pollErrorSeatPacerFileInvalid},
		{"no reading for the account", func(t *testing.T, path string) {
			mustWrite(t, path, `{"version":1,"seats":{},"readings":{"someone-else.json":{"observed_at":"2026-10-11T20:59:00Z","windows":[{"kind":"seven_day","utilization":0.1,"resets_at":"2026-10-13T00:00:00Z"}]}}}`)
		}, pollErrorSeatPacerNoReading},
		{"reading with no usable window", func(t *testing.T, path string) {
			mustWrite(t, path, `{"version":1,"readings":{"`+charlesAuthID+`":{"observed_at":"2026-10-11T20:59:00Z","windows":[{"kind":"seven_day","utilization":-1}]}}}`)
		}, pollErrorSeatPacerNoReading},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "history.json")
			tt.write(t, path)
			host := seatPacerHost()
			fetcher := &fakeFetcher{}
			runtime := newTestRuntime(host, fetcher.fetch, now)
			cfg := seatPacerConfig(t, path)
			runtime.config.Store(&cfg)
			runtime.pollOnce(context.Background(), cfg)
			assertNoAnthropicContact(t, host, fetcher)

			if got := runtime.cache.snapshot(charlesAuthID).LastErrorCategory; got != tt.category {
				t.Fatalf("category = %q, want %q", got, tt.category)
			}
			if !strings.Contains(host.logText(), tt.category) || !strings.Contains(host.logText(), `"level":"warn"`) {
				t.Fatalf("failure was not logged at warn: %s", host.logText())
			}
			if strings.Contains(host.logText(), marker) {
				t.Fatal("the log carried the file's contents")
			}
			if _, refused := runtime.enforceReserve(afterAuthRequest(charlesAuthID, "claude-fable-5"), cfg); !refused {
				t.Fatal("block-on-unknown let a protected account through with no reading")
			}

			lenient := cfg
			lenient.BlockOnUnknown = false
			if _, refused := runtime.enforceReserve(afterAuthRequest(charlesAuthID, "claude-fable-5"), lenient); refused {
				t.Fatal("without block-on-unknown a missing reading should not refuse")
			}
		})
	}
}

func TestSeatPacerFileFailureKeepsLastReadingUntilStale(t *testing.T) {
	clock := &testClock{value: time.Date(2026, 10, 11, 21, 0, 0, 0, time.UTC)}
	path := filepath.Join(t.TempDir(), "history.json")
	writeSeatPacerReading(t, path, clock.now(), 10, clock.now().Add(24*time.Hour))
	host := seatPacerHost()
	runtime := newPluginRuntime(host, (&fakeFetcher{}).fetch, clock.now)
	cfg := seatPacerConfig(t, path)
	runtime.config.Store(&cfg)
	runtime.pollOnce(context.Background(), cfg)

	mustWrite(t, path, "not json")
	clock.set(clock.now().Add(time.Minute))
	runtime.pollOnce(context.Background(), cfg)
	if _, refused := runtime.enforceReserve(afterAuthRequest(charlesAuthID, "claude-fable-5"), cfg); refused {
		t.Fatal("a bad file should not discard a reading that is still fresh")
	}
	clock.set(clock.now().Add(cfg.reserveStaleAfter()))
	if _, refused := runtime.enforceReserve(afterAuthRequest(charlesAuthID, "claude-fable-5"), cfg); !refused {
		t.Fatal("the kept reading should refuse once it is stale")
	}
}

func TestSeatPacerWorkerReadsOnItsInterval(t *testing.T) {
	path := filepath.Join(t.TempDir(), "history.json")
	writeSeatPacerReading(t, path, time.Now(), 10, time.Now().Add(time.Hour))
	host := seatPacerHost()
	fetcher := &fakeFetcher{}
	runtime := newPluginRuntime(host, fetcher.fetch, time.Now)
	cfg := seatPacerConfig(t, path)
	cfg.PollInterval = 20 * time.Millisecond
	runtime.applyConfig(cfg)
	defer runtime.shutdown()
	waitFor(t, func() bool {
		lists, _ := host.counts()
		return lists >= 3 && !runtime.cache.snapshot(charlesAuthID).WindowsSampledAt.IsZero()
	})
	assertNoAnthropicContact(t, host, fetcher)
}

func TestEndpointModeStillPollsAnthropic(t *testing.T) {
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
	if cfg.UsageSource != usageSourceEndpoint {
		t.Fatalf("default usage-source = %q", cfg.UsageSource)
	}
	runtime.config.Store(&cfg)
	runtime.pollOnce(context.Background(), cfg)
	if fetcher.callCount() != 1 {
		t.Fatalf("endpoint mode fetched %d times", fetcher.callCount())
	}
	if !runtime.cache.snapshot("dad-auth").WindowsSampledAt.Equal(now) {
		t.Fatal("endpoint mode should stamp its own poll time")
	}
	var status map[string]any
	_ = json.Unmarshal(runtime.handleManagement(pluginapi.ManagementRequest{Method: http.MethodGet, Path: managementStatusFullPath}).Body, &status)
	if status["usage_source"] != usageSourceEndpoint {
		t.Fatalf("status usage_source = %#v", status["usage_source"])
	}
	if _, present := status["seat_pacer_file"]; present {
		t.Fatal("endpoint mode should not report a seat pacer file")
	}
}

func TestSeatPacerStatusReportsSourceAndReadings(t *testing.T) {
	now := fixtureObservedAt.Add(time.Minute)
	runtime := newTestRuntime(seatPacerHost(), (&fakeFetcher{}).fetch, now)
	cfg := seatPacerConfig(t, fixturePath)
	runtime.config.Store(&cfg)
	runtime.pollOnce(context.Background(), cfg)

	var status cutoffStatusResponse
	if err := json.Unmarshal(runtime.handleManagement(pluginapi.ManagementRequest{Method: http.MethodGet, Path: managementStatusFullPath}).Body, &status); err != nil {
		t.Fatal(err)
	}
	if status.UsageSource != usageSourceSeatPacerFile || status.SeatPacerFile != fixturePath {
		t.Fatalf("status source = %q, file = %q", status.UsageSource, status.SeatPacerFile)
	}
	for _, account := range status.Accounts {
		if account.ID == charlesAuthID && account.WindowsSampledAt != fixtureObservedAt.Format(time.RFC3339Nano) {
			t.Fatalf("charles windows_sampled_at = %q", account.WindowsSampledAt)
		}
	}
}

func TestSeatPacerConfigValidation(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	const cutoffs = "cutoffs:\n  - scope: seven_day\n    percent-used: 70\n"
	tests := []struct {
		name     string
		yaml     string
		wantErr  bool
		wantSrc  string
		wantPath string
	}{
		{"default is endpoint", "", false, usageSourceEndpoint, defaultSeatPacerFile},
		{"default path expands under home", "usage-source: seat-pacer-file\n" + cutoffs, false, usageSourceSeatPacerFile,
			filepath.Join(home, ".cli-proxy-api/plugins/claude-seat-pacer/history.json")},
		{"source is case-insensitive", "usage-source: Seat-Pacer-File\nseat-pacer-file: /var/sp/history.json\n" + cutoffs, false, usageSourceSeatPacerFile, "/var/sp/history.json"},
		{"tilde path expands", "usage-source: seat-pacer-file\nseat-pacer-file: ~/sp/h.json\n" + cutoffs, false, usageSourceSeatPacerFile, filepath.Join(home, "sp/h.json")},
		{"relative path is kept for the host's working dir", "usage-source: seat-pacer-file\nseat-pacer-file: sp/h.json\n" + cutoffs, false, usageSourceSeatPacerFile, "sp/h.json"},
		{"file mode requires cutoffs", "usage-source: seat-pacer-file\n", true, "", ""},
		{"unknown source", "usage-source: carrier-pigeon\n", true, "", ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg, err := decodeLifecycleConfig(lifecycleYAML(tt.yaml))
			if (err != nil) != tt.wantErr {
				t.Fatalf("err = %v, wantErr %v", err, tt.wantErr)
			}
			if tt.wantErr {
				return
			}
			if cfg.UsageSource != tt.wantSrc || cfg.SeatPacerFile != tt.wantPath {
				t.Fatalf("source = %q, path = %q; want %q, %q", cfg.UsageSource, cfg.SeatPacerFile, tt.wantSrc, tt.wantPath)
			}
		})
	}

	fields := map[string]pluginapi.ConfigFieldType{}
	for _, field := range pluginRegistration().Metadata.ConfigFields {
		fields[field.Name] = field.Type
	}
	if fields["usage-source"] != pluginapi.ConfigFieldTypeString || fields["seat-pacer-file"] != pluginapi.ConfigFieldTypeString {
		t.Fatalf("config fields = %#v", fields)
	}
}

func mustWrite(t *testing.T, path, body string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}
