package panel

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/deniz-ai-orchestration/ai-orchestrator/internal/provider"
	"github.com/deniz-ai-orchestration/ai-orchestrator/internal/store"
)

// maxLine caps one line of CLI output kept in memory while following a run;
// longer lines (a huge tool result) are skipped.
const maxLine = 8 << 20

// stderrLines is how much of a finished run's stderr the live view shows.
const stderrLines = 20

// stream sends a run's transcript as server-sent events: everything so far,
// then new lines as the run writes them, then an "end" event when it has
// finished.
func (s *Server) stream(w http.ResponseWriter, r *http.Request) {
	run, err := s.run(r)
	if err != nil {
		s.notFound(w, r, err)
		return
	}
	fl, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}
	h := w.Header()
	h.Set("Content-Type", "text/event-stream")
	h.Set("Cache-Control", "no-store")
	h.Set("X-Accel-Buffering", "no")
	send := func(event, data string) {
		if event != "" {
			fmt.Fprintf(w, "event: %s\n", event)
		}
		fmt.Fprintf(w, "data: %s\n\n", data)
	}
	err = s.follow(r.Context(), run, func(line string) { send("", clean(line)) }, fl.Flush)
	if err != nil {
		if r.Context().Err() == nil {
			s.Log.Warn("run stream ended early", "run", run.ID, "err", err)
			send("end", "the stream failed: "+clean(err.Error()))
			fl.Flush()
		}
		return
	}
	run, err = s.Store.GetRun(context.WithoutCancel(r.Context()), run.ID)
	if err != nil {
		return
	}
	end := run.Status
	if run.Outcome != "" {
		end += " (" + run.Outcome + ")"
	}
	send("end", clean(end))
	fl.Flush()
}

// follow emits each transcript line of the run's stdout until the run has
// finished and its output is read to the end, then the tail of its stderr.
func (s *Server) follow(ctx context.Context, run store.Run, emit func(string), flush func()) error {
	every := s.Follow
	if every <= 0 {
		every = 500 * time.Millisecond
	}
	cli := s.cli(run.Provider)
	var f *os.File
	defer func() {
		if f != nil {
			f.Close()
		}
	}()
	var pending []byte
	skipping := false
	buf := make([]byte, 64<<10)
	for {
		// Read the status before the output, so output written before the
		// run finished is never missed.
		r, err := s.Store.GetRun(ctx, run.ID)
		if err != nil {
			return err
		}
		if f == nil && r.LogDir != "" {
			f, err = os.Open(filepath.Join(r.LogDir, "stdout.jsonl"))
			if err != nil && !errors.Is(err, os.ErrNotExist) {
				return err
			}
			if err != nil {
				f = nil
			}
		}
		finished := r.Status != store.RunRunning
		wrote := false
		for f != nil {
			n, err := f.Read(buf)
			if n > 0 {
				pending = append(pending, buf[:n]...)
				for {
					i := bytes.IndexByte(pending, '\n')
					if i < 0 {
						break
					}
					if !skipping {
						for _, l := range provider.Transcript(cli, pending[:i]) {
							emit(l)
							wrote = true
						}
					}
					skipping = false
					pending = pending[i+1:]
				}
				if len(pending) > maxLine {
					pending, skipping = nil, true
				}
			}
			if err == io.EOF {
				break
			}
			if err != nil {
				return err
			}
		}
		if finished {
			if !skipping && len(pending) > 0 {
				for _, l := range provider.Transcript(cli, pending) {
					emit(l)
				}
			}
			if r.LogDir != "" {
				for _, l := range lastLines(filepath.Join(r.LogDir, "stderr.log"), stderrLines) {
					emit("stderr: " + l)
				}
			}
			flush()
			return nil
		}
		if wrote {
			flush()
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(every):
		}
	}
}

// lastLines returns the last n non-empty lines of a small file.
func lastLines(path string, n int) []string {
	f, err := os.Open(path)
	if err != nil {
		return nil
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		return nil
	}
	off := max(fi.Size()-tailBytes, 0)
	b, err := io.ReadAll(io.NewSectionReader(f, off, fi.Size()-off))
	if err != nil {
		return nil
	}
	var out []string
	for _, l := range strings.Split(string(b), "\n") {
		if strings.TrimSpace(l) != "" {
			out = append(out, l)
		}
	}
	if len(out) > n {
		out = out[len(out)-n:]
	}
	return out
}
