package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"text/tabwriter"
	"time"

	"github.com/dhruvasagar/dropgitbox/internal/client"
	"github.com/dhruvasagar/dropgitbox/internal/config"
	"github.com/dhruvasagar/dropgitbox/internal/server"
)

// clientEnv bundles the config and credentials client commands share.
type clientEnv struct {
	cfg *config.Config
	log *slog.Logger
	// token is the device token from $DROPGITBOX_TOKEN or the config file.
	token string
	// tokenFromConfig is true when token came from the config file (and
	// should therefore not be copied into folders).
	tokenFromConfig bool
}

func loadClient(cfgPath string, verbose bool) (*clientEnv, error) {
	cfg, err := loadConfig(cfgPath)
	if err != nil {
		return nil, err
	}
	e := &clientEnv{cfg: cfg, log: newLogger(verbose)}
	if t := os.Getenv(config.EnvToken); t != "" {
		e.token = t
	} else {
		if e.token, err = cfg.ClientToken(); err != nil {
			return nil, err
		}
		e.tokenFromConfig = true
	}
	return e, nil
}

func (e *clientEnv) load(dir string) (*client.Syncer, error) {
	return client.Load(dir, client.LoadOptions{Logger: e.log, Token: e.token})
}

// folders resolves which synced folders a command acts on: -dir if given;
// else the folder containing the current directory; else every folder in
// the config. With autoSetup, configured folders that are not set up yet
// on this device are set up (that is how a new device picks up folders).
func (e *clientEnv) folders(ctx context.Context, fs *flag.FlagSet, dir string, autoSetup bool) ([]*client.Syncer, error) {
	if isSet(fs, "dir") {
		s, err := e.load(dir)
		if err != nil {
			return nil, err
		}
		return []*client.Syncer{s}, nil
	}
	s, err := e.load(".")
	if err == nil {
		return []*client.Syncer{s}, nil
	}
	if !errors.Is(err, client.ErrNotSetUp) {
		return nil, err
	}
	if len(e.cfg.Folders) == 0 {
		return nil, fmt.Errorf("%w\n(or pass -dir, or list folders in the config: see `dropgitbox setup`)", err)
	}

	var out []*client.Syncer
	for _, f := range e.cfg.Folders {
		path := e.cfg.Resolve(f.Path)
		s, err := e.load(path)
		if errors.Is(err, client.ErrNotSetUp) && autoSetup {
			e.log.Info("setting up configured folder", "folder", path, "repo", f.Repo)
			s, err = e.setup(ctx, path, f.Repo, e.cfg.Client.Server, e.token, e.tokenFromConfig, e.cfg.Client.Device)
			if errors.Is(err, client.ErrConflict) {
				err = nil // set up, but paused; the daemon reports it
			}
		}
		if err != nil {
			e.log.Error("skipping folder", "folder", path, "err", err)
			continue
		}
		if f.Repo != "" && s.Config().Repo != f.Repo {
			e.log.Warn("folder syncs a different repo than the config says", "folder", path,
				"config", f.Repo, "actual", s.Config().Repo)
		}
		out = append(out, s)
	}
	if len(out) == 0 {
		return nil, errors.New("none of the configured folders could be opened")
	}
	return out, nil
}

func (e *clientEnv) setup(ctx context.Context, dir, repo, srvURL, token string, tokenFromConfig bool, device string) (*client.Syncer, error) {
	if srvURL == "" {
		return nil, errors.New("no server configured: run `dropgitbox join INVITE` first, or pass -server")
	}
	return client.Setup(ctx, client.SetupOptions{
		Dir: dir, Server: srvURL, Repo: repo, Token: token, TokenFromConfig: tokenFromConfig,
		Device: device, Logger: e.log,
	})
}

// ---- join / repos -----------------------------------------------------

