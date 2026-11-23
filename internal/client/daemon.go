package client

import (
	"context"
	"errors"
	"fmt"
	"sync/atomic"
	"time"
)

// EventType names something that happened to a synced folder.
type EventType string

// Folder events, emitted by Watch.
const (
	// EventPending: local changes were detected and will be synced once
	// the folder is quiet.
	EventPending EventType = "pending"
	// EventSyncing: a sync started.
	EventSyncing EventType = "syncing"
	// EventSynced: a sync finished successfully (possibly with nothing to do).
	EventSynced EventType = "synced"
	// EventRemoteChanges: a sync brought in changes made on other devices.
	EventRemoteChanges EventType = "remote_changes"
	// EventConflict: syncing paused on a merge conflict; the user must act.
	EventConflict EventType = "conflict"
	// EventResolved: the conflict was resolved and syncing resumed.
	EventResolved EventType = "resolved"
	// EventError: a sync attempt failed (retried automatically).
	EventError EventType = "error"
	// EventOffline: syncing has kept failing for DaemonOptions.OfflineAfter.
	EventOffline EventType = "offline"
	// EventOnline: syncing works again after EventOffline or EventUnauthorized.
	EventOnline EventType = "online"
	// EventUnauthorized: the server rejected this device's token.
	EventUnauthorized EventType = "unauthorized"
)

// AllEvents lists every event type.
var AllEvents = []EventType{EventPending, EventSyncing, EventSynced, EventRemoteChanges, EventConflict,
	EventResolved, EventError, EventOffline, EventOnline, EventUnauthorized}

// Event is one notification about a folder.
type Event struct {
	Time    time.Time `json:"time"`
	Type    EventType `json:"type"`
	Folder  string    `json:"folder"`
	Repo    string    `json:"repo"`
	Message string    `json:"message,omitempty"`
	// Files are the conflicted files (conflict), or the files involved in
	// a sync (synced: committed and pulled; remote_changes: pulled).
	Files []string `json:"files,omitempty"`
	// Counts for synced events.
	Committed int  `json:"committed,omitempty"`
	Pulled    int  `json:"pulled,omitempty"`
	Pushed    bool `json:"pushed,omitempty"`
}

// maxEventFiles caps the file list carried by one event.
const maxEventFiles = 50

// DaemonOptions tunes Watch.
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
	// OfflineAfter is how long syncing must keep failing before an
	// EventOffline is emitted (brief network blips stay quiet). Default 10m.
	OfflineAfter time.Duration
	// OnEvent receives the folder's events. It must not block for long.
	OnEvent func(Event)
	// Kick, when it receives, triggers an immediate sync.
	Kick <-chan struct{}
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
	if o.OfflineAfter <= 0 {
		o.OfflineAfter = 10 * time.Minute
	}
	if o.OnEvent == nil {
		o.OnEvent = func(Event) {}
	}
}

