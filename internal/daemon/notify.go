package daemon

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"time"

	"github.com/dhruvasagar/dropgitbox/internal/client"
)

// DefaultDesktopEvents are the events shown as desktop notifications by
// default: the ones that need the user's attention, plus recovery from
// them. Routine syncing is visible through `dropgitbox status` instead.
var DefaultDesktopEvents = []client.EventType{
	client.EventConflict, client.EventResolved, client.EventOffline,
	client.EventOnline, client.EventUnauthorized,
}

// DefaultCommandEvents are the events passed to the hook command by default.
var DefaultCommandEvents = []client.EventType{
	client.EventConflict, client.EventResolved, client.EventOffline, client.EventOnline,
	client.EventUnauthorized, client.EventRemoteChanges, client.EventSynced, client.EventError,
}

// NotifyOptions configures notifications.
type NotifyOptions struct {
	// Desktop enables native desktop notifications.
	Desktop       bool
	DesktopEvents []client.EventType
	// Command, if set, is run for each of CommandEvents.
	Command       []string
	CommandEvents []client.EventType
	// CommandTimeout bounds each hook run. Default 30s.
	CommandTimeout time.Duration
}

// ParseEvents turns configured event names into types, accepting "*" for
// all events.
func ParseEvents(names []string) ([]client.EventType, error) {
	var out []client.EventType
	for _, n := range names {
		if n == "*" || n == "all" {
			return slices.Clone(client.AllEvents), nil
		}
		t := client.EventType(n)
		if !slices.Contains(client.AllEvents, t) {
			return nil, fmt.Errorf("unknown event %q (known: %s)", n, eventNames())
		}
		out = append(out, t)
	}
	return out, nil
}

func eventNames() string {
	var s []string
	for _, e := range client.AllEvents {
		s = append(s, string(e))
	}
	return strings.Join(s, ", ")
}

type notifier interface {
	run(ctx context.Context, events <-chan client.Event)
}

func newNotifiers(o NotifyOptions, log *slog.Logger) []notifier {
	var ns []notifier
	if o.Desktop {
		if send := desktopSender(); send != nil {
			ns = append(ns, &desktop{events: o.DesktopEvents, send: send, log: log})
		} else {
			log.Info("desktop notifications unavailable (no notify-send/gdbus or no desktop session)")
		}
	}
	if len(o.Command) > 0 {
		timeout := o.CommandTimeout
		if timeout <= 0 {
			timeout = 30 * time.Second
		}
		ns = append(ns, &hook{argv: o.Command, events: o.CommandEvents, timeout: timeout, log: log})
	}
	return ns
}

// ---- desktop ------------------------------------------------------------

type desktop struct {
	events []client.EventType
	send   func(title, body string, urgent bool) error
	log    *slog.Logger
}

func (n *desktop) run(ctx context.Context, events <-chan client.Event) {
	for {
		select {
		case <-ctx.Done():
			return
		case ev := <-events:
			if !slices.Contains(n.events, ev.Type) {
				continue
			}
			title, body, urgent := Describe(ev)
			if err := n.send(title, body, urgent); err != nil {
				n.log.Warn("desktop notification failed", "err", err)
			}
		}
	}
}

// Describe renders an event as a short title and body.
func Describe(ev client.Event) (title, body string, urgent bool) {
	name := filepath.Base(ev.Folder)
	files := func(max int) string {
		if len(ev.Files) == 0 {
			return ""
		}
		shown := ev.Files
		if len(shown) > max {
			shown = shown[:max]
		}
		s := strings.Join(shown, ", ")
		if extra := len(ev.Files) - len(shown); extra > 0 {
			s += fmt.Sprintf(" and %d more", extra)
		}
		return s
	}
	switch ev.Type {
	case client.EventConflict:
		return fmt.Sprintf("%s: sync paused, conflict", name),
			fmt.Sprintf("Conflicting changes in %s.\nEdit them, then run: dropgitbox resolve -dir %s", files(5), ev.Folder), true
	case client.EventResolved:
		return fmt.Sprintf("%s: conflict resolved", name), "Syncing has resumed.", false
	case client.EventOffline:
		return fmt.Sprintf("%s: can't sync", name), ev.Message, true
	case client.EventOnline:
		return fmt.Sprintf("%s: syncing again", name), "The connection to the server is working again.", false
	case client.EventUnauthorized:
		return fmt.Sprintf("%s: access denied", name),
			"The server rejected this device's token. Ask for a new invite and run `dropgitbox join -force`.", true
	case client.EventRemoteChanges:
		return fmt.Sprintf("%s: %d file(s) updated", name, ev.Pulled), files(5), false
	case client.EventSynced:
		return fmt.Sprintf("%s: synced", name),
			fmt.Sprintf("%d committed, %d updated from the server", ev.Committed, ev.Pulled), false
	case client.EventError:
		return fmt.Sprintf("%s: sync failed (will retry)", name), ev.Message, false
	case client.EventSyncing:
		return fmt.Sprintf("%s: syncing", name), "", false
	case client.EventPending:
		return fmt.Sprintf("%s: changes pending", name), "", false
	}
	return name, string(ev.Type), false
}

