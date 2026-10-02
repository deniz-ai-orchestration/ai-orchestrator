// Package router picks a model for each run from a role's ranked pool.
package router

import (
	"errors"
	"fmt"
	"strings"

	"github.com/deniz-ai-orchestration/ai-orchestrator/internal/config"
)

// ErrNoModel means no model in the role's pool can run now.
var ErrNoModel = errors.New("no usable model")

// Choice is the model picked for a run.
type Choice struct {
	Name     string // config models.<name>
	Model    config.Model
	Provider config.Provider
	Pinned   bool
}

// Options narrow the pick.
type Options struct {
	// Avoid is never picked (the developer's model when picking a reviewer).
	Avoid string
	// Usable rejects providers that cannot run here (no container support).
	Usable func(config.Provider) bool
	// Pin is the model the role is pinned to; it wins while it can run.
	Pin string
	// Skip says why a model cannot run right now (disabled, cooling down,
	// over budget), or "" when it can.
	Skip func(name string) string
}

// Pick returns the pinned model when it can run, else the first model in
// the role's pool that can. The error lists why each model was passed over.
func Pick(cfg *config.Config, role string, o Options) (Choice, error) {
	r, ok := cfg.Roles[role]
	if !ok {
		return Choice{}, fmt.Errorf("unknown role %q", role)
	}
	if !r.IsEnabled() {
		return Choice{}, fmt.Errorf("role %q is disabled", role)
	}
	var why []string
	check := func(name string) (Choice, bool) {
		m, ok := cfg.Models[name]
		if !ok {
			why = append(why, name+": not in the config")
			return Choice{}, false
		}
		p := cfg.Providers[m.Provider]
		reason := ""
		switch {
		case name == o.Avoid:
			reason = "same model as the developer"
		case m.Disabled || p.Disabled:
			reason = "disabled in the config"
		case p.CLI == "":
			reason = "provider has no CLI"
		case o.Usable != nil && !o.Usable(p):
			reason = "cannot run in a container yet"
		case o.Skip != nil:
			reason = o.Skip(name)
		}
		if reason != "" {
			why = append(why, name+": "+reason)
			return Choice{}, false
		}
		return Choice{Name: name, Model: m, Provider: p}, true
	}
	if o.Pin != "" {
		if c, ok := check(o.Pin); ok {
			c.Pinned = true
			return c, nil
		}
	}
	for _, name := range r.Pool {
		if name == o.Pin {
			continue
		}
		if c, ok := check(name); ok {
			return c, nil
		}
	}
	return Choice{}, fmt.Errorf("role %s: %w (%s)", role, ErrNoModel, strings.Join(why, "; "))
}
