package provider

import (
	"bufio"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func transcriptOf(t *testing.T, cli, fixture string) []string {
	t.Helper()
	f, err := os.Open(filepath.Join(fixtures, cli, fixture, "stdout"))
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	sc.Buffer(nil, 4<<20)
	var out []string
	for sc.Scan() {
		out = append(out, Transcript(cli, sc.Bytes())...)
	}
	if err := sc.Err(); err != nil {
		t.Fatal(err)
	}
	return out
}

func TestTranscript(t *testing.T) {
	cases := []struct {
		cli, fixture string
		want         []string
	}{
		{"claude", "success", []string{
			"● session started (claude-sonnet-5-5)",
			"▸ Read /tmp/orch-scratch/main.go",
			"  ↳ 1\tpackage main (+7 lines)",
			"▸ Edit /tmp/orch-scratch/main.go",
			"I added `sub(a, b int) int`",
			"■ success",
		}},
		{"claude", "max_turns", []string{"■ error_max_turns"}},
		{"codex", "workspace_write", []string{
			"● session started",
			"▸ $ /bin/bash -lc 'cat main.go'",
			"  ↳ exit 0: package main (+6 lines)",
			"▸ update /tmp/orch-scratch/main.go",
			"■ turn completed",
		}},
		{"codex", "auth_fail", []string{"! Reconnecting... 2/5", "■ turn failed: unexpected status 401"}},
		{"opencode", "success", []string{"▸ bash go run main.go", "  ↳ 5 (+1 lines)", "Done — `main.go` now defines `sub`"}},
		{"opencode", "auth_fail", []string{"! Invalid API key."}},
		{"agy", "success", []string{"● session started (gemini-3.1-pro-high)", "▸ view_file /tmp/orch-scratch/main.go", "  ↳ 8 lines, 108 bytes", "■ success"}},
		{"agy", "bad_model", []string{"■ error: invalid model selection"}},
	}
	for _, c := range cases {
		got := transcriptOf(t, c.cli, c.fixture)
		// Each wanted line is the prefix of a rendered line, in order.
		i := 0
		for _, line := range got {
			if i < len(c.want) && strings.HasPrefix(line, c.want[i]) {
				i++
			}
		}
		if i < len(c.want) {
			t.Errorf("%s/%s: missing %q in\n%s", c.cli, c.fixture, c.want[i], strings.Join(got, "\n"))
		}
		for _, line := range got {
			if strings.TrimSpace(line) == "" || strings.TrimSpace(line) == "↳" {
				t.Errorf("%s/%s: empty line rendered", c.cli, c.fixture)
			}
		}
	}
}

func TestTranscriptPlainAndShort(t *testing.T) {
	if got := Transcript("claude", []byte("not json\n")); len(got) != 1 || got[0] != "not json" {
		t.Errorf("plain line: %q", got)
	}
	if got := Transcript("claude", []byte("  \n")); got != nil {
		t.Errorf("blank line: %q", got)
	}
	if got := Transcript("codex", []byte(`{"type":"turn.started"}`)); got != nil {
		t.Errorf("bookkeeping event rendered: %q", got)
	}
	long := strings.Repeat("é", 300)
	if got := firstLine(long); len([]rune(got)) != 161 {
		t.Errorf("clip: %d runes", len([]rune(got)))
	}
}
