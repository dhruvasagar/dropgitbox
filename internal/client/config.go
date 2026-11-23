// Package client implements the dropgitbox sync client: it turns a folder
// into a git working tree, auto-commits every change, and keeps it in sync
// with a dropgitbox server.
package client

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/dhruvasagar/dropgitbox/internal/gitx"
	"github.com/dhruvasagar/dropgitbox/internal/server"
)

// Branch is the branch that is synced.
const Branch = server.Branch

const (
	cfgServer = "dropgitbox.server"
	cfgRepo   = "dropgitbox.repo"
	cfgDevice = "dropgitbox.device"
	tokenFile = "dropgitbox-token"
)

// defaultExcludes keeps editor droppings and OS metadata out of history.
// They go to .git/info/exclude so they apply locally without adding a
// tracked .gitignore to the user's folder.
var defaultExcludes = []string{
	"# added by dropgitbox",
	".DS_Store",
	"Thumbs.db",
	"desktop.ini",
	"*.swp",
	"*.swx",
	"*~",
	".#*",
	"~$*",
	"*.crdownload",
	"*.part",
}

// Config describes a synced folder.
type Config struct {
	Dir    string // working tree root
	Server string // base URL, e.g. https://box.example.com
	Repo   string // repository name on the server
	Token  string // shared secret; may be empty when the server has auth off
	Device string // name recorded in auto-commit messages
}

// RemoteURL is the git URL of the folder's repository.
func (c Config) RemoteURL() string {
	return strings.TrimRight(c.Server, "/") + "/git/" + url.PathEscape(c.Repo) + ".git"
}

// EventsURL is the push-notification stream for the folder's repository.
func (c Config) EventsURL() string {
	return strings.TrimRight(c.Server, "/") + "/api/v1/repos/" + url.PathEscape(c.Repo) + "/events"
}

// gitEnv passes the token to git through environment-provided config, so it
// never appears in the remote URL, .git/config, or the process list.
func (c Config) gitEnv() []string {
	if c.Token == "" {
		return nil
	}
	return []string{
		"GIT_CONFIG_COUNT=1",
		"GIT_CONFIG_KEY_0=http.extraHeader",
		"GIT_CONFIG_VALUE_0=Authorization: Bearer " + c.Token,
	}
}

// SetupOptions configures Setup.
type SetupOptions struct {
	Dir    string
	Server string
	Repo   string
	Token  string
	// TokenFromConfig means Token came from the user's config file: it is
	// then not copied into the folder, so rotating the token in the config
	// (e.g. by re-joining) applies to every folder at once.
	TokenFromConfig bool
	Device          string // defaults to the hostname
	Logger          *slog.Logger
}

// Setup turns Dir into a synced folder: it creates the repository on the
// server if needed, initialises (or adopts) a local git repo, and performs
// the first sync. Existing files in Dir are kept and merged with whatever
// is already on the server.
func Setup(ctx context.Context, opts SetupOptions) (*Syncer, error) {
	if opts.Server == "" || opts.Repo == "" || opts.Dir == "" {
		return nil, errors.New("setup: server, repo and dir are required")
	}
	if !server.ValidRepoName(opts.Repo) {
		return nil, fmt.Errorf("setup: invalid repository name %q", opts.Repo)
	}
	if _, err := url.ParseRequestURI(opts.Server); err != nil {
		return nil, fmt.Errorf("setup: invalid server URL: %w", err)
	}
	dir, err := filepath.Abs(opts.Dir)
	if err != nil {
		return nil, err
	}
	if opts.Device == "" {
		opts.Device = hostname()
	}
	cfg := Config{Dir: dir, Server: strings.TrimRight(opts.Server, "/"), Repo: opts.Repo, Token: opts.Token, Device: opts.Device}

	if err := createRemoteRepo(ctx, cfg); err != nil {
		return nil, err
	}

	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	g := &gitx.Repo{Dir: dir}
	if _, err := os.Stat(filepath.Join(dir, ".git")); errors.Is(err, os.ErrNotExist) {
		if _, err := g.Run(ctx, "init", "-q", "-b", Branch); err != nil {
			return nil, err
		}
	} else if err != nil {
		return nil, err
	} else if top, err := g.Run(ctx, "rev-parse", "--show-toplevel"); err != nil || !samePath(top, dir) {
		return nil, fmt.Errorf("setup: %s must be the root of its git repository", dir)
	}

	if err := configureLocal(ctx, g, cfg, !opts.TokenFromConfig); err != nil {
		return nil, err
	}

	s, err := Load(dir, LoadOptions{Logger: opts.Logger, Token: opts.Token})
	if err != nil {
		return nil, err
	}
	if err := s.Sync(ctx); err != nil {
		return s, fmt.Errorf("initial sync: %w", err)
	}
	return s, nil
}

