package domain

import (
	"encoding/json"
	"time"
)

// AccountConfigView is the UI-relevant slice of an account's config blob.
type AccountConfigView struct {
	PollIntervalSeconds int
	ProviderTotal       int // last_discovered_total
	Discovering         bool
	RateLimited         bool // worker-reported budget exhaustion still in its cooldown
}

// ParseAccountConfig reads the UI-relevant keys of an account's config.
func ParseAccountConfig(raw json.RawMessage, now time.Time) AccountConfigView {
	var v AccountConfigView
	if len(raw) == 0 {
		return v
	}
	var cfg map[string]any
	if json.Unmarshal(raw, &cfg) != nil {
		return v
	}
	if f, ok := cfg["poll_interval_seconds"].(float64); ok {
		v.PollIntervalSeconds = int(f)
	}
	if f, ok := cfg["last_discovered_total"].(float64); ok {
		v.ProviderTotal = int(f)
	}
	if b, ok := cfg["discovering"].(bool); ok {
		v.Discovering = b
	}
	if s, ok := cfg["rate_limited_until"].(string); ok && s != "" {
		if until, err := time.Parse(time.RFC3339, s); err == nil && now.Before(until) {
			v.RateLimited = true
		}
	}
	return v
}

// RateLimitedNow reports whether either provider window is exhausted and not
// yet reset.
func RateLimitedNow(rl *RateLimitSnapshot, now time.Time) bool {
	if rl == nil {
		return false
	}
	if rl.ShortWindowLimit > 0 && rl.ShortWindowUsed >= rl.ShortWindowLimit && now.Before(rl.ShortWindowResetAt) {
		return true
	}
	if rl.LongWindowLimit > 0 && rl.LongWindowUsed >= rl.LongWindowLimit && now.Before(rl.LongWindowResetAt) {
		return true
	}
	return false
}

// BudgetWindow is one live rate-limit window of a provider app.
type BudgetWindow struct {
	Used     int
	Limit    int
	ResetsAt time.Time
}

// RateBudget is the live API budget of a provider app; nil windows are unknown.
type RateBudget struct {
	Short *BudgetWindow
	Daily *BudgetWindow
}

// Empty reports whether no window is known.
func (b RateBudget) Empty() bool { return b.Short == nil && b.Daily == nil }

// Exhausted reports whether a known, active window is used up.
func (b RateBudget) Exhausted(now time.Time) bool {
	for _, w := range []*BudgetWindow{b.Short, b.Daily} {
		if w != nil && now.Before(w.ResetsAt) && w.Used >= w.Limit {
			return true
		}
	}
	return false
}

// WorkerPoolInfo is a provider pool as shown in pickers (docs/adr/0006).
type WorkerPoolInfo struct {
	Name          string
	Package       string
	ParserVersion string
	Release       string
	Online        bool
	Compatible    bool
}

// ProviderInfo describes a provider the instance knows how to connect.
type ProviderInfo struct {
	ID          string
	DisplayName string
	OAuth       bool // links accounts through an OAuth authorize round-trip
}
