package client

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
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
	// Side picks a version for the files. SideManual takes them as they
	// are in the folder (after the user edited them).
	Side Side
	// Paths are the files (or directories) to resolve, relative to the top
	// of the folder; empty means every conflicted file. Naming a file is an
	// explicit decision, like "git add FILE": it is taken as it is even if
	// it is binary or was deleted on one side.
	Paths []string
	// Force accepts files that still contain conflict markers.
	Force bool
}

// ResolveResult reports what Resolve did.
type ResolveResult struct {
	Resolved  []string   // files marked resolved by this call
	Remaining []Conflict // files still conflicted; the merge stays paused
	Commit    string     // the merge commit, once nothing remains
}

// ErrNoConflict is returned by Resolve when there is nothing to resolve.
var ErrNoConflict = errors.New("no conflict to resolve")

// UnresolvedError lists files Resolve won't take as they are.
type UnresolvedError struct {
	Markers    []string   // still contain conflict markers
	NeedChoice []Conflict // binary, or deleted on one side: nothing to edit
}

func (e *UnresolvedError) Error() string {
	return e.Format(func(p string) string { return p })
}

// Format is Error with paths shown by show.
func (e *UnresolvedError) Format(show func(string) string) string {
	var b strings.Builder
	b.WriteString("not resolved, nothing changed:")
	for _, f := range e.Markers {
		fmt.Fprintf(&b, "\n  %s: still has conflict markers", show(f))
	}
	for _, c := range e.NeedChoice {
		fmt.Fprintf(&b, "\n  %s: %s", show(c.Path), c.Describe())
	}
	b.WriteString("\n")
	if len(e.Markers) > 0 {
		b.WriteString("Edit out the <<<<<<< ======= >>>>>>> markers (or use --force if they belong there).\n")
	}
	if len(e.NeedChoice) > 0 {
		b.WriteString("To keep a file as it is in the folder, name it: dropgitbox resolve FILE\n")
	}
	b.WriteString("Or keep one side: --ours (this device's version) or --theirs (the server's).")
	return b.String()
}

// NotConflictedError is returned for a path that has no conflict.
type NotConflictedError struct {
	Path       string
	Conflicted []string
}

func (e *NotConflictedError) Error() string {
	return fmt.Sprintf("%s has no conflict (conflicted: %s)", e.Path, strings.Join(e.Conflicted, ", "))
}

// ConflictKind says how a file conflicts, in git's words. "Us" is this
// device, "them" the server.
type ConflictKind string

// Conflict kinds.
const (
	BothModified  ConflictKind = "both modified"
	BothAdded     ConflictKind = "both added"
	DeletedByUs   ConflictKind = "deleted by us"
	DeletedByThem ConflictKind = "deleted by them"
	AddedByUs     ConflictKind = "added by us"
	AddedByThem   ConflictKind = "added by them"
	BothDeleted   ConflictKind = "both deleted"
)

// Conflict is one conflicted file.
type Conflict struct {
	Path   string
	Kind   ConflictKind
	Binary bool // git could not merge its content, so it has no markers
}

// NeedsChoice reports whether the file has no markers to edit: it is
// binary, or exists on one side only.
func (c Conflict) NeedsChoice() bool {
	return c.Binary || (c.Kind != BothModified && c.Kind != BothAdded)
}

// Describe explains the conflict in a few words.
func (c Conflict) Describe() string {
	switch {
	case c.Binary:
		return string(c.Kind) + ", binary (this device's version is in the folder)"
	case c.Kind == DeletedByUs:
		return "deleted on this device, changed on the server"
	case c.Kind == DeletedByThem:
		return "changed on this device, deleted on the server"
	}
	return string(c.Kind)
}

// Conflicts lists the conflicted files, sorted by path.
func (s *Syncer) Conflicts(ctx context.Context) ([]Conflict, error) {
	out, err := s.git.Run(ctx, "ls-files", "--unmerged", "-z")
	if err != nil {
		return nil, err
	}
	stages := map[string]string{} // path -> stages present, e.g. "123"
	var order []string
	for _, entry := range strings.Split(out, "\x00") {
		meta, path, ok := strings.Cut(entry, "\t")
		if !ok {
			continue
		}
		f := strings.Fields(meta)
		if len(f) != 3 {
			continue
		}
		if _, seen := stages[path]; !seen {
			order = append(order, path)
		}
		stages[path] += f[2]
	}
	sort.Strings(order)
	var cs []Conflict
	for _, p := range order {
		st := stages[p]
		has := func(n string) bool { return strings.Contains(st, n) }
		c := Conflict{Path: p}
		switch {
		case has("2") && has("3") && has("1"):
			c.Kind = BothModified
		case has("2") && has("3"):
			c.Kind = BothAdded
		case has("1") && has("2"):
			c.Kind = DeletedByThem
		case has("1") && has("3"):
			c.Kind = DeletedByUs
		case has("2"):
			c.Kind = AddedByUs
		case has("3"):
			c.Kind = AddedByThem
		default:
			c.Kind = BothDeleted
		}
		if c.Kind == BothModified || c.Kind == BothAdded {
			c.Binary = isBinaryFile(filepath.Join(s.cfg.Dir, p))
		}
		cs = append(cs, c)
	}
	return cs, nil
}

