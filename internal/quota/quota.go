// Package quota turns run outcomes into per-model cooldowns and disables,
// enforces the optional runs-per-5h budget, and reports model status for
// /quota and the control panel.
package quota

import (
	"context"
	"fmt"
	"sort"
	"time"

	"github.com/deniz-ai-orchestration/ai-orchestrator/internal/config"
	"github.com/deniz-ai-orchestration/ai-orchestrator/internal/provider"
	"github.com/deniz-ai-orchestration/ai-orchestrator/internal/store"
)

// Window is the subscription quota window the budget counts runs in.
const Window = 5 * time.Hour

// Backoff when a rate limit names no reset time: 1h, 2h, then 4h.
var rateBackoff = []time.Duration{time.Hour, 2 * time.Hour, 4 * time.Hour}

// CapacityPause is how long a model rests after an overload error.
const CapacityPause = 10 * time.Minute

// Tracker reads and writes model state.
type Tracker struct {
	Store *store.Store
	Cfg   *config.Config
	Now   func() time.Time
}

func (t *Tracker) now() time.Time {
	if t.Now != nil {
		return t.Now()
	}
	return time.Now()
}

// Skip returns a function that says why a model cannot run now: switched
// off, cooling down, or over its runs-per-5h budget.
func (t *Tracker) Skip(ctx context.Context) (func(name string) string, error) {
	states, err := t.Store.ModelStates(ctx)
	if err != nil {
		return nil, err
	}
	now := t.now()
	runs, err := t.Store.RunsSince(ctx, now.Add(-Window))
	if err != nil {
		return nil, err
	}
	return func(name string) string {
		st := states[name]
		switch {
		case st.Disabled:
			return "switched off"
		case st.CooldownUntil.After(now):
			return "cooling down until " + st.CooldownUntil.Local().Format("15:04")
		}
		if b := t.Cfg.Models[name].MaxRunsPer5h; b > 0 && runs[name] >= b {
			return fmt.Sprintf("budget used (%d of %d runs in 5h)", runs[name], b)
		}
		return ""
	}, nil
}

// Record updates a model's state from a run outcome. A rate limit cools
// the model down until its reset (or a growing backoff), an overload rests
// it briefly, an auth failure switches off every model of that provider,
// and a success resets the backoff.
func (t *Tracker) Record(ctx context.Context, model string, runID int64, out provider.Outcome) error {
	now := t.now()
	states, err := t.Store.ModelStates(ctx)
	if err != nil {
		return err
	}
	st := states[model]
	switch out.Kind {
	case provider.RateLimited:
		until, step := out.ResetAt, st.CooldownStep
		if !until.After(now) {
			until = now.Add(rateBackoff[min(step, len(rateBackoff)-1)])
			step++
		}
		until = until.Add(time.Minute) // limits often lift a little after the stated time
		if err := t.Store.SetCooldown(ctx, model, until, step); err != nil {
			return err
		}
		return t.Store.AddCapacityEvent(ctx, store.CapacityEvent{Model: model, RunID: runID, Kind: "rate_limit", ResetAt: out.ResetAt, Detail: out.Detail})
	case provider.Unavailable:
		if err := t.Store.SetCooldown(ctx, model, now.Add(CapacityPause), st.CooldownStep); err != nil {
			return err
		}
		return t.Store.AddCapacityEvent(ctx, store.CapacityEvent{Model: model, RunID: runID, Kind: "capacity", Detail: out.Detail})
	case provider.AuthFailed:
		p := t.Cfg.Models[model].Provider
		for name, m := range t.Cfg.Models {
			if m.Provider == p {
				if err := t.Store.SetDisabled(ctx, name, true); err != nil {
					return err
				}
			}
		}
		return t.Store.AddCapacityEvent(ctx, store.CapacityEvent{Model: model, RunID: runID, Kind: "auth_failed", Detail: out.Detail})
	case provider.OK, provider.Blocked:
		if st.CooldownStep > 0 {
			return t.Store.SetCooldown(ctx, model, time.Time{}, 0)
		}
	}
	return nil
}

// Status is one model's line in /quota.
type Status struct {
	Name, Provider, ModelID string
	ConfigDisabled          bool // off in config.yaml
	SwitchedOff             bool // off at runtime (auth failure or /disable)
	CoolingUntil            time.Time
	RunsIn5h, Budget        int
	PinnedFor               []string
	Last                    *store.CapacityEvent
}

// State is a one-word summary.
func (s Status) State(now time.Time) string {
	switch {
	case s.ConfigDisabled:
		return "off (config)"
	case s.SwitchedOff:
		return "off"
	case s.CoolingUntil.After(now):
		return "cooling"
	case s.Budget > 0 && s.RunsIn5h >= s.Budget:
		return "budget used"
	}
	return "ready"
}

// Report returns every configured model's status, sorted by name.
func (t *Tracker) Report(ctx context.Context) ([]Status, error) {
	states, err := t.Store.ModelStates(ctx)
	if err != nil {
		return nil, err
	}
	runs, err := t.Store.RunsSince(ctx, t.now().Add(-Window))
	if err != nil {
		return nil, err
	}
	pins, err := t.Store.RolePins(ctx)
	if err != nil {
		return nil, err
	}
	last, err := t.Store.LastCapacityEvents(ctx)
	if err != nil {
		return nil, err
	}
	var out []Status
	for name, m := range t.Cfg.Models {
		p := t.Cfg.Providers[m.Provider]
		s := Status{Name: name, Provider: m.Provider, ModelID: m.Model,
			ConfigDisabled: m.Disabled || p.Disabled, SwitchedOff: states[name].Disabled,
			CoolingUntil: states[name].CooldownUntil, RunsIn5h: runs[name], Budget: m.MaxRunsPer5h}
		for role, pinned := range pins {
			if pinned == name {
				s.PinnedFor = append(s.PinnedFor, role)
			}
		}
		sort.Strings(s.PinnedFor)
		if e, ok := last[name]; ok {
			s.Last = &e
		}
		out = append(out, s)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

// Pin pins role to model ("" or "auto" removes the pin). The not_same_as
// rule is still enforced when a model is picked.
func (t *Tracker) Pin(ctx context.Context, role, model string) error {
	if _, ok := t.Cfg.Roles[role]; !ok {
		return fmt.Errorf("unknown role %q", role)
	}
	if model == "auto" {
		model = ""
	}
	if model != "" {
		if _, ok := t.Cfg.Models[model]; !ok {
			return fmt.Errorf("unknown model %q", model)
		}
	}
	return t.Store.SetPin(ctx, role, model)
}

// SetEnabled switches a model on or off at runtime. Switching on also
// clears its cooldown.
func (t *Tracker) SetEnabled(ctx context.Context, model string, on bool) error {
	if _, ok := t.Cfg.Models[model]; !ok {
		return fmt.Errorf("unknown model %q", model)
	}
	if err := t.Store.SetDisabled(ctx, model, !on); err != nil {
		return err
	}
	if on {
		return t.Store.SetCooldown(ctx, model, time.Time{}, 0)
	}
	return nil
}
