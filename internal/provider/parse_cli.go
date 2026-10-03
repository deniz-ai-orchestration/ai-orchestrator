package provider

import (
	"encoding/json"
	"strconv"
	"strings"
)

// Parsers follow the fixtures recorded on PC2 (testdata/fixtures/README.md).

// Parse reads Claude Code's stream-json. The last line is type "result";
// on API errors its subtype is still "success", so is_error and
// terminal_reason decide. rate_limit_event lines carry the quota windows.
func (claude) Parse(req Request, run RunOutput) Outcome {
	var o Outcome
	var result map[string]json.RawMessage
	var apiErr string
	for _, ev := range events(run.Stdout) {
		switch str(ev, "type") {
		case "result":
			result = ev
		case "assistant":
			if e := str(ev, "error"); e != "" {
				apiErr = e
			}
		case "rate_limit_event":
			info := obj(ev, "rate_limit_info")
			o.ResetAt = unixTime(num(info, "resetsAt"))
			if s := str(info, "status"); s != "" && s != "allowed" && s != "allowed_warning" {
				apiErr = "rate_limit: " + s
			}
		}
	}
	if run.timedOut() {
		o.Kind, o.Detail = Timeout, "killed before a result"
		return o
	}
	if result == nil {
		o.Kind, o.Detail = CLIError, "no result line: "+truncate(string(run.Stderr), 300)
		if run.ExitCode == 0 {
			o.Kind = BadOutput
		}
		return o
	}
	o.Session = str(result, "session_id")
	usage := obj(result, "usage")
	o.InputTokens, o.OutputTokens = num(usage, "input_tokens"), num(usage, "output_tokens")
	text := str(result, "result")

	var isErr bool
	_ = json.Unmarshal(result["is_error"], &isErr)
	if isErr || run.ExitCode != 0 {
		if str(result, "terminal_reason") == "max_turns" {
			o.Kind, o.Detail = CLIError, "max turns reached"
			return o
		}
		o.Kind = classifyError(apiErr + " " + text + " " + string(run.Stderr))
		o.Detail = truncate(text, 300)
		return o
	}
	return structured(req, result["structured_output"], text, o)
}

// Parse reads codex exec --json. turn.completed or turn.failed ends the
// run; intermediate error events (reconnect retries) are not fatal.
func (codex) Parse(req Request, run RunOutput) Outcome {
	var o Outcome
	var last, failure string
	var completed bool
	for _, ev := range events(run.Stdout) {
		switch str(ev, "type") {
		case "item.completed":
			if it := obj(ev, "item"); str(it, "type") == "agent_message" {
				last = str(it, "text")
			}
		case "turn.completed":
			completed = true
			u := obj(ev, "usage")
			o.InputTokens, o.OutputTokens = num(u, "input_tokens"), num(u, "output_tokens")
		case "turn.failed":
			failure = str(obj(ev, "error"), "message")
		}
	}
	switch {
	case failure != "":
		o.Kind, o.Detail = classifyError(failure), truncate(failure, 300)
		if strings.Contains(failure, "invalid_request_error") {
			o.Kind = CLIError
		}
		return o
	case run.timedOut():
		o.Kind, o.Detail = Timeout, "killed before turn end"
		return o
	case !completed:
		o.Kind, o.Detail = CLIError, "no turn.completed: "+truncate(string(run.Stderr), 300)
		return o
	}
	return structured(req, nil, last, o)
}

// AuthFailing reports whether a partial codex stream already shows a 401,
// so the runner can stop the ~15 s of reconnect retries early.
func (codex) AuthFailing(stdout []byte) bool {
	for _, ev := range events(stdout) {
		if str(ev, "type") == "error" && strings.Contains(str(ev, "message"), "401 Unauthorized") {
			return true
		}
	}
	return false
}

// Parse reads agy's stream-json, whose events are keyed by "event". A
// --print-timeout ends with status SUCCESS and an empty response, which is
// a timeout, not a success.
func (agy) Parse(req Request, run RunOutput) Outcome {
	var o Outcome
	var result map[string]json.RawMessage
	for _, ev := range events(run.Stdout) {
		if str(ev, "event") == "result" {
			result = obj(ev, "result")
		}
	}
	if result == nil {
		if run.timedOut() {
			o.Kind, o.Detail = Timeout, "killed before a result"
			return o
		}
		o.Kind, o.Detail = CLIError, "no result event: "+truncate(string(run.Stderr), 300)
		return o
	}
	usage := obj(result, "usage")
	o.InputTokens, o.OutputTokens = num(usage, "input_tokens"), num(usage, "output_tokens")
	if str(result, "status") != "SUCCESS" {
		msg := str(result, "error")
		o.Kind, o.Detail = classifyError(msg), truncate(msg, 300)
		return o
	}
	text := str(result, "response")
	if strings.Contains(string(run.Stderr), "print timeout") || (text == "" && num(usage, "total_tokens") == 0) {
		o.Kind, o.Detail = Timeout, "agy print timeout"
		return o
	}
	return structured(req, result["structured_output"], text, o)
}

// Parse reads opencode run --format json. There is no final result event:
// the run is complete at a step_finish with reason "stop". Errors are an
// "error" event on stdout.
func (opencode) Parse(req Request, run RunOutput) Outcome {
	var o Outcome
	var texts []string
	var stopped bool
	var errName, errMsg string
	for _, ev := range events(run.Stdout) {
		part := obj(ev, "part")
		switch str(ev, "type") {
		case "step_start":
			texts = texts[:0] // keep only the final step's text
		case "text":
			texts = append(texts, str(part, "text"))
		case "step_finish":
			tok := obj(part, "tokens")
			o.InputTokens += num(tok, "input")
			o.OutputTokens += num(tok, "output")
			stopped = str(part, "reason") == "stop"
		case "error":
			e := obj(ev, "error")
			errName, errMsg = str(e, "name"), str(obj(e, "data"), "message")
			if code := num(obj(e, "data"), "statusCode"); code != 0 {
				errMsg = strings.TrimSpace(errMsg + " status " + strconv.Itoa(code))
			}
		}
	}
	switch {
	case errName != "":
		o.Kind, o.Detail = classifyError(errMsg), errName+": "+truncate(errMsg, 300)
		return o
	case run.timedOut():
		o.Kind, o.Detail = Timeout, "killed before the final step"
		return o
	case !stopped:
		o.Kind, o.Detail = BadOutput, "no final step"
		if run.ExitCode != 0 {
			o.Kind = CLIError
		}
		return o
	}
	return structured(req, nil, strings.Join(texts, "\n"), o)
}
