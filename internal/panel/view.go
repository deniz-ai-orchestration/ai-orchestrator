package panel

import (
	"context"
	"errors"
	"fmt"
	"html/template"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/deniz-ai-orchestration/ai-orchestrator/internal/engine"
	"github.com/deniz-ai-orchestration/ai-orchestrator/internal/provider"
	"github.com/deniz-ai-orchestration/ai-orchestrator/internal/router"
	"github.com/deniz-ai-orchestration/ai-orchestrator/internal/runner"
	"github.com/deniz-ai-orchestration/ai-orchestrator/internal/store"
	"github.com/deniz-ai-orchestration/ai-orchestrator/internal/telegram"
)

// View is everything the page shows. Only the requested sections are filled.
type View struct {
	Projects []ProjectRow
	Paused   bool
	Agents   []Card
	Tasks    []TaskRow
	Roles    []RoleRow
	Models   []ModelRow
	// Providers and Choices fill each role's provider and model selectors:
	// providers that can run an agent, and their models.
	Providers []string
	Choices   []ModelChoice
	// Chats are the summoned agents; Summon fills the summon form.
	Chats  []AgentCard
	Summon *SummonForm
}

// Card is one agent run.
type Card struct {
	store.Run
	Task    store.Task
	Running bool
	Elapsed string
	Preview []string
}

// TaskRow is one active task.
type TaskRow struct {
	store.Task
	Detail string
	Issue  string
	PR     string
}

// RoleRow is one configured role.
type RoleRow struct {
	Name    string
	Enabled bool
	Pool    []string
	Pin     string // model chosen for the role; "" means auto
	PinProv string // provider of Pin
	Next    string // the model the next run would get, or why none can run
	NextOK  bool
}

// ModelChoice is one model in a role's model selector.
type ModelChoice struct{ Name, Provider, ModelID string }

// ModelRow is one model's quota state.
type ModelRow struct {
	Name, Provider, ModelID, State, Runs, PinnedFor, Last string
	Key                                                   string // first word of State, for styling
	On                                                    bool
}

// previewLines is how many transcript lines an agent card shows.
const previewLines = 4

var active = []engine.State{engine.Queued, engine.Developing, engine.AwaitingCI, engine.ReviewQueued,
	engine.Reviewing, engine.ReadyForHuman, engine.NeedsHuman}

func (s *Server) view(ctx context.Context, parts ...string) (View, error) {
	var v View
	var err error
	if v.Paused, err = s.Store.Paused(ctx); err != nil {
		return v, err
	}
	for _, p := range parts {
		switch p {
		case "projects":
			err = s.projects(ctx, &v)
		case "agents":
			err = s.agents(ctx, &v)
		case "tasks":
			err = s.tasks(ctx, &v)
		case "roles":
			err = s.roles(ctx, &v)
		case "quota":
			err = s.models(ctx, &v)
		case "chats":
			err = s.chatCards(ctx, &v)
		case "summon":
			err = s.summonForm(ctx, &v)
		}
		if err != nil {
			return v, fmt.Errorf("%s: %w", p, err)
		}
	}
	return v, nil
}

func (s *Server) agents(ctx context.Context, v *View) error {
	runs, err := s.Store.RecentRuns(ctx, 12)
	if err != nil {
		return err
	}
	for _, r := range runs {
		c, err := s.card(ctx, r, true)
		if err != nil {
			return err
		}
		v.Agents = append(v.Agents, c)
	}
	return nil
}

func (s *Server) card(ctx context.Context, r store.Run, preview bool) (Card, error) {
	c := Card{Run: r, Running: r.Status == store.RunRunning}
	if r.TaskID != 0 {
		t, err := s.Store.GetTask(ctx, r.TaskID)
		if err != nil && !errors.Is(err, store.ErrNotFound) {
			return c, err
		}
		c.Task = t
	}
	end := r.EndedAt
	if c.Running || end.IsZero() {
		end = s.now()
	}
	if !r.StartedAt.IsZero() {
		c.Elapsed = duration(end.Sub(r.StartedAt))
	}
	if preview && r.LogDir != "" {
		c.Preview = tailTranscript(filepath.Join(r.LogDir, "stdout.jsonl"), s.cli(r.Provider), previewLines)
	}
	return c, nil
}

func (s *Server) cli(providerName string) string {
	return s.Cfg.Providers[providerName].CLI
}

func (s *Server) tasks(ctx context.Context, v *View) error {
	ts, err := s.Store.ListTasks(ctx, active...)
	if err != nil {
		return err
	}
	for _, t := range ts {
		row := TaskRow{Task: t, Issue: fmt.Sprintf("https://github.com/%s/issues/%d", t.Repo, t.IssueNumber)}
		if t.PRNumber > 0 {
			row.PR = fmt.Sprintf("https://github.com/%s/pull/%d", t.Repo, t.PRNumber)
		}
		switch t.State {
		case engine.Queued, engine.Developing:
			row.Detail = strings.TrimSpace(string(t.Work) + " " + t.DevModel)
		case engine.Reviewing:
			row.Detail = t.ReviewModel
		case engine.NeedsHuman:
			row.Detail = telegram.ReasonText(t.Reason)
		}
		v.Tasks = append(v.Tasks, row)
	}
	return nil
}