// Resolve marks conflicted files resolved, like "git add" (or, with a
// Side, "git checkout --ours/--theirs" first). Once no conflicted file is
// left, it concludes the merge with a commit of the merge result and the
// resolved files only: other changes made while the folder was paused are
// left in the folder, to be committed and synced on their own. Callers
// should sync afterwards (the watch daemon does so by itself).
func (s *Syncer) Resolve(ctx context.Context, opts ResolveOptions) (*ResolveResult, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if !s.Conflicted() {
		return nil, ErrNoConflict
	}
	all, err := s.Conflicts(ctx)
	if err != nil {
		return nil, err
	}

	targets := all
	explicit := len(opts.Paths) > 0
	if explicit {
		if targets, err = matchConflicts(all, opts.Paths); err != nil {
			return nil, err
		}
	}

	if opts.Side == SideManual {
		ue := &UnresolvedError{}
		for _, c := range targets {
			switch {
			case c.NeedsChoice():
				if !explicit {
					ue.NeedChoice = append(ue.NeedChoice, c)
				}
			case !opts.Force:
				has, err := hasConflictMarkers(filepath.Join(s.cfg.Dir, c.Path))
				if err != nil {
					return nil, err
				}
				if has {
					ue.Markers = append(ue.Markers, c.Path)
				}
			}
		}
		if len(ue.Markers) > 0 || len(ue.NeedChoice) > 0 {
			return nil, ue
		}
	} else {
		paths := make([]string, len(targets))
		for i, c := range targets {
			paths[i] = c.Path
		}
		if err := s.pickSide(ctx, opts.Side, paths); err != nil {
			return nil, err
		}
	}

	res := &ResolveResult{}
	for _, c := range targets {
		if err := s.markResolved(ctx, c.Path); err != nil {
			return nil, err
		}
		res.Resolved = append(res.Resolved, c.Path)
	}
	if res.Remaining, err = s.Conflicts(ctx); err != nil {
		return nil, err
	}
	if len(res.Remaining) > 0 {
		return res, nil
	}

	// Commit what is staged: the merge and the resolutions, nothing else.
	if _, err := s.git.Run(ctx, "commit", "--quiet", "--no-edit", "--no-verify"); err != nil {
		return nil, err
	}
	if res.Commit, err = s.git.RevParse(ctx, "HEAD"); err != nil {
		return nil, err
	}
	s.log.Debug("conflict resolved", "commit", short(res.Commit))
	return res, nil
}

// matchConflicts picks the conflicts named by paths: a file, or a
// directory containing conflicted files ("." is the whole folder).
func matchConflicts(all []Conflict, paths []string) ([]Conflict, error) {
	var names []string
	for _, c := range all {
		names = append(names, c.Path)
	}
	picked := map[string]bool{}
	var out []Conflict
	for _, p := range paths {
		p = strings.TrimSuffix(filepath.ToSlash(filepath.Clean(p)), "/")
		found := false
		for _, c := range all {
			if p == "." || c.Path == p || strings.HasPrefix(c.Path, p+"/") {
				found = true
				if !picked[c.Path] {
					picked[c.Path] = true
					out = append(out, c)
				}
			}
		}
		if !found {
			return nil, &NotConflictedError{Path: p, Conflicted: names}
		}
	}
	return out, nil
}

// markResolved stages a file as it is in the folder, or its deletion.
func (s *Syncer) markResolved(ctx context.Context, path string) error {
	if _, err := os.Lstat(filepath.Join(s.cfg.Dir, path)); err == nil {
		_, err := s.git.Run(ctx, "add", "--", path)
		return err
	}
	_, err := s.git.Run(ctx, "rm", "--cached", "--quiet", "--ignore-unmatch", "--", path)
	return err
}

// isBinaryFile applies git's test: a NUL byte in the first 8000 bytes.
func isBinaryFile(path string) bool {
	f, err := os.Open(path)
	if err != nil {
		return false
	}
	defer f.Close()
	buf := make([]byte, 8000)
	n, _ := io.ReadFull(f, buf)
	return bytes.IndexByte(buf[:n], 0) >= 0
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
	Conflicts   []Conflict
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
		if st.Conflicts, err = s.Conflicts(ctx); err != nil {
			return nil, err
		}
	}
	return st, nil
}
