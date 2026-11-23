// Package server hosts dropgitbox repositories.
//
// Each synced folder is a bare git repository under the data directory.
// Clients talk git's smart HTTP protocol (served by git-http-backend) under
// /git/, and subscribe to /api/v1/repos/{name}/events, a Server-Sent Events
// stream that announces every push so other devices can sync immediately
// instead of polling.
package server

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/cgi"
	"net/netip"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/dhruvasagar/dropgitbox/internal/gitx"
)

// Branch is the single branch every dropgitbox repository syncs.
const Branch = "main"

var validName = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,99}$`)

// ValidRepoName reports whether name is acceptable as a repository name.
func ValidRepoName(name string) bool {
	return validName.MatchString(name) && !strings.HasSuffix(name, ".git") && !strings.Contains(name, "..")
}

// Options configures a Server.
type Options struct {
	// DataDir holds the bare repositories (<name>.git).
	DataDir string
	// Token is an optional shared secret accepted in addition to the
	// per-device tokens in DevicesFile.
	Token string
	// NoAuth disables authentication entirely (trusted networks only).
	NoAuth bool
	// Logger receives request and push logs. Defaults to slog.Default().
	Logger *slog.Logger
	// Heartbeat is the interval between keep-alive comments on event
	// streams. Defaults to 25s.
	Heartbeat time.Duration
	// TrustedProxies are the reverse proxies whose X-Forwarded-For header
	// identifies the real client. Defaults to DefaultTrustedProxies.
	TrustedProxies []netip.Prefix
	// AuthMaxFailures invalid tokens within AuthWindow block a client for
	// AuthBlock. Defaults: 20, 5m, 15m.
	AuthMaxFailures int
	AuthWindow      time.Duration
	AuthBlock       time.Duration
	// MaxStreamsPerDevice caps concurrent event streams per device.
	// Defaults to 32.
	MaxStreamsPerDevice int
	// ActivityFlush is how often device activity is written to disk.
	// Defaults to 5s.
	ActivityFlush time.Duration
}

// MinSharedTokenLength is the shortest shared token accepted. Device
// tokens are always 256-bit random values.
const MinSharedTokenLength = 24

// Server serves dropgitbox repositories over HTTP.
type Server struct {
	dataDir   string
	token     string
	noAuth    bool
	devices   *registry
	log       *slog.Logger
	heartbeat time.Duration
	hub       *hub
	backend   *cgi.Handler
	trusted   []netip.Prefix
	limiter   *authLimiter
	streams   *streamLimiter
	activity  *activity

	createMu sync.Mutex
}

// New prepares a Server, creating the data directory if needed.
func New(opts Options) (*Server, error) {
	if opts.DataDir == "" {
		return nil, errors.New("server: DataDir is required")
	}
	dataDir, err := filepath.Abs(opts.DataDir)
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(dataDir, 0o755); err != nil {
		return nil, err
	}
	if opts.Logger == nil {
		opts.Logger = slog.Default()
	}
	if opts.Heartbeat <= 0 {
		opts.Heartbeat = 25 * time.Second
	}
	if opts.Token != "" && len(opts.Token) < MinSharedTokenLength {
		return nil, fmt.Errorf("server: the shared token must be at least %d characters (generate one with: openssl rand -hex 32)", MinSharedTokenLength)
	}
	if len(opts.TrustedProxies) == 0 {
		opts.TrustedProxies = DefaultTrustedProxies
	}
	if opts.AuthMaxFailures <= 0 {
		opts.AuthMaxFailures = 20
	}
	if opts.AuthWindow <= 0 {
		opts.AuthWindow = 5 * time.Minute
	}
	if opts.AuthBlock <= 0 {
		opts.AuthBlock = 15 * time.Minute
	}
	if opts.MaxStreamsPerDevice <= 0 {
		opts.MaxStreamsPerDevice = 32
	}
	if opts.ActivityFlush <= 0 {
		opts.ActivityFlush = 5 * time.Second
	}

	execPath, err := (&gitx.Repo{}).Run(context.Background(), "--exec-path")
	if err != nil {
		return nil, fmt.Errorf("server: locating git: %w", err)
	}
	backendPath := filepath.Join(execPath, "git-http-backend")
	if _, err := os.Stat(backendPath); err != nil {
		return nil, fmt.Errorf("server: git-http-backend not found: %w", err)
	}

	return &Server{
		dataDir:   dataDir,
		token:     opts.Token,
		noAuth:    opts.NoAuth,
		devices:   newRegistry(dataDir),
		log:       opts.Logger,
		heartbeat: opts.Heartbeat,
		hub:       newHub(),
		trusted:   opts.TrustedProxies,
		limiter:   newAuthLimiter(opts.AuthMaxFailures, opts.AuthWindow, opts.AuthBlock),
		streams:   &streamLimiter{max: opts.MaxStreamsPerDevice},
		activity:  newActivity(dataDir, opts.Logger, opts.ActivityFlush),
		backend: &cgi.Handler{
			Path: backendPath,
			Root: "/git",
			Env: []string{
				"GIT_PROJECT_ROOT=" + dataDir,
				"GIT_HTTP_EXPORT_ALL=1",
				"GIT_CONFIG_NOSYSTEM=1",
				// Enforced for every repository regardless of its own
				// config: history is append-only, and pushed objects are
				// checked for corruption or malformed content.
				"GIT_CONFIG_COUNT=5",
				"GIT_CONFIG_KEY_0=receive.denyNonFastForwards", "GIT_CONFIG_VALUE_0=true",
				"GIT_CONFIG_KEY_1=receive.denyDeletes", "GIT_CONFIG_VALUE_1=true",
				"GIT_CONFIG_KEY_2=receive.fsckObjects", "GIT_CONFIG_VALUE_2=true",
				"GIT_CONFIG_KEY_3=http.receivepack", "GIT_CONFIG_VALUE_3=true",
				"GIT_CONFIG_KEY_4=http.getanyfile", "GIT_CONFIG_VALUE_4=false",
			},
			InheritEnv: []string{"PATH", "HOME"},
		},
	}, nil
}

// Close flushes recorded device activity. Call it on shutdown.
func (s *Server) Close() error {
	s.activity.close()
	return nil
}

// Activity returns a snapshot of device activity.
func (s *Server) Activity() map[string]*DeviceActivity { return s.activity.snapshot() }

// Handler returns the HTTP handler for the server.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintln(w, "ok")
	})
	mux.Handle("GET /api/v1/whoami", s.auth(http.HandlerFunc(s.whoami)))
	mux.Handle("POST /api/v1/status", s.auth(http.HandlerFunc(s.reportStatus)))
	mux.Handle("GET /api/v1/repos", s.auth(http.HandlerFunc(s.listRepos)))
	mux.Handle("GET /api/v1/repos/{name}", s.auth(http.HandlerFunc(s.getRepo)))
	mux.Handle("POST /api/v1/repos/{name}", s.auth(http.HandlerFunc(s.createRepo)))
	mux.Handle("GET /api/v1/repos/{name}/events", s.auth(http.HandlerFunc(s.events)))
	mux.Handle("/git/", s.auth(http.HandlerFunc(s.git)))
	return securityHeaders(mux)
}

func (s *Server) repoPath(name string) string {
	return filepath.Join(s.dataDir, name+".git")
}

func (s *Server) repoExists(name string) bool {
	fi, err := os.Stat(filepath.Join(s.repoPath(name), "HEAD"))
	return err == nil && !fi.IsDir()
}

func (s *Server) head(ctx context.Context, name string) (string, error) {
	return RepoHead(ctx, s.dataDir, name)
}

// RepoHead returns the current head of a repository in dataDir ("" when
// it has no commits or does not exist).
func RepoHead(ctx context.Context, dataDir, name string) (string, error) {
	if !ValidRepoName(name) {
		return "", fmt.Errorf("invalid repository name %q", name)
	}
	path := filepath.Join(dataDir, name+".git")
	if _, err := os.Stat(path); err != nil {
		return "", nil
	}
	return (&gitx.Repo{Dir: path}).RevParse(ctx, "refs/heads/"+Branch)
}

type ctxKey struct{}

// DeviceFrom returns the authenticated device name for a request.
func DeviceFrom(r *http.Request) string {
	name, _ := r.Context().Value(ctxKey{}).(string)
	return name
}

// NumDevices returns how many devices are registered.
func (s *Server) NumDevices() (int, error) { return s.devices.count() }

// AuthEnabled reports whether requests must be authenticated.
func (s *Server) AuthEnabled() bool { return !s.noAuth }

// auth authenticates a request by device token (or the optional shared
// token). It accepts "Authorization: Bearer <token>" (used by dropgitbox
// clients) and HTTP Basic auth with the token as the password (so plain git
// can clone too). Clients presenting too many invalid tokens are blocked
// for a while; every failure is logged with the client's address, so tools
// like fail2ban can act on it.
func (s *Server) auth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ip := clientIP(r, s.trusted)
		key := limitKey(ip)
		if until, blocked := s.limiter.blocked(key); blocked {
			w.Header().Set("Retry-After", strconv.Itoa(int(time.Until(until).Seconds())+1))
			http.Error(w, "too many failed authentication attempts", http.StatusTooManyRequests)
			return
		}

		device, presented, err := s.authenticate(r)
		if err != nil {
			s.log.Error("authentication error", "err", err)
			http.Error(w, "internal error", http.StatusInternalServerError)
			return
		}
		if device == "" {
			if presented {
				s.log.Warn("auth failed: invalid token", "ip", ip, "method", r.Method, "path", r.URL.Path,
					"user_agent", r.UserAgent())
				if s.limiter.fail(key) {
					s.log.Warn("auth: client blocked after repeated failures", "ip", ip, "key", key,
						"duration", s.limiter.block)
				}
			}
			w.Header().Set("WWW-Authenticate", `Basic realm="dropgitbox"`)
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		s.activity.seen(device, ip.String(), clientVersion(r.UserAgent()))
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), ctxKey{}, device)))
	})
}

// clientVersion extracts the version from a "dropgitbox/<version>" user
// agent; git's own requests carry a git user agent and are ignored.
func clientVersion(ua string) string {
	v, ok := strings.CutPrefix(ua, "dropgitbox/")
	if !ok {
		return ""
	}
	if i := strings.IndexByte(v, ' '); i >= 0 {
		v = v[:i]
	}
	if len(v) > 64 {
		v = v[:64]
	}
	return v
}

// StatusReport is what a client sends after each sync attempt.
type StatusReport struct {
	Repo      string   `json:"repo"`
	State     string   `json:"state"`
	Message   string   `json:"message,omitempty"`
	Head      string   `json:"head,omitempty"`
	Conflicts []string `json:"conflicts,omitempty"`
}

func (s *Server) reportStatus(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, 64<<10)
	var rep StatusReport
	if err := json.NewDecoder(r.Body).Decode(&rep); err != nil {
		http.Error(w, "invalid status report", http.StatusBadRequest)
		return
	}
	if !ValidRepoName(rep.Repo) || !s.repoExists(rep.Repo) {
		http.NotFound(w, r)
		return
	}
	switch rep.State {
	case StateOK, StateConflict, StateError:
	default:
		http.Error(w, "invalid state", http.StatusBadRequest)
		return
	}
	if len(rep.Message) > 500 {
		rep.Message = rep.Message[:500]
	}
	if len(rep.Conflicts) > 100 {
		rep.Conflicts = rep.Conflicts[:100]
	}
	if len(rep.Head) > 64 {
		rep.Head = rep.Head[:64]
	}
	s.activity.reported(DeviceFrom(r), rep.Repo, RepoActivity{
		State: rep.State, Message: rep.Message, Head: rep.Head, Conflicts: rep.Conflicts,
	})
	w.WriteHeader(http.StatusNoContent)
}

// authenticate returns the device a request's token belongs to ("" if
// none), and whether a token was presented at all.
func (s *Server) authenticate(r *http.Request) (device string, presented bool, err error) {
	if s.noAuth {
		return "anonymous", false, nil
	}
	var got string
	if h := r.Header.Get("Authorization"); len(h) > 7 && strings.EqualFold(h[:7], "Bearer ") {
		got = strings.TrimSpace(h[7:])
	} else if _, pw, ok := r.BasicAuth(); ok {
		got = pw
	}
	if got == "" {
		return "", false, nil
	}
	if s.token != "" && subtle.ConstantTimeCompare([]byte(got), []byte(s.token)) == 1 {
		return "shared", true, nil
	}
	name, ok, err := s.devices.lookup(got)
	if err != nil || !ok {
		return "", true, err
	}
	return name, true, nil
}

func (s *Server) whoami(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{"device": DeviceFrom(r)})
}

type repoInfo struct {
	Name string `json:"name"`
	Head string `json:"head"`
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func (s *Server) listRepos(w http.ResponseWriter, r *http.Request) {
	entries, err := os.ReadDir(s.dataDir)
	if err != nil {
		s.log.Error("listing repos", "err", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	repos := []repoInfo{}
	for _, e := range entries {
		name, ok := strings.CutSuffix(e.Name(), ".git")
		if !ok || !e.IsDir() || !ValidRepoName(name) || !s.repoExists(name) {
			continue
		}
		head, _ := s.head(r.Context(), name)
		repos = append(repos, repoInfo{Name: name, Head: head})
	}
	sort.Slice(repos, func(i, j int) bool { return repos[i].Name < repos[j].Name })
	writeJSON(w, http.StatusOK, repos)
}

func (s *Server) getRepo(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	if !ValidRepoName(name) || !s.repoExists(name) {
		http.NotFound(w, r)
		return
	}
	head, err := s.head(r.Context(), name)
	if err != nil {
		s.log.Error("reading head", "repo", name, "err", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusOK, repoInfo{Name: name, Head: head})
}

// createRepo creates a bare repository. It is idempotent: creating an
// existing repository returns 200 instead of 201.
func (s *Server) createRepo(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	if !ValidRepoName(name) {
		http.Error(w, "invalid repository name", http.StatusBadRequest)
		return
	}

	s.createMu.Lock()
	defer s.createMu.Unlock()

	if s.repoExists(name) {
		writeJSON(w, http.StatusOK, repoInfo{Name: name})
		return
	}
	if err := s.initBare(r.Context(), name); err != nil {
		s.log.Error("create repo failed", "repo", name, "err", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	s.log.Info("created repo", "repo", name, "device", DeviceFrom(r))
	writeJSON(w, http.StatusCreated, repoInfo{Name: name})
}

func (s *Server) initBare(ctx context.Context, name string) error {
	path := s.repoPath(name)
	if _, err := (&gitx.Repo{Dir: s.dataDir}).Run(ctx, "init", "--bare", "-q", "-b", Branch, path); err != nil {
		return err
	}
	bare := &gitx.Repo{Dir: path}
	for _, kv := range [][2]string{
		{"http.receivepack", "true"},
		// History is the whole point: never let a client rewrite or
		// delete it.
		{"receive.denyNonFastForwards", "true"},
		{"receive.denyDeletes", "true"},
		{"core.logAllRefUpdates", "true"},
	} {
		if err := bare.SetConfig(ctx, kv[0], kv[1]); err != nil {
			_ = os.RemoveAll(path)
			return err
		}
	}
	return nil
}

// git proxies git smart-HTTP requests to git-http-backend and announces
// successful pushes to subscribers.
func (s *Server) git(w http.ResponseWriter, r *http.Request) {
	rest := strings.TrimPrefix(r.URL.Path, "/git/")
	repoDir, endpoint, _ := strings.Cut(rest, "/")
	name, ok := strings.CutSuffix(repoDir, ".git")
	if !ok || !ValidRepoName(name) || !gitRoute(r.Method, endpoint, r.URL.Query().Get("service")) || !s.repoExists(name) {
		http.NotFound(w, r)
		return
	}

	device := DeviceFrom(r)
	// git-http-backend only accepts pushes when REMOTE_USER is set; the
	// request is already authenticated, so pass the device name along.
	backend := *s.backend
	backend.Env = append(slices.Clone(s.backend.Env), "REMOTE_USER="+device)

	before, _ := s.head(r.Context(), name)
	backend.ServeHTTP(w, r)

	if r.Method == http.MethodPost && endpoint == "git-upload-pack" {
		s.activity.fetched(device, name)
	}
	if r.Method == http.MethodPost && endpoint == "git-receive-pack" {
		after, err := s.head(r.Context(), name)
		if err != nil {
			s.log.Error("reading head after push", "repo", name, "err", err)
			return
		}
		if after != "" && after != before {
			s.log.Info("push", "repo", name, "device", device, "head", after)
			s.activity.pushed(device, name, after)
			s.hub.publish(Event{Repo: name, Head: after})
		}
	}
}

// events streams push notifications for a repository as Server-Sent
// Events. The current head is sent immediately on connect so a client
// that missed pushes while disconnected catches up straight away.
func (s *Server) events(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	if !ValidRepoName(name) || !s.repoExists(name) {
		http.NotFound(w, r)
		return
	}

	device := DeviceFrom(r)
	if !s.streams.acquire(device) {
		http.Error(w, "too many open event streams for this device", http.StatusTooManyRequests)
		return
	}
	defer s.streams.release(device)

	ch, cancel := s.hub.subscribe(name)
	defer cancel()

	rc := http.NewResponseController(w)
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("X-Accel-Buffering", "no") // disable proxy buffering (nginx)
	w.WriteHeader(http.StatusOK)

	send := func(ev Event) error {
		b, _ := json.Marshal(ev)
		if _, err := fmt.Fprintf(w, "event: head\ndata: %s\n\n", b); err != nil {
			return err
		}
		return rc.Flush()
	}

	head, _ := s.head(r.Context(), name)
	if err := send(Event{Repo: name, Head: head}); err != nil {
		return
	}

	tick := time.NewTicker(s.heartbeat)
	defer tick.Stop()
	for {
		select {
		case <-r.Context().Done():
			return
		case ev := <-ch:
			if err := send(ev); err != nil {
				return
			}
		case <-tick.C:
			// Drop streams of devices revoked since they connected.
			if dev, _, err := s.authenticate(r); err != nil || dev == "" {
				return
			}
			if _, err := fmt.Fprint(w, ": ping\n\n"); err != nil {
				return
			}
			if err := rc.Flush(); err != nil {
				return
			}
		}
	}
}
