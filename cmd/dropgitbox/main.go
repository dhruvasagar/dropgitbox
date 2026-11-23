// Command dropgitbox is a Dropbox-like folder sync service backed by git.
// Every change is auto-committed before it is synced, so the full history
// of every file is kept on every device and on the server.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/dhruvasagar/dropgitbox/internal/client"
	"github.com/dhruvasagar/dropgitbox/internal/server"
)

const usage = `dropgitbox - git-backed folder sync

Usage:
  dropgitbox server  [-addr :8080] [-data ./data] [-token T | -no-auth]
  dropgitbox setup   -server URL -repo NAME [-token T] [-device NAME] [-dir DIR]
  dropgitbox watch   [-dir DIR]            run the sync daemon
  dropgitbox sync    [-dir DIR]            commit and sync once
  dropgitbox status  [-dir DIR]
  dropgitbox resolve [-dir DIR] [-ours | -theirs] [-force] [PATH...]
  dropgitbox log     [-dir DIR] [-n N] [PATH...]
  dropgitbox restore [-dir DIR] -rev REV PATH...

The token may also be given in the DROPGITBOX_TOKEN environment variable.
Run "dropgitbox <command> -h" for the flags of a command.
`

func main() {
	if len(os.Args) < 2 {
		fmt.Fprint(os.Stderr, usage)
		os.Exit(2)
	}
	cmd, args := os.Args[1], os.Args[2:]

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	var err error
	switch cmd {
	case "server":
		err = cmdServer(ctx, args)
	case "setup":
		err = cmdSetup(ctx, args)
	case "watch":
		err = cmdWatch(ctx, args)
	case "sync":
		err = cmdSync(ctx, args)
	case "status":
		err = cmdStatus(ctx, args)
	case "resolve":
		err = cmdResolve(ctx, args)
	case "log":
		err = cmdLog(ctx, args)
	case "restore":
		err = cmdRestore(ctx, args)
	case "help", "-h", "--help":
		fmt.Print(usage)
		return
	default:
		fmt.Fprintf(os.Stderr, "dropgitbox: unknown command %q\n\n%s", cmd, usage)
		os.Exit(2)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "dropgitbox:", err)
		os.Exit(1)
	}
}

func newLogger(verbose bool) *slog.Logger {
	level := slog.LevelInfo
	if verbose {
		level = slog.LevelDebug
	}
	return slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: level}))
}

func flagSet(name string) *flag.FlagSet {
	return flag.NewFlagSet("dropgitbox "+name, flag.ExitOnError)
}

func cmdServer(ctx context.Context, args []string) error {
	fs := flagSet("server")
	addr := fs.String("addr", ":8080", "listen address")
	data := fs.String("data", "./data", "directory holding the repositories")
	token := fs.String("token", os.Getenv("DROPGITBOX_TOKEN"), "shared access token")
	noAuth := fs.Bool("no-auth", false, "disable authentication (trusted networks only)")
	verbose := fs.Bool("v", false, "verbose logging")
	_ = fs.Parse(args)

	if *token == "" && !*noAuth {
		return errors.New("server: set -token (or DROPGITBOX_TOKEN), or pass -no-auth explicitly")
	}
	if *noAuth {
		*token = ""
	}
	log := newLogger(*verbose)
	srv, err := server.New(server.Options{DataDir: *data, Token: *token, Logger: log})
	if err != nil {
		return err
	}

	hs := &http.Server{
		Addr:              *addr,
		Handler:           srv.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
		// Long-lived event streams end when the server shuts down.
		BaseContext: func(net.Listener) context.Context { return ctx },
	}

	errc := make(chan error, 1)
	go func() { errc <- hs.ListenAndServe() }()
	log.Info("dropgitbox server listening", "addr", *addr, "data", *data, "auth", *token != "")

	select {
	case err := <-errc:
		return err
	case <-ctx.Done():
	}
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	return hs.Shutdown(shutdownCtx)
}

func cmdSetup(ctx context.Context, args []string) error {
	fs := flagSet("setup")
	srvURL := fs.String("server", "", "server base URL, e.g. https://box.example.com")
	repo := fs.String("repo", "", "repository name on the server (created if missing)")
	token := fs.String("token", os.Getenv("DROPGITBOX_TOKEN"), "shared access token")
	device := fs.String("device", "", "device name used in commit messages (default: hostname)")
	dir := fs.String("dir", ".", "folder to sync")
	_ = fs.Parse(args)

	s, err := client.Setup(ctx, client.SetupOptions{
		Dir: *dir, Server: *srvURL, Repo: *repo, Token: *token, Device: *device, Logger: newLogger(false),
	})
	if err != nil {
		if errors.Is(err, client.ErrConflict) {
			fmt.Fprintln(os.Stderr, "Setup finished, but local files conflict with the server's. Run `dropgitbox status` and `dropgitbox resolve`.")
		}
		return err
	}
	fmt.Printf("%s is now synced with %s (repo %q).\nRun `dropgitbox watch -dir %s` to keep it in sync.\n",
		s.Dir(), s.Config().Server, s.Config().Repo, s.Dir())
	return nil
}

func cmdWatch(ctx context.Context, args []string) error {
	fs := flagSet("watch")
	dir := fs.String("dir", ".", "synced folder")
	debounce := fs.Duration("debounce", 2*time.Second, "quiet period before committing a change")
	maxDelay := fs.Duration("max-delay", 30*time.Second, "longest a burst of changes may postpone a commit")
	poll := fs.Duration("poll", 5*time.Minute, "safety-net sync interval")
	verbose := fs.Bool("v", false, "verbose logging")
	_ = fs.Parse(args)

	s, err := client.Load(*dir, newLogger(*verbose))
	if err != nil {
		return err
	}
	return s.Watch(ctx, client.DaemonOptions{Debounce: *debounce, MaxDelay: *maxDelay, Poll: *poll})
}

