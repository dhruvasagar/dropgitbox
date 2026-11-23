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
	"github.com/dhruvasagar/dropgitbox/internal/server"
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

// Result describes what a sync did.
type Result struct {
	Committed []string // local paths committed before syncing
	Pulled    []string // paths changed by merging in the server's changes
	Pushed    bool     // local commits were pushed to the server
	Head      string   // commit the folder is at afterwards
}

// Sync commits all local changes, merges in changes from the server and
// pushes the result. It returns ErrConflict if the merge needs the user.
// The outcome is reported to the server (best effort), so the server's
// `dropgitbox device list` shows each device's sync state.
func (s *Syncer) Sync(ctx context.Context) error {
	_, err := s.SyncWithResult(ctx)
	return err
}

// SyncWithResult is Sync, also returning what changed.
func (s *Syncer) SyncWithResult(ctx context.Context) (*Result, error) {
	res, err := s.sync(ctx)
	s.report(ctx, err)
	return res, err
}

// report tells the server how the last sync went.
func (s *Syncer) report(ctx context.Context, syncErr error) {
	if errors.Is(syncErr, ErrUnauthorized) || ctx.Err() != nil {
		return
	}
	rep := server.StatusReport{Repo: s.cfg.Repo, State: server.StateOK}
	switch {
	case errors.Is(syncErr, ErrConflict):
		rep.State = server.StateConflict
		rep.Conflicts, _ = s.ConflictedFiles(ctx)
	case syncErr != nil:
		rep.State, rep.Message = server.StateError, syncErr.Error()
	}
	rep.Head, _ = s.Head(ctx)
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if err := apiCall(ctx, "POST", s.cfg.Server, "/api/v1/status", s.cfg.Token, rep, nil); err != nil {
		s.log.Debug("could not report status to server", "err", err)
	}
}

func (s *Syncer) sync(ctx context.Context) (*Result, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	res := &Result{}
	if s.Conflicted() {
		return res, ErrConflict
	}
	_, committed, err := s.commitLocal(ctx)
	if err != nil {
		return res, err
	}
	res.Committed = committed

	for range maxPushAttempts {
		if _, err := s.git.Run(ctx, "fetch", "--quiet", "--prune", "origin"); err != nil {
			return res, network(err)
		}
		remote, err := s.git.RevParse(ctx, "refs/remotes/origin/"+Branch)
		if err != nil {
			return res, err
		}
		local, err := s.git.RevParse(ctx, "HEAD")
		if err != nil {
			return res, err
		}

		if remote != "" && remote != local {
			upToDate := false
			if local != "" {
				if upToDate, err = s.git.IsAncestor(ctx, remote, local); err != nil {
					return res, err
				}
			}
			if !upToDate {
				before := local
				err := s.merge(ctx)
				if isOverwriteErr(err) {
					// A file was saved between our commit and the merge:
					// commit that too and merge again.
					_, more, cerr := s.commitLocal(ctx)
					if cerr != nil {
						return res, cerr
					}
					res.Committed = append(res.Committed, more...)
					if before, err = s.git.RevParse(ctx, "HEAD"); err != nil {
						return res, err
					}
					err = s.merge(ctx)
				}
				if err != nil {
					return res, err
				}
				if local, err = s.git.RevParse(ctx, "HEAD"); err != nil {
					return res, err
				}
				pulled, err := s.changedBetween(ctx, before, local)
				if err != nil {
					return res, err
				}
				res.Pulled = append(res.Pulled, pulled...)
			}
		}
		res.Head = local

		if local == "" || local == remote {
			return res, nil // nothing to push
		}
		if _, err := s.git.Run(ctx, "push", "--quiet", "origin", "HEAD:refs/heads/"+Branch); err != nil {
			if isPushRace(err) {
				s.log.Debug("push raced with another device, retrying")
				continue
			}
			return res, network(err)
		}
		s.log.Info("pushed", "head", short(local))
		res.Pushed = true
		return res, nil
	}
	return res, fmt.Errorf("gave up after %d attempts: the server keeps changing underneath us", maxPushAttempts)
}

// changedBetween lists paths that differ between two commits (from == ""
// means the empty tree).
func (s *Syncer) changedBetween(ctx context.Context, from, to string) ([]string, error) {
	if from == "" {
		out, err := s.git.Run(ctx, "ls-tree", "-r", "--name-only", to)
		return gitx.Lines(out), err
	}
	out, err := s.git.Run(ctx, "diff", "--name-only", "--no-renames", from, to)
	return gitx.Lines(out), err
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
	head, _, err := s.commitLocal(ctx)
	return head, err
}

func (s *Syncer) commitLocal(ctx context.Context) (string, []string, error) {
	if _, err := s.git.Run(ctx, "add", "--all"); err != nil {
		return "", nil, err
	}
	if err := s.refuseEmbeddedRepos(ctx); err != nil {
		return "", nil, err
	}
	out, err := s.git.Run(ctx, "diff", "--cached", "--name-status", "--no-renames")
	if err != nil {
		return "", nil, err
	}
	changes := gitx.Lines(out)
	if len(changes) == 0 {
		return "", nil, nil
	}

	subject, body := commitMessage(s.cfg.Device, changes, time.Now())
	if _, err := s.git.Run(ctx, "commit", "--quiet", "--no-verify", "-m", subject, "-m", body); err != nil {
		return "", nil, err
	}
	head, err := s.git.RevParse(ctx, "HEAD")
	if err != nil {
		return "", nil, err
	}
	s.log.Info("committed", "commit", short(head), "files", len(changes))
	paths := make([]string, len(changes))
	for i, c := range changes {
		_, paths[i], _ = strings.Cut(c, "\t")
	}
	return head, paths, nil
}

// refuseEmbeddedRepos stops a commit that would record a git repository
// created inside the folder (e.g. by `git clone`) as a gitlink, which syncs
// a commit pointer instead of the files. The staged entry is undone and an
// ErrNested error returned, so the folder stops until the repository is
// moved out.
func (s *Syncer) refuseEmbeddedRepos(ctx context.Context) error {
	out, err := s.git.Run(ctx, "diff", "--cached", "--raw", "--no-renames", "--diff-filter=AM")
	if err != nil {
		return err
	}
	var nested []string
	for _, l := range gitx.Lines(out) {
		// ":000000 160000 0000000 1234567 A\tpath": the new mode is 160000.
		meta, path, ok := strings.Cut(l, "\t")
		if f := strings.Fields(meta); ok && len(f) >= 2 && f[1] == "160000" {
			nested = append(nested, path)
		}
	}
	if len(nested) == 0 {
		return nil
	}
	// --cached touches only the index, never the files on disk.
	args := append([]string{"rm", "--cached", "--force", "--quiet", "-r", "--"}, nested...)
	if _, err := s.git.Run(ctx, args...); err != nil {
		return err
	}
	return fmt.Errorf("%w: %s contains git repositories (%s).\n"+
		"Their files would not be synced, so this folder has stopped syncing. Move them out of the folder "+
		"(or delete their .git), then restart the daemon (e.g. systemctl --user restart dropgitbox)",
		ErrNested, s.cfg.Dir, strings.Join(nested, ", "))
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

// isOverwriteErr reports a merge refused because uncommitted changes
// would be overwritten.
func isOverwriteErr(err error) bool {
	var ge *gitx.Error
	return errors.As(err, &ge) && strings.Contains(ge.Stderr, "would be overwritten by merge")
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
