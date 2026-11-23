package client

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/dhruvasagar/dropgitbox/internal/gitx"
)

// ErrConflict is returned while the folder has an unresolved merge
// conflict. Syncing (including auto-commits) is paused until the user
// resolves it, so conflict markers never get committed by accident.
var ErrConflict = errors.New("unresolved merge conflict; syncing is paused until it is resolved")

// ErrUnauthorized is returned when the server rejects this device's token.
var ErrUnauthorized = errors.New("the server rejected this device's token (was the device revoked? get a new invite with `dropgitbox device add` and run `dropgitbox join -force`)")

// network wraps errors from git commands that talk to the server, turning
// authentication failures into ErrUnauthorized.
func network(err error) error {
	var ge *gitx.Error
	if errors.As(err, &ge) {
		msg := ge.Stderr
		if strings.Contains(msg, "could not read Username") || strings.Contains(msg, "Authentication failed") ||
			strings.Contains(msg, "returned error: 401") {
			return ErrUnauthorized
		}
	}
	return err
}

// maxPushAttempts bounds the fetch/merge/push loop when other devices keep
// pushing in between our fetch and our push.
const maxPushAttempts = 5

// Syncer synchronises one folder with its server repository.
type Syncer struct {
	cfg    Config
	git    *gitx.Repo
	gitDir string
	log    *slog.Logger

	mu sync.Mutex // serialises git operations from this process
}

// Config returns the folder's configuration.
func (s *Syncer) Config() Config { return s.cfg }

// Dir returns the folder's root directory.
func (s *Syncer) Dir() string { return s.cfg.Dir }

// Git exposes the underlying repository (with server credentials set).
func (s *Syncer) Git() *gitx.Repo { return s.git }

// Head returns the current local commit, or "" if there is none yet.
func (s *Syncer) Head(ctx context.Context) (string, error) {
	return s.git.RevParse(ctx, "HEAD")
}

// Conflicted reports whether a merge is in progress (i.e. paused on a
// conflict).
func (s *Syncer) Conflicted() bool {
	_, err := os.Stat(filepath.Join(s.gitDir, "MERGE_HEAD"))
	return err == nil
}

// ConflictedFiles lists the paths that still have unmerged changes.
func (s *Syncer) ConflictedFiles(ctx context.Context) ([]string, error) {
	out, err := s.git.Run(ctx, "diff", "--name-only", "--diff-filter=U")
	if err != nil {
		return nil, err
	}
	return gitx.Lines(out), nil
}

// Sync commits all local changes, merges in changes from the server and
// pushes the result. It returns ErrConflict if the merge needs the user.
func (s *Syncer) Sync(ctx context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.Conflicted() {
		return ErrConflict
	}
	if _, err := s.commitLocal(ctx); err != nil {
		return err
	}

	for range maxPushAttempts {
		if _, err := s.git.Run(ctx, "fetch", "--quiet", "--prune", "origin"); err != nil {
			return network(err)
		}
		remote, err := s.git.RevParse(ctx, "refs/remotes/origin/"+Branch)
		if err != nil {
			return err
		}
		local, err := s.git.RevParse(ctx, "HEAD")
		if err != nil {
			return err
		}

		if remote != "" && remote != local {
			upToDate := false
			if local != "" {
				if upToDate, err = s.git.IsAncestor(ctx, remote, local); err != nil {
					return err
				}
			}
			if !upToDate {
				if err := s.merge(ctx); err != nil {
					return err
				}
				if local, err = s.git.RevParse(ctx, "HEAD"); err != nil {
					return err
				}
			}
		}

		if local == "" || local == remote {
			return nil // nothing to push
		}
		if _, err := s.git.Run(ctx, "push", "--quiet", "origin", "HEAD:refs/heads/"+Branch); err != nil {
			if isPushRace(err) {
				s.log.Debug("push raced with another device, retrying")
				continue
			}
			return network(err)
		}
		s.log.Info("pushed", "head", short(local))
		return nil
	}
	return fmt.Errorf("gave up after %d attempts: the server keeps changing underneath us", maxPushAttempts)
}