// Watch runs the sync loop for this folder until ctx is cancelled. It
// syncs on local file changes, on push notifications from the server, on
// Kick, and periodically. On a merge conflict it pauses, and resumes by
// itself once the conflict has been resolved (see Resolve).
func (s *Syncer) Watch(ctx context.Context, opts DaemonOptions) error {
	opts.defaults()
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	emit := func(ev Event) {
		ev.Time, ev.Folder, ev.Repo = time.Now(), s.cfg.Dir, s.cfg.Repo
		if len(ev.Files) > maxEventFiles {
			ev.Files = ev.Files[:maxEventFiles]
		}
		opts.OnEvent(ev)
	}

	var syncing atomic.Bool
	kick := make(chan struct{}, 1)
	trigger := func() {
		select {
		case kick <- struct{}{}:
		default:
		}
	}

	watchErr := make(chan error, 1)
	go func() {
		watchErr <- watchTree(ctx, s.cfg.Dir, opts.Debounce, opts.MaxDelay, s.log, trigger, func() {
			// Files written by our own merge aren't the user's changes.
			if !syncing.Load() {
				emit(Event{Type: EventPending})
			}
		})
	}()
	go subscribe(ctx, s.cfg, 90*time.Second, s.log, func(head string) {
		if local, err := s.Head(ctx); err != nil || local != head {
			trigger()
		}
	}, trigger) // a rejected token: sync now, which reports EventUnauthorized

	poll := time.NewTicker(opts.Poll)
	defer poll.Stop()
	pauseCheck := time.NewTicker(2 * time.Second)
	defer pauseCheck.Stop()

	var (
		paused       = s.Conflicted()
		retry        *time.Timer
		retryDelay   = 5 * time.Second
		failingSince time.Time
		offline      bool // EventOffline sent, awaiting recovery
		unauthorized bool // EventUnauthorized sent, awaiting recovery
	)
	if paused {
		s.reportConflict(ctx, emit)
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
			// On cancellation the watcher returns nil, possibly before
			// this select sees ctx.Done(): that's a clean stop.
			if ctx.Err() != nil {
				s.log.Info("stopped")
				return nil
			}
			if err == nil {
				err = errors.New("stopped unexpectedly")
			}
			return fmt.Errorf("file watcher: %w", err)

		case <-poll.C:
			trigger()

		case <-opts.Kick:
			trigger()

		case <-pauseCheck.C:
			if paused && !s.Conflicted() {
				paused = false
				s.log.Info("conflict resolved; resuming sync")
				emit(Event{Type: EventResolved, Message: "Conflict resolved; syncing resumed."})
				trigger()
			}

		case <-kick:
			if paused {
				continue // already reported; stay quiet until resolved
			}
			emit(Event{Type: EventSyncing})
			syncing.Store(true)
			res, err := s.SyncWithResult(ctx)
			// Let the watcher deliver events caused by the sync before
			// treating new changes as the user's.
			time.AfterFunc(opts.Debounce, func() { syncing.Store(false) })
			switch {
			case err == nil:
				retryDelay = 5 * time.Second
				failingSince = time.Time{}
				if offline || unauthorized {
					offline, unauthorized = false, false
					emit(Event{Type: EventOnline, Message: "Syncing works again."})
				}
				emit(Event{
					Type: EventSynced, Committed: len(res.Committed), Pulled: len(res.Pulled), Pushed: res.Pushed,
					Files: append(append([]string(nil), res.Committed...), res.Pulled...),
				})
				if len(res.Pulled) > 0 {
					emit(Event{Type: EventRemoteChanges, Pulled: len(res.Pulled), Files: res.Pulled})
				}

			case errors.Is(err, ErrNested):
				// Retrying can't help until the user moves the repository.
				emit(Event{Type: EventError, Message: err.Error()})
				return err

			case errors.Is(err, ErrConflict):
				paused = true
				s.reportConflict(ctx, emit)

			case ctx.Err() != nil:
				return nil

			default:
				s.log.Error("sync failed", "err", err, "retry_in", retryDelay)
				if failingSince.IsZero() {
					failingSince = time.Now()
				}
				emit(Event{Type: EventError, Message: err.Error()})
				if errors.Is(err, ErrUnauthorized) {
					if !unauthorized {
						unauthorized = true
						emit(Event{Type: EventUnauthorized, Message: err.Error()})
					}
				} else if !offline && time.Since(failingSince) >= opts.OfflineAfter {
					offline = true
					emit(Event{Type: EventOffline, Message: fmt.Sprintf("Syncing has failed for %s: %v",
						time.Since(failingSince).Round(time.Minute), err)})
				}
				if retry != nil {
					retry.Stop()
				}
				retry = time.AfterFunc(retryDelay, trigger)
				retryDelay = min(retryDelay*2, opts.Poll)
			}
		}
	}
}

func (s *Syncer) reportConflict(ctx context.Context, emit func(Event)) {
	files, _ := s.ConflictedFiles(ctx)
	s.log.Warn("SYNC PAUSED: merge conflict needs your attention. Edit the files (or pick a side), then run `dropgitbox resolve`.",
		"files", files)
	emit(Event{Type: EventConflict, Files: files,
		Message: fmt.Sprintf("Sync paused: edit the conflicted files, then run `dropgitbox resolve -dir %s`.", s.cfg.Dir)})
}
