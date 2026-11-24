package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"

	"github.com/dhruvasagar/dropgitbox/internal/client"
	"github.com/dhruvasagar/dropgitbox/internal/config"
	"github.com/dhruvasagar/dropgitbox/internal/daemon"
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

// folders resolves which synced folders a command acts on: dir if it was
// given (dirSet); else the folder containing the current directory; else
// every folder in the config. With autoSetup, configured folders that are
// not set up yet on this device are set up (that is how a new device picks
// up folders).
func (e *clientEnv) folders(ctx context.Context, dir string, dirSet, autoSetup bool) ([]*client.Syncer, error) {
	if dirSet {
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
		return nil, fmt.Errorf("%w\n(or pass --dir, or list folders in the config: see `dropgitbox setup`)", err)
	}
	out := e.configuredFolders(ctx, autoSetup)
	if len(out) == 0 {
		return nil, errors.New("none of the configured folders could be opened")
	}
	return out, nil
}

// configuredFolders opens the folders listed in the config, setting up any
// that aren't yet (with autoSetup). Folders that can't be opened are
// logged and skipped.
func (e *clientEnv) configuredFolders(ctx context.Context, autoSetup bool) []*client.Syncer {
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
	return out
}

func (e *clientEnv) setup(ctx context.Context, dir, repo, srvURL, token string, tokenFromConfig bool, device string) (*client.Syncer, error) {
	if srvURL == "" {
		return nil, errors.New("no server configured: run `dropgitbox join INVITE` first, or pass --server")
	}
	return client.Setup(ctx, client.SetupOptions{
		Dir: dir, Server: srvURL, Repo: repo, Token: token, TokenFromConfig: tokenFromConfig,
		Device: device, Logger: e.log,
	})
}

// dirFlag adds the --dir flag that picks the synced folder a command acts on.
func dirFlag(cmd *cobra.Command, dir *string, def, usage string) {
	cmd.Flags().StringVar(dir, "dir", def, usage)
	_ = cmd.MarkFlagDirname("dir")
}

const dirAllUsage = "act on the synced folder `DIR` (default: the one containing the current directory, or every configured folder)"

// ---- join / repos -----------------------------------------------------

func (a *app) joinCmd() *cobra.Command {
	var device string
	var force bool
	cmd := &cobra.Command{
		Use:   "join INVITE",
		Short: "Connect this device to a server",
		Long: `Connect this device to a dropgitbox server, using an invite printed by
"dropgitbox device add NAME" on the server.

The invite is checked with the server, then the device's token is saved to a
file readable only by you, and the server URL and device name are saved to
the config. join then lists the folders on the server; sync them to this
device with "dropgitbox setup".

To move to a new token (after "dropgitbox device rotate") or another server,
join again with --force. Folders already set up stay as they are.`,
		Example: `  dropgitbox join dgb1.eyJzIjoiaHR0cHM6Ly9ib3guZXhhbXBsZS5jb20i...
  dropgitbox join --force dgb1.eyJ...      # after the token was rotated`,
		Args: argsRange(1, 1),
		RunE: func(cmd *cobra.Command, args []string) error {
			inv, err := client.DecodeInvite(args[0])
			if err != nil {
				return err
			}
			if device == "" {
				device = inv.Device
			}
			return a.join(cmd.Context(), inv, device, force)
		},
	}
	cmd.Flags().StringVar(&device, "device", "", "name this device `NAME` in commit messages (default: the name in the invite)")
	cmd.Flags().BoolVar(&force, "force", false, "replace the server or token already in the config")
	return cmd
}

func (a *app) join(ctx context.Context, inv client.Invite, device string, force bool) error {
	cfg, err := loadConfig(a.cfgPath)
	if err != nil {
		return err
	}
	if cur := cfg.Client.Server; cur != "" && cur != inv.Server && !force {
		return fmt.Errorf("this device is already joined to %s; pass --force to switch to %s", cur, inv.Server)
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
	if err := config.SetClient(writePath, [][2]string{{"server", inv.Server}, {"device", device}}); err != nil {
		return err
	}

	fmt.Printf("Joined %s as device %q.\n  config: %s\n  token:  %s\n\n", inv.Server, who, writePath, tokenPath)
	repos, err := client.ListRepos(ctx, inv.Server, inv.Token)
	if err != nil {
		return err
	}
	if len(repos) == 0 {
		fmt.Println("The server has no folders yet. Create one with:\n\n  dropgitbox setup --repo NAME --dir ~/NAME")
	} else {
		fmt.Println("Folders on the server:")
		for _, r := range repos {
			fmt.Printf("  %s\n", r.Name)
		}
		fmt.Printf("\nSync one to this device with:\n\n  dropgitbox setup --repo %s --dir ~/%s\n", repos[0].Name, repos[0].Name)
	}
	fmt.Println("\nThen keep everything in sync with `dropgitbox watch` (or the dropgitbox service).")
	return nil
}

func (a *app) reposCmd() *cobra.Command {
	var srvURL string
	cmd := &cobra.Command{
		Use:   "repos",
		Short: "List the folders on the server",
		Long: `List the folders (git repositories) on the server, with each one's latest
commit and where it is synced on this device, if it is.`,
		Example: `  dropgitbox repos`,
		Args:    cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			e, err := loadClient(a.cfgPath, false)
			if err != nil {
				return err
			}
			if !cmd.Flags().Changed("server") {
				srvURL = e.cfg.Client.Server
			}
			if srvURL == "" {
				return errors.New("no server configured: run `dropgitbox join INVITE` first, or pass --server")
			}
			repos, err := client.ListRepos(cmd.Context(), srvURL, e.token)
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
		},
	}
	cmd.Flags().StringVar(&srvURL, "server", "", "ask the server at `URL` (config: client.server)")
	return cmd
}

