// Package daemon runs the dropgitbox client daemon: one sync loop per
// folder, an event bus that feeds desktop notifications and user hooks, and
// a local control socket that `dropgitbox status`, `dropgitbox events` and
// `dropgitbox sync` talk to.
package daemon

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"slices"
	"sync"
	"time"

	"github.com/dhruvasagar/dropgitbox/internal/client"
)

// Folder states, as shown by `dropgitbox status`.
const (
	StateIdle         = "idle"         // in sync
	StatePending      = "pending"      // local changes waiting for the debounce
	StateSyncing      = "syncing"      // sync in progress
	StateConflict     = "conflict"     // paused; the user must resolve
	StateError        = "error"        // last attempt failed; retrying
	StateOffline      = "offline"      // failing for a while
	StateUnauthorized = "unauthorized" // token rejected
)

// FolderStatus is the live state of one folder.
type FolderStatus struct {
	Folder    string    `json:"folder"`
	Repo      string    `json:"repo"`
	State     string    `json:"state"`
	Since     time.Time `json:"since"`              // when State was entered
	LastSync  time.Time `json:"last_sync,omitzero"` // last successful sync
	LastError string    `json:"last_error,omitempty"`
	Conflicts []string  `json:"conflicts,omitempty"`
}

// Status is the daemon's overall state.
type Status struct {
	PID     int            `json:"pid"`
	Version string         `json:"version"`
	Started time.Time      `json:"started"`
	Folders []FolderStatus `json:"folders"`
}

// Options configures Run.
type Options struct {
	Sync   client.DaemonOptions // per-folder timing; OnEvent and Kick are set by Run
	Notify NotifyOptions
	// SocketPath is the control socket. Default: SocketPath().
	SocketPath string
	Version    string
	Logger     *slog.Logger
}

// Daemon is a running client daemon.
type Daemon struct {
	opts    Options
	log     *slog.Logger
	started time.Time

	mu      sync.Mutex
	folders map[string]*FolderStatus // by folder path
	order   []string
	kicks   map[string]chan struct{}
	subs    map[chan client.Event]struct{}
}

// ErrAlreadyRunning is returned when another daemon owns the socket.
var ErrAlreadyRunning = errors.New("a dropgitbox daemon is already running")

// Run syncs folders until ctx is cancelled.
func Run(ctx context.Context, folders []*client.Syncer, opts Options) error {
	if opts.Logger == nil {
		opts.Logger = slog.Default()
	}
	if opts.SocketPath == "" {
		opts.SocketPath = SocketPath()
	}
	d := &Daemon{
		opts:    opts,
		log:     opts.Logger,
		started: time.Now(),
		folders: map[string]*FolderStatus{},
		kicks:   map[string]chan struct{}{},
		subs:    map[chan client.Event]struct{}{},
	}
	for _, s := range folders {
		d.folders[s.Dir()] = &FolderStatus{Folder: s.Dir(), Repo: s.Config().Repo, State: StateIdle, Since: time.Now()}
		d.order = append(d.order, s.Dir())
		d.kicks[s.Dir()] = make(chan struct{}, 1)
	}

	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	ln, err := listen(opts.SocketPath)
	if err != nil {
		return err
	}
	srvDone := make(chan struct{})
	go func() {
		defer close(srvDone)
		d.serve(ctx, ln)
	}()

	var notifiers sync.WaitGroup
	for _, n := range newNotifiers(opts.Notify, d.log) {
		ch := d.subscribe()
		notifiers.Add(1)
		go func() {
			defer notifiers.Done()
			defer d.unsubscribe(ch)
			n.run(ctx, ch)
		}()
	}

	var (
		wg     sync.WaitGroup
		failMu sync.Mutex
		failed int
	)
	for _, s := range folders {
		o := opts.Sync
		o.OnEvent = d.publish
		o.Kick = d.kicks[s.Dir()]
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := s.Watch(ctx, o); err != nil {
				d.log.Error("folder stopped", "folder", s.Dir(), "err", err)
				d.setError(s.Dir(), err)
				failMu.Lock()
				failed++
				failMu.Unlock()
			}
		}()
	}
	d.log.Info("dropgitbox daemon running", "folders", len(folders), "socket", opts.SocketPath, "version", opts.Version)

	wg.Wait()
	cancel()
	<-srvDone
	notifiers.Wait()
	if failed > 0 && failed == len(folders) {
		return fmt.Errorf("all %d folder(s) stopped with errors", failed)
	}
	return nil
}

// publish applies an event to the state table and fans it out.
func (d *Daemon) publish(ev client.Event) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if f := d.folders[ev.Folder]; f != nil {
		apply(f, ev)
	}
	for ch := range d.subs {
		select {
		case ch <- ev:
		default: // slow subscriber: drop rather than stall syncing
		}
	}
}

func apply(f *FolderStatus, ev client.Event) {
	set := func(state string) {
		if f.State != state {
			f.State, f.Since = state, ev.Time
		}
	}
	switch ev.Type {
	case client.EventPending:
		if f.State == StateIdle {
			set(StatePending)
		}
	case client.EventSyncing:
		// Keep showing a persistent problem while retrying.
		if f.State != StateOffline && f.State != StateUnauthorized {
			set(StateSyncing)
		}
	case client.EventSynced:
		set(StateIdle)
		f.LastSync, f.LastError, f.Conflicts = ev.Time, "", nil
	case client.EventConflict:
		set(StateConflict)
		f.Conflicts = ev.Files
	case client.EventResolved:
		set(StateIdle)
		f.Conflicts = nil
	case client.EventError:
		if f.State != StateOffline && f.State != StateUnauthorized {
			set(StateError)
		}
		f.LastError = ev.Message
	case client.EventOffline:
		set(StateOffline)
	case client.EventUnauthorized:
		set(StateUnauthorized)
		f.LastError = ev.Message
	case client.EventOnline:
		set(StateIdle)
	}
}

func (d *Daemon) setError(folder string, err error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if f := d.folders[folder]; f != nil {
		f.State, f.Since, f.LastError = StateError, time.Now(), "stopped: "+err.Error()
	}
}

func (d *Daemon) subscribe() chan client.Event {
	ch := make(chan client.Event, 256)
	d.mu.Lock()
	d.subs[ch] = struct{}{}
	d.mu.Unlock()
	return ch
}

func (d *Daemon) unsubscribe(ch chan client.Event) {
	d.mu.Lock()
	delete(d.subs, ch)
	d.mu.Unlock()
}

// Status returns a snapshot of the daemon's state.
func (d *Daemon) Status() Status {
	d.mu.Lock()
	defer d.mu.Unlock()
	st := Status{PID: os.Getpid(), Version: d.opts.Version, Started: d.started}
	for _, p := range d.order {
		f := *d.folders[p]
		f.Conflicts = slices.Clone(f.Conflicts)
		st.Folders = append(st.Folders, f)
	}
	return st
}

// kick asks folders to sync now. An empty folder means all.
func (d *Daemon) kick(folder string) []string {
	d.mu.Lock()
	defer d.mu.Unlock()
	var kicked []string
	for _, p := range d.order {
		if folder != "" && p != folder {
			continue
		}
		select {
		case d.kicks[p] <- struct{}{}:
		default:
		}
		kicked = append(kicked, p)
	}
	return kicked
}
