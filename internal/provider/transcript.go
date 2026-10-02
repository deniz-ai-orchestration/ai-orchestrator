package provider

import (
	"encoding/json"
	"fmt"
	"strings"
)

// Transcript turns one line of a CLI's JSON stream into readable lines for
// the control panel: what the agent said, the tools it called and how they
// ended. Bookkeeping events render as nothing; a line that is not JSON is
// returned as it is.
func Transcript(cli string, line []byte) []string {
	text := strings.TrimRight(string(line), "\r\n")
	if strings.TrimSpace(text) == "" {
		return nil
	}
	var ev map[string]json.RawMessage
	if json.Unmarshal(line, &ev) != nil {
		return []string{text}
	}
	switch cli {
	case "claude":
		return claudeLines(ev)
	case "codex":
		return codexLines(ev)
	case "opencode":
		return opencodeLines(ev)
	case "agy":
		return agyLines(ev)
	}
	return []string{text}
}

const (
	toolMark   = "▸ "
	resultMark = "  ↳ "
	errorMark  = "! "
	endMark    = "■ "
)

func claudeLines(ev map[string]json.RawMessage) []string {
	switch str(ev, "type") {
	case "system":
		if str(ev, "subtype") == "init" {
			return []string{"● session started (" + str(ev, "model") + ")"}
		}
	case "assistant":
		var out []string
		for _, c := range contentBlocks(obj(ev, "message")) {
			switch str(c, "type") {
			case "text":
				out = append(out, splitText(str(c, "text"))...)
			case "tool_use":
				out = append(out, toolMark+str(c, "name")+" "+brief(c["input"]))
			}
		}
		return out
	case "user":
		var out []string
		for _, c := range contentBlocks(obj(ev, "message")) {
			if str(c, "type") != "tool_result" {
				continue
			}
			mark := resultMark
			if string(c["is_error"]) == "true" {
				mark = resultMark + "error: "
			}
			out = append(out, mark+firstLine(toolResultText(c["content"])))
		}
		return out
	case "result":
		line := endMark + str(ev, "subtype")
		if r := str(ev, "result"); r != "" && str(ev, "subtype") != "success" {
			line += ": " + firstLine(r)
		}
		return []string{line}
	}
	return nil
}

func contentBlocks(msg map[string]json.RawMessage) []map[string]json.RawMessage {
	var blocks []map[string]json.RawMessage
	_ = json.Unmarshal(msg["content"], &blocks)
	return blocks
}

// toolResultText reads a tool_result's content, which is a string or a list
// of text blocks.
func toolResultText(raw json.RawMessage) string {
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s
	}
	var blocks []map[string]json.RawMessage
	_ = json.Unmarshal(raw, &blocks)
	for _, b := range blocks {
		if t := str(b, "text"); t != "" {
			return t
		}
	}
	return ""
}

func codexLines(ev map[string]json.RawMessage) []string {
	item := obj(ev, "item")
	switch typ := str(ev, "type"); typ {
	case "thread.started":
		return []string{"● session started"}
	case "error":
		return []string{errorMark + str(ev, "message")}
	case "turn.failed":
		return []string{endMark + "turn failed: " + str(obj(ev, "error"), "message")}
	case "turn.completed":
		return []string{endMark + "turn completed"}
	case "item.started":
		if str(item, "type") == "command_execution" {
			return []string{toolMark + "$ " + str(item, "command")}
		}
	case "item.completed":
		switch str(item, "type") {
		case "agent_message":
			return splitText(str(item, "text"))
		case "command_execution":
			line := fmt.Sprintf("%sexit %s", resultMark, string(item["exit_code"]))
			if out := firstLine(str(item, "aggregated_output")); out != "" {
				line += ": " + out
			}
			return []string{line}
		case "file_change":
			var changes []struct{ Path, Kind string }
			_ = json.Unmarshal(item["changes"], &changes)
			var out []string
			for _, c := range changes {
				out = append(out, toolMark+c.Kind+" "+c.Path)
			}
			return out
		case "error":
			return []string{errorMark + str(item, "message")}
		}
	}
	return nil
}

func opencodeLines(ev map[string]json.RawMessage) []string {
	part := obj(ev, "part")
	switch str(ev, "type") {
	case "text":
		return splitText(str(part, "text"))
	case "tool_use":
		state := obj(part, "state")
		out := []string{toolMark + str(part, "tool") + " " + brief(state["input"])}
		if str(state, "status") == "error" {
			return append(out, resultMark+"error: "+firstLine(str(state, "error")))
		}
		return append(out, resultMark+firstLine(str(state, "output")))
	case "error":
		e := obj(ev, "error")
		msg := str(obj(e, "data"), "message")
		if msg == "" {
			msg = str(e, "name")
		}
		return []string{errorMark + msg}
	}
	return nil
}

func agyLines(ev map[string]json.RawMessage) []string {
	switch str(ev, "event") {
	case "init":
		return []string{"● session started (" + str(obj(ev, "init"), "model") + ")"}
	case "step_update":
		su := obj(ev, "step_update")
		if str(su, "step_type") != "tool" {
			return nil
		}
		info := obj(su, "tool_info")
		if str(su, "state") == "ACTIVE" {
			return []string{toolMark + str(su, "tool_name") + " " + brief(info["parameters"])}
		}
		if out := firstLine(str(info, "output")); out != "" {
			return []string{resultMark + out}
		}
		return nil
	case "result":
		r := obj(ev, "result")
		out := splitText(str(r, "response"))
		line := endMark + strings.ToLower(str(r, "status"))
		if e := str(r, "error"); e != "" {
			line += ": " + firstLine(e)
		}
		return append(out, line)
	}
	return nil
}

// brief shows a tool's input on one line: the value of a well-known field
// when there is one, else the compact JSON.
func brief(raw json.RawMessage) string {
	var in map[string]json.RawMessage
	if json.Unmarshal(raw, &in) == nil {
		for _, k := range []string{"command", "file_path", "filePath", "path", "AbsolutePath", "TargetFile", "pattern", "url", "description"} {
			if v := str(in, k); v != "" {
				return firstLine(v)
			}
		}
	}
	return firstLine(string(raw))
}

// firstLine returns the first non-empty line, shortened, with a count of
// the lines left out.
func firstLine(s string) string {
	lines := strings.Split(strings.TrimSpace(s), "\n")
	first := clipRunes(strings.TrimSpace(lines[0]), 160)
	if len(lines) > 1 {
		first += fmt.Sprintf(" (+%d lines)", len(lines)-1)
	}
	return first
}

func splitText(s string) []string {
	s = strings.TrimSpace(s)
	if s == "" {
		return nil
	}
	return strings.Split(s, "\n")
}

// clipRunes shortens s to n runes without splitting a character.
func clipRunes(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "…"
}
