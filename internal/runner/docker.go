package runner

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"sort"
	"strings"
	"time"
)

// Paths inside the agent container.
const (
	WorkDir = "/work"       // the workspace clone
	IODir   = "/orch"       // schema in, result out
	HomeDir = "/home/agent" // HOME for every CLI
	Prefix  = "orch-run-"   // container name prefix; stale ones are removed at start-up
	killAge = 10 * time.Second
)

// Spec is one container run.
type Spec struct {
	Name    string
	Image   string
	User    string            // uid:gid
	Work    string            // host workspace, mounted at WorkDir
	IO      string            // host run io dir, mounted at IODir
	Volumes map[string]string // named volume -> path in the container
	Env     map[string]string // non-secret environment
	Secrets map[string]string // env var -> value, passed by name only
	CPUs    string
	Memory  string
	Pids    int
	Argv    []string
	Stdin   string
}

// Args returns the docker run arguments. Secret values are not in them:
// "-e NAME" makes docker copy NAME from the docker client's environment.
func (s Spec) Args() []string {
	a := []string{"run", "--rm", "-i", "--name", s.Name, "--init",
		"--cap-drop", "ALL", "--security-opt", "no-new-privileges"}
	if s.User != "" {
		a = append(a, "--user", s.User)
	}
	if s.CPUs != "" {
		a = append(a, "--cpus", s.CPUs)
	}
	if s.Memory != "" {
		a = append(a, "--memory", s.Memory)
	}
	if s.Pids > 0 {
		a = append(a, "--pids-limit", fmt.Sprint(s.Pids))
	}
	if s.Work != "" {
		a = append(a, "-v", s.Work+":"+WorkDir, "-w", WorkDir)
	}
	if s.IO != "" {
		a = append(a, "-v", s.IO+":"+IODir)
	}
	for _, v := range sortedKeys(s.Volumes) {
		a = append(a, "-v", v+":"+s.Volumes[v])
	}
	for _, k := range sortedKeys(s.Env) {
		a = append(a, "-e", k+"="+s.Env[k])
	}
	for _, k := range sortedKeys(s.Secrets) {
		a = append(a, "-e", k)
	}
	a = append(a, s.Image)
	return append(a, s.Argv...)
}

// Result is how a container run ended.
type Result struct {
	ExitCode int
	Killed   bool // stopped because ctx ended (the role's timeout)
}

// Containers starts and stops agent containers.
type Containers interface {
	Run(ctx context.Context, s Spec, stdout, stderr io.Writer) (Result, error)
	RemoveStale(ctx context.Context) error
}

// Docker runs containers with the docker CLI.
type Docker struct {
	Bin string // default "docker"
}

func (d Docker) bin() string {
	if d.Bin == "" {
		return "docker"
	}
	return d.Bin
}

// Run starts the container and waits for it. When ctx ends first the
// container is killed and Result.Killed is set.
func (d Docker) Run(ctx context.Context, s Spec, stdout, stderr io.Writer) (Result, error) {
	cmd := exec.Command(d.bin(), s.Args()...)
	cmd.Stdin = strings.NewReader(s.Stdin)
	cmd.Stdout, cmd.Stderr = stdout, stderr
	cmd.Env = os.Environ()
	for k, v := range s.Secrets {
		cmd.Env = append(cmd.Env, k+"="+v)
	}
	if err := cmd.Start(); err != nil {
		return Result{}, fmt.Errorf("docker run: %w", err)
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()

	var res Result
	var err error
	select {
	case err = <-done:
	case <-ctx.Done():
		res.Killed = true
		kctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), killAge)
		_ = exec.CommandContext(kctx, d.bin(), "kill", s.Name).Run()
		cancel()
		select {
		case err = <-done:
		case <-time.After(killAge):
			_ = cmd.Process.Kill()
			err = <-done
		}
	}
	var exit *exec.ExitError
	switch {
	case err == nil:
	case errors.As(err, &exit):
		res.ExitCode = exit.ExitCode()
	default:
		return res, err
	}
	return res, nil
}

// RemoveStale force-removes containers left over from a previous orch
// process. orch runs one process per host, so every orch-run-* container
// at start-up is stale.
func (d Docker) RemoveStale(ctx context.Context) error {
	out, err := exec.CommandContext(ctx, d.bin(), "ps", "-aq", "--filter", "name=^"+Prefix).Output()
	if err != nil {
		return fmt.Errorf("docker ps: %w", err)
	}
	ids := strings.Fields(string(out))
	if len(ids) == 0 {
		return nil
	}
	if out, err := exec.CommandContext(ctx, d.bin(), append([]string{"rm", "-f"}, ids...)...).CombinedOutput(); err != nil {
		return fmt.Errorf("docker rm: %v: %s", err, out)
	}
	return nil
}

func sortedKeys(m map[string]string) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