func cmdJoin(ctx context.Context, args []string) error {
	fs, cfgPath := newFlags("join")
	device := fs.String("device", "", "device name (default: the name in the invite)")
	force := fs.Bool("force", false, "replace an existing server in the config")
	pos := parse(fs, args)
	if len(pos) != 1 {
		return errors.New("usage: dropgitbox join INVITE   (get an invite with `dropgitbox device add NAME` on the server)")
	}
	inv, err := client.DecodeInvite(pos[0])
	if err != nil {
		return err
	}
	if *device == "" {
		*device = inv.Device
	}

	cfg, err := loadConfig(*cfgPath)
	if err != nil {
		return err
	}
	if cur := cfg.Client.Server; cur != "" && cur != inv.Server && !*force {
		return fmt.Errorf("this device is already joined to %s; pass -force to switch to %s", cur, inv.Server)
	}
	if cfg.Client.Token != "" {
		return fmt.Errorf("%s sets client.token inline; remove it so the new token can be used", cfg.Path())
	}

	warnInsecure(inv.Server)
	who, err := client.WhoAmI(ctx, inv.Server, inv.Token)
	if err != nil {
		return fmt.Errorf("checking the invite with %s: %w", inv.Server, err)
	}

	writePath, err := cfg.WritePath()
	if err != nil {
		return err
	}
	tokenPath := filepath.Join(filepath.Dir(writePath), config.TokenFileName)
	if cfg.Client.TokenFile != "" {
		tokenPath = cfg.ClientTokenFile()
	}
	if err := config.WriteSecret(tokenPath, inv.Token); err != nil {
		return err
	}
	if err := config.SetClient(writePath, [][2]string{{"server", inv.Server}, {"device", *device}}); err != nil {
		return err
	}

	fmt.Printf("Joined %s as device %q.\n  config: %s\n  token:  %s\n\n", inv.Server, who, writePath, tokenPath)
	repos, err := client.ListRepos(ctx, inv.Server, inv.Token)
	if err != nil {
		return err
	}
	if len(repos) == 0 {
		fmt.Println("The server has no folders yet. Create one with:\n\n  dropgitbox setup -repo NAME -dir ~/NAME")
	} else {
		fmt.Println("Folders on the server:")
		for _, r := range repos {
			fmt.Printf("  %s\n", r.Name)
		}
		fmt.Printf("\nSync one to this device with:\n\n  dropgitbox setup -repo %s -dir ~/%s\n", repos[0].Name, repos[0].Name)
	}
	fmt.Println("\nThen keep everything in sync with `dropgitbox watch` (or the dropgitbox service).")
	return nil
}

