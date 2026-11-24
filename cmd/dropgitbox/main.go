// Command dropgitbox is a Dropbox-like folder sync service backed by git.
// Every change is auto-committed before it is synced, so the full history
// of every file is kept on every device and on the server.
//
// The commands live in internal/cli; run "dropgitbox help" for the list.
package main

//go:generate go run ../../internal/gendocs -markdown ../../docs/cli-reference.md

import (
	"context"
	"os"
	"os/signal"
	"syscall"

	"github.com/dhruvasagar/dropgitbox/internal/cli"
)

// Set by the release build (-ldflags "-X main.version=...").
var (
	version = "dev"
	commit  = ""
	date    = ""
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	code := cli.Execute(ctx, cli.BuildInfo{Version: version, Commit: commit, Date: date}, os.Args[1:])
	stop()
	os.Exit(code)
}
