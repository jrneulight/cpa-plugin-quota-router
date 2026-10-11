package main

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"io/fs"
	"math"
	"os"
	"strings"
	"time"
)

// seatPacerFileVersion is the only history.json format this build reads. A
// file of any other version is refused rather than misread.
const seatPacerFileVersion = 1

// seatPacerFile is the part of claude-seat-pacer's history.json this plugin
// reads: each credential's last reading, keyed by CLIProxyAPI auth ID. The
// per-window history Seat Pacer keeps under "seats" is ignored.
type seatPacerFile struct {
	Version  *int                        `json:"version"`
	Readings map[string]seatPacerReading `json:"readings"`
}

type seatPacerReading struct {
	ObservedAt time.Time         `json:"observed_at"`
	Source     string            `json:"source"`
	Windows    []seatPacerWindow `json:"windows"`
}

// seatPacerWindow mirrors model.Window in claude-seat-pacer. Utilization is a
// fraction (1.0 is 100%) and may exceed 1 once usage runs past a cap. A zero
// ResetsAt means the window has not started.
type seatPacerWindow struct {
	Kind        string    `json:"kind"`
	Scope       *string   `json:"scope"`
	Utilization *float64  `json:"utilization"`
	ResetsAt    time.Time `json:"resets_at"`
	Status      string    `json:"status"`
}

// seatPacerRejectedStatus is the status Seat Pacer copies from Anthropic's
// rate-limit headers for a window that is refusing requests.
const seatPacerRejectedStatus = "rejected"

// seatPacerUsage is one credential's reading converted to this plugin's
// shape, with the time Seat Pacer took it.
type seatPacerUsage struct {
	ObservedAt time.Time
	Result     usageResult
}

// readSeatPacerFile reads and converts the file. It returns a failure
// category, never the file's contents, when the file cannot be used at all.
// A credential whose reading is absent or carries no usable window is left
// out of the map.
func readSeatPacerFile(path string) (map[string]seatPacerUsage, string) {
	file, err := os.Open(path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, pollErrorSeatPacerFileMissing
		}
		return nil, pollErrorSeatPacerFileUnreadable
	}
	defer file.Close()
	body, err := io.ReadAll(io.LimitReader(file, maxSeatPacerFileBytes+1))
	if err != nil {
		return nil, pollErrorSeatPacerFileUnreadable
	}
	if len(body) > maxSeatPacerFileBytes {
		return nil, pollErrorSeatPacerFileInvalid
	}
	return parseSeatPacerFile(body)
}

func parseSeatPacerFile(body []byte) (map[string]seatPacerUsage, string) {
	var decoded seatPacerFile
	if json.Unmarshal(body, &decoded) != nil || decoded.Version == nil {
		return nil, pollErrorSeatPacerFileInvalid
	}
	if *decoded.Version != seatPacerFileVersion {
		return nil, pollErrorSeatPacerFileVersion
	}
	out := make(map[string]seatPacerUsage, len(decoded.Readings))
	for authID, reading := range decoded.Readings {
		if strings.TrimSpace(authID) == "" || reading.ObservedAt.IsZero() {
			continue
		}
		result := seatPacerUsageResult(reading.Windows)
		if len(result.Windows) == 0 {
			continue
		}
		out[authID] = seatPacerUsage{ObservedAt: reading.ObservedAt, Result: result}
	}
	return out, ""
}

// seatPacerUsageResult converts Seat Pacer's windows into the result the
// usage endpoint would have produced: percent points keyed by five_hour,
// seven_day or the lowercased weekly_scoped model name.
func seatPacerUsageResult(windows []seatPacerWindow) usageResult {
	result := usageResult{
		ScopedWeekly: make(map[string]usageWindowResult),
		Windows:      make(map[string]usageWindowResult),
	}
	for _, window := range windows {
		key := seatPacerWindowKey(window)
		if key == "" {
			continue
		}
		if _, seen := result.Windows[key]; seen {
			continue
		}
		percent, ok := seatPacerPercent(window)
		if !ok {
			continue
		}
		converted := usageWindowResult{PercentUsed: percent, ResetAt: window.ResetsAt}
		result.Windows[key] = converted
		if window.ResetsAt.IsZero() {
			continue
		}
		switch key {
		case fiveHourUsageScope:
		case defaultUsageScope:
			result.WeeklyPercentUsed = percent
			result.ResetAt = window.ResetsAt
		default:
			result.ScopedWeekly[key] = converted
		}
	}
	return result
}