func configureLocal(ctx context.Context, g *gitx.Repo, cfg Config, storeToken bool) error {
	// Make sure we are on the synced branch (adopted repos may not be).
	if cur, _ := g.Run(ctx, "symbolic-ref", "--short", "HEAD"); cur != Branch {
		if head, _ := g.RevParse(ctx, "HEAD"); head == "" {
			if _, err := g.Run(ctx, "symbolic-ref", "HEAD", "refs/heads/"+Branch); err != nil {
				return err
			}
		} else if exists, _ := g.RevParse(ctx, "refs/heads/"+Branch); exists != "" {
			if _, err := g.Run(ctx, "checkout", "-q", Branch); err != nil {
				return err
			}
		} else if _, err := g.Run(ctx, "checkout", "-q", "-b", Branch); err != nil {
			return err
		}
	}

	if _, err := g.Run(ctx, "remote", "get-url", "origin"); err != nil {
		if _, err := g.Run(ctx, "remote", "add", "origin", cfg.RemoteURL()); err != nil {
			return err
		}
	} else if _, err := g.Run(ctx, "remote", "set-url", "origin", cfg.RemoteURL()); err != nil {
		return err
	}

	settings := [][2]string{
		{cfgServer, cfg.Server},
		{cfgRepo, cfg.Repo},
		{cfgDevice, cfg.Device},
		{"core.quotePath", "false"}, // show non-ASCII file names as-is
		{"pull.rebase", "false"},
	}
	for _, kv := range settings {
		if err := g.SetConfig(ctx, kv[0], kv[1]); err != nil {
			return err
		}
	}

	// Commits need an identity; fall back to one naming the device.
	if email, _ := g.Config(ctx, "user.email"); email == "" {
		if err := g.SetConfig(ctx, "user.email", "dropgitbox@"+cfg.Device); err != nil {
			return err
		}
	}
	if name, _ := g.Config(ctx, "user.name"); name == "" {
		if err := g.SetConfig(ctx, "user.name", "dropgitbox ("+cfg.Device+")"); err != nil {
			return err
		}
	}

	gitDir, err := absGitDir(ctx, g)
	if err != nil {
		return err
	}
	token := cfg.Token
	if !storeToken {
		token = "" // removes any stale per-folder token
	}
	if err := writeToken(gitDir, token); err != nil {
		return err
	}
	return addExcludes(gitDir)
}

// ErrNotSetUp is returned by Load for a folder that has not been set up.
var ErrNotSetUp = errors.New("not a dropgitbox folder (run `dropgitbox setup` first)")

// LoadOptions configures Load.
type LoadOptions struct {
	Logger *slog.Logger
	// Token is used when the folder has no token of its own (the token
	// from the user's config file).
	Token string
}

