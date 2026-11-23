package client_test

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/dhruvasagar/dropgitbox/internal/client"
	"github.com/dhruvasagar/dropgitbox/internal/gitx"
	"github.com/dhruvasagar/dropgitbox/internal/server"
)

const token = "test-shared-token-0123456789"

var quiet = slog.New(slog.NewTextHandler(io.Discard, nil))

// isolateGit keeps the user's own git config out of the tests.
func isolateGit(t *testing.T) {
	t.Helper()
	t.Setenv("GIT_CONFIG_GLOBAL", filepath.Join(t.TempDir(), "gitconfig"))
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
}

func startServer(t *testing.T) *httptest.Server {
	t.Helper()
	isolateGit(t)
	srv, err := server.New(server.Options{DataDir: t.TempDir(), Token: token, Logger: quiet, Heartbeat: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)
	return ts
}

func setup(t *testing.T, ts *httptest.Server, device string) *client.Syncer {
	t.Helper()
	return setupIn(t, ts, device, t.TempDir())
}

func setupIn(t *testing.T, ts *httptest.Server, device, dir string) *client.Syncer {
	t.Helper()
	s, err := client.Setup(context.Background(), client.SetupOptions{
		Dir: dir, Server: ts.URL, Repo: "docs", Token: token, Device: device, Logger: quiet,
	})
	if err != nil {
		t.Fatalf("setup %s: %v", device, err)
	}
	return s
}

func write(t *testing.T, s *client.Syncer, rel, content string) {
	t.Helper()
	p := filepath.Join(s.Dir(), rel)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func read(t *testing.T, s *client.Syncer, rel string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(s.Dir(), rel))
	if err != nil {
		t.Fatalf("read %s: %v", rel, err)
	}
	return string(b)
}

func mustSync(t *testing.T, s *client.Syncer) {
	t.Helper()
	if err := s.Sync(context.Background()); err != nil {
		t.Fatalf("sync %s: %v", s.Config().Device, err)
	}
}

func git(t *testing.T, s *client.Syncer, args ...string) string {
	t.Helper()
	out, err := s.Git().Run(context.Background(), args...)
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func TestSyncPropagatesChangesAndKeepsHistory(t *testing.T) {
	ts := startServer(t)
	a, b := setup(t, ts, "laptop"), setup(t, ts, "desktop")

	write(t, a, "notes/todo.txt", "v1\n")
	mustSync(t, a)
	mustSync(t, b)
	if got := read(t, b, "notes/todo.txt"); got != "v1\n" {
		t.Fatalf("b sees %q, want v1", got)
	}

	write(t, b, "notes/todo.txt", "v2\n")
	mustSync(t, b)
	mustSync(t, a)
	if got := read(t, a, "notes/todo.txt"); got != "v2\n" {
		t.Fatalf("a sees %q, want v2", got)
	}

	// Both versions are in history, with descriptive auto-commit messages.
	log := git(t, a, "log", "--format=%s", "--", "notes/todo.txt")
	if want := "Modified notes/todo.txt\nAdded notes/todo.txt"; log != want {
		t.Fatalf("history:\n%s\nwant:\n%s", log, want)
	}
	if old := git(t, a, "show", "HEAD~1:notes/todo.txt"); old != "v1" {
		t.Fatalf("old version = %q", old)
	}
	body := git(t, a, "log", "-1", "--format=%b")
	if !strings.Contains(body, "desktop") {
		t.Fatalf("commit body should name the device: %q", body)
	}

	// Deletions sync too.
	if err := os.Remove(filepath.Join(a.Dir(), "notes/todo.txt")); err != nil {
		t.Fatal(err)
	}
	mustSync(t, a)
	mustSync(t, b)
	if _, err := os.Stat(filepath.Join(b.Dir(), "notes/todo.txt")); !os.IsNotExist(err) {
		t.Fatalf("deleted file still present on b: %v", err)
	}
}

func TestConcurrentEditsToDifferentFilesMerge(t *testing.T) {
	ts := startServer(t)
	a, b := setup(t, ts, "a"), setup(t, ts, "b")

	write(t, a, "shared.txt", "line1\nline2\nline3\n")
	mustSync(t, a)
	mustSync(t, b)

	write(t, a, "from-a.txt", "a\n")
	write(t, b, "from-b.txt", "b\n")
	write(t, a, "shared.txt", "LINE1\nline2\nline3\n") // non-overlapping edits
	write(t, b, "shared.txt", "line1\nline2\nLINE3\n")
	mustSync(t, a)
	mustSync(t, b) // merges a's commit, pushes merge
	mustSync(t, a)

	for _, s := range []*client.Syncer{a, b} {
		if read(t, s, "from-a.txt") != "a\n" || read(t, s, "from-b.txt") != "b\n" {
			t.Fatalf("%s missing files", s.Config().Device)
		}
		if got := read(t, s, "shared.txt"); got != "LINE1\nline2\nLINE3\n" {
			t.Fatalf("%s shared.txt = %q", s.Config().Device, got)
		}
	}
	if git(t, a, "rev-parse", "HEAD") != git(t, b, "rev-parse", "HEAD") {
		t.Fatal("devices did not converge on the same commit")
	}
}

func TestConflictPausesSyncUntilResolved(t *testing.T) {
	ts := startServer(t)
	a, b := setup(t, ts, "a"), setup(t, ts, "b")
	ctx := context.Background()

	write(t, a, "doc.txt", "original\n")
	mustSync(t, a)
	mustSync(t, b)

	write(t, a, "doc.txt", "from a\n")
	write(t, b, "doc.txt", "from b\n")
	mustSync(t, a)
	if err := b.Sync(ctx); !errors.Is(err, client.ErrConflict) {
		t.Fatalf("want ErrConflict, got %v", err)
	}
	if !b.Conflicted() {
		t.Fatal("b should be paused")
	}
	files, err := b.ConflictedFiles(ctx)
	if err != nil || len(files) != 1 || files[0] != "doc.txt" {
		t.Fatalf("conflicted files = %v, %v", files, err)
	}

	// While paused, further edits are neither committed nor pushed.
	write(t, b, "other.txt", "x\n")
	if err := b.Sync(ctx); !errors.Is(err, client.ErrConflict) {
		t.Fatalf("paused sync should still report conflict, got %v", err)
	}
	if _, err := b.CommitLocal(ctx); !errors.Is(err, client.ErrConflict) {
		t.Fatalf("paused commit should report conflict, got %v", err)
	}

	// Refuses to conclude while markers remain.
	var me *client.MarkersError
	if _, err := b.Resolve(ctx, client.ResolveOptions{}); !errors.As(err, &me) {
		t.Fatalf("want MarkersError, got %v", err)
	}

	write(t, b, "doc.txt", "from a and b\n")
	if _, err := b.Resolve(ctx, client.ResolveOptions{}); err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if b.Conflicted() {
		t.Fatal("still conflicted after resolve")
	}
	mustSync(t, b)
	mustSync(t, a)
	if got := read(t, a, "doc.txt"); got != "from a and b\n" {
		t.Fatalf("a doc.txt = %q", got)
	}
	if got := read(t, a, "other.txt"); got != "x\n" {
		t.Fatalf("a other.txt = %q", got)
	}
	// Both conflicting versions survive in history.
	for _, v := range []string{"from a", "from b"} {
		if !strings.Contains(git(t, a, "log", "-p", "--all", "--", "doc.txt"), v) {
			t.Fatalf("history lost %q", v)
		}
	}
}

func TestResolveTheirsAndOurs(t *testing.T) {
	for _, tc := range []struct {
		side client.Side
		want string
	}{{client.SideTheirs, "from a\n"}, {client.SideOurs, "from b\n"}} {
		ts := startServer(t)
		a, b := setup(t, ts, "a"), setup(t, ts, "b")
		ctx := context.Background()

		write(t, a, "doc.txt", "original\n")
		mustSync(t, a)
		mustSync(t, b)
		write(t, a, "doc.txt", "from a\n")
		write(t, b, "doc.txt", "from b\n")
		mustSync(t, a)
		if err := b.Sync(ctx); !errors.Is(err, client.ErrConflict) {
			t.Fatalf("want conflict, got %v", err)
		}
		if _, err := b.Resolve(ctx, client.ResolveOptions{Side: tc.side}); err != nil {
			t.Fatal(err)
		}
		mustSync(t, b)
		mustSync(t, a)
		if got := read(t, a, "doc.txt"); got != tc.want {
			t.Fatalf("side %v: got %q want %q", tc.side, got, tc.want)
		}
	}
}

func TestResolveModifyDeleteConflict(t *testing.T) {
	ts := startServer(t)
	a, b := setup(t, ts, "a"), setup(t, ts, "b")
	ctx := context.Background()

	write(t, a, "doc.txt", "original\n")
	mustSync(t, a)
	mustSync(t, b)
	if err := os.Remove(filepath.Join(a.Dir(), "doc.txt")); err != nil {
		t.Fatal(err)
	}
	write(t, b, "doc.txt", "edited on b\n")
	mustSync(t, a)
	if err := b.Sync(ctx); !errors.Is(err, client.ErrConflict) {
		t.Fatalf("want conflict, got %v", err)
	}
	// "theirs" (the server) deleted it.
	if _, err := b.Resolve(ctx, client.ResolveOptions{Side: client.SideTheirs}); err != nil {
		t.Fatal(err)
	}
	mustSync(t, b)
	if _, err := os.Stat(filepath.Join(b.Dir(), "doc.txt")); !os.IsNotExist(err) {
		t.Fatalf("doc.txt should be deleted, stat err = %v", err)
	}
}

func TestSetupMergesExistingFiles(t *testing.T) {
	ts := startServer(t)
	a := setup(t, ts, "a")
	write(t, a, "server-file.txt", "s\n")
	mustSync(t, a)

	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "local-file.txt"), []byte("l\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	b := setupIn(t, ts, "b", dir)
	if read(t, b, "server-file.txt") != "s\n" || read(t, b, "local-file.txt") != "l\n" {
		t.Fatal("setup should keep local files and bring in the server's")
	}
	mustSync(t, a)
	if read(t, a, "local-file.txt") != "l\n" {
		t.Fatal("local files from b did not reach a")
	}
}

func TestRestoreBringsBackOldVersionAsNewCommit(t *testing.T) {
	ts := startServer(t)
	a := setup(t, ts, "a")
	ctx := context.Background()

	write(t, a, "f.txt", "good\n")
	mustSync(t, a)
	good := git(t, a, "rev-parse", "HEAD")
	write(t, a, "f.txt", "oops\n")
	mustSync(t, a)

	if err := a.Restore(ctx, good, []string{"f.txt"}); err != nil {
		t.Fatal(err)
	}
	mustSync(t, a)
	if got := read(t, a, "f.txt"); got != "good\n" {
		t.Fatalf("f.txt = %q", got)
	}
	if n := git(t, a, "rev-list", "--count", "HEAD"); n != "3" {
		t.Fatalf("restore should add a commit, not rewrite history; commits = %s", n)
	}
}

func TestStatus(t *testing.T) {
	ts := startServer(t)
	a := setup(t, ts, "a")
	ctx := context.Background()

	write(t, a, "f.txt", "x\n")
	st, err := a.Status(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(st.Uncommitted) != 1 || st.Conflicted {
		t.Fatalf("status before sync = %+v", st)
	}
	mustSync(t, a)
	if st, err = a.Status(ctx); err != nil {
		t.Fatal(err)
	}
	if len(st.Uncommitted) != 0 || st.Ahead != 0 || st.Behind != 0 || st.Head != st.Remote {
		t.Fatalf("status after sync = %+v", st)
	}
}

// TestWatchDaemonSyncsLive runs two daemons and checks that a change on
// one device shows up on the other without any polling: the file watcher
// commits it and the server's push notification wakes the other device.
func TestWatchDaemonSyncsLive(t *testing.T) {
	ts := startServer(t)
	a, b := setup(t, ts, "a"), setup(t, ts, "b")

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	opts := client.DaemonOptions{Debounce: 100 * time.Millisecond, MaxDelay: time.Second, Poll: time.Hour}
	done := make(chan struct{}, 2)
	for _, s := range []*client.Syncer{a, b} {
		go func() {
			if err := s.Watch(ctx, opts); err != nil {
				t.Errorf("watch %s: %v", s.Config().Device, err)
			}
			done <- struct{}{}
		}()
	}
	time.Sleep(300 * time.Millisecond) // let watchers and streams attach

	write(t, a, "deep/new/dir/hello.txt", "hi from a\n")
	waitFor(t, 15*time.Second, func() bool {
		b, err := os.ReadFile(filepath.Join(b.Dir(), "deep/new/dir/hello.txt"))
		return err == nil && string(b) == "hi from a\n"
	})

	write(t, b, "deep/new/dir/hello.txt", "edited on b\n")
	waitFor(t, 15*time.Second, func() bool {
		b, err := os.ReadFile(filepath.Join(a.Dir(), "deep/new/dir/hello.txt"))
		return err == nil && string(b) == "edited on b\n"
	})

	cancel()
	<-done
	<-done
}

func waitFor(t *testing.T, timeout time.Duration, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatal("timed out waiting for sync")
		}
		time.Sleep(50 * time.Millisecond)
	}
}

func TestServerRejectsBadToken(t *testing.T) {
	ts := startServer(t)
	_, err := client.Setup(context.Background(), client.SetupOptions{
		Dir: t.TempDir(), Server: ts.URL, Repo: "docs", Token: "wrong", Logger: quiet,
	})
	if err == nil || !strings.Contains(err.Error(), "401") {
		t.Fatalf("want 401, got %v", err)
	}

	for _, path := range []string{"/api/v1/repos", "/git/docs.git/info/refs?service=git-upload-pack"} {
		resp, err := http.Get(ts.URL + path)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusUnauthorized {
			t.Fatalf("%s without token: %s", path, resp.Status)
		}
	}
}

func TestServerRefusesHistoryRewrite(t *testing.T) {
	ts := startServer(t)
	a := setup(t, ts, "a")
	ctx := context.Background()

	write(t, a, "f.txt", "1\n")
	mustSync(t, a)
	write(t, a, "f.txt", "2\n")
	mustSync(t, a)

	g := a.Git()
	if _, err := g.Run(ctx, "reset", "--hard", "HEAD~1"); err != nil {
		t.Fatal(err)
	}
	_, err := g.Run(ctx, "push", "--force", "origin", "HEAD:refs/heads/main")
	if err == nil {
		t.Fatal("force push should be rejected")
	}
	if gitx.ExitCode(err) <= 0 {
		t.Fatalf("unexpected error: %v", err)
	}
	if _, err := g.Run(ctx, "push", "origin", ":refs/heads/main"); err == nil {
		t.Fatal("branch deletion should be rejected")
	}
}

func TestRevokedDeviceGetsClearError(t *testing.T) {
	isolateGit(t)
	dataDir := t.TempDir()
	srv, err := server.New(server.Options{DataDir: dataDir, Logger: quiet})
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	tok, err := server.AddDevice(dataDir, "phone")
	if err != nil {
		t.Fatal(err)
	}
	s, err := client.Setup(context.Background(), client.SetupOptions{
		Dir: t.TempDir(), Server: ts.URL, Repo: "docs", Token: tok, TokenFromConfig: true, Logger: quiet,
	})
	if err != nil {
		t.Fatal(err)
	}
	// TokenFromConfig: the token is not copied into the folder...
	if _, err := os.Stat(filepath.Join(s.Dir(), ".git", "dropgitbox-token")); !os.IsNotExist(err) {
		t.Fatalf("token copied into folder: %v", err)
	}
	// ...but a fresh Load picks it up from the caller (the config file).
	s, err = client.Load(s.Dir(), client.LoadOptions{Logger: quiet, Token: tok})
	if err != nil {
		t.Fatal(err)
	}
	write(t, s, "f.txt", "x\n")
	mustSync(t, s)

	if err := server.RevokeDevice(dataDir, "phone"); err != nil {
		t.Fatal(err)
	}
	write(t, s, "f.txt", "y\n")
	if err := s.Sync(context.Background()); !errors.Is(err, client.ErrUnauthorized) {
		t.Fatalf("want ErrUnauthorized, got %v", err)
	}
}

func TestServerRecordsDeviceActivity(t *testing.T) {
	isolateGit(t)
	dataDir := t.TempDir()
	srv, err := server.New(server.Options{DataDir: dataDir, Logger: quiet, ActivityFlush: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()
	setupDev := func(name string) *client.Syncer {
		tok, err := server.AddDevice(dataDir, name)
		if err != nil {
			t.Fatal(err)
		}
		s, err := client.Setup(context.Background(), client.SetupOptions{
			Dir: t.TempDir(), Server: ts.URL, Repo: "docs", Token: tok, Device: name, Logger: quiet,
		})
		if err != nil {
			t.Fatal(err)
		}
		return s
	}
	a, b := setupDev("a"), setupDev("b")
	write(t, a, "f.txt", "1\n")
	mustSync(t, a)
	write(t, a, "f.txt", "a\n")
	write(t, b, "f.txt", "b\n")
	mustSync(t, a)
	if err := b.Sync(context.Background()); !errors.Is(err, client.ErrConflict) {
		t.Fatalf("want conflict: %v", err)
	}

	act := srv.Activity()
	ra, rb := act["a"].Repos["docs"], act["b"].Repos["docs"]
	if ra.State != server.StateOK || ra.LastPush.IsZero() || ra.LastFetch.IsZero() {
		t.Fatalf("a = %+v", ra)
	}
	head, _ := a.Head(context.Background())
	if ra.Head != head || ra.PushHead != head {
		t.Fatalf("a head %s, reported %s, pushed %s", head, ra.Head, ra.PushHead)
	}
	if rb.State != server.StateConflict || len(rb.Conflicts) != 1 || rb.Conflicts[0] != "f.txt" {
		t.Fatalf("b = %+v", rb)
	}
	if act["a"].ClientVersion == "" {
		t.Fatal("client version not recorded")
	}
}

// History protection is enforced server-wide, even if a repository's own
// config is weakened.
func TestHistoryProtectionIgnoresRepoConfig(t *testing.T) {
	isolateGit(t)
	dataDir := t.TempDir()
	srv, err := server.New(server.Options{DataDir: dataDir, Token: token, Logger: quiet})
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()
	a, err := client.Setup(context.Background(), client.SetupOptions{Dir: t.TempDir(), Server: ts.URL, Repo: "docs", Token: token, Logger: quiet})
	if err != nil {
		t.Fatal(err)
	}
	write(t, a, "f.txt", "1\n")
	mustSync(t, a)
	write(t, a, "f.txt", "2\n")
	mustSync(t, a)

	bare := &gitx.Repo{Dir: filepath.Join(dataDir, "docs.git")}
	ctx := context.Background()
	for _, k := range []string{"receive.denyNonFastForwards", "receive.denyDeletes"} {
		if err := bare.SetConfig(ctx, k, "false"); err != nil {
			t.Fatal(err)
		}
	}
	g := a.Git()
	if _, err := g.Run(ctx, "reset", "--hard", "HEAD~1"); err != nil {
		t.Fatal(err)
	}
	if _, err := g.Run(ctx, "push", "--force", "origin", "HEAD:refs/heads/main"); err == nil {
		t.Fatal("force push accepted after weakening repo config")
	}
	if _, err := g.Run(ctx, "push", "origin", ":refs/heads/main"); err == nil {
		t.Fatal("branch deletion accepted after weakening repo config")
	}
}