// ---- setup / watch / sync / status ------------------------------------

func (a *app) setupCmd() *cobra.Command {
	var (
		srvURL, repo, token, device, dir string
		noRegister                       bool
	)
	cmd := &cobra.Command{
		Use:   "setup [--repo NAME] [--dir DIR]",
		Short: "Sync a folder with a repository on the server",
		Long: `Sync the folder DIR (default: the current directory) with the repository
NAME on the server, creating the repository if it doesn't exist yet.

Files already in DIR are kept and merged with what is on the server, and
DIR is created if it doesn't exist. The folder is then added to the config,
so "dropgitbox watch" keeps it in sync; a running daemon picks it up within
seconds.

A folder can't be inside another synced folder, and can't contain other git
repositories: their files wouldn't sync.`,
		Example: `  dropgitbox setup --repo notes --dir ~/Notes
  cd ~/Photos && dropgitbox setup          # repository "Photos"
  dropgitbox setup --repo scratch --dir /tmp/scratch --no-register`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			e, err := loadClient(a.cfgPath, false)
			if err != nil {
				return err
			}
			f := cmd.Flags()
			if !f.Changed("server") {
				srvURL = e.cfg.Client.Server
			}
			tok, fromConfig := e.token, e.tokenFromConfig
			if f.Changed("token") {
				tok, fromConfig = token, false
			}
			if !f.Changed("device") {
				device = e.cfg.Client.Device
			}
			return a.setup(cmd.Context(), e, setupArgs{srvURL, repo, tok, fromConfig, device, dir, noRegister})
		},
	}
	f := cmd.Flags()
	f.StringVar(&repo, "repo", "", "sync with the repository `NAME`, created if missing (default: the folder's name)")
	dirFlag(cmd, &dir, ".", "the folder `DIR` to sync")
	f.StringVar(&srvURL, "server", "", "use the server at `URL` (config: client.server)")
	f.StringVar(&token, "token", "", "authenticate with `TOKEN` ($DROPGITBOX_TOKEN, config: client.token_file)")
	f.StringVar(&device, "device", "", "name this device `NAME` in commit messages (config: client.device, default: hostname)")
	f.BoolVar(&noRegister, "no-register", false, "don't add the folder to the config file")
	_ = cmd.RegisterFlagCompletionFunc("repo", a.completeRepos)
	return cmd
}

type setupArgs struct {
	srvURL, repo, token string
	tokenFromConfig     bool
	device, dir         string
	noRegister          bool
}

