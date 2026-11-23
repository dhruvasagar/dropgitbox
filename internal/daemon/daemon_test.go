package daemon_test

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/dhruvasagar/dropgitbox/internal/client"
	"github.com/dhruvasagar/dropgitbox/internal/daemon"
	"github.com/dhruvasagar/dropgitbox/internal/server"
)

const token = "test-shared-token-0123456789"

var quiet = slog.New(slog.NewTextHandler(io.Discard, nil))

// down makes the test server fail every request while set.
var down atomic.Bool

type switchable struct{ h http.Handler }

func (s switchable) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if down.Load() {
		http.Error(w, "down for maintenance", http.StatusServiceUnavailable)
		return
	}
	s.h.ServeHTTP(w, r)
}

func setup(t *testing.T) (*httptest.Server, func(device string) *client.Syncer) {
	t.Helper()
	t.Setenv("GIT_CONFIG_GLOBAL", filepath.Join(t.TempDir(), "gitconfig"))
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	srv, err := server.New(server.Options{DataDir: t.TempDir(), Token: token, Logger: quiet, Heartbeat: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { srv.Close() })
	down.Store(false)
	ts := httptest.NewServer(switchable{srv.Handler()})
	t.Cleanup(ts.Close)
	return ts, func(device string) *client.Syncer {
		s, err := client.Setup(context.Background(), client.SetupOptions{
			Dir: t.TempDir(), Server: ts.URL, Repo: "docs", Token: token, Device: device, Logger: quiet,
		})
		if err != nil {
			t.Fatal(err)
		}
		return s
	}
}

// socketPath returns a short path (unix sockets have a ~104 byte limit).
func socketPath(t *testing.T) string {
	t.Helper()
	d, err := os.MkdirTemp("", "dgb")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(d) })
	return filepath.Join(d, "d.sock")
}

type running struct {
	sock   string
	cancel context.CancelFunc
	done   chan error
	hook   string // file the hook appends "event folder files" lines to
}

func start(t *testing.T, folders ...*client.Syncer) *running {
	t.Helper()
	r := &running{sock: socketPath(t), done: make(chan error, 1), hook: filepath.Join(t.TempDir(), "hook.log")}
	ctx, cancel := context.WithCancel(context.Background())
	r.cancel = cancel
	opts := daemon.Options{
		Sync:       client.DaemonOptions{Debounce: 100 * time.Millisecond, MaxDelay: time.Second, Poll: time.Hour},
		SocketPath: r.sock,
		Version:    "test",
		Logger:     quiet,
		Notify: daemon.NotifyOptions{
			Command:       []string{"sh", "-c", `printf '%s|%s|%s\n' "$DROPGITBOX_EVENT" "$DROPGITBOX_REPO" "$(echo $DROPGITBOX_FILES)" >> "$0"`, r.hook},
			CommandEvents: client.AllEvents,
		},
	}
	go func() { r.done <- daemon.Run(ctx, folders, opts) }()
	waitFor(t, 5*time.Second, func() bool {
		c, err := daemon.Dial(r.sock)
		if err != nil {
			return false
		}
		_, err = c.Status(context.Background())
		return err == nil
	})
	t.Cleanup(r.stop)
	return r
}

func (r *running) stop() {
	r.cancel()
	select {
	case <-r.done:
	case <-time.After(10 * time.Second):
	}
}

func (r *running) client(t *testing.T) *daemon.Client {
	t.Helper()
	c, err := daemon.Dial(r.sock)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func (r *running) hookLines() []string {
	b, _ := os.ReadFile(r.hook)
	return strings.Split(strings.TrimSpace(string(b)), "\n")
}

func (r *running) state(t *testing.T) daemon.FolderStatus {
	t.Helper()
	st, err := r.client(t).Status(context.Background())
	if err != nil || len(st.Folders) != 1 {
		t.Fatalf("status: %+v %v", st, err)
	}
	return st.Folders[0]
}

func waitFor(t *testing.T, timeout time.Duration, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatal("timed out")
		}
		time.Sleep(25 * time.Millisecond)
	}
}

