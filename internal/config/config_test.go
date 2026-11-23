package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func isolate(t *testing.T) (cfgHome string) {
	t.Helper()
	root := t.TempDir()
	cfgHome = filepath.Join(root, "config")
	t.Setenv("HOME", filepath.Join(root, "home"))
	t.Setenv("XDG_CONFIG_HOME", cfgHome)
	t.Setenv("XDG_CONFIG_DIRS", filepath.Join(root, "etc-xdg"))
	t.Setenv("XDG_DATA_HOME", filepath.Join(root, "data"))
	t.Setenv(EnvConfig, "")
	return cfgHome
}

func writeFile(t *testing.T, path, content string, perm os.FileMode) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), perm); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, perm); err != nil { // WriteFile keeps existing perms
		t.Fatal(err)
	}
}

func TestSearchOrderAndXDG(t *testing.T) {
	cfgHome := isolate(t)
	root := filepath.Dir(cfgHome)

	c, err := Load("")
	if err != nil || c.Path() != "" {
		t.Fatalf("no config: path=%q err=%v", c.Path(), err)
	}
	if w, _ := c.WritePath(); w != filepath.Join(cfgHome, "dropgitbox", "config.toml") {
		t.Fatalf("write path = %s", w)
	}

	sys := filepath.Join(root, "etc-xdg", "dropgitbox", "config.toml")
	writeFile(t, sys, "[server]\naddr = \":9\"\n", 0o644)
	if c, _ = Load(""); c.Path() != sys || c.Server.Addr != ":9" {
		t.Fatalf("XDG_CONFIG_DIRS not searched: %q", c.Path())
	}
	// Saving settings never targets a system file.
	if w, _ := c.WritePath(); w == sys {
		t.Fatal("WritePath should not be the system config")
	}

	user := filepath.Join(cfgHome, "dropgitbox", "config.toml")
	writeFile(t, user, "[server]\naddr = \":8\"\n", 0o600)
	if c, _ = Load(""); c.Path() != user {
		t.Fatalf("user config should win, got %q", c.Path())
	}

	explicit := filepath.Join(root, "custom.toml")
	writeFile(t, explicit, "[server]\naddr = \":7\"\n", 0o600)
	t.Setenv(EnvConfig, explicit)
	if c, _ = Load(""); c.Server.Addr != ":7" {
		t.Fatalf("$%s ignored", EnvConfig)
	}
	if c, _ = Load(user); c.Server.Addr != ":8" {
		t.Fatal("-config should beat the environment")
	}
	if d, _ := DefaultDataDir(); d != filepath.Join(root, "data", "dropgitbox") {
		t.Fatalf("data dir = %s", d)
	}
}

func TestDecodeFullConfig(t *testing.T) {
	cfgHome := isolate(t)
	path := filepath.Join(cfgHome, "dropgitbox", "config.toml")
	writeFile(t, path, `
[server]
data_dir = "data"
public_url = "https://box.example.com"

[client]
server = "https://box.example.com"
device = "laptop"
debounce = "500ms"
poll = "10m"

[[folders]]
path = "~/Box"
repo = "docs"
`, 0o600)
	writeFile(t, filepath.Join(cfgHome, "dropgitbox", "token"), "dgb_abc\n", 0o600)

	c, err := Load("")
	if err != nil {
		t.Fatal(err)
	}
	if c.Client.Debounce != 500*time.Millisecond || c.Client.Poll != 10*time.Minute {
		t.Fatalf("durations: %v %v", c.Client.Debounce, c.Client.Poll)
	}
	if got := c.Resolve(c.Server.DataDir); got != filepath.Join(cfgHome, "dropgitbox", "data") {
		t.Fatalf("relative path resolved to %s", got)
	}
	home, _ := os.UserHomeDir()
	if got := c.Resolve(c.Folders[0].Path); got != filepath.Join(home, "Box") {
		t.Fatalf("~ expanded to %s", got)
	}
	if tok, err := c.ClientToken(); err != nil || tok != "dgb_abc" {
		t.Fatalf("token = %q, %v", tok, err)
	}
}

