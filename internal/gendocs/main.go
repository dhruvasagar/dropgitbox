// Command gendocs generates documentation from the command definitions:
//
//	go run ./internal/gendocs [-version V] [-date RFC3339] DIR
//	go run ./internal/gendocs -markdown docs/cli-reference.md
//
// The first form, used for the release archives and packages, creates the
// man page DIR/man/dropgitbox.1, covering every command, and the shell
// completion scripts DIR/completions/{dropgitbox.bash,_dropgitbox,
// dropgitbox.fish}. With the same -version and -date, the output is
// byte-for-byte identical.
//
// The second writes the Markdown command reference kept in docs/ (run
// "go generate ./cmd/dropgitbox"); a test fails when it is out of date.
package main

import (
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/spf13/cobra"

	"github.com/dhruvasagar/dropgitbox/internal/cli"
)

func main() {
	version := flag.String("version", "dev", "version shown in the man pages")
	date := flag.String("date", "", "date shown in the man pages, RFC 3339 (default: now)")
	md := flag.String("markdown", "", "write the Markdown command reference to `FILE`")
	flag.Usage = func() {
		fmt.Fprintln(os.Stderr, "usage: go run ./internal/gendocs [-version V] [-date RFC3339] DIR\n"+
			"       go run ./internal/gendocs -markdown FILE")
		flag.PrintDefaults()
	}
	flag.Parse()
	if flag.NArg() > 1 || (flag.NArg() == 0 && *md == "") {
		flag.Usage()
		os.Exit(2)
	}
	var err error
	if *md != "" {
		err = os.WriteFile(*md, []byte(markdown(cli.NewRoot(cli.BuildInfo{}))), 0o644)
	}
	if err == nil && flag.NArg() == 1 {
		err = run(flag.Arg(0), *version, *date)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "gendocs:", err)
		os.Exit(1)
	}
}

func run(dir, version, date string) error {
	when := time.Now()
	if date != "" {
		t, err := time.Parse(time.RFC3339, date)
		if err != nil {
			return fmt.Errorf("-date: %w", err)
		}
		when = t
	}

	root := cli.NewRoot(cli.BuildInfo{Version: version})
	// Cobra adds the completion command when the command runs; add it so
	// the man page documents it.
	root.InitDefaultCompletionCmd()
	root.InitDefaultHelpFlag()
	root.InitDefaultVersionFlag()

	manDir := filepath.Join(dir, "man")
	compDir := filepath.Join(dir, "completions")
	for _, d := range []string{manDir, compDir} {
		// Start clean, so pages of removed commands don't linger.
		if err := os.RemoveAll(d); err != nil {
			return err
		}
		if err := os.MkdirAll(d, 0o755); err != nil {
			return err
		}
	}

	if err := os.WriteFile(filepath.Join(manDir, "dropgitbox.1"), manPage(root, version, when), 0o644); err != nil {
		return fmt.Errorf("man page: %w", err)
	}

	scripts := []struct {
		name string
		gen  func(*cobra.Command, *os.File) error
	}{
		{"dropgitbox.bash", func(c *cobra.Command, f *os.File) error { return c.GenBashCompletionV2(f, true) }},
		{"_dropgitbox", func(c *cobra.Command, f *os.File) error { return c.GenZshCompletion(f) }},
		{"dropgitbox.fish", func(c *cobra.Command, f *os.File) error { return c.GenFishCompletion(f, true) }},
	}
	for _, s := range scripts {
		f, err := os.Create(filepath.Join(compDir, s.name))
		if err != nil {
			return err
		}
		err = s.gen(root, f)
		if cerr := f.Close(); err == nil {
			err = cerr
		}
		if err != nil {
			return fmt.Errorf("%s: %w", s.name, err)
		}
	}
	return nil
}