func cmdSync(ctx context.Context, args []string) error {
	fs := flagSet("sync")
	dir := fs.String("dir", ".", "synced folder")
	_ = fs.Parse(args)

	s, err := client.Load(*dir, newLogger(false))
	if err != nil {
		return err
	}
	if err := s.Sync(ctx); err != nil {
		if errors.Is(err, client.ErrConflict) {
			printConflictHelp(ctx, s)
		}
		return err
	}
	fmt.Println("Up to date.")
	return nil
}

func cmdStatus(ctx context.Context, args []string) error {
	fs := flagSet("status")
	dir := fs.String("dir", ".", "synced folder")
	_ = fs.Parse(args)

	s, err := client.Load(*dir, newLogger(false))
	if err != nil {
		return err
	}
	st, err := s.Status(ctx)
	if err != nil {
		return err
	}
	fmt.Printf("Folder:  %s\nServer:  %s\nRepo:    %s\nDevice:  %s\n", st.Config.Dir, st.Config.Server, st.Config.Repo, st.Config.Device)
	fmt.Printf("Head:    %s\n", orNone(st.Head))
	fmt.Printf("Server head (last fetched): %s\n", orNone(st.Remote))
	fmt.Printf("Ahead %d / behind %d, %d uncommitted change(s)\n", st.Ahead, st.Behind, len(st.Uncommitted))
	if st.Conflicted {
		fmt.Println()
		printConflictHelp(ctx, s)
	}
	return nil
}

func cmdResolve(ctx context.Context, args []string) error {
	fs := flagSet("resolve")
	dir := fs.String("dir", ".", "synced folder")
	ours := fs.Bool("ours", false, "keep this device's version of the conflicted files")
	theirs := fs.Bool("theirs", false, "keep the server's version of the conflicted files")
	force := fs.Bool("force", false, "conclude even if conflict markers remain")
	noSync := fs.Bool("no-sync", false, "only commit the resolution, don't sync")
	_ = fs.Parse(args)

	if *ours && *theirs {
		return errors.New("resolve: -ours and -theirs are mutually exclusive")
	}
	opts := client.ResolveOptions{Paths: fs.Args(), Force: *force}
	switch {
	case *ours:
		opts.Side = client.SideOurs
	case *theirs:
		opts.Side = client.SideTheirs
	}

	s, err := client.Load(*dir, newLogger(false))
	if err != nil {
		return err
	}
	head, err := s.Resolve(ctx, opts)
	if err != nil {
		return err
	}
	fmt.Printf("Conflict resolved in commit %s.\n", head[:10])
	if *noSync {
		return nil
	}
	if err := s.Sync(ctx); err != nil {
		if errors.Is(err, client.ErrConflict) {
			printConflictHelp(ctx, s)
		}
		return err
	}
	fmt.Println("Synced.")
	return nil
}

func cmdLog(ctx context.Context, args []string) error {
	fs := flagSet("log")
	dir := fs.String("dir", ".", "synced folder")
	n := fs.Int("n", 20, "number of commits to show")
	_ = fs.Parse(args)

	s, err := client.Load(*dir, newLogger(false))
	if err != nil {
		return err
	}
	gitArgs := []string{"log", fmt.Sprintf("-n%d", *n), "--date=format:%Y-%m-%d %H:%M:%S",
		"--format=%h  %ad  %s", "--stat=100", "--stat-graph-width=20"}
	if paths := fs.Args(); len(paths) > 0 {
		gitArgs = append(gitArgs, "--")
		gitArgs = append(gitArgs, paths...)
	}
	out, err := s.Git().RunRaw(ctx, gitArgs...)
	if err != nil {
		return err
	}
	fmt.Print(out)
	return nil
}

func cmdRestore(ctx context.Context, args []string) error {
	fs := flagSet("restore")
	dir := fs.String("dir", ".", "synced folder")
	rev := fs.String("rev", "", "commit to restore from (see `dropgitbox log`)")
	_ = fs.Parse(args)

	if *rev == "" || fs.NArg() == 0 {
		return errors.New("restore: -rev and at least one path are required")
	}
	s, err := client.Load(*dir, newLogger(false))
	if err != nil {
		return err
	}
	if err := s.Restore(ctx, *rev, fs.Args()); err != nil {
		return err
	}
	fmt.Printf("Restored %s from %s. The change will be committed and synced like any other edit.\n",
		strings.Join(fs.Args(), ", "), *rev)
	return nil
}

func printConflictHelp(ctx context.Context, s *client.Syncer) {
	files, _ := s.ConflictedFiles(ctx)
	fmt.Println("SYNC PAUSED: changes on this device conflict with changes from the server.")
	fmt.Println("Conflicted files:")
	for _, f := range files {
		fmt.Println("  " + f)
	}
	fmt.Println(`
To resolve, either:
  - edit each file to remove the <<<<<<< ======= >>>>>>> markers, then run: dropgitbox resolve
  - keep this device's version:   dropgitbox resolve -ours [PATH...]
  - keep the server's version:    dropgitbox resolve -theirs [PATH...]`)
}

func orNone(s string) string {
	if s == "" {
		return "(none)"
	}
	return s
}