func write(t *testing.T, s *client.Syncer, rel, content string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(s.Dir(), rel), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestLiveStatusEventsAndHooks(t *testing.T) {
	_, newDevice := setup(t)
	a, b := newDevice("a"), newDevice("b")
	da, db := start(t, a), start(t, b)

	// Stream b's events.
	var (
		mu     sync.Mutex
		events []client.Event
	)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go db.client(t).Events(ctx, func(ev client.Event) {
		mu.Lock()
		events = append(events, ev)
		mu.Unlock()
	})
	waitFor(t, 5*time.Second, func() bool { return db.state(t).State == daemon.StateIdle && !db.state(t).LastSync.IsZero() })

	write(t, a, "hello.txt", "hi\n")
	waitFor(t, 15*time.Second, func() bool {
		mu.Lock()
		defer mu.Unlock()
		return slices.ContainsFunc(events, func(ev client.Event) bool {
			return ev.Type == client.EventRemoteChanges && slices.Contains(ev.Files, "hello.txt")
		})
	})
	if st := da.state(t); st.LastSync.IsZero() || st.Repo != "docs" {
		t.Fatalf("a status = %+v", st)
	}
	// The hook saw a's own commit and b's incoming change.
	waitFor(t, 5*time.Second, func() bool {
		return slices.Contains(db.hookLines(), "remote_changes|docs|hello.txt") &&
			slices.Contains(da.hookLines(), "synced|docs|hello.txt")
	})

	// `dropgitbox sync` through the socket.
	out, err := da.client(t).Sync(context.Background(), "")
	if err != nil || len(out) != 1 || out[0].Result != "synced" {
		t.Fatalf("socket sync: %+v %v", out, err)
	}
}

func TestConflictPausesAndResumesWithEvents(t *testing.T) {
	_, newDevice := setup(t)
	a, b := newDevice("a"), newDevice("b")
	ctx := context.Background()

	write(t, a, "doc.txt", "base\n")
	if err := a.Sync(ctx); err != nil {
		t.Fatal(err)
	}
	if err := b.Sync(ctx); err != nil {
		t.Fatal(err)
	}
	write(t, a, "doc.txt", "from a\n")
	if err := a.Sync(ctx); err != nil {
		t.Fatal(err)
	}
	write(t, b, "doc.txt", "from b\n")

	db := start(t, b) // b's first sync hits the conflict
	waitFor(t, 10*time.Second, func() bool { return db.state(t).State == daemon.StateConflict })
	if st := db.state(t); !slices.Equal(st.Conflicts, []string{"doc.txt"}) {
		t.Fatalf("conflicts = %v", st.Conflicts)
	}
	out, err := db.client(t).Sync(ctx, "")
	if err != nil || len(out) != 1 || out[0].Result != "conflict" {
		t.Fatalf("sync while paused: %+v %v", out, err)
	}

	write(t, b, "doc.txt", "merged\n")
	if _, err := b.Resolve(ctx, client.ResolveOptions{}); err != nil {
		t.Fatal(err)
	}
	waitFor(t, 10*time.Second, func() bool {
		st := db.state(t)
		return st.State == daemon.StateIdle && len(st.Conflicts) == 0
	})
	waitFor(t, 5*time.Second, func() bool {
		l := db.hookLines()
		return slices.Contains(l, "conflict|docs|doc.txt") && slices.Contains(l, "resolved|docs|")
	})
	// Only one conflict notification despite the file changes and the
	// explicit sync request while paused.
	n := 0
	for _, l := range db.hookLines() {
		if strings.HasPrefix(l, "conflict|") {
			n++
		}
	}
	if n != 1 {
		t.Fatalf("conflict notified %d times:\n%s", n, strings.Join(db.hookLines(), "\n"))
	}
}

func TestSecondDaemonRefused(t *testing.T) {
	_, newDevice := setup(t)
	a := newDevice("a")
	r := start(t, a)
	err := daemon.Run(context.Background(), []*client.Syncer{a}, daemon.Options{SocketPath: r.sock, Logger: quiet})
	if !errors.Is(err, daemon.ErrAlreadyRunning) {
		t.Fatalf("want ErrAlreadyRunning, got %v", err)
	}
	if fi, err := os.Stat(r.sock); err != nil || fi.Mode().Perm() != 0o600 {
		t.Fatalf("socket perms: %v %v", fi, err)
	}
}

func TestOfflineAndOnlineEvents(t *testing.T) {
	_, newDevice := setup(t)
	a := newDevice("a")
	var (
		mu     sync.Mutex
		events []client.EventType
	)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	go func() {
		defer close(done)
		a.Watch(ctx, client.DaemonOptions{
			Debounce: 50 * time.Millisecond, Poll: 200 * time.Millisecond, OfflineAfter: 300 * time.Millisecond,
			OnEvent: func(ev client.Event) {
				mu.Lock()
				events = append(events, ev.Type)
				mu.Unlock()
			},
		})
	}()
	has := func(t client.EventType) bool {
		mu.Lock()
		defer mu.Unlock()
		return slices.Contains(events, t)
	}
	waitFor(t, 5*time.Second, func() bool { return has(client.EventSynced) })

	// Take the server away: errors, then offline after OfflineAfter.
	down.Store(true)
	defer down.Store(false)
	write(t, a, "x.txt", "x\n")
	waitFor(t, 10*time.Second, func() bool { return has(client.EventOffline) })
	if !has(client.EventError) {
		t.Fatal("no error events before offline")
	}
	cancel()
	<-done
}

func TestDescribeCoversAllEvents(t *testing.T) {
	for _, typ := range client.AllEvents {
		title, _, _ := daemon.Describe(client.Event{Type: typ, Folder: "/home/me/Box", Files: []string{"a", "b"}})
		if !strings.HasPrefix(title, "Box") {
			t.Errorf("%s: title %q", typ, title)
		}
	}
	if _, err := daemon.ParseEvents([]string{"conflict", "bogus"}); err == nil {
		t.Error("bogus event accepted")
	}
	if all, _ := daemon.ParseEvents([]string{"*"}); len(all) != len(client.AllEvents) {
		t.Error("* should mean all events")
	}
}

func TestSocketPathFallsBackWhenTooLong(t *testing.T) {
	t.Setenv(daemon.EnvSocket, "")
	t.Setenv("XDG_RUNTIME_DIR", "/run/user/1000")
	if p := daemon.SocketPath(); p != "/run/user/1000/dropgitbox/daemon.sock" {
		t.Fatalf("runtime dir path = %s", p)
	}
	t.Setenv("XDG_RUNTIME_DIR", "/"+strings.Repeat("x", 120))
	if p := daemon.SocketPath(); len(p) > 103 || !strings.HasPrefix(p, os.TempDir()) {
		t.Fatalf("long path not replaced: %s", p)
	}
}
