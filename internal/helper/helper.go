// Package helper calls the local Ollama API for small jobs such as
// summarizing a failed CI log. It is best effort: when Ollama is down or
// slow, callers get a trimmed copy of the input instead.
package helper

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/deniz-ai-orchestration/ai-orchestrator/internal/config"
)

// Summarizer shortens text for a prompt.
type Summarizer interface {
	Summarize(ctx context.Context, instruction, text string) string
}

// Ollama summarizes with a local model.
type Ollama struct {
	Endpoint string // e.g. http://localhost:11434
	Model    string
	Timeout  time.Duration
	// MaxInput bounds what is sent to the model (the tail is kept).
	MaxInput int
	// Fallback bounds the trimmed text returned when the model fails.
	Fallback int
	http     *http.Client
}

// FromConfig returns an Ollama helper for the first model in the helper
// role's pool whose provider has an endpoint, or a Trim when there is none.
func FromConfig(cfg *config.Config) Summarizer {
	r, ok := cfg.Roles["helper"]
	if ok && r.IsEnabled() {
		for _, name := range r.Pool {
			m := cfg.Models[name]
			p := cfg.Providers[m.Provider]
			if p.Endpoint != "" && !p.Disabled && !m.Disabled {
				return &Ollama{Endpoint: p.Endpoint, Model: m.Model}
			}
		}
	}
	return Trim{Max: 6000}
}

// Summarize asks the model to apply instruction to text. On any failure it
// returns the tail of text.
func (o *Ollama) Summarize(ctx context.Context, instruction, text string) string {
	maxIn, fallback := o.MaxInput, o.Fallback
	if maxIn <= 0 {
		maxIn = 24000
	}
	if fallback <= 0 {
		fallback = 6000
	}
	timeout := o.Timeout
	if timeout <= 0 {
		timeout = 90 * time.Second
	}
	client := o.http
	if client == nil {
		client = &http.Client{Timeout: timeout}
	}
	in := Tail(text, maxIn)
	body, _ := json.Marshal(map[string]any{
		"model": o.Model, "stream": false,
		"prompt":  instruction + "\n\n" + in,
		"options": map[string]any{"temperature": 0},
	})
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, "POST", strings.TrimRight(o.Endpoint, "/")+"/api/generate", bytes.NewReader(body))
	if err != nil {
		return Tail(text, fallback)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		return Tail(text, fallback)
	}
	defer resp.Body.Close()
	var out struct {
		Response string `json:"response"`
		Error    string `json:"error"`
	}
	if resp.StatusCode != 200 || json.NewDecoder(resp.Body).Decode(&out) != nil || out.Error != "" || strings.TrimSpace(out.Response) == "" {
		return Tail(text, fallback)
	}
	return fmt.Sprintf("%s\n\n(summary by %s; last lines of the log follow)\n%s",
		strings.TrimSpace(out.Response), o.Model, Tail(text, 1500))
}

// Trim is the no-model fallback: the tail of the text.
type Trim struct{ Max int }

// Summarize returns the last Max bytes of text.
func (t Trim) Summarize(_ context.Context, _, text string) string { return Tail(text, t.Max) }

// Tail returns about the last n bytes of s, cut at a line start.
func Tail(s string, n int) string {
	if len(s) <= n {
		return s
	}
	s = s[len(s)-n:]
	if i := strings.IndexByte(s, '\n'); i >= 0 && i < len(s)-1 {
		s = s[i+1:]
	}
	return "…\n" + s
}
