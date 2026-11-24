// Package cli implements the dropgitbox command line.
package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"regexp"
	"runtime/debug"
	"strings"

	"github.com/spf13/cobra"

	"github.com/dhruvasagar/dropgitbox/internal/client"
	"github.com/dhruvasagar/dropgitbox/internal/config"
)

// BuildInfo identifies the running build. The release build sets it with
// -ldflags; development builds fill it in from the Go build info.
type BuildInfo struct {
	Version string
	Commit  string
	Date    string
}

// version is this build's version, reported to the server and by the daemon.
var version = "dev"

// app holds the settings shared by all commands.
type app struct {
	cfgPath string
}

// Command groups, in the order the root help lists them.
const (
	groupServer  = "server"
	groupDevice  = "device"
	groupSync    = "sync"
	groupHistory = "history"
	groupOther   = "other"
)

const rootLong = `dropgitbox keeps folders in sync between your devices through a server you
run yourself. Every change is committed to git before it is synced, so the
full history of every file is kept on every device and on the server.

Set up the server node:
  dropgitbox server                 run the server (usually as a system service)
  dropgitbox device add NAME        register a device and print its invite

Set up each device:
  dropgitbox join INVITE            connect to the server
  dropgitbox setup --repo NAME --dir DIR
  dropgitbox watch                  keep every folder in sync (usually a user service)

Settings are read from a TOML config file: --config FILE, $DROPGITBOX_CONFIG,
or the first that exists of $XDG_CONFIG_HOME/dropgitbox/config.toml,
$XDG_CONFIG_DIRS and /etc/dropgitbox/config.toml ("dropgitbox config" shows
which is used).

Folder commands take --dir DIR. Without it they act on the synced folder
containing the current directory, or else on every folder in the config.`

// usageTemplate is cobra's default, except that a command with
// subcommands doesn't also show its own "[flags]" usage line.
const usageTemplate = `Usage:{{if and .Runnable (not .HasAvailableSubCommands)}}
  {{.UseLine}}{{end}}{{if .HasAvailableSubCommands}}
  {{.CommandPath}} [command]{{end}}{{if gt (len .Aliases) 0}}

Aliases:
  {{.NameAndAliases}}{{end}}{{if .HasExample}}

Examples:
{{.Example}}{{end}}{{if .HasAvailableSubCommands}}{{$cmds := .Commands}}{{if eq (len .Groups) 0}}

Available Commands:{{range $cmds}}{{if (or .IsAvailableCommand (eq .Name "help"))}}
  {{rpad .Name .NamePadding }} {{.Short}}{{end}}{{end}}{{else}}{{range $group := .Groups}}

{{.Title}}{{range $cmds}}{{if (and (eq .GroupID $group.ID) (or .IsAvailableCommand (eq .Name "help")))}}
  {{rpad .Name .NamePadding }} {{.Short}}{{end}}{{end}}{{end}}{{if not .AllChildCommandsHaveGroup}}

Additional Commands:{{range $cmds}}{{if (and (eq .GroupID "") (or .IsAvailableCommand (eq .Name "help")))}}
  {{rpad .Name .NamePadding }} {{.Short}}{{end}}{{end}}{{end}}{{end}}{{end}}{{if .HasAvailableLocalFlags}}

Flags:
{{.LocalFlags.FlagUsages | trimTrailingWhitespaces}}{{end}}{{if .HasAvailableInheritedFlags}}

Global Flags:
{{.InheritedFlags.FlagUsages | trimTrailingWhitespaces}}{{end}}{{if .HasHelpSubCommands}}

Additional help topics:{{range .Commands}}{{if .IsAdditionalHelpTopicCommand}}
  {{rpad .CommandPath .CommandPathPadding}} {{.Short}}{{end}}{{end}}{{end}}{{if .HasAvailableSubCommands}}

Use "{{.CommandPath}} [command] --help" for more information about a command.{{end}}
`