func (a *app) setup(ctx context.Context, e *clientEnv, o setupArgs) error {
	abs, err := filepath.Abs(config.ExpandHome(o.dir))
	if err != nil {
		return err
	}
	if o.repo == "" {
		o.repo = filepath.Base(abs)
		if !server.ValidRepoName(o.repo) {
			return fmt.Errorf("cannot use the folder name %q as a repo name; pass --repo", o.repo)
		}
	}

	warnInsecure(o.srvURL)
	s, err := e.setup(ctx, abs, o.repo, o.srvURL, o.token, o.tokenFromConfig, o.device)
	if err != nil && !errors.Is(err, client.ErrConflict) {
		return err
	}
	conflict := err != nil

	if !o.noRegister {
		writePath, werr := e.cfg.WritePath()
		if werr == nil {
			werr = config.AddFolder(writePath, config.Folder{Path: tildify(abs), Repo: o.repo})
		}
		if werr == nil && e.cfg.Client.Server == "" {
			werr = config.SetClient(writePath, [][2]string{{"server", o.srvURL}, {"device", o.device}})
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
	fmt.Printf("%s is now synced with %s (repo %q).\n", s.Dir(), s.Config().Server, s.Config().Repo)
	switch {
	case daemonRunning(ctx) && !o.noRegister:
		fmt.Println("The running daemon picks it up within a few seconds (`dropgitbox status` shows it).")
	case daemonRunning(ctx):
		fmt.Println("It isn't in the config (--no-register), so the running daemon won't sync it; use `dropgitbox sync --dir` to sync it.")
	default:
		fmt.Println("Run `dropgitbox watch` (or enable the dropgitbox service) to keep it in sync.")
	}
	return nil
}

func daemonRunning(ctx context.Context) bool {
	c, err := daemon.Dial("")
	if err != nil {
		return false
	}
	_, err = c.Status(ctx)
	return err == nil
}

func (a *app) watchCmd() *cobra.Command {
	var (
		dir                      string
		debounce, maxDelay, poll time.Duration
		noDesktop, verbose       bool
	)
	cmd := &cobra.Command{
		Use:   "watch",
		Short: "Run the sync daemon",
		Long: `Run the sync daemon in the foreground until interrupted. It is usually run
as the dropgitbox user service instead:

  systemctl --user enable --now dropgitbox

Each folder is synced when its files change (once they have been quiet for
--debounce), as soon as the server announces changes from another device,
and every --poll as a safety net. On a merge conflict, syncing of that folder
pauses until you resolve it ("dropgitbox resolve"); other folders carry on.

Without --dir, every folder in the config is synced, and folders added to or
removed from the config are picked up while the daemon runs. Configured
folders not yet set up on this device are set up first.

The daemon shows desktop notifications for conflicts and lost connections,
and can run a command on events ([notify] in the config). "dropgitbox
status", "events" and "sync" talk to it while it runs. Only one daemon runs
per user.`,
		Example: `  dropgitbox watch
  dropgitbox watch --dir ~/Notes -v
  dropgitbox watch --debounce 10s --no-desktop-notify`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return a.watch(cmd, dir, debounce, maxDelay, poll, noDesktop, verbose)
		},
	}
	f := cmd.Flags()
	dirFlag(cmd, &dir, "", "sync only the folder `DIR` (default: every configured folder)")
	f.DurationVar(&debounce, "debounce", 2*time.Second, "commit once files have been quiet this long (config: client.debounce)")
	f.DurationVar(&maxDelay, "max-delay", 30*time.Second, "longest a burst of changes may postpone a commit (config: client.max_delay)")
	f.DurationVar(&poll, "poll", 5*time.Minute, "sync at least this often, in case a notification is missed (config: client.poll)")
	f.BoolVar(&noDesktop, "no-desktop-notify", false, "don't show desktop notifications (config: notify.desktop)")
	f.BoolVarP(&verbose, "verbose", "v", false, "log in more detail")
	return cmd
}

func (a *app) watch(cmd *cobra.Command, dir string, debounce, maxDelay, poll time.Duration, noDesktop, verbose bool) error {
	ctx, f := cmd.Context(), cmd.Flags()
	e, err := loadClient(a.cfgPath, verbose)
	if err != nil {
		return err
	}
	cc := e.cfg.Client
	syncOpts := client.DaemonOptions{Debounce: cc.Debounce, MaxDelay: cc.MaxDelay, Poll: cc.Poll,
		OfflineAfter: e.cfg.Notify.OfflineAfter}
	if f.Changed("debounce") || syncOpts.Debounce == 0 {
		syncOpts.Debounce = debounce
	}
	if f.Changed("max-delay") || syncOpts.MaxDelay == 0 {
		syncOpts.MaxDelay = maxDelay
	}
	if f.Changed("poll") || syncOpts.Poll == 0 {
		syncOpts.Poll = poll
	}
	notify, err := notifyOptions(e.cfg)
	if err != nil {
		return err
	}
	if noDesktop {
		notify.Desktop = false
	}

	// Refuse early if a daemon is already running (before touching folders).
	if c, err := daemon.Dial(""); err == nil {
		if st, err := c.Status(ctx); err == nil {
			return fmt.Errorf("%w (pid %d); stop it first, or use `dropgitbox status`", daemon.ErrAlreadyRunning, st.PID)
		}
	}

	dirSet := f.Changed("dir")
	folders, err := e.folders(ctx, dir, dirSet, true)
	if err != nil {
		return err
	}
	opts := daemon.Options{Sync: syncOpts, Notify: notify, Version: version, Logger: e.log}

	// Syncing the folders listed in the config (rather than one given with
	// --dir or the current directory): follow changes to that list, so
	// `dropgitbox setup` takes effect without a restart.
	if fromConfig := !dirSet && !isSyncedDir("."); fromConfig {
		if path, err := e.cfg.WritePath(); err == nil {
			opts.ConfigPath = path
			opts.Reload = func(ctx context.Context) ([]*client.Syncer, error) {
				fresh, err := loadClient(a.cfgPath, verbose)
				if err != nil {
					return nil, err
				}
				fresh.log = e.log
				return fresh.configuredFolders(ctx, true), nil
			}
		}
	}
	return daemon.Run(ctx, folders, opts)
}

// isSyncedDir reports whether dir is (inside) a set-up dropgitbox folder.
func isSyncedDir(dir string) bool {
	_, err := client.Load(dir, client.LoadOptions{Logger: slog.New(slog.DiscardHandler)})
	return err == nil
}

func notifyOptions(cfg *config.Config) (daemon.NotifyOptions, error) {
	n := cfg.Notify
	o := daemon.NotifyOptions{
		Desktop:       n.Desktop == nil || *n.Desktop,
		DesktopEvents: daemon.DefaultDesktopEvents,
		CommandEvents: daemon.DefaultCommandEvents,
	}
	var err error
	if len(n.DesktopEvents) > 0 {
		if o.DesktopEvents, err = daemon.ParseEvents(n.DesktopEvents); err != nil {
			return o, fmt.Errorf("notify.desktop_events: %w", err)
		}
	}
	if len(n.CommandEvents) > 0 {
		if o.CommandEvents, err = daemon.ParseEvents(n.CommandEvents); err != nil {
			return o, fmt.Errorf("notify.command_events: %w", err)
		}
	}
	if len(n.Command) > 0 {
		o.Command = append([]string{cfg.Resolve(n.Command[0])}, n.Command[1:]...)
		if _, err := exec.LookPath(o.Command[0]); err != nil {
			return o, fmt.Errorf("notify.command: %w", err)
		}
	}
	return o, nil
}

func (a *app) syncCmd() *cobra.Command {
	var dir string
	var direct bool
	cmd := &cobra.Command{
		Use:   "sync",
		Short: "Commit and sync once",
		Long: `Commit local changes and sync with the server once, then exit.

If the daemon is running, sync asks it to sync the folders it manages, so two
processes never run git in the same folder at once; it waits for the result.
--direct syncs in this process anyway. Configured folders not yet set up on
this device are set up first.`,
		Example: `  dropgitbox sync
  dropgitbox sync --dir ~/Notes`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return a.sync(cmd.Context(), dir, cmd.Flags().Changed("dir"), direct)
		},
	}
	dirFlag(cmd, &dir, "", dirAllUsage)
	cmd.Flags().BoolVar(&direct, "direct", false, "sync in this process even if the daemon is running")
	return cmd
}

