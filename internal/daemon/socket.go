package daemon

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/dhruvasagar/dropgitbox/internal/client"
)

// EnvSocket overrides the control socket location.
const EnvSocket = "DROPGITBOX_SOCKET"

// maxSocketPath is the longest usable unix socket path (sun_path is 104
// bytes on macOS/BSD and 108 on Linux, including the terminator).
const maxSocketPath = 103

// SocketPath is the per-user control socket: $DROPGITBOX_SOCKET, else
// $XDG_RUNTIME_DIR/dropgitbox/daemon.sock, else
// $XDG_STATE_HOME/dropgitbox/daemon.sock (default ~/.local/state, also on
// macOS). If that path is too long for a unix socket, a private directory
// under the system temp dir is used instead.
func SocketPath() string {
	if p := os.Getenv(EnvSocket); p != "" {
		return p
	}
	var p string
	if d := os.Getenv("XDG_RUNTIME_DIR"); filepath.IsAbs(d) {
		p = filepath.Join(d, "dropgitbox", "daemon.sock")
	} else if base := os.Getenv("XDG_STATE_HOME"); filepath.IsAbs(base) {
		p = filepath.Join(base, "dropgitbox", "daemon.sock")
	} else if home, err := os.UserHomeDir(); err == nil {
		p = filepath.Join(home, ".local", "state", "dropgitbox", "daemon.sock")
	}
	if p == "" || len(p) > maxSocketPath {
		p = filepath.Join(os.TempDir(), fmt.Sprintf("dropgitbox-%d", os.Getuid()), "daemon.sock")
	}
	return p
}

// listen opens the control socket, refusing if another daemon answers on
// it. The directory is private to the user (0700) and the socket 0600, so
// only the same user can talk to the daemon.
func listen(path string) (net.Listener, error) {
	if len(path) > maxSocketPath {
		return nil, fmt.Errorf("control socket path is too long for a unix socket (%d > %d bytes): %s; set $%s to a shorter path",
			len(path), maxSocketPath, path, EnvSocket)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, err
	}
	// The directory must be ours and private: refuse one another user
	// could have planted (e.g. in a shared temp dir).
	if fi, err := os.Lstat(filepath.Dir(path)); err != nil {
		return nil, err
	} else if !fi.IsDir() || !ownedByMe(fi) {
		return nil, fmt.Errorf("refusing to use %s for the control socket: not a directory owned by you", filepath.Dir(path))
	}
	if err := os.Chmod(filepath.Dir(path), 0o700); err != nil {
		return nil, err
	}
	if c, err := Dial(path); err == nil {
		st, err := c.Status(context.Background())
		if err == nil {
			return nil, fmt.Errorf("%w (pid %d, socket %s)", ErrAlreadyRunning, st.PID, path)
		}
	}
	_ = os.Remove(path) // stale socket from a crashed daemon
	ln, err := net.Listen("unix", path)
	if err != nil {
		return nil, err
	}
	if err := os.Chmod(path, 0o600); err != nil {
		ln.Close()
		return nil, err
	}
	return ln, nil
}

func (d *Daemon) serve(ctx context.Context, ln net.Listener) {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/status", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, d.Status())
	})
	mux.HandleFunc("GET /v1/events", d.serveEvents)
	mux.HandleFunc("POST /v1/sync", d.serveSync)

	hs := &http.Server{Handler: mux, ReadHeaderTimeout: 5 * time.Second,
		BaseContext: func(net.Listener) context.Context { return ctx }}
	go func() {
		<-ctx.Done()
		sctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = hs.Shutdown(sctx)
	}()
	if err := hs.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
		d.log.Error("control socket stopped", "err", err)
	}
	_ = os.Remove(d.opts.SocketPath)
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

// serveEvents streams events as JSON lines until the client disconnects.
func (d *Daemon) serveEvents(w http.ResponseWriter, r *http.Request) {
	ch := d.subscribe()
	defer d.unsubscribe(ch)
	rc := http.NewResponseController(w)
	w.Header().Set("Content-Type", "application/x-ndjson")
	w.WriteHeader(http.StatusOK)
	_ = rc.Flush()
	enc := json.NewEncoder(w)
	for {
		select {
		case <-r.Context().Done():
			return
		case ev := <-ch:
			if enc.Encode(ev) != nil || rc.Flush() != nil {
				return
			}
		}
	}
}

// SyncOutcome is the result of a sync requested over the socket.
type SyncOutcome struct {
	Folder  string `json:"folder"`
	Result  string `json:"result"` // synced, conflict, error, timeout
	Message string `json:"message,omitempty"`
}

