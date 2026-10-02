// Package runner runs agent CLIs, one throwaway Docker container per run.
//
// It owns everything around a run that adapters do not: the workspace
// clone, the container and its limits, the role's GitHub token and the
// provider credential, the timeout, and the run's log directory. Secrets
// reach the container as environment variables passed by name, so they never
// appear in a command line.
package runner