func (a *app) sync(ctx context.Context, dir string, dirSet, direct bool) error {
	e, err := loadClient(a.cfgPath, false)
	if err != nil {
		return err
	}
	folders, err := e.folders(ctx, dir, dirSet, true)
	if err != nil {
		return err
	}

	// With a daemon running, ask it to sync rather than running git in the
	// same folders concurrently.
	if c, err := daemon.Dial(""); err == nil && !direct {
		if st, err := c.Status(ctx); err == nil {
			managed := map[string]bool{}
			for _, f := range st.Folders {
				managed[f.Folder] = true
			}
			var failed []string
			for _, s := range folders {
				if !managed[s.Dir()] {
					continue
				}
				out, err := c.Sync(ctx, s.Dir())
				if err != nil {
					return err
				}
				for _, o := range out {
					fmt.Printf("%s: %s", o.Folder, o.Result)
					if o.Message != "" {
						fmt.Printf(" (%s)", o.Message)
					}
					fmt.Println()
					if o.Result != "synced" {
						failed = append(failed, o.Folder)
					}
				}
			}
			var unmanaged []*client.Syncer
			for _, s := range folders {
				if !managed[s.Dir()] {
					unmanaged = append(unmanaged, s)
				}
			}
			folders = unmanaged
			if len(folders) == 0 {
				if len(failed) > 0 {
					return fmt.Errorf("sync did not complete for %s (see `dropgitbox status`)", strings.Join(failed, ", "))
				}
				return nil
			}
		}
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

func (a *app) statusCmd() *cobra.Command {
	var dir string
	var short, asJSON bool
	cmd := &cobra.Command{
		Use:   "status",
		Short: "Show the daemon's state and folder details",
		Long: `Show whether the daemon is running and, for each folder, its server and
repository, the daemon's live state (in sync, pending, syncing, PAUSED on a
conflict, retrying, OFFLINE, ACCESS DENIED or STOPPED), and how far it is
ahead of or behind the server.

--short prints one line for status bars and shell prompts, naming the most
urgent state across all folders, such as "dropgitbox: synced" or
"dropgitbox: CONFLICT notes". --json prints the daemon's full live state.
Neither needs the config.`,
		Example: `  dropgitbox status
  dropgitbox status --short
  dropgitbox status --json | jq '.folders[] | {folder, state}'`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return a.status(cmd.Context(), dir, cmd.Flags().Changed("dir"), short, asJSON)
		},
	}
	dirFlag(cmd, &dir, "", dirAllUsage)
	cmd.Flags().BoolVar(&short, "short", false, "print one line summarising the daemon's state")
	cmd.Flags().BoolVar(&asJSON, "json", false, "print the daemon's live state as JSON")
	cmd.MarkFlagsMutuallyExclusive("short", "json")
	return cmd
}

func (a *app) status(ctx context.Context, dir string, dirSet, short, asJSON bool) error {
	var live *daemon.Status
	if c, err := daemon.Dial(""); err == nil {
		live, _ = c.Status(ctx)
	}
	if short {
		fmt.Println(shortStatus(live))
		return nil
	}
	if asJSON {
		if live == nil {
			return errors.New("the dropgitbox daemon is not running")
		}
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(live)
	}

	e, err := loadClient(a.cfgPath, false)
	if err != nil {
		return err
	}
	folders, err := e.folders(ctx, dir, dirSet, false)
	if err != nil {
		return err
	}
	if live == nil {
		fmt.Println("Daemon:  not running (start it with `dropgitbox watch` or the dropgitbox service)")
	} else {
		fmt.Printf("Daemon:  running (pid %d, version %s, up %s)\n", live.PID, live.Version,
			time.Since(live.Started).Round(time.Second))
	}
	for _, s := range folders {
		fmt.Println()
		st, err := s.Status(ctx)
		if err != nil {
			return err
		}
		fmt.Printf("Folder:  %s\nServer:  %s\nRepo:    %s\nDevice:  %s\n", st.Config.Dir, st.Config.Server, st.Config.Repo, st.Config.Device)
		if live != nil {
			for _, f := range live.Folders {
				if f.Folder == s.Dir() {
					fmt.Printf("State:   %s\n", describeState(f))
				}
			}
		}
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

func describeState(f daemon.FolderStatus) string {
	last := "never"
	if !f.LastSync.IsZero() {
		last = time.Since(f.LastSync).Round(time.Second).String() + " ago"
	}
	switch f.State {
	case daemon.StateIdle:
		return "in sync (last synced " + last + ")"
	case daemon.StatePending:
		return "changes pending"
	case daemon.StateSyncing:
		return "syncing..."
	case daemon.StateConflict:
		return "PAUSED: conflict in " + strings.Join(f.Conflicts, ", ")
	case daemon.StateError:
		return "retrying after error: " + f.LastError
	case daemon.StateOffline:
		return "OFFLINE since " + f.Since.Local().Format(time.DateTime) + ": " + f.LastError
	case daemon.StateUnauthorized:
		return "ACCESS DENIED: " + f.LastError
	case daemon.StateStopped:
		return "STOPPED, not syncing: " + f.LastError
	}
	return f.State
}

// shortStatus summarises all folders in one line, most urgent first.
func shortStatus(st *daemon.Status) string {
	if st == nil {
		return "dropgitbox: not running"
	}
	rank := map[string]int{daemon.StateUnauthorized: 7, daemon.StateStopped: 6, daemon.StateConflict: 5, daemon.StateOffline: 4,
		daemon.StateError: 3, daemon.StateSyncing: 2, daemon.StatePending: 1, daemon.StateIdle: 0}
	worst, names := daemon.StateIdle, []string{}
	for _, f := range st.Folders {
		if rank[f.State] > rank[worst] {
			worst, names = f.State, nil
		}
		if f.State == worst {
			names = append(names, filepath.Base(f.Folder))
		}
	}
	in := " " + strings.Join(names, ",")
	switch worst {
	case daemon.StateUnauthorized:
		return "dropgitbox: access denied" + in
	case daemon.StateStopped:
		return "dropgitbox: STOPPED" + in
	case daemon.StateConflict:
		return "dropgitbox: CONFLICT" + in
	case daemon.StateOffline:
		return "dropgitbox: offline" + in
	case daemon.StateError:
		return "dropgitbox: retrying" + in
	case daemon.StateSyncing:
		return "dropgitbox: syncing" + in
	case daemon.StatePending:
		return "dropgitbox: pending" + in
	}
	return "dropgitbox: synced"
}

func (a *app) eventsCmd() *cobra.Command {
	var asJSON bool
	names := make([]string, len(client.AllEvents))
	for i, ev := range client.AllEvents {
		names[i] = string(ev)
	}
	cmd := &cobra.Command{
		Use:   "events",
		Short: "Stream sync events from the daemon",
		Long: `Print the running daemon's events as they happen, until interrupted: one
line per event with its time, type and description.

Event types: ` + strings.Join(names, ", ") + `.

--json prints each event as a JSON object on its own line, for scripts. To
run a command on events instead, set notify.command in the config.`,
		Example: `  dropgitbox events
  dropgitbox events --json | jq -r 'select(.type == "conflict") | .folder'`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			c, err := daemon.Dial("")
			if err != nil {
				return errors.New("the dropgitbox daemon is not running")
			}
			if _, err := c.Status(cmd.Context()); err != nil {
				return errors.New("the dropgitbox daemon is not running")
			}
			enc := json.NewEncoder(os.Stdout)
			return c.Events(cmd.Context(), func(ev client.Event) {
				if asJSON {
					_ = enc.Encode(ev)
					return
				}
				title, body, _ := daemon.Describe(ev)
				line := fmt.Sprintf("%s  %-14s %s", ev.Time.Local().Format(time.TimeOnly), ev.Type, title)
				if body != "" {
					line += ": " + strings.ReplaceAll(body, "\n", " ")
				}
				fmt.Println(line)
			})
		},
	}
	cmd.Flags().BoolVar(&asJSON, "json", false, "print events as JSON lines")
	return cmd
}

func (a *app) notifyCmd() *cobra.Command {
	cmd := groupCmd(&cobra.Command{
		Use:   "notify",
		Short: "Check notifications",
		Long: `Check how the daemon notifies you about sync events: desktop notifications,
and the command run on events (notify.command in the config).`,
	})
	cmd.AddCommand(&cobra.Command{
		Use:   "test",
		Short: "Send a test notification and run the hook command",
		Long: `Send a test desktop notification, and run notify.command (if configured)
with a test event, reporting whether each worked. Use it after changing the
[notify] settings in the config.`,
		Example: `  dropgitbox notify test`,
		Args:    cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return a.notifyTest(cmd.Context())
		},
	})
	return cmd
}

func (a *app) notifyTest(ctx context.Context) error {
	cfg, err := loadConfig(a.cfgPath)
	if err != nil {
		return err
	}
	o, err := notifyOptions(cfg)
	if err != nil {
		return err
	}
	ok := true
	if o.Desktop {
		if err := daemon.SendTestNotification(); err != nil {
			fmt.Println("desktop: FAILED:", err)
			ok = false
		} else {
			fmt.Println("desktop: sent a test notification")
		}
	} else {
		fmt.Println("desktop: disabled (notify.desktop = false)")
	}
	if len(o.Command) > 0 {
		ev := client.Event{Time: time.Now(), Type: "test", Folder: "/path/to/folder", Repo: "test",
			Message: "This is a test event from `dropgitbox notify test`."}
		if err := daemon.RunHook(ctx, o.Command, ev, 30*time.Second); err != nil {
			fmt.Println("command: FAILED:", err)
			ok = false
		} else {
			fmt.Printf("command: ran %s successfully\n", o.Command[0])
		}
	} else {
		fmt.Println("command: none configured (notify.command)")
	}
	if !ok {
		return errors.New("some notification channels failed")
	}
	return nil
}

// ---- resolve / log / restore (single folder) --------------------------

func (a *app) resolveCmd() *cobra.Command {
	var dir string
	var ours, theirs, force, noSync bool
	cmd := &cobra.Command{
		Use:   "resolve [--ours | --theirs] [PATH...]",
		Short: "Mark conflicted files resolved",
		Long: `Mark conflicted files resolved, like "git add" during a merge. Once no
conflicted file is left, the merge is committed and the folder syncs again.

When changes on this device conflict with changes from the server, syncing
of that folder pauses; "dropgitbox status" lists the conflicted files. For
each one, either:

  - edit it: keep what you want and remove the <<<<<<< ======= >>>>>>>
    markers, then run "dropgitbox resolve FILE";
  - or keep one side with --ours (this device's version) or --theirs (the
    server's version), which may mean deleting the file if that side did.

PATHs are files or directories, relative to the current directory (or to
--dir). Without PATHs, resolve acts on every conflicted file. It refuses
files that still have conflict markers (unless --force), and, unless named
or given a side, files that have no markers to edit: binary files and files
deleted on one side.

Only the merge and the resolved files are committed. Other changes made
while the folder was paused are synced afterwards, as their own commit.
The sync goes through the daemon when it is running.`,
		Example: `  dropgitbox status                  # what is conflicted
  dropgitbox resolve notes.txt       # after editing out the markers
  dropgitbox resolve --theirs photo.jpg
  dropgitbox resolve                 # every conflicted file, as edited
  dropgitbox resolve --dir ~/Notes --ours`,
		Args:              cobra.ArbitraryArgs,
		ValidArgsFunction: a.completeConflicts(&dir),
		RunE: func(cmd *cobra.Command, paths []string) error {
			return a.resolve(cmd.Context(), dir, paths, ours, theirs, force, noSync)
		},
	}
	f := cmd.Flags()
	dirFlag(cmd, &dir, ".", "the synced folder `DIR`; PATHs are relative to it")
	f.BoolVar(&ours, "ours", false, "keep this device's version of the files")
	f.BoolVar(&theirs, "theirs", false, "keep the server's version of the files")
	f.BoolVar(&force, "force", false, "accept files that still contain conflict markers")
	f.BoolVar(&noSync, "no-sync", false, "commit the merge but don't sync")
	cmd.MarkFlagsMutuallyExclusive("ours", "theirs")
	return cmd
}

func (a *app) resolve(ctx context.Context, dir string, args []string, ours, theirs, force, noSync bool) error {
	e, err := loadClient(a.cfgPath, false)
	if err != nil {
		return err
	}
	s, err := e.load(dir)
	if err != nil {
		return err
	}
	paths, err := folderPaths(s, dir, args)
	if err != nil {
		return err
	}
	opts := client.ResolveOptions{Paths: paths, Force: force}
	switch {
	case ours:
		opts.Side = client.SideOurs
	case theirs:
		opts.Side = client.SideTheirs
	}

	show := displayPaths(s, dir)
	res, err := s.Resolve(ctx, opts)
	var ue *client.UnresolvedError
	var nc *client.NotConflictedError
	switch {
	case errors.As(err, &ue):
		return errors.New(ue.Format(show))
	case errors.As(err, &nc):
		names := make([]string, len(nc.Conflicted))
		for i, p := range nc.Conflicted {
			names[i] = show(p)
		}
		return fmt.Errorf("%s has no conflict (conflicted: %s)", show(nc.Path), strings.Join(names, ", "))
	case err != nil:
		return err
	}

	for _, p := range res.Resolved {
		fmt.Printf("resolved: %s\n", show(p))
	}
	if len(res.Remaining) > 0 {
		fmt.Println()
		printConflicts(res.Remaining, show)
		fmt.Println("\nSyncing resumes once these are resolved too.")
		return nil
	}
	fmt.Printf("All conflicts resolved; merge committed as %s.\n", res.Commit[:10])
	if noSync {
		fmt.Println("Not synced (--no-sync); the daemon, or `dropgitbox sync`, will sync it.")
		return nil
	}
	return a.sync(ctx, s.Dir(), true, false)
}

// folderPaths turns PATH arguments, relative to base (the current
// directory, or --dir as with "git -C"), into paths relative to the top of
// the synced folder, as git wants them.
func folderPaths(s *client.Syncer, base string, args []string) ([]string, error) {
	top, baseAbs, err := folderAndBase(s, base)
	if err != nil {
		return nil, err
	}
	out := make([]string, 0, len(args))
	for _, p := range args {
		abs := p
		if !filepath.IsAbs(abs) {
			abs = filepath.Join(baseAbs, p)
		}
		rel, err := filepath.Rel(top, filepath.Clean(abs))
		if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			return nil, usageErrorf("%s is outside the synced folder %s", p, s.Dir())
		}
		if rel == ".git" || strings.HasPrefix(rel, ".git"+string(filepath.Separator)) {
			return nil, usageErrorf("%s is inside the folder's .git directory", p)
		}
		out = append(out, filepath.ToSlash(rel))
	}
	return out, nil
}