// NewRoot builds the dropgitbox command tree.
func NewRoot(bi BuildInfo) *cobra.Command {
	// List commands in the order they are added, which follows the workflow.
	cobra.EnableCommandSorting = false

	bi = fillBuildInfo(bi)
	version = bi.Version
	client.Version = bi.Version

	a := &app{}
	root := &cobra.Command{
		Use:     "dropgitbox",
		Short:   "Git-backed folder sync",
		Long:    rootLong,
		Version: versionLine(bi),
		// No Args: cobra then reports unknown commands, with suggestions.
		// Errors are printed once, by Execute.
		SilenceErrors: true,
		SilenceUsage:  true,
		// Generated man pages stay identical between builds.
		DisableAutoGenTag: true,
	}
	root.SetVersionTemplate("{{.Version}}\n")
	root.SetUsageTemplate(usageTemplate)
	root.SetFlagErrorFunc(flagError)
	root.PersistentFlags().StringVar(&a.cfgPath, "config", "",
		"read settings from `FILE` (default: search $XDG_CONFIG_HOME, $XDG_CONFIG_DIRS, /etc)")
	_ = root.MarkPersistentFlagFilename("config", "toml")

	root.AddGroup(
		&cobra.Group{ID: groupServer, Title: "Server node:"},
		&cobra.Group{ID: groupDevice, Title: "Set up a device:"},
		&cobra.Group{ID: groupSync, Title: "Sync:"},
		&cobra.Group{ID: groupHistory, Title: "Conflicts and history:"},
		&cobra.Group{ID: groupOther, Title: "Other:"},
	)
	add := func(group string, cmds ...*cobra.Command) {
		for _, c := range cmds {
			c.GroupID = group
			root.AddCommand(c)
		}
	}
	add(groupServer, a.serverCmd(), a.deviceCmd())
	add(groupDevice, a.joinCmd(), a.reposCmd(), a.setupCmd())
	add(groupSync, a.watchCmd(), a.syncCmd(), a.statusCmd(), a.eventsCmd(), a.notifyCmd())
	add(groupHistory, a.resolveCmd(), a.logCmd(), a.restoreCmd())
	add(groupOther, a.configCmd(), versionCmd(bi))

	root.SetHelpCommandGroupID(groupOther)
	root.SetCompletionCommandGroupID(groupOther)
	// Cobra adds the help and completion commands when the command runs.
	// (The man page generator adds them itself.)

	// Commands that take no file arguments shouldn't offer files to the
	// shell's completion.
	walk(root, func(c *cobra.Command) {
		if c.ValidArgsFunction == nil && c.Annotations[annotationFileArgs] == "" {
			c.ValidArgsFunction = noFiles
		}
	})
	return root
}

// Execute runs the command line args and returns the process exit code:
// 0 on success, 2 for a usage error, 1 for any other error.
func Execute(ctx context.Context, bi BuildInfo, args []string) int {
	return execute(ctx, NewRoot(bi), args, os.Stderr)
}

func execute(ctx context.Context, root *cobra.Command, args []string, stderr io.Writer) int {
	root.SetArgs(args)
	cmd, err := root.ExecuteContextC(ctx)
	if err == nil {
		return 0
	}
	fmt.Fprintln(stderr, "dropgitbox:", oneLine(err.Error()))
	if isUsageError(err) {
		fmt.Fprintf(stderr, "Run '%s --help' for usage.\n", cmd.CommandPath())
		return 2
	}
	return 1
}

// oneLine folds cobra's multi-line command suggestions
// ("...\n\nDid you mean this?\n\tstatus\n") into the message.
func oneLine(msg string) string {
	msg = strings.TrimRight(msg, "\n")
	head, sugg, ok := strings.Cut(msg, "\n\nDid you mean this?\n")
	if !ok {
		return msg
	}
	var names []string
	for _, l := range strings.Split(sugg, "\n") {
		if l = strings.TrimSpace(l); l != "" {
			names = append(names, l)
		}
	}
	return head + "; did you mean " + strings.Join(names, " or ") + "?"
}

// cobraUsageErrors start the messages of the usage errors cobra returns
// itself (unknown commands, flag-rule violations).
var cobraUsageErrors = []string{
	"unknown command ",
	"required flag(s) ",
	"if any flags in the group ",
	"at least one of the flags in the group ",
}

func isUsageError(err error) bool {
	var ue *usageError
	if errors.As(err, &ue) {
		return true
	}
	for _, p := range cobraUsageErrors {
		if strings.HasPrefix(err.Error(), p) {
			return true
		}
	}
	return false
}

// usageError is a mistake in how a command was invoked.
type usageError struct{ err error }

func (e *usageError) Error() string { return e.err.Error() }
func (e *usageError) Unwrap() error { return e.err }

func usageErrorf(format string, a ...any) error {
	return &usageError{fmt.Errorf(format, a...)}
}

// singleDash matches pflag's error for "-dir", which it reads as the
// shorthand flags -d -i -r.
var singleDash = regexp.MustCompile(`unknown shorthand flag: '.' in -([A-Za-z][\w-]+)`)

