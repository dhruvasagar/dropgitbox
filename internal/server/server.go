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
	"os"
	"path/filepath"
	"regexp"
	"sort"
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
	// Token is the shared secret clients must present. An empty token
	// disables authentication entirely.
	Token string
	// Logger receives request and push logs. Defaults to slog.Default().
	Logger *slog.Logger
	// Heartbeat is the interval between keep-alive comments on event
	// streams. Defaults to 25s.
	Heartbeat time.Duration
}

// Server serves dropgitbox repositories over HTTP.
type Server struct {
	dataDir   string
	token     string
	log       *slog.Logger
	heartbeat time.Duration
	hub       *hub
	backend   *cgi.Handler

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
		log:       opts.Logger,
		heartbeat: opts.Heartbeat,
		hub:       newHub(),
		backend: &cgi.Handler{
			Path: backendPath,
			Root: "/git",
			Env: []string{
				"GIT_PROJECT_ROOT=" + dataDir,
				"GIT_HTTP_EXPORT_ALL=1",
				// Requests reaching the backend are already authenticated;
				// git-http-backend only allows pushes when REMOTE_USER is set.
				"REMOTE_USER=dropgitbox",
				"GIT_CONFIG_NOSYSTEM=1",
			},
			InheritEnv: []string{"PATH", "HOME"},
		},
	}, nil
}

// Handler returns the HTTP handler for the server.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintln(w, "ok")
	})
	mux.Handle("GET /api/v1/repos", s.auth(http.HandlerFunc(s.listRepos)))
	mux.Handle("GET /api/v1/repos/{name}", s.auth(http.HandlerFunc(s.getRepo)))
	mux.Handle("POST /api/v1/repos/{name}", s.auth(http.HandlerFunc(s.createRepo)))
	mux.Handle("GET /api/v1/repos/{name}/events", s.auth(http.HandlerFunc(s.events)))
	mux.Handle("/git/", s.auth(http.HandlerFunc(s.git)))
	return mux
}

func (s *Server) repoPath(name string) string {
	return filepath.Join(s.dataDir, name+".git")
}

func (s *Server) repoExists(name string) bool {
	fi, err := os.Stat(filepath.Join(s.repoPath(name), "HEAD"))
	return err == nil && !fi.IsDir()
}

func (s *Server) head(ctx context.Context, name string) (string, error) {
	bare := &gitx.Repo{Dir: s.repoPath(name)}
	return bare.RevParse(ctx, "refs/heads/"+Branch)
}

// auth enforces the shared token. It accepts "Authorization: Bearer <token>"
// (used by dropgitbox clients) and HTTP Basic auth with the token as the
// password (so the repository can also be cloned with plain git).
func (s *Server) auth(next http.Handler) http.Handler {
	if s.token == "" {
		return next
	}
	want := []byte(s.token)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var got string
		if h := r.Header.Get("Authorization"); strings.HasPrefix(h, "Bearer ") {
			got = strings.TrimPrefix(h, "Bearer ")
		} else if _, pw, ok := r.BasicAuth(); ok {
			got = pw
		}
		if subtle.ConstantTimeCompare([]byte(got), want) != 1 {
			w.Header().Set("WWW-Authenticate", `Basic realm="dropgitbox"`)
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		next.ServeHTTP(w, r)
	})
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
		http.Error(w, err.Error(), http.StatusInternalServerError)
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
		http.Error(w, err.Error(), http.StatusInternalServerError)
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
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	s.log.Info("created repo", "repo", name)
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
	repoDir, _, _ := strings.Cut(rest, "/")
	name, ok := strings.CutSuffix(repoDir, ".git")
	if !ok || !ValidRepoName(name) || !s.repoExists(name) {
		http.NotFound(w, r)
		return
	}

	before, _ := s.head(r.Context(), name)
	s.backend.ServeHTTP(w, r)

	if r.Method == http.MethodPost && strings.HasSuffix(rest, "/git-receive-pack") {
		after, err := s.head(r.Context(), name)
		if err != nil {
			s.log.Error("reading head after push", "repo", name, "err", err)
			return
		}
		if after != "" && after != before {
			s.log.Info("push", "repo", name, "head", after)
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
			if _, err := fmt.Fprint(w, ": ping\n\n"); err != nil {
				return
			}
			if err := rc.Flush(); err != nil {
				return
			}
		}
	}
}