// displayPaths returns a function showing folder paths relative to base,
// the way they can be typed back.
func displayPaths(s *client.Syncer, base string) func(string) string {
	top, baseAbs, err := folderAndBase(s, base)
	if err != nil {
		return func(p string) string { return p }
	}
	return func(p string) string {
		rel, err := filepath.Rel(baseAbs, filepath.Join(top, filepath.FromSlash(p)))
		if err != nil {
			return p
		}
		return rel
	}
}

// folderAndBase resolves the folder's top and base, following symlinks so
// the two compare.
func folderAndBase(s *client.Syncer, base string) (top, baseAbs string, err error) {
	if top, err = filepath.EvalSymlinks(s.Dir()); err != nil {
		return "", "", err
	}
	if baseAbs, err = filepath.Abs(base); err != nil {
		return "", "", err
	}
	if b, err := filepath.EvalSymlinks(baseAbs); err == nil {
		baseAbs = b
	}
	if rel, err := filepath.Rel(top, baseAbs); err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		baseAbs = top // e.g. status listing every folder from elsewhere
	}
	return top, baseAbs, nil
}

func (a *app) logCmd() *cobra.Command {
	var dir string
	var n int
	cmd := &cobra.Command{
		Use:   "log [PATH...]",
		Short: "Show a folder's history",
		Long: `Show a synced folder's history: the latest commits, newest first, each with
its date, a summary, and the files it changed. PATHs (files or
directories, relative to the current directory or to --dir) limit the
history to commits that touched them.

Use a commit's ID with "dropgitbox restore" to bring files back.`,
		Example: `  dropgitbox log
  dropgitbox log -n 5 notes/todo.md
  dropgitbox log --dir ~/Notes`,
		Annotations: map[string]string{annotationFileArgs: "true"},
		RunE: func(cmd *cobra.Command, paths []string) error {
			e, err := loadClient(a.cfgPath, false)
			if err != nil {
				return err
			}
			s, err := e.load(dir)
			if err != nil {
				return err
			}
			paths, err = folderPaths(s, dir, paths)
			if err != nil {
				return err
			}
			gitArgs := []string{"log", fmt.Sprintf("-n%d", n), "--date=format:%Y-%m-%d %H:%M:%S",
				"--format=%h  %ad  %s", "--stat=100", "--stat-graph-width=20"}
			if len(paths) > 0 {
				gitArgs = append(append(gitArgs, "--"), paths...)
			}
			out, err := s.Git().RunRaw(cmd.Context(), gitArgs...)
			if err != nil {
				return err
			}
			fmt.Print(out)
			return nil
		},
	}
	dirFlag(cmd, &dir, ".", "the synced folder `DIR`")
	cmd.Flags().IntVarP(&n, "number", "n", 20, "show at most this many commits")
	return cmd
}

