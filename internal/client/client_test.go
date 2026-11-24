package client_test

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
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
	var ue *client.UnresolvedError
	if _, err := b.Resolve(ctx, client.ResolveOptions{}); !errors.As(err, &ue) || len(ue.Markers) != 1 {
		t.Fatalf("want UnresolvedError with markers, got %v", err)
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

// conflictOn makes a and b change the same files differently; b's sync
// then pauses on a conflict. contents maps file -> [original, a's, b's].
func conflictOn(t *testing.T, contents map[string][3]string) (a, b *client.Syncer) {
	t.Helper()
	ts := startServer(t)
	a, b = setup(t, ts, "a"), setup(t, ts, "b")
	for f, c := range contents {
		write(t, a, f, c[0])
	}
	mustSync(t, a)
	mustSync(t, b)
	for f, c := range contents {
		write(t, a, f, c[1])
		write(t, b, f, c[2])
	}
	mustSync(t, a)
	if err := b.Sync(context.Background()); !errors.Is(err, client.ErrConflict) {
		t.Fatalf("want conflict, got %v", err)
	}
	return a, b
}

func TestResolveFileByFile(t *testing.T) {
	a, b := conflictOn(t, map[string][3]string{
		"one.txt": {"x\n", "a1\n", "b1\n"},
		"two.txt": {"x\n", "a2\n", "b2\n"},
	})
	ctx := context.Background()

	cs, err := b.Conflicts(ctx)
	if err != nil || len(cs) != 2 || cs[0].Path != "one.txt" || cs[0].Kind != client.BothModified || cs[0].Binary {
		t.Fatalf("conflicts = %+v, %v", cs, err)
	}

	write(t, b, "one.txt", "merged 1\n")
	res, err := b.Resolve(ctx, client.ResolveOptions{Paths: []string{"one.txt"}})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Remaining) != 1 || res.Remaining[0].Path != "two.txt" || res.Commit != "" || !b.Conflicted() {
		t.Fatalf("after one file: %+v, conflicted=%v", res, b.Conflicted())
	}
	// A file that isn't conflicted (any more) is an error, naming the rest.
	var nc *client.NotConflictedError
	if _, err := b.Resolve(ctx, client.ResolveOptions{Paths: []string{"one.txt"}}); !errors.As(err, &nc) ||
		len(nc.Conflicted) != 1 {
		t.Fatalf("want NotConflictedError, got %v", err)
	}

	res, err = b.Resolve(ctx, client.ResolveOptions{Paths: []string{"two.txt"}, Side: client.SideTheirs})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Remaining) != 0 || res.Commit == "" || b.Conflicted() {
		t.Fatalf("after both files: %+v, conflicted=%v", res, b.Conflicted())
	}
	if _, err := b.Resolve(ctx, client.ResolveOptions{}); !errors.Is(err, client.ErrNoConflict) {
		t.Fatalf("want ErrNoConflict, got %v", err)
	}
	mustSync(t, b)
	mustSync(t, a)
	if read(t, a, "one.txt") != "merged 1\n" || read(t, a, "two.txt") != "a2\n" {
		t.Fatalf("a has %q, %q", read(t, a, "one.txt"), read(t, a, "two.txt"))
	}
}

func TestResolveBinaryNeedsAChoice(t *testing.T) {
	_, b := conflictOn(t, map[string][3]string{
		"photo.bin": {"\x00orig", "\x00from a", "\x00from b"},
		"notes.txt": {"x\n", "a\n", "b\n"},
	})
	ctx := context.Background()
	cs, err := b.Conflicts(ctx)
	if err != nil || len(cs) != 2 || cs[1].Path != "photo.bin" || !cs[1].Binary || !cs[1].NeedsChoice() {
		t.Fatalf("conflicts = %+v, %v", cs, err)
	}

	// Plain resolve won't silently keep this device's binary.
	write(t, b, "notes.txt", "a and b\n")
	var ue *client.UnresolvedError
	if _, err := b.Resolve(ctx, client.ResolveOptions{}); !errors.As(err, &ue) ||
		len(ue.NeedChoice) != 1 || ue.NeedChoice[0].Path != "photo.bin" || len(ue.Markers) != 0 {
		t.Fatalf("want UnresolvedError for photo.bin, got %v", err)
	}
	if left, _ := b.ConflictedFiles(ctx); len(left) != 2 {
		t.Fatalf("a refused resolve must change nothing, conflicted = %v", left)
	}
	// Naming it keeps it as it is, like git add.
	res, err := b.Resolve(ctx, client.ResolveOptions{Paths: []string{"photo.bin", "notes.txt"}})
	if err != nil || res.Commit == "" {
		t.Fatalf("resolve named: %+v, %v", res, err)
	}
	if got := read(t, b, "photo.bin"); got != "\x00from b" {
		t.Fatalf("photo.bin = %q", got)
	}
}

func TestResolveModifyDeleteNeedsAChoice(t *testing.T) {
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
	cs, _ := b.Conflicts(ctx)
	if len(cs) != 1 || cs[0].Kind != client.DeletedByThem {
		t.Fatalf("conflicts = %+v", cs)
	}
	var ue *client.UnresolvedError
	if _, err := b.Resolve(ctx, client.ResolveOptions{}); !errors.As(err, &ue) || len(ue.NeedChoice) != 1 {
		t.Fatalf("want UnresolvedError, got %v", err)
	}
	if _, err := b.Resolve(ctx, client.ResolveOptions{Side: client.SideOurs}); err != nil {
		t.Fatal(err)
	}
	mustSync(t, b)
	mustSync(t, a)
	if got := read(t, a, "doc.txt"); got != "edited on b\n" {
		t.Fatalf("a doc.txt = %q", got)
	}
}