func (s *Server) roles(ctx context.Context, v *View) error {
	pins, err := s.Store.RolePins(ctx)
	if err != nil {
		return err
	}
	skip, err := s.Quota.Skip(ctx)
	if err != nil {
		return err
	}
	for name, r := range s.Cfg.Roles {
		row := RoleRow{Name: name, Enabled: r.IsEnabled(), Pool: r.Pool, Pin: pins[name]}
		switch {
		case !row.Enabled:
		case name == "helper":
			// The helper calls Ollama directly, outside the router.
			row.Next = "no model in the pool can run"
			for _, m := range r.Pool {
				if skip(m) == "" {
					row.Next, row.NextOK = m, true
					break
				}
			}
		default:
			c, err := router.Pick(s.Cfg, name, router.Options{Usable: runner.ContainerReady, Pin: pins[name], Skip: skip})
			if err != nil {
				row.Next = err.Error()
			} else {
				row.Next, row.NextOK = c.Name, true
			}
		}
		v.Roles = append(v.Roles, row)
	}
	sort.Slice(v.Roles, func(i, j int) bool { return roleOrder(v.Roles[i].Name) < roleOrder(v.Roles[j].Name) })
	seen := map[string]bool{}
	for name, m := range s.Cfg.Models {
		p := s.Cfg.Providers[m.Provider]
		if m.Disabled || p.Disabled || p.CLI == "" {
			continue // cannot run an agent
		}
		v.Choices = append(v.Choices, ModelChoice{Name: name, Provider: m.Provider, ModelID: m.Model})
		if !seen[m.Provider] {
			seen[m.Provider] = true
			v.Providers = append(v.Providers, m.Provider)
		}
	}
	sortChoices(v.Providers, v.Choices)
	for i := range v.Roles {
		if m, ok := s.Cfg.Models[v.Roles[i].Pin]; ok {
			v.Roles[i].PinProv = m.Provider
		}
	}
	return nil
}

func sortChoices(providers []string, choices []ModelChoice) {
	sort.Strings(providers)
	sort.Slice(choices, func(i, j int) bool { return choices[i].Name < choices[j].Name })
}

// roleOrder lists the workflow's roles first, in workflow order, then the
// rest by name.
func roleOrder(name string) string {
	for i, r := range []string{"developer", "reviewer", "functional_tester", "adversarial_tester", "helper"} {
		if r == name {
			return fmt.Sprintf("%d", i)
		}
	}
	return "9" + name
}

func (s *Server) models(ctx context.Context, v *View) error {
	rep, err := s.Quota.Report(ctx)
	if err != nil {
		return err
	}
	now := s.now()
	for _, st := range rep {
		key := st.State(now)
		state := key
		key = strings.Fields(key)[0]
		if state == "cooling" {
			state += " until " + st.CoolingUntil.Local().Format("Jan 2 15:04")
		}
		runs := fmt.Sprint(st.RunsIn5h)
		if st.Budget > 0 {
			runs += fmt.Sprintf(" / %d", st.Budget)
		}
		last := ""
		if st.Last != nil {
			last = st.Last.Kind + " " + st.Last.At.Local().Format("Jan 2 15:04")
		}
		v.Models = append(v.Models, ModelRow{Name: st.Name, Key: key, Provider: st.Provider, ModelID: st.ModelID, State: state,
			Runs: runs, PinnedFor: strings.Join(st.PinnedFor, ", "), Last: last,
			On: !st.SwitchedOff && !st.ConfigDisabled})
	}
	return nil
}

// tailBytes is how much of a run's output a card reads to find its last
// lines.
const tailBytes = 64 << 10

// tailTranscript renders the last n transcript lines of a run's output.
func tailTranscript(path, cli string, n int) []string {
	f, err := os.Open(path)
	if err != nil {
		return nil
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		return nil
	}
	off := fi.Size() - tailBytes
	if off < 0 {
		off = 0
	}
	buf, err := io.ReadAll(io.NewSectionReader(f, off, fi.Size()-off))
	if err != nil {
		return nil
	}
	lines := strings.Split(string(buf), "\n")
	if off > 0 {
		lines = lines[1:] // starts mid-line
	}
	var out []string
	for _, l := range lines {
		out = append(out, provider.Transcript(cli, []byte(l))...)
	}
	if len(out) > n {
		out = out[len(out)-n:]
	}
	for i := range out {
		out[i] = clean(out[i])
	}
	return out
}

// clean drops control characters, so agent output cannot drive the
// terminal with escape sequences.
func clean(s string) string {
	return strings.Map(func(r rune) rune {
		if r == '\t' {
			return ' '
		}
		if r < 0x20 || r == 0x7f || (r >= 0x80 && r < 0xa0) {
			return -1
		}
		return r
	}, s)
}

func duration(d time.Duration) string {
	d = d.Round(time.Second)
	switch {
	case d < time.Minute:
		return d.String()
	case d < time.Hour:
		return fmt.Sprintf("%dm%02ds", int(d.Minutes()), int(d.Seconds())%60)
	}
	return fmt.Sprintf("%dh%02dm", int(d.Hours()), int(d.Minutes())%60)
}

var funcs = template.FuncMap{
	"human": func(s any) string { return strings.ReplaceAll(fmt.Sprint(s), "_", " ") },
	"lineClass": func(l string) string {
		switch {
		case strings.HasPrefix(l, "▸"):
			return "tool"
		case strings.HasPrefix(l, "  ↳"):
			return "result"
		case strings.HasPrefix(l, "!"):
			return "error"
		case strings.HasPrefix(l, "■"), strings.HasPrefix(l, "●"):
			return "mark"
		}
		return ""
	},
}
