package sync

import (
	"context"
	"encoding/json"
	"time"

	"github.com/opencairn-org/cairn-core/internal/domain"
	"github.com/opencairn-org/cairn-core/internal/port"
)

// KVOpener opens a KV bucket; port.JobBus satisfies it.
type KVOpener interface {
	KV(bucket string) (port.KV, error)
}

// Budgets reads the live provider API budget the workers sync into the
// shared cairn_rate_limits KV from the providers' usage headers.
type Budgets struct {
	Bus         KVOpener
	Connections port.UserProviderConfigRepo // may be nil: no app scope
	Now         func() time.Time
}

// budgetState mirrors the worker rate limiter's KV value.
type budgetState struct {
	Capacity      int       `json:"capacity"`
	Used          int       `json:"used"`
	WindowResetAt time.Time `json:"window_reset_at"`
}

func (b *Budgets) now() time.Time {
	if b.Now != nil {
		return b.Now()
	}
	return time.Now().UTC()
}

// AppID is the hash of the account's connection app (client id), the scope
// of its budget; "" when the connection is unknown.
func (b *Budgets) AppID(ctx context.Context, a domain.ExternalAccount) string {
	if a.ConnectionID == nil || b.Connections == nil {
		return ""
	}
	conn, err := b.Connections.GetByID(ctx, *a.ConnectionID)
	if err != nil {
		return ""
	}
	return domain.AppID(conn.ClientID)
}

// For is the budget of the account's app on its assigned pool.
func (b *Budgets) For(ctx context.Context, a domain.ExternalAccount) domain.RateBudget {
	out, _ := b.Of(ctx, a.Provider, b.AppID(ctx, a), a.AssignedWorkerName)
	return out
}

// Of reads <provider>.<app>.<window>, else the pool's
// <provider>.<pool>.<window>, else <provider>.<window>, and reports whether
// an active window is used up.
func (b *Budgets) Of(ctx context.Context, provider, app, pool string) (domain.RateBudget, bool) {
	var out domain.RateBudget
	if b.Bus == nil {
		return out, false
	}
	kv, err := b.Bus.KV("cairn_rate_limits")
	if err != nil {
		return out, false
	}
	now := b.now()
	for _, win := range []string{"short", "daily"} {
		st, ok := readBudgetState(ctx, kv, budgetKeys(provider, app, pool, win))
		if !ok {
			continue
		}
		w := &domain.BudgetWindow{Used: st.Used, Limit: st.Capacity, ResetsAt: st.WindowResetAt.UTC()}
		if !now.Before(st.WindowResetAt) {
			w.Used = 0 // window elapsed; budget is fresh even if no call re-synced yet
		}
		if win == "short" {
			out.Short = w
		} else {
			out.Daily = w
		}
	}
	return out, out.Exhausted(now)
}

// budgetKeys lists the KV keys of one window, most specific first.
func budgetKeys(provider, app, pool, win string) []string {
	keys := []string{}
	for _, scope := range []string{app, pool} {
		if scope != "" {
			keys = append(keys, provider+"."+scope+"."+win)
		}
	}
	return append(keys, provider+"."+win)
}

// readBudgetState returns the first key's valid state.
func readBudgetState(ctx context.Context, kv port.KV, keys []string) (budgetState, bool) {
	for _, key := range keys {
		entry, err := kv.Get(ctx, key)
		if err != nil {
			continue
		}
		var st budgetState
		if json.Unmarshal(entry.Value, &st) == nil && st.Capacity > 0 {
			return st, true
		}
	}
	return budgetState{}, false
}
