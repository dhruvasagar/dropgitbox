package cli

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/spf13/cobra"
	"github.com/spf13/pflag"
)

func newTestRoot(t *testing.T) *cobra.Command {
	t.Helper()
	// Keep the tests away from the user's config and daemon.
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("XDG_CONFIG_DIRS", t.TempDir())
	t.Setenv("DROPGITBOX_CONFIG", "")
	t.Setenv("DROPGITBOX_SOCKET", filepath.Join(t.TempDir(), "none.sock"))
	return NewRoot(BuildInfo{Version: "v1.2.3", Commit: "0123456789abcdef", Date: "2026-01-02T03:04:05Z"})
}

// ownCommands lists dropgitbox's commands, leaving out the help and
// completion commands that cobra provides (and documents) itself.
func ownCommands(root *cobra.Command) []*cobra.Command {
	var out []*cobra.Command
	var visit func(*cobra.Command)
	visit = func(c *cobra.Command) {
		if c.Hidden || (c.Parent() == root && (c.Name() == "help" || c.Name() == "completion")) {
			return
		}
		out = append(out, c)
		for _, sub := range c.Commands() {
			visit(sub)
		}
	}
	visit(root)
	return out
}

func TestEveryCommandIsDocumented(t *testing.T) {
	root := newTestRoot(t)
	for _, c := range ownCommands(root) {
		path := c.CommandPath()
		if c.Short == "" {
			t.Errorf("%s: no Short description", path)
		}
		if strings.HasSuffix(c.Short, ".") {
			t.Errorf("%s: Short description should not end with a period: %q", path, c.Short)
		}
		if strings.TrimSpace(c.Long) == "" {
			t.Errorf("%s: no Long description", path)
		}
		// Every command that does something shows how to use it.
		if c.Runnable() && !c.HasSubCommands() && c != root && c.Name() != "version" && c.Example == "" {
			t.Errorf("%s: no Example", path)
		}
		for _, l := range strings.Split(c.Example, "\n") {
			if l != "" && !strings.HasPrefix(l, "  ") {
				t.Errorf("%s: example lines should be indented two spaces: %q", path, l)
			}
		}
		c.LocalFlags().VisitAll(func(f *pflag.Flag) {
			if f.Usage == "" {
				t.Errorf("%s --%s: no usage text", path, f.Name)
			}
			if strings.Count(f.Usage, "`")%2 != 0 {
				t.Errorf("%s --%s: unbalanced backquote in usage %q", path, f.Name, f.Usage)
			}
		})
	}
}

func TestHelpRendersForEveryCommand(t *testing.T) {
	for _, c := range ownCommands(newTestRoot(t)) {
		args := append(strings.Fields(c.CommandPath())[1:], "--help")
		t.Run(strings.Join(args, " "), func(t *testing.T) {
			root := newTestRoot(t)
			var out, errOut bytes.Buffer
			root.SetOut(&out)
			if code := execute(context.Background(), root, args, &errOut); code != 0 {
				t.Fatalf("exit %d: %s", code, errOut.String())
			}
			if !strings.Contains(out.String(), "Usage:") {
				t.Fatalf("no usage in help output:\n%s", out.String())
			}
			if c.Long != "" && !strings.Contains(out.String(), strings.SplitN(c.Long, "\n", 2)[0]) {
				t.Errorf("help doesn't show the long description:\n%s", out.String())
			}
		})
	}
}

func TestUsageErrors(t *testing.T) {
	tests := []struct {
		args string
		want string
	}{
		{"setup -dir x", "unknown flag -dir: long flags take two dashes: --dir"},
		{"device add laptop -config x", "long flags take two dashes: --config"},
		{"status --bogus", "unknown flag: --bogus"},
		{"bogus", `unknown command "bogus" for "dropgitbox"`},
		{"stauts", `unknown command "stauts" for "dropgitbox"; did you mean status?`},
		{"device ad laptop", `unknown command "ad" for "dropgitbox device"; did you mean add?`},
		{"notify tset", `unknown command "tset" for "dropgitbox notify"; did you mean test?`},
		{"join", "usage: dropgitbox join INVITE [flags]"},
		{"device add", "usage: dropgitbox device add NAME [flags]"},
		{"device show a b", "usage: dropgitbox device show NAME [flags]"},
		{"restore --rev HEAD", "usage: dropgitbox restore --rev REV PATH... [flags]"},
		{"restore file.txt", `required flag(s) "rev" not set`},
		{"resolve --ours --theirs", "[ours theirs] were all set"},
		{"status --short --json", "[json short] were all set"},
		{"version extra", `unknown command "extra" for "dropgitbox version"`},
	}
	for _, tt := range tests {
		t.Run(tt.args, func(t *testing.T) {
			root := newTestRoot(t)
			var errOut bytes.Buffer
			code := execute(context.Background(), root, strings.Fields(tt.args), &errOut)
			if code != 2 {
				t.Errorf("exit %d, want 2", code)
			}
			if !strings.Contains(errOut.String(), tt.want) {
				t.Errorf("stderr %q does not contain %q", errOut.String(), tt.want)
			}
			if !strings.Contains(errOut.String(), "--help' for usage.") {
				t.Errorf("stderr %q does not point to --help", errOut.String())
			}
		})
	}
}