// desktopSender picks the platform's notification mechanism, or nil when
// there is none (e.g. a headless machine).
func desktopSender() func(title, body string, urgent bool) error {
	switch runtime.GOOS {
	case "darwin":
		if _, err := exec.LookPath("osascript"); err != nil {
			return nil
		}
		return func(title, body string, _ bool) error {
			// Texts are passed as arguments, never interpolated into the
			// script, so nothing in a file name can inject AppleScript.
			return run("osascript",
				"-e", "on run argv",
				"-e", "display notification (item 2 of argv) with title \"dropgitbox\" subtitle (item 1 of argv)",
				"-e", "end run", title, body)
		}
	case "linux", "freebsd", "openbsd", "netbsd":
		if os.Getenv("DBUS_SESSION_BUS_ADDRESS") == "" && !hasSessionBus() {
			return nil
		}
		if _, err := exec.LookPath("notify-send"); err == nil {
			return func(title, body string, urgent bool) error {
				urgency := "normal"
				if urgent {
					urgency = "critical"
				}
				return run("notify-send", "--app-name=dropgitbox", "--urgency="+urgency, "--", title, body)
			}
		}
		if _, err := exec.LookPath("gdbus"); err == nil {
			return func(title, body string, urgent bool) error {
				hints := "{}"
				if urgent {
					hints = "{'urgency': <byte 2>}"
				}
				return run("gdbus", "call", "--session",
					"--dest", "org.freedesktop.Notifications",
					"--object-path", "/org/freedesktop/Notifications",
					"--method", "org.freedesktop.Notifications.Notify",
					"dropgitbox", "0", "", title, body, "[]", hints, "-1")
			}
		}
	}
	return nil
}

func hasSessionBus() bool {
	d := os.Getenv("XDG_RUNTIME_DIR")
	if d == "" {
		return false
	}
	_, err := os.Stat(filepath.Join(d, "bus"))
	return err == nil
}

func run(name string, args ...string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, name, args...).CombinedOutput()
	if err != nil {
		return fmt.Errorf("%s: %w: %s", name, err, bytes.TrimSpace(out))
	}
	return nil
}

// SendTestNotification shows a desktop notification now, for checking
// that notifications work.
func SendTestNotification() error {
	send := desktopSender()
	if send == nil {
		return errors.New("no desktop notification mechanism found (Linux: install notify-send, i.e. libnotify; needs a desktop session)")
	}
	return send("dropgitbox: test notification", "Desktop notifications are working.", false)
}

// ---- hook command -------------------------------------------------------

type hook struct {
	argv    []string
	events  []client.EventType
	timeout time.Duration
	log     *slog.Logger
}

func (h *hook) run(ctx context.Context, events <-chan client.Event) {
	for {
		select {
		case <-ctx.Done():
			return
		case ev := <-events:
			if !slices.Contains(h.events, ev.Type) {
				continue
			}
			if err := RunHook(ctx, h.argv, ev, h.timeout); err != nil {
				h.log.Warn("notification hook failed", "command", h.argv[0], "event", ev.Type, "err", err)
			}
		}
	}
}

// RunHook runs argv for ev. The event is passed as JSON on stdin and in
// DROPGITBOX_* environment variables. The command runs directly (no
// shell), so event contents can never be interpreted as shell syntax.
func RunHook(ctx context.Context, argv []string, ev client.Event, timeout time.Duration) error {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	payload, err := json.Marshal(ev)
	if err != nil {
		return err
	}
	title, body, urgent := Describe(ev)
	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
	cmd.Stdin = bytes.NewReader(payload)
	cmd.Env = append(os.Environ(),
		"DROPGITBOX_EVENT="+string(ev.Type),
		"DROPGITBOX_FOLDER="+ev.Folder,
		"DROPGITBOX_REPO="+ev.Repo,
		"DROPGITBOX_MESSAGE="+ev.Message,
		"DROPGITBOX_FILES="+strings.Join(ev.Files, "\n"),
		"DROPGITBOX_TITLE="+title,
		"DROPGITBOX_BODY="+body,
		fmt.Sprintf("DROPGITBOX_URGENT=%t", urgent),
		"DROPGITBOX_TIME="+ev.Time.Format(time.RFC3339),
	)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("%w: %s", err, bytes.TrimSpace(out))
	}
	return nil
}
