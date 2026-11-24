package main

import (
	"os"
	"strings"
	"testing"

	"github.com/dhruvasagar/dropgitbox/internal/cli"
)

// TestCLIReferenceUpToDate keeps docs/cli-reference.md in step with the
// command definitions.
func TestCLIReferenceUpToDate(t *testing.T) {
	want := markdown(cli.NewRoot(cli.BuildInfo{}))
	got, err := os.ReadFile("../../docs/cli-reference.md")
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != want {
		t.Fatal("docs/cli-reference.md is out of date: run `go generate ./cmd/dropgitbox` and commit the result")
	}
}

func TestManPagesAndCompletions(t *testing.T) {
	dir := t.TempDir()
	if err := run(dir, "v1.2.3", "2026-01-02T03:04:05Z"); err != nil {
		t.Fatal(err)
	}
	for _, f := range []string{"man/dropgitbox.1", "completions/_dropgitbox",
		"completions/dropgitbox.bash", "completions/dropgitbox.fish"} {
		data, err := os.ReadFile(dir + "/" + f)
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(string(data), "dropgitbox") {
			t.Errorf("%s doesn't mention dropgitbox", f)
		}
	}
}
