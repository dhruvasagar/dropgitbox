package client

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"strings"
	"time"
)

// DaemonOptions tunes the watch daemon.
type DaemonOptions struct {
	// Debounce is how long the folder must be quiet before a change is
	// committed and synced. Default 2s.
	Debounce time.Duration
	// MaxDelay caps how long a continuous burst of changes can postpone a
	// commit. Default 30s.
	MaxDelay time.Duration
	// Poll is a safety-net sync interval in case a notification is missed.
	// Default 5m.
	Poll time.Duration
	// Notify, when set, is called once each time syncing pauses on a
	// conflict. Defaults to a desktop notification via notify-send when
	// available.
	Notify func(files []string)
}

func (o *DaemonOptions) defaults() {
	if o.Debounce <= 0 {
		o.Debounce = 2 * time.Second
	}
	if o.MaxDelay <= 0 {
		o.MaxDelay = 30 * time.Second
	}
	if o.Poll <= 0 {
		o.Poll = 5 * time.Minute
	}
	if o.Notify == nil {
		o.Notify = desktopNotify
	}
}

// Watch runs the sync daemon until ctx is cancelled. It syncs on local
// file changes, on push notifications from the server, and periodically.
// On a merge conflict it pauses and resumes by itself once the conflict
// has been resolved (see Resolve).
func (s *Syncer) Watch(ctx context.Context, opts DaemonOptions) error {
	opts.defaults()
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	kick := make(chan struct{}, 1)
	trigger := func() {
		select {
		case kick <- struct{}{}:
		default:
		}
	}

	watchErr := make(chan error, 1)
	go func() {
		watchErr <- watchTree(ctx, s.cfg.Dir, opts.Debounce, opts.MaxDelay, s.log, trigger)
	}()
	go subscribe(ctx, s.cfg, 90*time.Second, s.log, func(head string) {
		if local, err := s.Head(ctx); err != nil || local != head {
			trigger()
		}
	})

	poll := time.NewTicker(opts.Poll)
	defer poll.Stop()
	pauseCheck := time.NewTicker(2 * time.Second)
	defer pauseCheck.Stop()

	var (
		paused     = s.Conflicted()
		retry      *time.Timer
		retryDelay = 5 * time.Second
	)
	if paused {
		s.reportConflict(ctx, opts)
	}
	defer func() {
		if retry != nil {
			retry.Stop()
		}
	}()

	s.log.Info("watching", "server", s.cfg.Server, "repo", s.cfg.Repo)
	trigger()

	for {
		select {
		case <-ctx.Done():
			s.log.Info("stopped")
			return nil

		case err := <-watchErr:
			return fmt.Errorf("file watcher: %w", err)

		case <-poll.C:
			trigger()

		case <-pauseCheck.C:
			if paused && !s.Conflicted() {
				paused = false
				s.log.Info("conflict resolved; resuming sync")
				trigger()
			}

		case <-kick:
			if paused {
				continue
			}
			err := s.Sync(ctx)
			switch {
			case err == nil:
				retryDelay = 5 * time.Second
			case errors.Is(err, ErrConflict):
				paused = true
				s.reportConflict(ctx, opts)
			case ctx.Err() != nil:
				return nil
			default:
				s.log.Error("sync failed", "err", err, "retry_in", retryDelay)
				if retry != nil {
					retry.Stop()
				}
				retry = time.AfterFunc(retryDelay, trigger)
				retryDelay = min(retryDelay*2, opts.Poll)
			}
		}
	}
}

func (s *Syncer) reportConflict(ctx context.Context, opts DaemonOptions) {
	files, _ := s.ConflictedFiles(ctx)
	s.log.Warn("SYNC PAUSED: merge conflict needs your attention. Edit the files (or pick a side), then run `dropgitbox resolve`.",
		"files", strings.Join(files, ", "))
	opts.Notify(files)
}

func desktopNotify(files []string) {
	bin, err := exec.LookPath("notify-send")
	if err != nil {
		return
	}
	msg := "Sync paused. Resolve with `dropgitbox resolve`.\n" + strings.Join(files, "\n")
	_ = exec.Command(bin, "--urgency=critical", "dropgitbox: merge conflict", msg).Run()
}
