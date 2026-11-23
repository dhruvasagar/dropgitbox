package client

import (
	"context"
	"errors"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/fsnotify/fsnotify"
)

// watchTree calls trigger once the tree under root has been quiet for
// debounce after a change, or at most maxDelay after the first change of a
// burst, so continuous writes still get committed periodically.
// Changes inside .git are ignored.
func watchTree(ctx context.Context, root string, debounce, maxDelay time.Duration, log *slog.Logger, trigger func()) error {
	w, err := fsnotify.NewWatcher()
	if err != nil {
		return err
	}
	defer w.Close()

	// fsnotify is not recursive on Linux/BSD, so every directory gets its
	// own watch, including ones created later.
	addTree := func(dir string) {
		_ = filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
			if err != nil {
				return nil // vanished or unreadable; skip
			}
			if !d.IsDir() {
				return nil
			}
			if d.Name() == ".git" {
				return filepath.SkipDir
			}
			if err := w.Add(p); err != nil {
				log.Warn("cannot watch directory", "dir", p, "err", err)
			}
			return nil
		})
	}
	addTree(root)

	gitDir := filepath.Join(root, ".git")
	ignored := func(p string) bool {
		return p == gitDir || strings.HasPrefix(p, gitDir+string(filepath.Separator))
	}

	var (
		timer    *time.Timer
		timerC   <-chan time.Time
		burstAt  time.Time
		inBurst  bool
		resetFor = func(d time.Duration) {
			if timer == nil {
				timer = time.NewTimer(d)
			} else {
				timer.Reset(d)
			}
			timerC = timer.C
		}
	)

	for {
		select {
		case <-ctx.Done():
			return nil

		case ev, ok := <-w.Events:
			if !ok {
				return errors.New("watcher closed")
			}
			if ignored(ev.Name) {
				continue
			}
			if ev.Has(fsnotify.Create) {
				if fi, err := os.Lstat(ev.Name); err == nil && fi.IsDir() {
					addTree(ev.Name)
				}
			}
			now := time.Now()
			if !inBurst {
				inBurst, burstAt = true, now
			}
			wait := debounce
			if deadline := burstAt.Add(maxDelay); now.Add(wait).After(deadline) {
				wait = max(time.Until(deadline), 0)
			}
			resetFor(wait)

		case <-timerC:
			inBurst, timerC = false, nil
			trigger()

		case err, ok := <-w.Errors:
			if !ok {
				return errors.New("watcher closed")
			}
			// Typically an event-queue overflow: we may have missed changes,
			// so just sync to be safe.
			log.Warn("file watcher error", "err", err)
			trigger()
		}
	}
}
