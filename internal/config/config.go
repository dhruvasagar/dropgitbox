// Package config loads and edits dropgitbox's TOML configuration file.
//
// The file is looked up following the XDG Base Directory spec:
//
//  1. the -config flag, or $DROPGITBOX_CONFIG
//  2. $XDG_CONFIG_HOME/dropgitbox/config.toml (default ~/.config/...)
//  3. each $XDG_CONFIG_DIRS entry (default /etc/xdg)/dropgitbox/config.toml
//  4. /etc/dropgitbox/config.toml (used by the packaged server)
//
// The first file found wins; files are not merged. Settings are applied
// with the precedence: command-line flag > environment > file > default.
package config

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"slices"
	"strings"
	"time"

	"github.com/BurntSushi/toml"
)

const (
	// AppName names the config/data subdirectories.
	AppName = "dropgitbox"
	// FileName is the config file's base name.
	FileName = "config.toml"
	// TokenFileName is the default client token file, stored next to the
	// config file so the config itself can be shared (e.g. in dotfiles).
	TokenFileName = "token"
	// EnvConfig overrides the config file location.
	EnvConfig = "DROPGITBOX_CONFIG"
	// EnvToken supplies a token (client or server) via the environment.
	EnvToken = "DROPGITBOX_TOKEN"
	// SystemPath is the system-wide config used by packaged installs.
	SystemPath = "/etc/dropgitbox/config.toml"
)

// Config is the parsed configuration file.
type Config struct {
	Server  Server   `toml:"server"`
	Client  Client   `toml:"client"`
	Folders []Folder `toml:"folders"`
	Notify  Notify   `toml:"notify"`

	path     string   // file this was loaded from ("" when none was found)
	explicit bool     // path came from -config / $DROPGITBOX_CONFIG
	warnings []string // non-fatal problems found while loading
}

// Server configures `dropgitbox server` and `dropgitbox device`.
type Server struct {
	Addr      string `toml:"addr"`       // listen address, default ":8080"
	DataDir   string `toml:"data_dir"`   // default $XDG_DATA_HOME/dropgitbox
	PublicURL string `toml:"public_url"` // URL devices use to reach the server (for invites)
	Token     string `toml:"token"`      // optional shared token, accepted besides device tokens
	TokenFile string `toml:"token_file"` // file holding the shared token
	NoAuth    bool   `toml:"no_auth"`    // disable authentication entirely
	TLSCert   string `toml:"tls_cert"`   // serve HTTPS directly with this certificate...
	TLSKey    string `toml:"tls_key"`    // ...and key
	// TrustedProxies are CIDRs of reverse proxies whose X-Forwarded-For
	// header is believed. Default: loopback only.
	TrustedProxies []string `toml:"trusted_proxies"`
}

// Client configures the sync client.
type Client struct {
	Server    string        `toml:"server"`     // server base URL
	Token     string        `toml:"token"`      // device token (prefer token_file)
	TokenFile string        `toml:"token_file"` // default: "token" next to the config file
	Device    string        `toml:"device"`     // device name, default hostname
	Debounce  time.Duration `toml:"debounce"`   // e.g. "2s"
	MaxDelay  time.Duration `toml:"max_delay"`  // e.g. "30s"
	Poll      time.Duration `toml:"poll"`       // e.g. "5m"
}

// Notify configures the client daemon's notifications.
type Notify struct {
	// Desktop toggles native desktop notifications (default true).
	Desktop *bool `toml:"desktop"`
	// DesktopEvents overrides which events pop up (default: conflict,
	// resolved, offline, online, unauthorized). "*" means all.
	DesktopEvents []string `toml:"desktop_events"`
	// Command is run for each of CommandEvents, e.g. ["~/bin/notify.sh"].
	// It is executed directly, not through a shell.
	Command       []string      `toml:"command"`
	CommandEvents []string      `toml:"command_events"`
	OfflineAfter  time.Duration `toml:"offline_after"` // default "10m"
}

// Folder is a synced folder the daemon manages.
type Folder struct {
	Path string `toml:"path"`
	Repo string `toml:"repo"`
}