func (a *app) restoreCmd() *cobra.Command {
	var dir, rev string
	cmd := &cobra.Command{
		Use:   "restore --rev REV PATH...",
		Short: "Bring files back from an earlier commit",
		Long: `Bring PATHs back to how they were at commit REV, as listed by
"dropgitbox log". PATHs are relative to the current directory (or to
--dir). The restored files are ordinary changes: they are
committed and synced like any other edit, so restoring never rewrites
history and can itself be undone.`,
		Example: `  dropgitbox log notes/todo.md
  dropgitbox restore --rev 3f2a1c9 notes/todo.md`,
		Args:        argsRange(1, -1),
		Annotations: map[string]string{annotationFileArgs: "true"},
		RunE: func(cmd *cobra.Command, paths []string) error {
			e, err := loadClient(a.cfgPath, false)
			if err != nil {
				return err
			}
			s, err := e.load(dir)
			if err != nil {
				return err
			}
			rel, err := folderPaths(s, dir, paths)
			if err != nil {
				return err
			}
			if err := s.Restore(cmd.Context(), rev, rel); err != nil {
				return err
			}
			fmt.Printf("Restored %s from %s. The change will be committed and synced like any other edit.\n",
				strings.Join(paths, ", "), rev)
			return nil
		},
	}
	dirFlag(cmd, &dir, ".", "the synced folder `DIR`")
	cmd.Flags().StringVar(&rev, "rev", "", "restore from commit `REV` (see dropgitbox log)")
	_ = cmd.MarkFlagRequired("rev")
	_ = cmd.RegisterFlagCompletionFunc("rev", noFiles)
	return cmd
}