// Load opens an already set-up synced folder at (or above) dir.
func Load(dir string, opts LoadOptions) (*Syncer, error) {
	ctx := context.Background()
	log := opts.Logger
	if log == nil {
		log = slog.Default()
	}
	abs, err := filepath.Abs(dir)
	if err != nil {
		return nil, err
	}
	probe := &gitx.Repo{Dir: abs}
	top, err := probe.Run(ctx, "rev-parse", "--show-toplevel")
	if err != nil {
		return nil, fmt.Errorf("%s: %w", abs, ErrNotSetUp)
	}
	g := &gitx.Repo{Dir: top}

	cfg := Config{Dir: top}
	if cfg.Server, err = g.Config(ctx, cfgServer); err != nil {
		return nil, err
	}
	if cfg.Repo, err = g.Config(ctx, cfgRepo); err != nil {
		return nil, err
	}
	if cfg.Server == "" || cfg.Repo == "" {
		return nil, fmt.Errorf("%s is a git repository but %w", top, ErrNotSetUp)
	}
	if cfg.Device, _ = g.Config(ctx, cfgDevice); cfg.Device == "" {
		cfg.Device = hostname()
	}
	gitDir, err := absGitDir(ctx, g)
	if err != nil {
		return nil, err
	}
	if b, err := os.ReadFile(filepath.Join(gitDir, tokenFile)); err == nil {
		cfg.Token = strings.TrimSpace(string(b))
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	if cfg.Token == "" {
		cfg.Token = opts.Token
	}

	g.Env = cfg.gitEnv()
	return &Syncer{cfg: cfg, git: g, gitDir: gitDir, log: log.With("folder", top)}, nil
}

// createRemoteRepo asks the server to create the repository (idempotent).
func createRemoteRepo(ctx context.Context, cfg Config) error {
	err := apiCall(ctx, http.MethodPost, cfg.Server, "/api/v1/repos/"+url.PathEscape(cfg.Repo), cfg.Token, nil)
	if err != nil {
		return fmt.Errorf("creating repository on server: %w", err)
	}
	return nil
}

// RepoInfo describes a repository on the server.
type RepoInfo struct {
	Name string `json:"name"`
	Head string `json:"head"`
}

// ListRepos lists the repositories on a server.
func ListRepos(ctx context.Context, serverURL, token string) ([]RepoInfo, error) {
	var repos []RepoInfo
	err := apiCall(ctx, http.MethodGet, serverURL, "/api/v1/repos", token, &repos)
	return repos, err
}

// WhoAmI returns the device name the server associates with token.
func WhoAmI(ctx context.Context, serverURL, token string) (string, error) {
	var resp struct {
		Device string `json:"device"`
	}
	err := apiCall(ctx, http.MethodGet, serverURL, "/api/v1/whoami", token, &resp)
	return resp.Device, err
}

func apiCall(ctx context.Context, method, serverURL, path, token string, out any) error {
	req, err := http.NewRequestWithContext(ctx, method, strings.TrimRight(serverURL, "/")+path, nil)
	if err != nil {
		return err
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	hc := &http.Client{Timeout: 30 * time.Second}
	resp, err := hc.Do(req)
	if err != nil {
		return fmt.Errorf("contacting server: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		if resp.StatusCode == http.StatusUnauthorized {
			return fmt.Errorf("%s: the server rejected this device's token (was it revoked?)", resp.Status)
		}
		return fmt.Errorf("%s: %s", resp.Status, bytes.TrimSpace(body))
	}
	if out == nil {
		return nil
	}
	return json.NewDecoder(resp.Body).Decode(out)
}

func absGitDir(ctx context.Context, g *gitx.Repo) (string, error) {
	return g.Run(ctx, "rev-parse", "--absolute-git-dir")
}

func writeToken(gitDir, token string) error {
	p := filepath.Join(gitDir, tokenFile)
	if token == "" {
		if err := os.Remove(p); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
		return nil
	}
	return os.WriteFile(p, []byte(token+"\n"), 0o600)
}

func addExcludes(gitDir string) error {
	p := filepath.Join(gitDir, "info", "exclude")
	existing, err := os.ReadFile(p)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if bytes.Contains(existing, []byte(defaultExcludes[0])) {
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		return err
	}
	f, err := os.OpenFile(p, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	defer f.Close()
	prefix := ""
	if len(existing) > 0 && !bytes.HasSuffix(existing, []byte("\n")) {
		prefix = "\n"
	}
	_, err = f.WriteString(prefix + strings.Join(defaultExcludes, "\n") + "\n")
	return err
}

func hostname() string {
	h, err := os.Hostname()
	if err != nil || h == "" {
		return "unknown"
	}
	return h
}

func samePath(a, b string) bool {
	ra, err1 := filepath.EvalSymlinks(a)
	rb, err2 := filepath.EvalSymlinks(b)
	if err1 != nil || err2 != nil {
		return filepath.Clean(a) == filepath.Clean(b)
	}
	return ra == rb
}
