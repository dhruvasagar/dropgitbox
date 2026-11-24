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
	StateStopped      = "stopped"      // not syncing until fixed (see LastError)
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

	// Reload, if set, returns the folders the daemon should sync now. It
	// is called whenever ConfigPath changes, so folders added with
	// `dropgitbox setup` (or removed from the config) start or stop
	// syncing without a restart. With Reload set, the daemon keeps running
	// even with no folders; without it, Run returns once every folder has
	// stopped.
	Reload     func(context.Context) ([]*client.Syncer, error)
	ConfigPath string
	// ReloadInterval is how often ConfigPath is checked. Default 2s.
	ReloadInterval time.Duration
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
	running map[string]*folderRun
	subs    map[chan client.Event]struct{}
	wg      sync.WaitGroup // folder goroutines
}

// folderRun is one folder's sync loop.
type folderRun struct {
	syncer *client.Syncer
	cancel context.CancelFunc
	done   chan struct{} // closed when the loop has exited
}

func (r *folderRun) exited() bool {
	select {
	case <-r.done:
		return true
	default:
		return false
	}
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
	if opts.ReloadInterval <= 0 {
		opts.ReloadInterval = 2 * time.Second
	}
	d := &Daemon{
		opts:    opts,
		log:     opts.Logger,
		started: time.Now(),
		folders: map[string]*FolderStatus{},
		kicks:   map[string]chan struct{}{},
		running: map[string]*folderRun{},
		subs:    map[chan client.Event]struct{}{},
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

	for _, s := range folders {
		d.startFolder(ctx, s)
	}
	d.log.Info("dropgitbox daemon running", "folders", len(folders), "socket", opts.SocketPath, "version", opts.Version)

	var result error
	if opts.Reload != nil {
		d.watchConfig(ctx) // until ctx is cancelled
	} else {
		d.wg.Wait()
		if ctx.Err() == nil && len(folders) > 0 {
			result = d.stoppedError()
		}
	}

	cancel()
	d.wg.Wait()
	<-srvDone
	notifiers.Wait()
	return result
}

// stoppedError explains why every folder stopped, once the daemon is
// about to exit and `status` can no longer be asked.
func (d *Daemon) stoppedError() error {
	d.mu.Lock()
	defer d.mu.Unlock()
	var errs []error
	for _, p := range d.order {
		if f := d.folders[p]; f.State == StateStopped {
			errs = append(errs, fmt.Errorf("%s: %s", p, f.LastError))
		}
	}
	return fmt.Errorf("no folders left to sync:\n%w", errors.Join(errs...))
}

// startFolder starts syncing s, unless it is already syncing. A folder
// whose loop has stopped (e.g. with an error) is started again.
func (d *Daemon) startFolder(ctx context.Context, s *client.Syncer) {
	dir := s.Dir()
	d.mu.Lock()
	if r := d.running[dir]; r != nil && !r.exited() {
		d.mu.Unlock()
		return
	}
	if d.folders[dir] == nil {
		d.order = append(d.order, dir)
	}
	d.folders[dir] = &FolderStatus{Folder: dir, Repo: s.Config().Repo, State: StateIdle, Since: time.Now()}
	kick := make(chan struct{}, 1)
	d.kicks[dir] = kick
	fctx, cancel := context.WithCancel(ctx)
	r := &folderRun{syncer: s, cancel: cancel, done: make(chan struct{})}
	d.running[dir] = r
	d.wg.Add(1)
	d.mu.Unlock()

	o := d.opts.Sync
	o.OnEvent = d.publish
	o.Kick = kick
	go func() {
		defer d.wg.Done()
		defer close(r.done)
		defer cancel()
		// Checked here as well as at setup, for folders set up before the
		// check existed and for repositories created inside a folder since.
		if err := client.CheckNesting(fctx, dir); err != nil {
			if fctx.Err() == nil {
				d.log.Error("not syncing folder", "folder", dir, "err", err)
				d.setStopped(dir, err)
			}
			return
		}
		if err := s.Watch(fctx, o); err != nil {
			d.log.Error("folder stopped", "folder", dir, "err", err)
			d.setStopped(dir, err)
		}
	}()
}

// stopFolder stops syncing dir and forgets it. Its files stay as they are.
func (d *Daemon) stopFolder(dir string) {
	d.mu.Lock()
	r := d.running[dir]
	d.mu.Unlock()
	if r == nil {
		return
	}
	r.cancel()
	<-r.done
	d.mu.Lock()
	delete(d.running, dir)
	delete(d.folders, dir)
	delete(d.kicks, dir)
	d.order = slices.DeleteFunc(d.order, func(p string) bool { return p == dir })
	d.mu.Unlock()
}

// watchConfig applies config changes until ctx is cancelled: new folders
// start syncing, removed ones stop, and stopped ones get another try.
func (d *Daemon) watchConfig(ctx context.Context) {
	last := statFile(d.opts.ConfigPath)
	t := time.NewTicker(d.opts.ReloadInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		cur := statFile(d.opts.ConfigPath)
		if sameStat(last, cur) {
			continue
		}
		last = cur
		d.reload(ctx)
	}
}

func (d *Daemon) reload(ctx context.Context) {
	folders, err := d.opts.Reload(ctx)
	if err != nil {
		d.log.Warn("config changed but could not be loaded; keeping the current folders", "err", err)
		return
	}
	want := map[string]bool{}
	for _, s := range folders {
		want[s.Dir()] = true
	}
	d.mu.Lock()
	var gone []string
	for dir := range d.running {
		if !want[dir] {
			gone = append(gone, dir)
		}
	}
	d.mu.Unlock()
	for _, dir := range gone {
		d.log.Info("folder removed from the config; no longer syncing it", "folder", dir)
		d.stopFolder(dir)
	}
	for _, s := range folders {
		d.mu.Lock()
		r := d.running[s.Dir()]
		d.mu.Unlock()
		switch {
		case r == nil:
			d.log.Info("folder added to the config; syncing it", "folder", s.Dir(), "repo", s.Config().Repo)
		case r.exited():
			d.log.Info("config changed; retrying stopped folder", "folder", s.Dir())
		default:
			continue
		}
		d.startFolder(ctx, s)
	}
}

// fileStat is what's compared to notice a config change: config edits are
// atomic renames, so the inode changes even when size and mtime don't.
type fileStat struct {
	info os.FileInfo
	err  error
}

func statFile(path string) fileStat {
	fi, err := os.Stat(path)
	return fileStat{fi, err}
}

func sameStat(a, b fileStat) bool {
	if a.err != nil || b.err != nil {
		return (a.err != nil) == (b.err != nil)
	}
	return os.SameFile(a.info, b.info) && a.info.Size() == b.info.Size() && a.info.ModTime().Equal(b.info.ModTime())
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

func (d *Daemon) setStopped(folder string, err error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if f := d.folders[folder]; f != nil {
		f.State, f.Since, f.LastError = StateStopped, time.Now(), err.Error()
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

// stillConflicted reports whether a folder's merge is still unfinished. Its
// state lags a resolve by up to a couple of seconds, so look at git itself.
func (d *Daemon) stillConflicted(folder string) bool {
	d.mu.Lock()
	r := d.running[folder]
	d.mu.Unlock()
	return r == nil || r.syncer.Conflicted()
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