// Path returns the file the config was loaded from, or "".
func (c *Config) Path() string { return c.path }

// Warnings returns non-fatal problems found while loading.
func (c *Config) Warnings() []string { return c.warnings }

// Dir returns the directory relative paths in the config resolve against.
func (c *Config) Dir() string {
	if c.path != "" {
		return filepath.Dir(c.path)
	}
	if d, err := UserDir(); err == nil {
		return d
	}
	return "."
}

// WritePath returns the file that commands which save settings (join,
// setup) should write to: the loaded file, else the user config file.
func (c *Config) WritePath() (string, error) {
	if c.path != "" && (c.explicit || !isSystemPath(c.path)) {
		return c.path, nil
	}
	d, err := UserDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(d, FileName), nil
}

func isSystemPath(p string) bool {
	if p == SystemPath {
		return true
	}
	for _, d := range xdgConfigDirs() {
		if strings.HasPrefix(p, d+string(filepath.Separator)) {
			return true
		}
	}
	return false
}

// UserDir is the per-user config directory, $XDG_CONFIG_HOME/dropgitbox.
// XDG is honoured on macOS too, as is conventional for CLI tools.
func UserDir() (string, error) {
	if x := os.Getenv("XDG_CONFIG_HOME"); filepath.IsAbs(x) {
		return filepath.Join(x, AppName), nil
	}
	if runtime.GOOS == "windows" {
		d, err := os.UserConfigDir()
		if err != nil {
			return "", err
		}
		return filepath.Join(d, AppName), nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".config", AppName), nil
}

// DefaultDataDir is the per-user data directory, $XDG_DATA_HOME/dropgitbox.
func DefaultDataDir() (string, error) {
	if x := os.Getenv("XDG_DATA_HOME"); filepath.IsAbs(x) {
		return filepath.Join(x, AppName), nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".local", "share", AppName), nil
}

func xdgConfigDirs() []string {
	var dirs []string
	for _, d := range filepath.SplitList(os.Getenv("XDG_CONFIG_DIRS")) {
		if filepath.IsAbs(d) {
			dirs = append(dirs, d)
		}
	}
	if len(dirs) == 0 {
		dirs = []string{"/etc/xdg"}
	}
	return dirs
}

// SearchPaths lists the implicit config locations in lookup order.
func SearchPaths() []string {
	var paths []string
	if d, err := UserDir(); err == nil {
		paths = append(paths, filepath.Join(d, FileName))
	}
	for _, d := range xdgConfigDirs() {
		paths = append(paths, filepath.Join(d, AppName, FileName))
	}
	return append(paths, SystemPath)
}

// Load finds and parses the config file. explicit is the -config flag
// value ("" if unset). A missing implicit config is not an error: an empty
// Config is returned. Unknown keys are rejected, since they are almost
// always typos.
func Load(explicit string) (*Config, error) {
	c := &Config{}
	path, isExplicit := explicit, explicit != ""
	if path == "" {
		if path = os.Getenv(EnvConfig); path != "" {
			isExplicit = true
		}
	}
	if path != "" {
		abs, err := filepath.Abs(ExpandHome(path))
		if err != nil {
			return nil, err
		}
		c.path, c.explicit = abs, isExplicit
		if _, err := os.Stat(abs); err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				return c, nil // will be created on first write
			}
			return nil, err
		}
	} else {
		for _, p := range SearchPaths() {
			if _, err := os.Stat(p); err == nil {
				c.path = p
				break
			}
		}
		if c.path == "" {
			return c, nil
		}
	}

	md, err := toml.DecodeFile(c.path, c)
	if err != nil {
		return nil, fmt.Errorf("config %s: %w", c.path, err)
	}
	if undec := md.Undecoded(); len(undec) > 0 {
		keys := make([]string, len(undec))
		for i, k := range undec {
			keys[i] = k.String()
		}
		return nil, fmt.Errorf("config %s: unknown setting(s): %s", c.path, strings.Join(keys, ", "))
	}
	if (c.Client.Token != "" || c.Server.Token != "") && worldReadable(c.path) {
		c.warnings = append(c.warnings, fmt.Sprintf(
			"%s contains a token but is readable by other users; run: chmod 600 %s", c.path, c.path))
	}
	return c, nil
}