// CommitLocal snapshots the working tree if anything changed, returning the
// new commit id ("" when there was nothing to commit). It is a no-op while
// a conflict is pending.
func (s *Syncer) CommitLocal(ctx context.Context) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.Conflicted() {
		return "", ErrConflict
	}
	return s.commitLocal(ctx)
}

func (s *Syncer) commitLocal(ctx context.Context) (string, error) {
	if _, err := s.git.Run(ctx, "add", "--all"); err != nil {
		return "", err
	}
	out, err := s.git.Run(ctx, "diff", "--cached", "--name-status", "--no-renames")
	if err != nil {
		return "", err
	}
	changes := gitx.Lines(out)
	if len(changes) == 0 {
		return "", nil
	}

	subject, body := commitMessage(s.cfg.Device, changes, time.Now())
	if _, err := s.git.Run(ctx, "commit", "--quiet", "--no-verify", "-m", subject, "-m", body); err != nil {
		return "", err
	}
	head, err := s.git.RevParse(ctx, "HEAD")
	if err != nil {
		return "", err
	}
	s.log.Info("committed", "commit", short(head), "files", len(changes))
	return head, nil
}

func (s *Syncer) merge(ctx context.Context) error {
	msg := fmt.Sprintf("dropgitbox: merge changes from server into %s", s.cfg.Device)
	_, err := s.git.Run(ctx, "merge", "--quiet", "--no-edit", "--allow-unrelated-histories",
		"-m", msg, "refs/remotes/origin/"+Branch)
	if err == nil {
		head, _ := s.git.RevParse(ctx, "HEAD")
		s.log.Info("pulled changes from server", "head", short(head))
		return nil
	}
	if s.Conflicted() {
		files, _ := s.ConflictedFiles(ctx)
		s.log.Warn("merge conflict; syncing paused", "files", files)
		return ErrConflict
	}
	return err
}

func isPushRace(err error) bool {
	var ge *gitx.Error
	if !errors.As(err, &ge) {
		return false
	}
	msg := ge.Stderr
	return strings.Contains(msg, "[rejected]") || strings.Contains(msg, "fetch first") ||
		strings.Contains(msg, "non-fast-forward") || strings.Contains(msg, "failed to update ref") ||
		strings.Contains(msg, "cannot lock ref")
}

// commitMessage builds an auto-commit message summarising the change list
// (git diff --name-status lines).
func commitMessage(device string, changes []string, now time.Time) (subject, body string) {
	verbs := map[byte]string{'A': "added", 'M': "modified", 'D': "deleted", 'T': "modified"}
	counts := map[string]int{}
	for _, c := range changes {
		v := verbs[c[0]]
		if v == "" {
			v = "changed"
		}
		counts[v]++
	}

	if len(changes) == 1 {
		status, path, _ := strings.Cut(changes[0], "\t")
		v := verbs[status[0]]
		if v == "" {
			v = "changed"
		}
		subject = fmt.Sprintf("%s %s", capitalize(v), path)
	} else {
		var parts []string
		for _, v := range []string{"added", "modified", "deleted", "changed"} {
			if n := counts[v]; n > 0 {
				parts = append(parts, fmt.Sprintf("%d %s", n, v))
			}
		}
		subject = fmt.Sprintf("%d files: %s", len(changes), strings.Join(parts, ", "))
	}

	const maxListed = 100
	var b strings.Builder
	fmt.Fprintf(&b, "Auto-committed by dropgitbox on %s at %s.\n\n", device, now.Format(time.RFC3339))
	for i, c := range changes {
		if i == maxListed {
			fmt.Fprintf(&b, "... and %d more\n", len(changes)-maxListed)
			break
		}
		b.WriteString(c)
		b.WriteByte('\n')
	}
	return subject, strings.TrimRight(b.String(), "\n")
}

func capitalize(s string) string {
	if s == "" {
		return s
	}
	return strings.ToUpper(s[:1]) + s[1:]
}

func short(sha string) string {
	if len(sha) > 10 {
		return sha[:10]
	}
	return sha
}
