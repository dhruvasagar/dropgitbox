package main

import (
	"fmt"
	"regexp"
	"strings"

	"github.com/spf13/cobra"
	"github.com/spf13/pflag"
)

// markdown renders the whole command tree as one Markdown reference page.
func markdown(root *cobra.Command) string {
	cmds := commandList(root)

	var b strings.Builder
	b.WriteString("# Command reference\n\n")
	b.WriteString("<!-- Generated from the command definitions by `go generate ./cmd/dropgitbox`. Do not edit. -->\n\n")
	b.WriteString("Every command also prints this with `--help` (or `dropgitbox help COMMAND`), and the\n")
	b.WriteString("installers include it as the man page `man dropgitbox`. See [the CLI guide](cli.md)\n")
	b.WriteString("for conventions shared by all commands.\n\n")
	for _, c := range cmds {
		depth := len(strings.Fields(c.CommandPath())) - 1
		fmt.Fprintf(&b, "%s- [%s](#%s): %s\n", strings.Repeat("  ", max(depth-1, 0)), c.CommandPath(), anchor(c), c.Short)
	}
	for _, c := range cmds {
		writeCommand(&b, c)
	}
	return b.String()
}

// commandList lists root and its documented commands, depth first, in
// the order the help shows them.
func commandList(root *cobra.Command) []*cobra.Command {
	var cmds []*cobra.Command
	var visit func(*cobra.Command)
	visit = func(c *cobra.Command) {
		if c.Hidden || c.Name() == "help" {
			return
		}
		cmds = append(cmds, c)
		for _, sub := range c.Commands() {
			visit(sub)
		}
	}
	visit(root)
	return cmds
}

func anchor(c *cobra.Command) string {
	return strings.ReplaceAll(c.CommandPath(), " ", "-")
}

func writeCommand(b *strings.Builder, c *cobra.Command) {
	fmt.Fprintf(b, "\n## %s\n\n%s.\n\n", c.CommandPath(), c.Short)
	if c.HasAvailableSubCommands() {
		fmt.Fprintf(b, "```text\n%s COMMAND\n```\n\n", c.CommandPath())
	} else {
		fmt.Fprintf(b, "```text\n%s\n```\n\n", c.UseLine())
	}
	if c.Long != "" {
		b.WriteString(prose(c.Long))
	}
	if c.HasAvailableSubCommands() {
		b.WriteString("Commands:\n\n")
		for _, sub := range c.Commands() {
			if sub.IsAvailableCommand() && sub.Name() != "help" {
				fmt.Fprintf(b, "- [%s](#%s): %s\n", sub.Name(), anchor(sub), sub.Short)
			}
		}
		b.WriteString("\n")
	}
	if len(c.Aliases) > 0 {
		fmt.Fprintf(b, "Aliases: `%s`\n\n", strings.Join(c.Aliases, "`, `"))
	}
	if c.Example != "" {
		b.WriteString("Examples:\n\n```sh\n")
		for _, l := range strings.Split(c.Example, "\n") {
			b.WriteString(strings.TrimPrefix(l, "  ") + "\n")
		}
		b.WriteString("```\n\n")
	}
	flagTable(b, "Flags", c.LocalNonPersistentFlags(), c.PersistentFlags(), c)
	flagTable(b, "Inherited flags", c.InheritedFlags(), nil, c)
}

// flagTable lists flags (and extra, for persistent flags defined on c).
func flagTable(b *strings.Builder, title string, flags, extra *pflag.FlagSet, c *cobra.Command) {
	type row struct{ flag, def, usage string }
	var rows []row
	add := func(f *pflag.Flag) {
		if f.Hidden || f.Name == "help" {
			return
		}
		name, usage := pflag.UnquoteUsage(f)
		cell := "`--" + f.Name
		if name != "" && f.Value.Type() != "bool" {
			cell += " " + name
		}
		cell += "`"
		if f.Shorthand != "" {
			cell = "`-" + f.Shorthand + "`, " + cell
		}
		def := ""
		switch f.DefValue {
		case "", "false", "[]":
		default:
			def = "`" + f.DefValue + "`"
		}
		rows = append(rows, row{cell, def, strings.ReplaceAll(usage, "|", `\|`)})
	}
	flags.VisitAll(add)
	if extra != nil && c.HasParent() {
		extra.VisitAll(add)
	}
	if len(rows) == 0 {
		return
	}
	fmt.Fprintf(b, "%s:\n\n| Flag | Default | Description |\n| --- | --- | --- |\n", title)
	for _, r := range rows {
		fmt.Fprintf(b, "| %s | %s | %s |\n", r.flag, r.def, r.usage)
	}
	b.WriteString("\n")
}

var (
	quotedCmd = regexp.MustCompile(`"(dropgitbox[^"]*)"`)
	markers   = regexp.MustCompile(`<{7} ={7} >{7}`)
	flagWord  = regexp.MustCompile(`(^|[\s(])(--?[a-z][a-z-]*)\b`)
)

// prose converts a command's plain-text description to Markdown: runs of
// indented lines become code blocks, other lines are joined into
// paragraphs, and command names and flags are set in code.
func prose(text string) string {
	var b strings.Builder
	var para, code []string
	flushPara := func() {
		if len(para) == 0 {
			return
		}
		p := strings.Join(para, " ")
		p = markers.ReplaceAllString(p, "`$0`")
		p = quotedCmd.ReplaceAllString(p, "`$1`")
		p = outsideCode(p, func(s string) string { return flagWord.ReplaceAllString(s, "$1`$2`") })
		b.WriteString(p + "\n\n")
		para = nil
	}
	flushCode := func() {
		if len(code) == 0 {
			return
		}
		b.WriteString("```text\n" + strings.Join(code, "\n") + "\n```\n\n")
		code = nil
	}
	for _, l := range strings.Split(strings.TrimSpace(text), "\n") {
		switch {
		case strings.TrimSpace(l) == "":
			flushPara()
			flushCode()
		case strings.HasPrefix(l, "  "):
			flushPara()
			code = append(code, strings.TrimPrefix(l, "  "))
		default:
			flushCode()
			para = append(para, l)
		}
	}
	flushPara()
	flushCode()
	return b.String()
}

// outsideCode applies fn to the parts of s that are not `code spans`.
func outsideCode(s string, fn func(string) string) string {
	parts := strings.Split(s, "`")
	for i := 0; i < len(parts); i += 2 {
		parts[i] = fn(parts[i])
	}
	return strings.Join(parts, "`")
}