// Resolve turns a path from the config into an absolute one: $VARS are
// expanded (e.g. systemd's $CREDENTIALS_DIRECTORY), "~/" expands to the
// home directory, and relative paths resolve against the config file's
// directory.
func (c *Config) Resolve(p string) string {
	if p == "" {
		return ""
	}
	p = ExpandHome(os.ExpandEnv(p))
	if !filepath.IsAbs(p) {
		p = filepath.Join(c.Dir(), p)
	}
	return filepath.Clean(p)
}

// LoadServer is Load for server-side commands: when no config was named
// explicitly and the one found has no [server] table (a client config),
// the system config is used if it exists. This makes `sudo dropgitbox
// device add` work on a machine that is both a client and a server.
func LoadServer(explicit string) (*Config, error) {
	c, err := Load(explicit)
	if err != nil || c.explicit || !reflect.ValueOf(c.Server).IsZero() || c.path == SystemPath {
		return c, err
	}
	if _, err := os.Stat(SystemPath); err != nil {
		return c, nil
	}
	sys, err := Load(SystemPath)
	if err != nil {
		return nil, err
	}
	sys.explicit = false
	return sys, nil
}

// ExpandHome expands a leading "~/" (or a bare "~").
func ExpandHome(p string) string {
	if p == "~" || strings.HasPrefix(p, "~/") {
		if home, err := os.UserHomeDir(); err == nil {
			return filepath.Join(home, strings.TrimPrefix(p, "~"))
		}
	}
	return p
}

// ClientTokenFile is where the client token is (or would be) stored.
func (c *Config) ClientTokenFile() string {
	if c.Client.TokenFile != "" {
		return c.Resolve(c.Client.TokenFile)
	}
	return filepath.Join(c.Dir(), TokenFileName)
}

// ClientToken returns the client token from the config: the inline token,
// else the token file (which may not exist; then "" is returned).
func (c *Config) ClientToken() (string, error) {
	if c.Client.Token != "" {
		return c.Client.Token, nil
	}
	return readSecret(c.ClientTokenFile(), c.Client.TokenFile != "")
}

// ServerToken returns the optional shared server token from the config.
func (c *Config) ServerToken() (string, error) {
	if c.Server.Token != "" {
		return c.Server.Token, nil
	}
	if c.Server.TokenFile == "" {
		return "", nil
	}
	return readSecret(c.Resolve(c.Server.TokenFile), true)
}

func readSecret(path string, required bool) (string, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		if !required && errors.Is(err, fs.ErrNotExist) {
			return "", nil
		}
		return "", fmt.Errorf("reading token: %w", err)
	}
	if worldReadable(path) {
		return "", fmt.Errorf("token file %s is readable by other users; run: chmod 600 %s", path, path)
	}
	return strings.TrimSpace(string(b)), nil
}

func worldReadable(path string) bool {
	if runtime.GOOS == "windows" {
		return false
	}
	fi, err := os.Stat(path)
	return err == nil && fi.Mode().Perm()&0o044 != 0
}

// FolderFor returns the configured folder whose path is dir, if any.
func (c *Config) FolderFor(dir string) (Folder, bool) {
	for _, f := range c.Folders {
		if samePath(c.Resolve(f.Path), dir) {
			return f, true
		}
	}
	return Folder{}, false
}

func samePath(a, b string) bool {
	ra, err1 := filepath.EvalSymlinks(a)
	rb, err2 := filepath.EvalSymlinks(b)
	if err1 == nil && err2 == nil {
		return ra == rb
	}
	return filepath.Clean(a) == filepath.Clean(b)
}

// ---- writing ----------------------------------------------------------

const header = `# dropgitbox configuration.
# Reference: https://github.com/dhruvasagar/dropgitbox/blob/main/packaging/config.example.toml
`

// SetClient sets keys in the [client] table of the config file at path,
// creating the file if needed. Other content, including comments, is
// preserved. Empty values are skipped.
func SetClient(path string, kv [][2]string) error {
	return edit(path, func(src string) (string, error) {
		var set [][2]string
		for _, p := range kv {
			if p[1] != "" {
				set = append(set, [2]string{p[0], quote(p[1])})
			}
		}
		return setTableKeys(src, "client", set), nil
	})
}

