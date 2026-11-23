package client

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/dhruvasagar/dropgitbox/internal/gitx"
)

// ErrNested is returned for a folder that would nest inside, or contain,
// another git repository.
//
// Git can't track a repository inside another one: the outer repository
// records only a pointer to the inner one's current commit (a "gitlink"),
// not its files. A synced folder inside another synced folder, or any git
// repository inside a synced folder, would silently not sync.
var ErrNested = errors.New("nested git repositories can't be synced")

// maxNestedReported caps how many nested repositories an error lists.
const maxNestedReported = 5

// CheckNesting returns an ErrNested error if dir is inside another synced
// folder, or if any git repository (a synced folder or not) is inside dir.
// dir's own .git is fine. A git repository that merely contains dir, such
// as a dotfiles repo at ~/.git, is not a problem for dir and is allowed.
func CheckNesting(ctx context.Context, dir string) error {
	dir, err := canonical(dir)
	if err != nil {
		return err
	}

	for p := filepath.Dir(dir); ; p = filepath.Dir(p) {
		if isSyncedFolder(ctx, p) {
			return fmt.Errorf("%w: %s is inside the synced folder %s.\n"+
				"Sync it as part of %s instead, or move it outside", ErrNested, dir, p, p)
		}
		if parent := filepath.Dir(p); parent == p {
			break
		}
	}

	var found []string
	errEnough := errors.New("enough")
	err = filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			if p == dir {
				return err
			}
			return nil // unreadable or vanished: skip
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if d.Name() != ".git" {
			return nil
		}
		if filepath.Dir(p) == dir {
			// dir's own repository.
			if d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		rel, _ := filepath.Rel(dir, filepath.Dir(p))
		found = append(found, rel)
		if len(found) >= maxNestedReported {
			return errEnough
		}
		if d.IsDir() {
			return filepath.SkipDir
		}
		return nil
	})
	if errors.Is(err, fs.ErrNotExist) {
		return nil // nothing there yet, so nothing nested
	}
	if err != nil && !errors.Is(err, errEnough) {
		return err
	}
	if len(found) > 0 {
		list := strings.Join(found, ", ")
		if len(found) >= maxNestedReported {
			list += ", …"
		}
		return fmt.Errorf("%w: %s contains git repositories (%s).\n"+
			"Their files would not be synced. Move them out of the folder, or delete their .git if you don't need it",
			ErrNested, dir, list)
	}
	return nil
}

// isSyncedFolder reports whether p is the root of a dropgitbox folder.
func isSyncedFolder(ctx context.Context, p string) bool {
	gitDir := filepath.Join(p, ".git")
	if fi, err := os.Stat(gitDir); err != nil || !fi.IsDir() {
		return false
	}
	g := &gitx.Repo{Dir: p}
	v, err := g.Run(ctx, "config", "--file", filepath.Join(gitDir, "config"), "--get", cfgServer)
	return err == nil && strings.TrimSpace(v) != ""
}

// canonical returns dir as an absolute path with symlinks resolved, as far
// as it exists, so a folder reached through a symlink is compared by where
// it really lives.
func canonical(dir string) (string, error) {
	abs, err := filepath.Abs(dir)
	if err != nil {
		return "", err
	}
	if real, err := filepath.EvalSymlinks(abs); err == nil {
		return real, nil
	}
	// Resolve the longest existing ancestor and append the rest.
	parent, rest := filepath.Dir(abs), filepath.Base(abs)
	for parent != filepath.Dir(parent) {
		if real, err := filepath.EvalSymlinks(parent); err == nil {
			return filepath.Join(real, rest), nil
		}
		parent, rest = filepath.Dir(parent), filepath.Join(filepath.Base(parent), rest)
	}
	return abs, nil
}