func TestRejectsUnknownKeysAndLeakyTokens(t *testing.T) {
	cfgHome := isolate(t)
	path := filepath.Join(cfgHome, "dropgitbox", "config.toml")
	writeFile(t, path, "[client]\nservr = \"x\"\n", 0o600)
	if _, err := Load(""); err == nil || !strings.Contains(err.Error(), "client.servr") {
		t.Fatalf("want unknown-key error, got %v", err)
	}

	writeFile(t, path, "[client]\ntoken = \"dgb_x\"\n", 0o644)
	c, err := Load("")
	if err != nil || len(c.Warnings()) == 0 {
		t.Fatalf("want a permissions warning, got %v %v", c.Warnings(), err)
	}

	writeFile(t, path, "", 0o600)
	writeFile(t, filepath.Join(cfgHome, "dropgitbox", "token"), "dgb_x\n", 0o644)
	if c, err = Load(""); err != nil {
		t.Fatal(err)
	}
	if _, err := c.ClientToken(); err == nil {
		t.Fatal("a world-readable token file must be refused")
	}
}

func TestEditingPreservesComments(t *testing.T) {
	cfgHome := isolate(t)
	path := filepath.Join(cfgHome, "dropgitbox", "config.toml")
	writeFile(t, path, `# my settings
[client]
# which server
server = "https://old.example.com"
poll = "1m"

[server]
addr = ":1234" # keep me
`, 0o600)

	if err := SetClient(path, [][2]string{{"server", "https://new.example.com"}, {"device", `we"ird`}}); err != nil {
		t.Fatal(err)
	}
	if err := AddFolder(path, Folder{Path: "~/Box", Repo: "docs"}); err != nil {
		t.Fatal(err)
	}
	if err := AddFolder(path, Folder{Path: "~/Box", Repo: "docs"}); err != nil { // idempotent
		t.Fatal(err)
	}
	b, _ := os.ReadFile(path)
	src := string(b)
	for _, want := range []string{"# my settings", "# which server", `addr = ":1234" # keep me`} {
		if !strings.Contains(src, want) {
			t.Fatalf("lost %q:\n%s", want, src)
		}
	}
	if strings.Count(src, "[[folders]]") != 1 {
		t.Fatalf("folder added twice:\n%s", src)
	}

	c, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if c.Client.Server != "https://new.example.com" || c.Client.Device != `we"ird` || c.Client.Poll != time.Minute {
		t.Fatalf("client = %+v", c.Client)
	}
	if c.Server.Addr != ":1234" || len(c.Folders) != 1 || c.Folders[0].Repo != "docs" {
		t.Fatalf("config = %+v", c)
	}
	if fi, _ := os.Stat(path); fi.Mode().Perm() != 0o600 {
		t.Fatalf("config perms = %v", fi.Mode().Perm())
	}
}

func TestEditingCreatesFile(t *testing.T) {
	cfgHome := isolate(t)
	path := filepath.Join(cfgHome, "dropgitbox", "config.toml")
	if err := SetClient(path, [][2]string{{"server", "https://box"}, {"device", ""}}); err != nil {
		t.Fatal(err)
	}
	c, err := Load("")
	if err != nil || c.Client.Server != "https://box" || c.Client.Device != "" {
		t.Fatalf("got %+v, %v", c, err)
	}
}

// The shipped config files must stay valid.
func TestPackagedConfigsParse(t *testing.T) {
	isolate(t)
	for _, f := range []string{"../../packaging/config.example.toml", "../../packaging/config.server.toml"} {
		c, err := Load(f)
		if err != nil {
			t.Fatalf("%s: %v", f, err)
		}
		if c.Path() == "" {
			t.Fatalf("%s not found", f)
		}
	}
	c, _ := Load("../../packaging/config.server.toml")
	if c.Server.DataDir != "/var/lib/dropgitbox" || c.Server.Addr != ":8080" {
		t.Fatalf("server config = %+v", c.Server)
	}
}
