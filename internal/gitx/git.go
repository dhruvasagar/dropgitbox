// Package gitx is a thin wrapper around the git command line.
//
// dropgitbox shells out to the real git binary instead of using a pure-Go
// implementation so that repositories behave exactly like ones the user
// manages by hand, and so that merges, packing and the wire protocol get
// git's own, battle-tested behaviour.
package gitx

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
)

// Error is returned when a git command exits unsuccessfully.
type Error struct {
	Args     []string
	ExitCode int
	Stderr   string
}

func (e *Error) Error() string {
	msg := strings.TrimSpace(e.Stderr)
	if msg == "" {
		msg = fmt.Sprintf("exit status %d", e.ExitCode)
	}
	return fmt.Sprintf("git %s: %s", strings.Join(e.Args, " "), msg)
}

// ExitCode returns the exit code of a failed git command, or -1 if err is
// not a git exit error.
func ExitCode(err error) int {
	var ge *Error
	if errors.As(err, &ge) {
		return ge.ExitCode
	}
	return -1
}

// Repo runs git commands against a single working tree (or bare repo).
type Repo struct {
	// Dir is the directory git runs in.
	Dir string
	// Env holds extra environment variables (KEY=VALUE) for every command.
	Env []string
}

// Run executes git with args and returns trimmed stdout.
func (r *Repo) Run(ctx context.Context, args ...string) (string, error) {
	out, err := r.RunRaw(ctx, args...)
	return strings.TrimRight(out, "\n"), err
}

// RunRaw executes git with args and returns stdout untouched.
func (r *Repo) RunRaw(ctx context.Context, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Dir = r.Dir
	cmd.Env = append(os.Environ(),
		"GIT_TERMINAL_PROMPT=0", // never block waiting for credentials
		"LC_ALL=C",              // stable, parseable messages
	)
	cmd.Env = append(cmd.Env, r.Env...)

	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	if err := cmd.Run(); err != nil {
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			return stdout.String(), &Error{Args: args, ExitCode: ee.ExitCode(), Stderr: stderr.String()}
		}
		return stdout.String(), fmt.Errorf("git %s: %w", strings.Join(args, " "), err)
	}
	return stdout.String(), nil
}

// RevParse resolves rev to a commit id. It returns "" (and no error) when
// the revision does not exist, e.g. HEAD on a branch with no commits yet.
func (r *Repo) RevParse(ctx context.Context, rev string) (string, error) {
	out, err := r.Run(ctx, "rev-parse", "-q", "--verify", rev+"^{commit}")
	if err != nil {
		if ExitCode(err) == 1 {
			return "", nil
		}
		return "", err
	}
	return out, nil
}

// IsAncestor reports whether commit a is an ancestor of (or equal to) b.
func (r *Repo) IsAncestor(ctx context.Context, a, b string) (bool, error) {
	_, err := r.Run(ctx, "merge-base", "--is-ancestor", a, b)
	if err == nil {
		return true, nil
	}
	if ExitCode(err) == 1 {
		return false, nil
	}
	return false, err
}

// Config reads a single config value, returning "" when it is unset.
func (r *Repo) Config(ctx context.Context, key string) (string, error) {
	out, err := r.Run(ctx, "config", "--get", key)
	if err != nil {
		if ExitCode(err) == 1 {
			return "", nil
		}
		return "", err
	}
	return out, nil
}

// SetConfig writes a local config value.
func (r *Repo) SetConfig(ctx context.Context, key, value string) error {
	_, err := r.Run(ctx, "config", "--local", key, value)
	return err
}

// Lines splits command output into non-empty lines.
func Lines(s string) []string {
	var out []string
	for _, l := range strings.Split(s, "\n") {
		if l = strings.TrimRight(l, "\r"); l != "" {
			out = append(out, l)
		}
	}
	return out
}