// AddFolder registers a folder in the config file at path (a no-op when a
// folder with the same path is already listed).
func AddFolder(path string, f Folder) error {
	return edit(path, func(src string) (string, error) {
		var existing Config
		if _, err := toml.Decode(src, &existing); err != nil {
			return "", err
		}
		existing.path = path
		if _, ok := existing.FolderFor(existing.Resolve(f.Path)); ok {
			return src, nil
		}
		if src != "" && !strings.HasSuffix(src, "\n") {
			src += "\n"
		}
		return src + fmt.Sprintf("\n[[folders]]\npath = %s\nrepo = %s\n", quote(f.Path), quote(f.Repo)), nil
	})
}

// WriteSecret writes a token file readable only by its owner.
func WriteSecret(path, secret string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	return writeAtomic(path, []byte(secret+"\n"), 0o600)
}

func edit(path string, fn func(string) (string, error)) error {
	b, err := os.ReadFile(path)
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	src := string(b)
	if src == "" {
		src = header
	}
	out, err := fn(src)
	if err != nil {
		return fmt.Errorf("config %s: %w", path, err)
	}
	if out == string(b) {
		return nil
	}
	var check Config
	if _, err := toml.Decode(out, &check); err != nil {
		return fmt.Errorf("config %s: refusing to write invalid TOML: %w", path, err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	return writeAtomic(path, []byte(out), 0o600)
}

func writeAtomic(path string, data []byte, perm os.FileMode) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+".*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Chmod(perm); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}

// setTableKeys sets key = value (value already TOML-encoded) inside
// [table], replacing existing assignments and inserting missing ones right
// after the table header. The table is appended if absent.
func setTableKeys(src, table string, kvs [][2]string) string {
	if len(kvs) == 0 {
		return src
	}
	lines := strings.Split(src, "\n")
	start := -1
	for i, l := range lines {
		if tableHeader(l) == "["+table+"]" {
			start = i
			break
		}
	}
	if start == -1 {
		if !strings.HasSuffix(src, "\n") {
			src += "\n"
		}
		var b strings.Builder
		b.WriteString(src + "\n[" + table + "]\n")
		for _, kv := range kvs {
			b.WriteString(kv[0] + " = " + kv[1] + "\n")
		}
		return b.String()
	}
	end := len(lines)
	for i := start + 1; i < len(lines); i++ {
		if strings.HasPrefix(strings.TrimSpace(lines[i]), "[") {
			end = i
			break
		}
	}

	var insert []string
	for _, kv := range kvs {
		replaced := false
		for i := start + 1; i < end; i++ {
			k, _, ok := strings.Cut(strings.TrimSpace(lines[i]), "=")
			if ok && strings.TrimSpace(k) == kv[0] {
				lines[i] = kv[0] + " = " + kv[1]
				replaced = true
				break
			}
		}
		if !replaced {
			insert = append(insert, kv[0]+" = "+kv[1])
		}
	}
	lines = slices.Insert(lines, start+1, insert...)
	return strings.Join(lines, "\n")
}

func tableHeader(line string) string {
	l := strings.TrimSpace(line)
	if i := strings.Index(l, "#"); i >= 0 {
		l = strings.TrimSpace(l[:i])
	}
	return strings.ReplaceAll(l, " ", "")
}

// quote encodes s as a TOML basic string.
func quote(s string) string {
	var b strings.Builder
	b.WriteByte('"')
	for _, r := range s {
		switch r {
		case '"':
			b.WriteString(`\"`)
		case '\\':
			b.WriteString(`\\`)
		case '\n':
			b.WriteString(`\n`)
		case '\t':
			b.WriteString(`\t`)
		case '\r':
			b.WriteString(`\r`)
		default:
			if r < 0x20 || r == 0x7f {
				fmt.Fprintf(&b, `\u%04X`, r)
			} else {
				b.WriteRune(r)
			}
		}
	}
	b.WriteByte('"')
	return b.String()
}