func TestGroupCommandAloneShowsHelp(t *testing.T) {
	for _, args := range []string{"device", "notify", ""} {
		root := newTestRoot(t)
		var out, errOut bytes.Buffer
		root.SetOut(&out)
		if code := execute(context.Background(), root, strings.Fields(args), &errOut); code != 0 {
			t.Fatalf("%q: exit %d: %s", args, code, errOut.String())
		}
		if !strings.Contains(out.String(), "Available Commands:") && !strings.Contains(out.String(), "Server node:") {
			t.Errorf("%q: no command list in:\n%s", args, out.String())
		}
	}
}

func TestVersion(t *testing.T) {
	const want = "dropgitbox v1.2.3 (commit 0123456789ab, built 2026-01-02T03:04:05Z)\n"
	for _, args := range []string{"version", "--version", "-v"} {
		root := newTestRoot(t)
		var out bytes.Buffer
		root.SetOut(&out)
		if code := execute(context.Background(), root, []string{args}, &out); code != 0 {
			t.Fatalf("%s: exit %d", args, code)
		}
		if out.String() != want {
			t.Errorf("%s printed %q, want %q", args, out.String(), want)
		}
	}
}

func TestCompletionScripts(t *testing.T) {
	for _, shell := range []string{"bash", "zsh", "fish"} {
		root := newTestRoot(t)
		var out, errOut bytes.Buffer
		root.SetOut(&out)
		if code := execute(context.Background(), root, []string{"completion", shell}, &errOut); code != 0 {
			t.Fatalf("%s: exit %d: %s", shell, code, errOut.String())
		}
		if !strings.Contains(out.String(), "dropgitbox") {
			t.Errorf("%s: script doesn't mention dropgitbox", shell)
		}
	}

	// Subcommands and flags complete; commands without file arguments
	// don't offer files.
	root := newTestRoot(t)
	var out bytes.Buffer
	root.SetOut(&out)
	execute(context.Background(), root, []string{cobra.ShellCompRequestCmd, "device", ""}, &out)
	for _, want := range []string{"add", "list", "revoke", ":4"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("device completion %q lacks %q", out.String(), want)
		}
	}
}

// TestDocsUseDoubleDashFlags keeps the documentation in step with the
// command line: long flags take two dashes ("--dir", not "-dir").
func TestDocsUseDoubleDashFlags(t *testing.T) {
	long := map[string]bool{}
	walk(newTestRoot(t), func(c *cobra.Command) {
		c.Flags().VisitAll(func(f *pflag.Flag) { long[f.Name] = true })
		c.PersistentFlags().VisitAll(func(f *pflag.Flag) { long[f.Name] = true })
	})

	files := []string{"../../README.md", "../../install.sh"}
	for _, pattern := range []string{"../../docs/*.md", "../../packaging/*", "../../packaging/*/*"} {
		m, _ := filepath.Glob(pattern)
		files = append(files, m...)
	}
	singleDash := regexp.MustCompile("(?:^|[\\s\"'(\\[`|/])-([a-z][a-z-]+)\\b")
	for _, file := range files {
		data, err := os.ReadFile(file)
		if err != nil {
			if fi, serr := os.Stat(file); serr == nil && fi.IsDir() {
				continue
			}
			t.Fatal(err)
		}
		for i, line := range strings.Split(string(data), "\n") {
			for _, m := range singleDash.FindAllStringSubmatch(line, -1) {
				if long[m[1]] {
					t.Errorf("%s:%d: -%s should be --%s: %s", file, i+1, m[1], m[1], strings.TrimSpace(line))
				}
			}
		}
	}
}