func seatPacerWindowKey(window seatPacerWindow) string {
	switch strings.ToLower(strings.TrimSpace(window.Kind)) {
	case fiveHourUsageScope:
		return fiveHourUsageScope
	case defaultUsageScope:
		return defaultUsageScope
	case "weekly_scoped":
		if window.Scope == nil {
			return ""
		}
		return strings.ToLower(strings.TrimSpace(*window.Scope))
	default:
		return ""
	}
}

// seatPacerPercent turns a 0..1 fraction into percent points. Usage past a
// cap, or a window Anthropic is refusing, reads as 100%.
func seatPacerPercent(window seatPacerWindow) (float64, bool) {
	if window.Utilization == nil {
		return 0, false
	}
	fraction := *window.Utilization
	if math.IsNaN(fraction) || math.IsInf(fraction, 0) || fraction < 0 {
		return 0, false
	}
	if strings.EqualFold(strings.TrimSpace(window.Status), seatPacerRejectedStatus) {
		return 100, true
	}
	percent := math.Round(fraction*100*1e6) / 1e6
	return math.Min(percent, 100), true
}

// refreshFromSeatPacer records each protected account's reading from Seat
// Pacer's file. It sends nothing to Anthropic and reads no credential. A
// reading is recorded as sampled when Seat Pacer took it, so the reserve's
// staleness rule measures Seat Pacer's age, not this read's.
func (r *pluginRuntime) refreshFromSeatPacer(ctx context.Context, cfg pluginConfig, all bool, authIDs map[string]struct{}) {
	entries, err := r.host.listAuth()
	if err != nil {
		r.log("warn", "quota router auth discovery failed", map[string]any{"category": "auth_list"})
		return
	}
	auths := physicalClaudeAuths(entries)
	r.cache.reconcile(auths)
	readAt := r.now()
	r.refreshMu.Lock()
	r.seatPacerReadAt = readAt
	r.refreshMu.Unlock()
	readings, fileCategory := readSeatPacerFile(cfg.SeatPacerFile)
	if fileCategory != "" {
		r.log("warn", "quota router seat pacer file unusable", map[string]any{
			"category": fileCategory,
			"path":     cfg.SeatPacerFile,
		})
	}
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
		r.cache.recordAttempt(auth.ID, readAt)
		if fileCategory != "" {
			r.cache.recordFailure(auth.ID, fileCategory)
			continue
		}
		reading, ok := readings[auth.ID]
		if !ok {
			r.recordPollFailure(auth.ID, pollErrorSeatPacerNoReading)
			continue
		}
		sampledAt := reading.ObservedAt
		if sampledAt.After(readAt) {
			// A reading stamped ahead of this clock is treated as just taken,
			// never as fresh into the future.
			sampledAt = readAt
		}
		r.recordUsage(auth.ID, cfg, reading.Result, sampledAt, readAt)
		r.cache.recordAttempt(auth.ID, readAt)
	}
}

// queueSeatPacerReread asks the worker to read the file again, at most once
// per seatPacerMinReread, for a request the reserve could not confirm.
func (r *pluginRuntime) queueSeatPacerReread() {
	r.lifecycleMu.Lock()
	defer r.lifecycleMu.Unlock()
	r.refreshMu.Lock()
	last := r.seatPacerReadAt
	r.refreshMu.Unlock()
	if !last.IsZero() && r.now().Before(last.Add(seatPacerMinReread)) {
		return
	}
	r.queueAllRefreshLocked()
}

// queueUnknownReserveRefresh refreshes after the reserve refused a request
// for want of a fresh reading.
func (r *pluginRuntime) queueUnknownReserveRefresh(cfg pluginConfig) {
	if cfg.UsageSource == usageSourceSeatPacerFile {
		r.queueSeatPacerReread()
		return
	}
	r.queueReserveRefresh()
}
