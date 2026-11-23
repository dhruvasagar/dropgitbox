// Command dropgitbox is a Dropbox-like folder sync service backed by git.
// Every change is auto-committed before it is synced, so the full history
// of every file is kept on every device and on the server.
package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"runtime/debug"
	"syscall"

	"github.com/dhruvasagar/dropgitbox/internal/client"
	"github.com/dhruvasagar/dropgitbox/internal/config"
)

// Set by the release build (-ldflags "-X main.version=...").
var (
	version = "dev"
	commit  = ""
	date    = ""
)

const usage = `dropgitbox - git-backed folder sync

Server node:
  dropgitbox server                      run the server
  dropgitbox device add NAME             register a device and print its invite
  dropgitbox device list | revoke NAME   manage devices

Client device:
  dropgitbox join INVITE                 connect this device to a server
  dropgitbox repos                       list folders available on the server
  dropgitbox setup -repo NAME -dir DIR   sync a folder (and register it in the config)
  dropgitbox watch                       run the sync daemon for all configured folders
  dropgitbox sync                        commit and sync once
  dropgitbox status [-short | -json]      live daemon state + folder details
  dropgitbox events [-json]              stream sync events from the daemon
  dropgitbox notify test                 check desktop notifications and the hook
  dropgitbox resolve [-ours | -theirs] [-force] [PATH...]
  dropgitbox log [-n N] [PATH...]
  dropgitbox restore -rev REV PATH...

Other:
  dropgitbox config                      show which config file is used
  dropgitbox version

Every command accepts -config FILE (default: $XDG_CONFIG_HOME/dropgitbox/config.toml).
Folder commands accept -dir DIR; without it they act on the folder containing
the current directory, or on every configured folder.
Run "dropgitbox <command> -h" for details.
`

func main() {
	if len(os.Args) < 2 {
		fmt.Fprint(os.Stderr, usage)
		os.Exit(2)
	}
	cmd, args := os.Args[1], os.Args[2:]

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	commands := map[string]func(context.Context, []string) error{
		"server":  cmdServer,
		"device":  cmdDevice,
		"join":    cmdJoin,
		"repos":   cmdRepos,
		"setup":   cmdSetup,
		"watch":   cmdWatch,
		"sync":    cmdSync,
		"status":  cmdStatus,
		"resolve": cmdResolve,
		"log":     cmdLog,
		"restore": cmdRestore,
		"events":  cmdEvents,
		"notify":  cmdNotify,
		"config":  cmdConfig,
		"version": cmdVersion,
	}
	switch cmd {
	case "help", "-h", "--help":
		fmt.Print(usage)
		return
	case "-v", "--version":
		cmd = "version"
	}
	client.Version = version
	run, ok := commands[cmd]
	if !ok {
		fmt.Fprintf(os.Stderr, "dropgitbox: unknown command %q\n\n%s", cmd, usage)
		os.Exit(2)
	}
	if err := run(ctx, args); err != nil {
		fmt.Fprintln(os.Stderr, "dropgitbox:", err)
		os.Exit(1)
	}
}

func cmdVersion(_ context.Context, _ []string) error {
	v, c, d := version, commit, date
	if bi, ok := debug.ReadBuildInfo(); ok && v == "dev" {
		if bi.Main.Version != "" && bi.Main.Version != "(devel)" {
			v = bi.Main.Version
		}
		for _, s := range bi.Settings {
			switch s.Key {
			case "vcs.revision":
				if c == "" {
					c = s.Value
				}
			case "vcs.time":
				if d == "" {
					d = s.Value
				}
			}
		}
	}
	fmt.Printf("dropgitbox %s", v)
	if c != "" {
		if len(c) > 12 {
			c = c[:12]
		}
		fmt.Printf(" (commit %s", c)
		if d != "" {
			fmt.Printf(", built %s", d)
		}
		fmt.Print(")")
	}
	fmt.Println()
	return nil
}

func cmdConfig(_ context.Context, args []string) error {
	fs, cfgPath := newFlags("config")
	parse(fs, args)
	cfg, err := loadConfig(*cfgPath)
	if err != nil {
		return err
	}
	if cfg.Path() == "" {
		fmt.Println("No config file found. Searched:")
	} else {
		fmt.Println("Using:", cfg.Path())
		fmt.Println("Search order:")
	}
	fmt.Println("  -config flag, $" + config.EnvConfig)
	for _, p := range config.SearchPaths() {
		fmt.Println("  " + p)
	}
	if w, err := cfg.WritePath(); err == nil {
		fmt.Println("Settings are saved to:", w)
	}
	return nil
}

// ---- helpers ----------------------------------------------------------

func newLogger(verbose bool) *slog.Logger {
	level := slog.LevelInfo
	if verbose {
		level = slog.LevelDebug
	}
	return slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: level}))
}

// newFlags returns a flag set with the common -config flag.
func newFlags(name string) (*flag.FlagSet, *string) {
	fs := flag.NewFlagSet("dropgitbox "+name, flag.ExitOnError)
	cfgPath := fs.String("config", "", "config file (default: search $XDG_CONFIG_HOME, $XDG_CONFIG_DIRS, /etc)")
	return fs, cfgPath
}

// parse parses args allowing flags and positional arguments to be mixed
// ("device add laptop -url X" as well as "device add -url X laptop").
func parse(fs *flag.FlagSet, args []string) []string {
	var positional []string
	for {
		_ = fs.Parse(args)
		rest := fs.Args()
		if len(rest) == 0 {
			return positional
		}
		if rest[0] == "--" {
			return append(positional, rest[1:]...)
		}
		positional = append(positional, rest[0])
		args = rest[1:]
	}
}

// isSet reports whether a flag was given explicitly.
func isSet(fs *flag.FlagSet, name string) bool {
	set := false
	fs.Visit(func(f *flag.Flag) {
		if f.Name == name {
			set = true
		}
	})
	return set
}

func loadConfig(path string) (*config.Config, error) {
	cfg, err := config.Load(path)
	if err != nil {
		return nil, err
	}
	for _, w := range cfg.Warnings() {
		fmt.Fprintln(os.Stderr, "dropgitbox: warning:", w)
	}
	return cfg, nil
}
