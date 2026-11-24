package main

import (
	"fmt"
	"strings"
	"time"

	"github.com/cpuguy83/go-md2man/v2/md2man"
	"github.com/spf13/cobra"
	"github.com/spf13/pflag"
)

// manPage renders the whole command tree as one man page, dropgitbox(1).
// It is written in go-md2man's Markdown dialect and converted to roff.
func manPage(root *cobra.Command, version string, date time.Time) []byte {
	var b strings.Builder
	fmt.Fprintf(&b, "%% %q %q %q %q %q\n\n", "DROPGITBOX", "1", date.UTC().Format("Jan 2006"),
		"dropgitbox "+version, "dropgitbox manual")

	b.WriteString("# NAME\n\ndropgitbox - " + strings.ToLower(root.Short[:1]) + root.Short[1:] + "\n\n")
	b.WriteString("# SYNOPSIS\n\n**dropgitbox** *command* [*flags*] [*arguments*]\n\n")
	b.WriteString("**dropgitbox** *command* **--help**\n\n")
	b.WriteString("# DESCRIPTION\n\n" + prose(root.Long))

	b.WriteString("# GLOBAL OPTIONS\n\n")
	root.PersistentFlags().VisitAll(func(f *pflag.Flag) { manFlag(&b, f) })
	manFlag(&b, root.Flags().Lookup("help"))
	manFlag(&b, root.Flags().Lookup("version"))

	b.WriteString("# COMMANDS\n\n")
	for _, c := range commandList(root)[1:] {
		manCommand(&b, c)
	}

	b.WriteString(manTrailer)
	return md2man.Render([]byte(b.String()))
}

func manCommand(b *strings.Builder, c *cobra.Command) {
	if c.HasAvailableSubCommands() {
		// Its subcommands follow, each with its own section.
		fmt.Fprintf(b, "## %s *command*\n\n%s\n\n", c.CommandPath(), prose(c.Long))
		c.LocalFlags().VisitAll(func(f *pflag.Flag) {
			if f.Name != "help" {
				manFlag(b, f)
			}
		})
		return
	}
	use := strings.TrimSuffix(c.UseLine(), " [flags]")
	fmt.Fprintf(b, "## %s\n\n%s", use, prose(c.Long))
	if len(c.Aliases) > 0 {
		fmt.Fprintf(b, "Also: %s.\n\n", "**"+strings.Join(c.Aliases, "**, **")+"**")
	}
	c.LocalFlags().VisitAll(func(f *pflag.Flag) {
		if f.Name != "help" {
			manFlag(b, f)
		}
	})
	if c.Example != "" {
		b.WriteString("Examples:\n\n```\n")
		for _, l := range strings.Split(c.Example, "\n") {
			b.WriteString(strings.TrimPrefix(l, "  ") + "\n")
		}
		b.WriteString("```\n\n")
	}
}

// manFlag writes one flag as an indented definition.
func manFlag(b *strings.Builder, f *pflag.Flag) {
	if f == nil || f.Hidden {
		return
	}
	name, usage := pflag.UnquoteUsage(f)
	term := "**--" + f.Name + "**"
	if f.Shorthand != "" {
		term = "**-" + f.Shorthand + "**, " + term
	}
	if name != "" && f.Value.Type() != "bool" {
		term += " *" + name + "*"
	}
	switch f.DefValue {
	case "", "false", "[]":
	default:
		usage += fmt.Sprintf(" (default %s)", f.DefValue)
	}
	fmt.Fprintf(b, "%s\n: %s\n\n", term, usage)
}

const manTrailer = `# FILES

*~/.config/dropgitbox/config.toml*
: The config file (see **dropgitbox config** for the full search order). It holds no secrets, so it can live in a dotfiles repository.

*~/.config/dropgitbox/token*
: This device's token, written by **dropgitbox join**. It must not be readable by other users.

*/etc/dropgitbox/config.toml*
: The config of the packaged server.

*~/.local/share/dropgitbox*, */var/lib/dropgitbox*
: The server's data: one bare git repository per folder, plus *devices.json* (token hashes) and *activity.json*. Back up this directory to back up the server.

*$XDG_RUNTIME_DIR/dropgitbox/daemon.sock*
: The daemon's control socket, used by **status**, **events** and **sync**.

# ENVIRONMENT

**DROPGITBOX_CONFIG**
: Config file to use, unless **--config** is given.

**DROPGITBOX_TOKEN**
: Device token (client) or shared token (server), overriding the config.

**DROPGITBOX_SOCKET**
: Path of the daemon's control socket.

**XDG_CONFIG_HOME**, **XDG_CONFIG_DIRS**, **XDG_DATA_HOME**, **XDG_RUNTIME_DIR**, **XDG_STATE_HOME**
: Where config, server data and the daemon socket live, as in the XDG Base Directory specification.

# EXIT STATUS

**0** on success, **2** when a command is used wrongly (an unknown command or flag, or missing arguments), and **1** for any other error.

# SEE ALSO

**git**(1)

The guides at https://github.com/dhruvasagar/dropgitbox/tree/main/docs cover setting up a server and devices, conflicts, history, notifications and configuration in detail.
`
