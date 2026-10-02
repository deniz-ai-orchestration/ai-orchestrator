package runner

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"slices"
	"strings"
	"testing"
	"time"
)

func TestSpecArgsKeepSecretsOut(t *testing.T) {
	s := Spec{Name: "orch-run-7", Image: "img", User: "1000:1000", Work: "/ws", IO: "/io",
		Volumes: map[string]string{"orch-codex-home": "/home/agent/.codex"},
		Env:     map[string]string{"HOME": "/home/agent"},
		Secrets: map[string]string{"GH_TOKEN": "ghp_secret", "CLAUDE_CODE_OAUTH_TOKEN": "sk-secret"},
		CPUs:    "2", Memory: "4g", Pids: 512, Argv: []string{"claude", "-p"}}
	a := s.Args()
	joined := strings.Join(a, " ")
	if strings.Contains(joined, "secret") {
		t.Fatalf("secret value in argv: %s", joined)
	}
	for _, want := range []string{"--rm", "--name orch-run-7", "--user 1000:1000", "--cap-drop ALL",
		"--security-opt no-new-privileges", "--cpus 2", "--memory 4g", "--pids-limit 512",
		"-v /ws:/work -w /work", "-v /io:/orch", "-v orch-codex-home:/home/agent/.codex",
		"-e HOME=/home/agent", "-e CLAUDE_CODE_OAUTH_TOKEN -e GH_TOKEN", "img claude -p"} {
		if !strings.Contains(joined, want) {
			t.Errorf("args missing %q: %s", want, joined)
		}
	}
	if a[len(a)-1] != "-p" || slices.Index(a, "img") != len(a)-3 {
		t.Errorf("image and command must come last: %v", a)
	}
}

// TestDockerRun runs real containers. It needs a Docker daemon and pulls
// busybox, so it only runs with ORCH_DOCKER_TEST=1.
func TestDockerRun(t *testing.T) {
	if os.Getenv("ORCH_DOCKER_TEST") != "1" {
		t.Skip("set ORCH_DOCKER_TEST=1 to run against Docker")
	}
	if err := exec.Command("docker", "pull", "-q", "busybox:1.36").Run(); err != nil {
		t.Fatalf("pull busybox: %v", err)
	}
	d := Docker{}
	ctx := context.Background()
	var out, errb bytes.Buffer
	res, err := d.Run(ctx, Spec{Name: Prefix + "test-ok", Image: "busybox:1.36", User: "1000:1000",
		Secrets: map[string]string{"SECRET_X": "s3cret"}, Env: map[string]string{"PLAIN": "p"},
		Argv: []string{"sh", "-c", `read line; echo "$line/$PLAIN/$SECRET_X/$(id -u)"; exit 3`}, Stdin: "hello\n"}, &out, &errb)
	if err != nil || res.ExitCode != 3 || res.Killed {
		t.Fatalf("res %+v err %v stderr %s", res, err, errb.String())
	}
	if got := strings.TrimSpace(out.String()); got != "hello/p/s3cret/1000" {
		t.Fatalf("stdout %q", got)
	}

	tctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	start := time.Now()
	res, err = d.Run(tctx, Spec{Name: Prefix + "test-kill", Image: "busybox:1.36", Argv: []string{"sleep", "60"}}, &out, &errb)
	if err != nil || !res.Killed || time.Since(start) > 20*time.Second {
		t.Fatalf("timeout: res %+v err %v after %s", res, err, time.Since(start))
	}

	if err := exec.Command("docker", "create", "--name", Prefix+"test-stale", "busybox:1.36", "true").Run(); err != nil {
		t.Fatal(err)
	}
	if err := d.RemoveStale(ctx); err != nil {
		t.Fatal(err)
	}
	if out, _ := exec.Command("docker", "ps", "-aq", "--filter", "name=^"+Prefix).Output(); len(bytes.TrimSpace(out)) != 0 {
		t.Fatalf("stale containers left: %s", out)
	}
}
