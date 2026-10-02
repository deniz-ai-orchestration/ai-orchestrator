// Package provider holds one adapter per coding-agent CLI. An adapter is
// pure: it turns a run request into a command line and turns the captured
// output into an Outcome. The runner
// owns processes, containers, timeouts and credentials for every provider.
package provider

import (
	"errors"
	"fmt"
	"time"

	"github.com/deniz-ai-orchestration/ai-orchestrator/internal/config"
	"github.com/deniz-ai-orchestration/ai-orchestrator/internal/engine"
)

// Request is everything an adapter needs to build one headless run.
type Request struct {
	Model      string // the CLI's model id (config models.<name>.model)
	Prompt     string // fed on stdin unless the adapter says otherwise
	SchemaPath string // JSON schema for the structured result, inside the container
	ResultPath string // where the CLI should write the final result, if it can
	MaxTurns   int    // 0 = CLI default
	ReadOnly   bool   // reviewers and testers: no edits to the workspace
	Agent      string // OpenCode agent name (e.g. "qa"); ignored elsewhere
}

// Command is a CLI invocation. It never holds secret values: SecretEnv maps
// an environment variable to a file name in secrets_dir, and the runner reads
// the file and injects the value into that run's container only.
type Command struct {
	Argv      []string
	Stdin     string
	Env       map[string]string // non-secret environment
	SecretEnv map[string]string // env var -> file in secrets_dir
	Mounts    []string          // named credential volumes the run needs
	Serialize bool              // at most one concurrent run for this provider
}

// OutcomeKind classifies how a run ended.
type OutcomeKind string

const (
	OK          OutcomeKind = "ok"           // structured result parsed
	Blocked     OutcomeKind = "blocked"      // agent returned a question
	RateLimited OutcomeKind = "rate_limited" // quota or rate limit; cool the model down
	AuthFailed  OutcomeKind = "auth_failed"  // disable the provider, no retries
	Unavailable OutcomeKind = "unavailable"  // provider capacity error
	CLIError    OutcomeKind = "cli_error"    // non-zero exit for another reason
	Timeout     OutcomeKind = "timeout"      // runner killed it at the role's timeout
	BadOutput   OutcomeKind = "bad_output"   // exit 0 but no valid structured result
)

// Outcome is what a run produced.
type Outcome struct {
	Kind         OutcomeKind
	Result       []byte    // the structured JSON result when Kind is OK or Blocked
	ResetAt      time.Time // when a rate limit lifts, if the CLI says
	InputTokens  int
	OutputTokens int
	Detail       string
}

// Reason maps a failed outcome to the engine's needs_human reason.
func (o Outcome) Reason() engine.Reason {
	switch o.Kind {
	case AuthFailed:
		return engine.ReasonAuthFailed
	case RateLimited, Unavailable:
		return engine.ReasonProviderUnavailable
	case Timeout:
		return engine.ReasonTimeout
	case BadOutput:
		return engine.ReasonBadOutput
	case Blocked:
		return engine.ReasonRequirementsUnclear
	case CLIError:
		return engine.ReasonCLIError
	default:
		return engine.ReasonNone
	}
}

// Adapter builds commands for one CLI and reads its output.
type Adapter interface {
	// CLI is the binary name this adapter drives (config providers.<p>.cli).
	CLI() string
	Build(Request) (Command, error)
	// Parse classifies a finished run. req is the request the command was
	// built from; a schema request without a structured result is BadOutput.
	Parse(req Request, run RunOutput) Outcome
}

var adapters = map[string]Adapter{}

func register(a Adapter) { adapters[a.CLI()] = a }

// For returns the adapter for a configured provider.
func For(p config.Provider) (Adapter, error) {
	a, ok := adapters[p.CLI]
	if !ok {
		return nil, fmt.Errorf("no adapter for cli %q", p.CLI)
	}
	return a, nil
}

var errNoModel = errors.New("model is required")

func check(r Request) error {
	if r.Model == "" {
		return errNoModel
	}
	if r.Prompt == "" {
		return errors.New("prompt is required")
	}
	return nil
}