// ---- shell completion -------------------------------------------------

// completeRepos offers the repositories on the server.
func (a *app) completeRepos(cmd *cobra.Command, _ []string, _ string) ([]string, cobra.ShellCompDirective) {
	e, err := loadClient(a.cfgPath, false)
	if err != nil || e.cfg.Client.Server == "" {
		return nil, cobra.ShellCompDirectiveNoFileComp
	}
	ctx, cancel := context.WithTimeout(cmd.Context(), 3*time.Second)
	defer cancel()
	repos, err := client.ListRepos(ctx, e.cfg.Client.Server, e.token)
	if err != nil {
		return nil, cobra.ShellCompDirectiveNoFileComp
	}
	names := make([]string, len(repos))
	for i, r := range repos {
		names[i] = r.Name
	}
	return names, cobra.ShellCompDirectiveNoFileComp
}

// completeConflicts offers the folder's conflicted files.
func (a *app) completeConflicts(dir *string) cobra.CompletionFunc {
	return func(cmd *cobra.Command, args []string, _ string) ([]string, cobra.ShellCompDirective) {
		s, err := client.Load(*dir, client.LoadOptions{Logger: slog.New(slog.DiscardHandler)})
		if err != nil {
			return nil, cobra.ShellCompDirectiveNoFileComp
		}
		files, _ := s.ConflictedFiles(cmd.Context())
		show := displayPaths(s, *dir)
		for i, f := range files {
			files[i] = show(f)
		}
		files = slices.DeleteFunc(files, func(f string) bool { return slices.Contains(args, f) })
		return files, cobra.ShellCompDirectiveNoFileComp
	}
}

// ---- output helpers ---------------------------------------------------

func printConflictHelp(ctx context.Context, s *client.Syncer) {
	cs, _ := s.Conflicts(ctx)
	show := displayPaths(s, ".")
	fmt.Printf("SYNC PAUSED in %s: changes on this device conflict with changes from the server.\n\n", s.Dir())
	printConflicts(cs, show)
	fmt.Print(`
For each file, either:
  - edit it to remove the <<<<<<< ======= >>>>>>> markers, then: dropgitbox resolve FILE
  - keep this device's version:  dropgitbox resolve --ours FILE
  - keep the server's version:   dropgitbox resolve --theirs FILE
Syncing resumes once every file is resolved.
`)
}

// printConflicts lists conflicted files the way "git status" does ("us" is
// this device, "them" the server).
func printConflicts(cs []client.Conflict, show func(string) string) {
	fmt.Println("Conflicted files (us: this device, them: the server):")
	for _, c := range cs {
		line := fmt.Sprintf("  %-16s %s", string(c.Kind)+":", show(c.Path))
		if c.Binary {
			line += "  (binary: no markers; pick a side, or name the file to keep it as it is)"
		}
		fmt.Println(line)
	}
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