func cmdRepos(ctx context.Context, args []string) error {
	fs, cfgPath := newFlags("repos")
	srvURL := fs.String("server", "", "server URL (config: client.server)")
	parse(fs, args)
	e, err := loadClient(*cfgPath, false)
	if err != nil {
		return err
	}
	if !isSet(fs, "server") {
		*srvURL = e.cfg.Client.Server
	}
	if *srvURL == "" {
		return errors.New("no server configured: run `dropgitbox join INVITE` first, or pass -server")
	}
	repos, err := client.ListRepos(ctx, *srvURL, e.token)
	if err != nil {
		return err
	}
	tw := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
	fmt.Fprintln(tw, "REPO\tHEAD\tLOCAL FOLDER")
	for _, r := range repos {
		local := "-"
		for _, f := range e.cfg.Folders {
			if f.Repo == r.Name {
				local = f.Path
			}
		}
		head := r.Head
		if len(head) > 10 {
			head = head[:10]
		}
		if head == "" {
			head = "(empty)"
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\n", r.Name, head, local)
	}
	return tw.Flush()
}

// ---- setup / watch / sync / status ------------------------------------

func cmdSetup(ctx context.Context, args []string) error {
	fs, cfgPath := newFlags("setup")
	srvURL := fs.String("server", "", "server URL (config: client.server)")
	repo := fs.String("repo", "", "repository name on the server, created if missing (default: the folder's name)")
	token := fs.String("token", "", "device token ($DROPGITBOX_TOKEN, config: client.token_file)")
	device := fs.String("device", "", "device name for commit messages (config: client.device, default: hostname)")
	dir := fs.String("dir", ".", "folder to sync")
	noRegister := fs.Bool("no-register", false, "don't add the folder to the config file")
	parse(fs, args)

	e, err := loadClient(*cfgPath, false)
	if err != nil {
		return err
	}
	if !isSet(fs, "server") {
		*srvURL = e.cfg.Client.Server
	}
	tok, fromConfig := e.token, e.tokenFromConfig
	if isSet(fs, "token") {
		tok, fromConfig = *token, false
	}
	if !isSet(fs, "device") {
		*device = e.cfg.Client.Device
	}
	abs, err := filepath.Abs(config.ExpandHome(*dir))
	if err != nil {
		return err
	}
	if *repo == "" {
		*repo = filepath.Base(abs)
		if !server.ValidRepoName(*repo) {
			return fmt.Errorf("cannot use the folder name %q as a repo name; pass -repo", *repo)
		}
	}

	warnInsecure(*srvURL)
	s, err := e.setup(ctx, abs, *repo, *srvURL, tok, fromConfig, *device)
	if err != nil && !errors.Is(err, client.ErrConflict) {
		return err
	}
	conflict := err != nil

	if !*noRegister {
		writePath, werr := e.cfg.WritePath()
		if werr == nil {
			werr = config.AddFolder(writePath, config.Folder{Path: tildify(abs), Repo: *repo})
		}
		if werr == nil && e.cfg.Client.Server == "" {
			werr = config.SetClient(writePath, [][2]string{{"server", *srvURL}, {"device", *device}})
		}
		if werr != nil {
			fmt.Fprintln(os.Stderr, "dropgitbox: warning: could not register the folder in the config:", werr)
		} else {
			fmt.Printf("Registered %s in %s\n", tildify(abs), writePath)
		}
	}
	if conflict {
		printConflictHelp(ctx, s)
		return client.ErrConflict
	}
	fmt.Printf("%s is now synced with %s (repo %q).\nRun `dropgitbox watch` (or enable the dropgitbox service) to keep it in sync.\n",
		s.Dir(), s.Config().Server, s.Config().Repo)
	return nil
}

func cmdWatch(ctx context.Context, args []string) error {
	fs, cfgPath := newFlags("watch")
	dir := fs.String("dir", ".", "synced folder (default: all configured folders)")
	debounce := fs.Duration("debounce", 2*time.Second, "quiet period before committing (config: client.debounce)")
	maxDelay := fs.Duration("max-delay", 30*time.Second, "longest a burst of changes may postpone a commit (config: client.max_delay)")
	poll := fs.Duration("poll", 5*time.Minute, "safety-net sync interval (config: client.poll)")
	verbose := fs.Bool("v", false, "verbose logging")
	parse(fs, args)

	e, err := loadClient(*cfgPath, *verbose)
	if err != nil {
		return err
	}
	opts := client.DaemonOptions{Debounce: e.cfg.Client.Debounce, MaxDelay: e.cfg.Client.MaxDelay, Poll: e.cfg.Client.Poll}
	if isSet(fs, "debounce") || opts.Debounce == 0 {
		opts.Debounce = *debounce
	}
	if isSet(fs, "max-delay") || opts.MaxDelay == 0 {
		opts.MaxDelay = *maxDelay
	}
	if isSet(fs, "poll") || opts.Poll == 0 {
		opts.Poll = *poll
	}

	folders, err := e.folders(ctx, fs, *dir, true)
	if err != nil {
		return err
	}
	e.log.Info("dropgitbox daemon starting", "folders", len(folders), "version", version)

	var (
		wg     sync.WaitGroup
		mu     sync.Mutex
		failed int
	)
	for _, s := range folders {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := s.Watch(ctx, opts); err != nil {
				e.log.Error("folder stopped", "folder", s.Dir(), "err", err)
				mu.Lock()
				failed++
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	if failed > 0 && ctx.Err() == nil {
		return fmt.Errorf("%d folder(s) stopped with errors", failed)
	}
	return nil
}

func cmdSync(ctx context.Context, args []string) error {
	fs, cfgPath := newFlags("sync")
	dir := fs.String("dir", ".", "synced folder (default: current or all configured folders)")
	parse(fs, args)

	e, err := loadClient(*cfgPath, false)
	if err != nil {
		return err
	}
	folders, err := e.folders(ctx, fs, *dir, true)
	if err != nil {
		return err
	}
	var failed []string
	for _, s := range folders {
		if err := s.Sync(ctx); err != nil {
			fmt.Printf("%s: %v\n", s.Dir(), err)
			if errors.Is(err, client.ErrConflict) {
				printConflictHelp(ctx, s)
			}
			failed = append(failed, s.Dir())
			continue
		}
		fmt.Printf("%s: up to date\n", s.Dir())
	}
	if len(failed) > 0 {
		return fmt.Errorf("sync failed for %s", strings.Join(failed, ", "))
	}
	return nil
}

func cmdStatus(ctx context.Context, args []string) error {
	fs, cfgPath := newFlags("status")
	dir := fs.String("dir", ".", "synced folder (default: current or all configured folders)")
	parse(fs, args)

	e, err := loadClient(*cfgPath, false)
	if err != nil {
		return err
	}
	folders, err := e.folders(ctx, fs, *dir, false)
	if err != nil {
		return err
	}
	for i, s := range folders {
		if i > 0 {
			fmt.Println()
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
	}
	return nil
}

// ---- resolve / log / restore (single folder) --------------------------

func cmdResolve(ctx context.Context, args []string) error {
	fs, cfgPath := newFlags("resolve")
	dir := fs.String("dir", ".", "synced folder")
	ours := fs.Bool("ours", false, "keep this device's version of the conflicted files")
	theirs := fs.Bool("theirs", false, "keep the server's version of the conflicted files")
	force := fs.Bool("force", false, "conclude even if conflict markers remain")
	noSync := fs.Bool("no-sync", false, "only commit the resolution, don't sync")
	paths := parse(fs, args)

	if *ours && *theirs {
		return errors.New("resolve: -ours and -theirs are mutually exclusive")
	}
	opts := client.ResolveOptions{Paths: paths, Force: *force}
	switch {
	case *ours:
		opts.Side = client.SideOurs
	case *theirs:
		opts.Side = client.SideTheirs
	}

	e, err := loadClient(*cfgPath, false)
	if err != nil {
		return err
	}
	s, err := e.load(*dir)
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
	fs, cfgPath := newFlags("log")
	dir := fs.String("dir", ".", "synced folder")
	n := fs.Int("n", 20, "number of commits to show")
	paths := parse(fs, args)

	e, err := loadClient(*cfgPath, false)
	if err != nil {
		return err
	}
	s, err := e.load(*dir)
	if err != nil {
		return err
	}
	gitArgs := []string{"log", fmt.Sprintf("-n%d", *n), "--date=format:%Y-%m-%d %H:%M:%S",
		"--format=%h  %ad  %s", "--stat=100", "--stat-graph-width=20"}
	if len(paths) > 0 {
		gitArgs = append(append(gitArgs, "--"), paths...)
	}
	out, err := s.Git().RunRaw(ctx, gitArgs...)
	if err != nil {
		return err
	}
	fmt.Print(out)
	return nil
}

func cmdRestore(ctx context.Context, args []string) error {
	fs, cfgPath := newFlags("restore")
	dir := fs.String("dir", ".", "synced folder")
	rev := fs.String("rev", "", "commit to restore from (see `dropgitbox log`)")
	paths := parse(fs, args)

	if *rev == "" || len(paths) == 0 {
		return errors.New("restore: -rev and at least one path are required")
	}
	e, err := loadClient(*cfgPath, false)
	if err != nil {
		return err
	}
	s, err := e.load(*dir)
	if err != nil {
		return err
	}
	if err := s.Restore(ctx, *rev, paths); err != nil {
		return err
	}
	fmt.Printf("Restored %s from %s. The change will be committed and synced like any other edit.\n",
		strings.Join(paths, ", "), *rev)
	return nil
}

// ---- output helpers ---------------------------------------------------

func printConflictHelp(ctx context.Context, s *client.Syncer) {
	files, _ := s.ConflictedFiles(ctx)
	fmt.Printf("SYNC PAUSED in %s: changes on this device conflict with changes from the server.\n", s.Dir())
	fmt.Println("Conflicted files:")
	for _, f := range files {
		fmt.Println("  " + f)
	}
	fmt.Printf(`
To resolve (in %[1]s), either:
  - edit each file to remove the <<<<<<< ======= >>>>>>> markers, then run: dropgitbox resolve
  - keep this device's version:   dropgitbox resolve -ours [PATH...]
  - keep the server's version:    dropgitbox resolve -theirs [PATH...]
`, s.Dir())
}

func warnInsecure(serverURL string) {
	if client.InsecureURL(serverURL) {
		fmt.Fprintf(os.Stderr, "dropgitbox: warning: %s uses plain HTTP, so this device's token and files travel unencrypted.\n"+
			"  That is only safe on a trusted LAN or inside a VPN such as WireGuard or Tailscale; otherwise use https://.\n", serverURL)
	}
}

func orNone(s string) string {
	if s == "" {
		return "(none)"
	}
	return s
}

// tildify shortens paths under the home directory to "~/...", so a config
// file stays valid when shared between machines with different usernames.
func tildify(p string) string {
	home, err := os.UserHomeDir()
	if err != nil {
		return p
	}
	if rel, err := filepath.Rel(home, p); err == nil && rel != "." && !strings.HasPrefix(rel, "..") {
		return "~/" + filepath.ToSlash(rel)
	}
	return p
}
