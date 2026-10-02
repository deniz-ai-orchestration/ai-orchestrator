package provider

import (
	"bufio"
	"bytes"
	"encoding/json"
	"strings"
	"time"
)

// RunOutput is what the runner captured from one CLI process.
type RunOutput struct {
	ExitCode int
	Stdout   []byte
	Stderr   []byte
	// Killed is set when the runner stopped the process at the role's
	// timeout. Exit code 137 (SIGKILL) is treated the same way.
	Killed bool
}

func (o RunOutput) timedOut() bool { return o.Killed || o.ExitCode == 137 }

// events decodes NDJSON, skipping lines that are not JSON objects.
func events(stdout []byte) []map[string]json.RawMessage {
	var out []map[string]json.RawMessage
	sc := bufio.NewScanner(bytes.NewReader(stdout))
	sc.Buffer(make([]byte, 0, 64*1024), 16*1024*1024)
	for sc.Scan() {
		line := bytes.TrimSpace(sc.Bytes())
		if len(line) == 0 || line[0] != '{' {
			continue
		}
		var ev map[string]json.RawMessage
		if json.Unmarshal(line, &ev) == nil {
			out = append(out, ev)
		}
	}
	return out
}

// str reads a string field, returning "" when absent or not a string.
func str(ev map[string]json.RawMessage, key string) string {
	var s string
	_ = json.Unmarshal(ev[key], &s)
	return s
}

// obj reads an object field.
func obj(ev map[string]json.RawMessage, key string) map[string]json.RawMessage {
	var m map[string]json.RawMessage
	_ = json.Unmarshal(ev[key], &m)
	return m
}

func num(ev map[string]json.RawMessage, key string) int {
	var n float64
	_ = json.Unmarshal(ev[key], &n)
	return int(n)
}

// classifyError maps provider error text to an outcome kind.
func classifyError(text string) OutcomeKind {
	t := strings.ToLower(text)
	switch {
	case containsAny(t, "401", "unauthorized", "authentication_failed", "not logged in",
		"invalid api key", "please run /login", "unauthenticated"):
		return AuthFailed
	case containsAny(t, "429", "rate limit", "rate_limit", "usage limit", "quota",
		"resource_exhausted", "too many requests"):
		return RateLimited
	case containsAny(t, "503", "529", "overloaded", "capacity", "unavailable"):
		return Unavailable
	default:
		return CLIError
	}
}

func containsAny(s string, subs ...string) bool {
	for _, sub := range subs {
		if strings.Contains(s, sub) {
			return true
		}
	}
	return false
}

// structured interprets the agent's final answer. raw is the CLI's own
// structured output when it has one, text the final message otherwise.
// A result with "status":"blocked" is a question for the human.
func structured(req Request, raw json.RawMessage, text string, o Outcome) Outcome {
	if len(raw) == 0 || string(raw) == "null" {
		raw = jsonObject(text)
	}
	if len(raw) == 0 {
		if req.SchemaPath != "" {
			o.Kind, o.Detail = BadOutput, "no structured result: "+truncate(text, 200)
			return o
		}
		o.Kind, o.Detail = OK, text
		return o
	}
	var res struct {
		Status  string `json:"status"`
		Summary string `json:"summary"`
	}
	if err := json.Unmarshal(raw, &res); err != nil {
		o.Kind, o.Detail = BadOutput, "result is not a JSON object"
		return o
	}
	o.Result, o.Detail, o.Kind = raw, res.Summary, OK
	if res.Status == "blocked" {
		o.Kind = Blocked
	}
	return o
}

// jsonObject returns text as a JSON object if it is one, allowing a
// surrounding markdown code fence.
func jsonObject(text string) json.RawMessage {
	t := strings.TrimSpace(text)
	if strings.HasPrefix(t, "```") {
		t = strings.TrimPrefix(t, "```json")
		t = strings.TrimPrefix(t, "```")
		t = strings.TrimSuffix(strings.TrimSpace(t), "```")
		t = strings.TrimSpace(t)
	}
	if !strings.HasPrefix(t, "{") || !json.Valid([]byte(t)) {
		return nil
	}
	return json.RawMessage(t)
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

func unixTime(sec int) time.Time {
	if sec <= 0 {
		return time.Time{}
	}
	return time.Unix(int64(sec), 0).UTC()
}