// serveSync triggers a sync of one folder (?folder=) or all, and waits for
// each to finish. Syncing through the daemon avoids two processes running
// git in the same folder at once.
func (d *Daemon) serveSync(w http.ResponseWriter, r *http.Request) {
	folder := r.URL.Query().Get("folder")
	ch := d.subscribe() // before kicking, so no outcome is missed
	defer d.unsubscribe(ch)

	pending := map[string]bool{}
	var out []SyncOutcome
	st := d.Status()
	for _, f := range st.Folders {
		if folder != "" && f.Folder != folder {
			continue
		}
		if f.State == StateConflict {
			out = append(out, SyncOutcome{Folder: f.Folder, Result: "conflict",
				Message: "paused on a conflict in: " + strings.Join(f.Conflicts, ", ")})
			continue
		}
		pending[f.Folder] = true
	}
	if folder != "" && len(pending) == 0 && len(out) == 0 {
		http.Error(w, "the daemon does not manage "+folder, http.StatusNotFound)
		return
	}
	for f := range pending {
		d.kick(f)
	}

	timeout := time.After(2 * time.Minute)
	for len(pending) > 0 {
		select {
		case <-r.Context().Done():
			return
		case <-timeout:
			for f := range pending {
				out = append(out, SyncOutcome{Folder: f, Result: "timeout"})
			}
			pending = nil
		case ev := <-ch:
			if !pending[ev.Folder] {
				continue
			}
			switch ev.Type {
			case client.EventSynced:
				out = append(out, SyncOutcome{Folder: ev.Folder, Result: "synced"})
			case client.EventConflict:
				out = append(out, SyncOutcome{Folder: ev.Folder, Result: "conflict",
					Message: "paused on a conflict in: " + strings.Join(ev.Files, ", ")})
			case client.EventError:
				out = append(out, SyncOutcome{Folder: ev.Folder, Result: "error", Message: ev.Message})
			default:
				continue
			}
			delete(pending, ev.Folder)
		}
	}
	slices.SortFunc(out, func(a, b SyncOutcome) int { return strings.Compare(a.Folder, b.Folder) })
	writeJSON(w, out)
}

// Client talks to a running daemon over its control socket.
type Client struct {
	hc *http.Client
}

// Dial connects to the daemon at path (SocketPath() if empty). It fails if
// no daemon is listening.
func Dial(path string) (*Client, error) {
	if path == "" {
		path = SocketPath()
	}
	if _, err := os.Stat(path); err != nil {
		return nil, err
	}
	hc := &http.Client{Transport: &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			var dl net.Dialer
			return dl.DialContext(ctx, "unix", path)
		},
	}}
	return &Client{hc: hc}, nil
}

func (c *Client) do(ctx context.Context, method, path string) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, method, "http://daemon"+path, nil)
	if err != nil {
		return nil, err
	}
	resp, err := c.hc.Do(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		defer resp.Body.Close()
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return nil, fmt.Errorf("daemon: %s: %s", resp.Status, strings.TrimSpace(string(b)))
	}
	return resp, nil
}

// Status fetches the daemon's state.
func (c *Client) Status(ctx context.Context) (*Status, error) {
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	resp, err := c.do(ctx, http.MethodGet, "/v1/status")
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	var st Status
	return &st, json.NewDecoder(resp.Body).Decode(&st)
}

// Sync asks the daemon to sync folder ("" for all) and waits for the result.
func (c *Client) Sync(ctx context.Context, folder string) ([]SyncOutcome, error) {
	path := "/v1/sync"
	if folder != "" {
		path += "?folder=" + url.QueryEscape(folder)
	}
	resp, err := c.do(ctx, http.MethodPost, path)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	var out []SyncOutcome
	return out, json.NewDecoder(resp.Body).Decode(&out)
}

// Events streams events to fn until ctx is cancelled or the daemon exits.
func (c *Client) Events(ctx context.Context, fn func(client.Event)) error {
	resp, err := c.do(ctx, http.MethodGet, "/v1/events")
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	sc := bufio.NewScanner(resp.Body)
	sc.Buffer(make([]byte, 64<<10), 1<<20)
	for sc.Scan() {
		var ev client.Event
		if json.Unmarshal(sc.Bytes(), &ev) == nil {
			fn(ev)
		}
	}
	if ctx.Err() != nil {
		return nil
	}
	if err := sc.Err(); err != nil {
		return err
	}
	return errors.New("daemon stopped")
}
