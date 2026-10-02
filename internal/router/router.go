// Package router picks a model for each run from a role's ranked pool.
//
// M3 takes the first usable model in the pool. M6 adds pins, cooldowns,
// budgets and quota-based fallback on top of the same entry point.
package router

import (
	"errors"
	"fmt"

	"github.com/deniz-ai-orchestration/ai-orchestrator/internal/config"
)

// ErrNoModel means no model in the role's pool can run now.
var ErrNoModel = errors.New("no usable model")

// Choice is the model picked for a run.
type Choice struct {
	Name     string // config models.<name>
	Model    config.Model
	Provider config.Provider
}

// Pick returns the first model in role's pool that is enabled, whose
// provider is enabled and runs a CLI, that usable accepts, and that is not
// avoid (the developer's model when picking a reviewer).
func Pick(cfg *config.Config, role, avoid string, usable func(config.Provider) bool) (Choice, error) {
	r, ok := cfg.Roles[role]
	if !ok {
		return Choice{}, fmt.Errorf("unknown role %q", role)
	}
	if !r.IsEnabled() {
		return Choice{}, fmt.Errorf("role %q is disabled", role)
	}
	for _, name := range r.Pool {
		if name == avoid {
			continue
		}
		m := cfg.Models[name]
		p := cfg.Providers[m.Provider]
		if m.Disabled || p.Disabled || p.CLI == "" || (usable != nil && !usable(p)) {
			continue
		}
		return Choice{Name: name, Model: m, Provider: p}, nil
	}
	return Choice{}, fmt.Errorf("role %s: %w", role, ErrNoModel)
}
