package client

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/dhruvasagar/dropgitbox/internal/gitx"
)

// Side picks a version of a conflicted file.
type Side int

const (
	// SideManual means the user edited the files themselves.
	SideManual Side = iota
	// SideOurs keeps this device's version.
	SideOurs
	// SideTheirs keeps the server's version.
	SideTheirs
)

// ResolveOptions configures Resolve.
type ResolveOptions struct {
	// Side picks a version for Paths (or all conflicted files if Paths is
	// empty). SideManual leaves files as the user edited them.
	Side  Side
	Paths []string
	// Force concludes the merge even if conflict markers remain.
	Force bool
}

// ErrNoConflict is returned by Resolve when there is nothing to resolve.
var ErrNoConflict = errors.New("no conflict to resolve")

// MarkersError reports files that still contain conflict markers.
type MarkersError struct{ Files []string }

func (e *MarkersError) Error() string {
	return fmt.Sprintf("conflict markers still present in: %s (edit them, pick a side with --ours/--theirs, or use --force)",
		strings.Join(e.Files, ", "))
}

// Resolve concludes a paused merge with a merge commit. Callers should run
// Sync afterwards (the watch daemon does so automatically).
func (s *Syncer) Resolve(ctx context.Context, opts ResolveOptions) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if !s.Conflicted() {
		return "", ErrNoConflict
	}
	unmerged, err := s.ConflictedFiles(ctx)
	if err != nil {
		return "", err
	}

	if opts.Side != SideManual {
		paths := opts.Paths
		if len(paths) == 0 {
			paths = unmerged
		}
		if err := s.pickSide(ctx, opts.Side, paths); err != nil {
			return "", err
		}
	}

	if !opts.Force {
		var marked []string
		for _, p := range unmerged {
			has, err := hasConflictMarkers(filepath.Join(s.cfg.Dir, p))
			if err != nil {
				return "", err
			}
			if has {
				marked = append(marked, p)
			}
		}
		if len(marked) > 0 {
			return "", &MarkersError{Files: marked}
		}
	}

	if _, err := s.git.Run(ctx, "add", "--all"); err != nil {
		return "", err
	}
	if _, err := s.git.Run(ctx, "commit", "--quiet", "--no-edit", "--no-verify"); err != nil {
		return "", err
	}
	head, err := s.git.RevParse(ctx, "HEAD")
	if err != nil {
		return "", err
	}
	s.log.Info("conflict resolved", "commit", short(head))
	return head, nil
}

// pickSide checks out one side's version of each path. A path deleted on
// the chosen side is removed.
func (s *Syncer) pickSide(ctx context.Context, side Side, paths []string) error {
	stage := "2" // ours
	flag := "--ours"
	if side == SideTheirs {
		stage, flag = "3", "--theirs"
	}
	for _, p := range paths {
		// Does the chosen side have this file at all?
		if _, err := s.git.Run(ctx, "cat-file", "-e", ":"+stage+":"+p); err != nil {
			if _, err := s.git.Run(ctx, "rm", "--quiet", "--force", "--ignore-unmatch", "--", p); err != nil {
				return err
			}
			continue
		}
		if _, err := s.git.Run(ctx, "checkout", flag, "--", p); err != nil {
			return err
		}
	}
	return nil
}

func hasConflictMarkers(path string) (bool, error) {
	f, err := os.Open(path)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 64*1024), 16*1024*1024)
	for sc.Scan() {
		line := sc.Text()
		if strings.HasPrefix(line, "<<<<<<< ") || strings.HasPrefix(line, ">>>>>>> ") {
			return true, nil
		}
	}
	// A line too long for the scanner means a binary-ish file; binary
	// conflicts never get markers, so treat it as clean.
	return false, nil
}

// Restore brings paths back to how they were at rev. The restored content
// is then auto-committed like any other change, so restoring never
// rewrites history.
func (s *Syncer) Restore(ctx context.Context, rev string, paths []string) error {
	if len(paths) == 0 {
		return errors.New("restore: at least one path is required")
	}
	if s.Conflicted() {
		return ErrConflict
	}
	args := append([]string{"restore", "--source", rev, "--worktree", "--"}, paths...)
	_, err := s.git.Run(ctx, args...)
	return err
}

// Status summarises a synced folder.
type Status struct {
	Config      Config
	Head        string
	Remote      string // last known server head
	Ahead       int    // local commits not yet on the server
	Behind      int    // server commits not yet merged locally
	Uncommitted []string
	Conflicted  bool
	Conflicts   []string
}

// Status inspects the folder without touching the network.
func (s *Syncer) Status(ctx context.Context) (*Status, error) {
	st := &Status{Config: s.cfg, Conflicted: s.Conflicted()}
	var err error
	if st.Head, err = s.git.RevParse(ctx, "HEAD"); err != nil {
		return nil, err
	}
	if st.Remote, err = s.git.RevParse(ctx, "refs/remotes/origin/"+Branch); err != nil {
		return nil, err
	}
	if st.Head != "" && st.Remote != "" {
		out, err := s.git.Run(ctx, "rev-list", "--left-right", "--count", "HEAD...refs/remotes/origin/"+Branch)
		if err != nil {
			return nil, err
		}
		if f := strings.Fields(out); len(f) == 2 {
			st.Ahead, _ = strconv.Atoi(f[0])
			st.Behind, _ = strconv.Atoi(f[1])
		}
	} else if st.Head != "" {
		out, err := s.git.Run(ctx, "rev-list", "--count", "HEAD")
		if err != nil {
			return nil, err
		}
		st.Ahead, _ = strconv.Atoi(out)
	}
	out, err := s.git.Run(ctx, "status", "--porcelain=v1", "--untracked-files=all")
	if err != nil {
		return nil, err
	}
	st.Uncommitted = gitx.Lines(out)
	if st.Conflicted {
		if st.Conflicts, err = s.ConflictedFiles(ctx); err != nil {
			return nil, err
		}
	}
	return st, nil
}