func flagError(c *cobra.Command, err error) error {
	if m := singleDash.FindStringSubmatch(err.Error()); m != nil && c.Flags().Lookup(m[1]) != nil {
		return usageErrorf("unknown flag -%s: long flags take two dashes: --%s", m[1], m[1])
	}
	return &usageError{err}
}

// argsRange accepts between min and max positional arguments (max < 0:
// any number).
func argsRange(min, max int) cobra.PositionalArgs {
	return func(cmd *cobra.Command, args []string) error {
		if len(args) < min || (max >= 0 && len(args) > max) {
			return usageErrorf("usage: %s", cmd.UseLine())
		}
		return nil
	}
}

// groupCmd makes cmd a container for subcommands: alone it prints its
// help; with an unknown subcommand it fails with a suggestion.
func groupCmd(cmd *cobra.Command) *cobra.Command {
	cmd.SuggestionsMinimumDistance = 2 // as cobra uses for the root
	cmd.Args = cobra.ArbitraryArgs
	cmd.RunE = func(c *cobra.Command, args []string) error {
		if len(args) == 0 {
			return c.Help()
		}
		msg := fmt.Sprintf("unknown command %q for %q", args[0], c.CommandPath())
		if s := c.SuggestionsFor(args[0]); len(s) > 0 {
			msg += "; did you mean " + strings.Join(s, " or ") + "?"
		}
		return usageErrorf("%s", msg)
	}
	return cmd
}

// annotationFileArgs marks commands whose arguments are paths, so the
// shell completes file names for them.
const annotationFileArgs = "dropgitbox/file-args"

func noFiles(*cobra.Command, []string, string) ([]string, cobra.ShellCompDirective) {
	return nil, cobra.ShellCompDirectiveNoFileComp
}

func walk(c *cobra.Command, fn func(*cobra.Command)) {
	fn(c)
	for _, sub := range c.Commands() {
		walk(sub, fn)
	}
}

// ---- version / config -------------------------------------------------

func fillBuildInfo(bi BuildInfo) BuildInfo {
	if bi.Version == "" {
		bi.Version = "dev"
	}
	info, ok := debug.ReadBuildInfo()
	if !ok || bi.Version != "dev" {
		return bi
	}
	if v := info.Main.Version; v != "" && v != "(devel)" {
		bi.Version = v
	}
	for _, s := range info.Settings {
		switch s.Key {
		case "vcs.revision":
			if bi.Commit == "" {
				bi.Commit = s.Value
			}
		case "vcs.time":
			if bi.Date == "" {
				bi.Date = s.Value
			}
		}
	}
	return bi
}

func versionLine(bi BuildInfo) string {
	s := "dropgitbox " + bi.Version
	if c := bi.Commit; c != "" {
		if len(c) > 12 {
			c = c[:12]
		}
		s += " (commit " + c
		if bi.Date != "" {
			s += ", built " + bi.Date
		}
		s += ")"
	}
	return s
}

func versionCmd(bi BuildInfo) *cobra.Command {
	return &cobra.Command{
		Use:   "version",
		Short: "Print the version",
		Long:  "Print dropgitbox's version, and the commit and date it was built from.",
		Args:  cobra.NoArgs,
		Run: func(cmd *cobra.Command, _ []string) {
			fmt.Fprintln(cmd.OutOrStdout(), versionLine(bi))
		},
	}
}

func (a *app) configCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "config",
		Short: "Show which config file is used",
		Long: `Show which config file is used, the order config files are searched in, and
the file that commands such as join and setup save settings to.

packaging/config.example.toml in the release archive (or
/usr/share/doc/dropgitbox/config.example.toml from a package) documents
every setting.`,
		Example: `  dropgitbox config
  dropgitbox --config ~/box.toml config`,
		Args: cobra.NoArgs,
		RunE: func(*cobra.Command, []string) error {
			cfg, err := loadConfig(a.cfgPath)
			if err != nil {
				return err
			}
			if cfg.Path() == "" {
				fmt.Println("No config file found. Searched:")
			} else {
				fmt.Println("Using:", cfg.Path())
				fmt.Println("Search order:")
			}
			fmt.Println("  --config flag, $" + config.EnvConfig)
			for _, p := range config.SearchPaths() {
				fmt.Println("  " + p)
			}
			if w, err := cfg.WritePath(); err == nil {
				fmt.Println("Settings are saved to:", w)
			}
			return nil
		},
	}
}

// ---- helpers ----------------------------------------------------------

func newLogger(verbose bool) *slog.Logger {
	level := slog.LevelInfo
	if verbose {
		level = slog.LevelDebug
	}
	return slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: level}))
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