func TestEditsWhilePausedAreNotInTheMerge(t *testing.T) {
	_, b := conflictOn(t, map[string][3]string{"doc.txt": {"x\n", "a\n", "b\n"}})
	ctx := context.Background()
	write(t, b, "later.txt", "written while paused\n")
	if _, err := b.Resolve(ctx, client.ResolveOptions{Side: client.SideOurs}); err != nil {
		t.Fatal(err)
	}
	merged := git(t, b, "diff", "--name-only", "HEAD^1", "HEAD")
	if strings.Contains(merged, "later.txt") {
		t.Fatalf("the merge commit took in later.txt: %q", merged)
	}
	if st := git(t, b, "status", "--porcelain"); !strings.Contains(st, "later.txt") {
		t.Fatalf("later.txt should still be uncommitted, status %q", st)
	}
	mustSync(t, b)
	if msg := git(t, b, "log", "-1", "--format=%s", "--", "later.txt"); !strings.Contains(msg, "later.txt") {
		t.Fatalf("later.txt should get its own commit, got %q", msg)
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

func setupErr(ts *httptest.Server, dir, repo string) error {
	_, err := client.Setup(context.Background(), client.SetupOptions{
		Dir: dir, Server: ts.URL, Repo: repo, Token: token, Device: "d", Logger: quiet,
	})
	return err
}

func TestSetupRefusesNestedFolders(t *testing.T) {
	ts := startServer(t)
	outer := setup(t, ts, "a")

	// Inside a synced folder, including through a symlink.
	inner := filepath.Join(outer.Dir(), "Work")
	if err := setupErr(ts, inner, "work"); !errors.Is(err, client.ErrNested) {
		t.Fatalf("inside a synced folder: want ErrNested, got %v", err)
	}
	link := filepath.Join(t.TempDir(), "link")
	if err := os.Symlink(outer.Dir(), link); err != nil {
		t.Fatal(err)
	}
	if err := setupErr(ts, filepath.Join(link, "Work"), "work"); !errors.Is(err, client.ErrNested) {
		t.Fatalf("inside a synced folder via a symlink: want ErrNested, got %v", err)
	}

	// Containing a git repository, synced or not.
	plain := t.TempDir()
	if out, err := exec.Command("git", "init", "-q", filepath.Join(plain, "deep", "repo")).CombinedOutput(); err != nil {
		t.Fatalf("%v: %s", err, out)
	}
	err := setupErr(ts, plain, "plain")
	if !errors.Is(err, client.ErrNested) || !strings.Contains(err.Error(), filepath.Join("deep", "repo")) {
		t.Fatalf("containing a repo: want ErrNested naming deep/repo, got %v", err)
	}
	if err := setupErr(ts, filepath.Dir(outer.Dir()), "parent"); !errors.Is(err, client.ErrNested) {
		t.Fatalf("containing a synced folder: want ErrNested, got %v", err)
	}

	// Refused before anything was created on the server.
	repos, err := client.ListRepos(context.Background(), ts.URL, token)
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range repos {
		if r.Name != "docs" {
			t.Errorf("refused setup still created repo %q on the server", r.Name)
		}
	}

	// An ordinary git repository around the folder (e.g. dotfiles in ~) is fine.
	home := t.TempDir()
	if out, err := exec.Command("git", "init", "-q", home).CombinedOutput(); err != nil {
		t.Fatalf("%v: %s", err, out)
	}
	if err := setupErr(ts, filepath.Join(home, "Box"), "box"); err != nil {
		t.Fatalf("inside a plain git repo should be allowed: %v", err)
	}
}

// A repository cloned into a synced folder later is never committed as a
// gitlink: the sync refuses, and nothing about it reaches the server.
func TestSyncRefusesEmbeddedRepo(t *testing.T) {
	ts := startServer(t)
	a := setup(t, ts, "a")
	write(t, a, "keep.txt", "fine\n")
	mustSync(t, a)
	if out, err := exec.Command("git", "init", "-q", filepath.Join(a.Dir(), "cloned")).CombinedOutput(); err != nil {
		t.Fatalf("%v: %s", err, out)
	}
	sub := &gitx.Repo{Dir: filepath.Join(a.Dir(), "cloned")}
	if err := os.WriteFile(filepath.Join(sub.Dir, "f"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := sub.Run(context.Background(), "-c", "user.name=t", "-c", "user.email=t@t", "commit", "-qm", "x", "--allow-empty"); err != nil {
		t.Fatal(err)
	}
	write(t, a, "other.txt", "also changed\n")

	err := a.Sync(context.Background())
	if !errors.Is(err, client.ErrNested) || !strings.Contains(err.Error(), "cloned") {
		t.Fatalf("want ErrNested naming cloned, got %v", err)
	}
	if out, _ := a.Git().Run(context.Background(), "ls-files", "--stage", "cloned"); strings.TrimSpace(out) != "" {
		t.Fatalf("nested repo left in the index: %q", out)
	}
	if out, _ := a.Git().Run(context.Background(), "log", "--format=%s", "-1"); strings.Contains(out, "cloned") {
		t.Fatalf("nested repo was committed: %q", out)
	}
}
