package server

import (
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func get(t *testing.T, url, token string) int {
	t.Helper()
	req, _ := http.NewRequest(http.MethodGet, url, nil)
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	return resp.StatusCode
}

func TestDeviceTokensAddRevokeWithoutRestart(t *testing.T) {
	dir := t.TempDir()
	srv, err := New(Options{DataDir: dir, Logger: slog.New(slog.NewTextHandler(io.Discard, nil))})
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()
	who := ts.URL + "/api/v1/whoami"

	if code := get(t, who, ""); code != http.StatusUnauthorized {
		t.Fatalf("no token: %d", code)
	}

	laptop, err := AddDevice(dir, "laptop")
	if err != nil {
		t.Fatal(err)
	}
	phone, err := AddDevice(dir, "phone")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(laptop, tokenPrefix) || laptop == phone {
		t.Fatalf("bad tokens %q %q", laptop, phone)
	}
	if _, err := AddDevice(dir, "laptop"); !errors.Is(err, ErrDeviceExists) {
		t.Fatalf("duplicate: %v", err)
	}
	if _, err := AddDevice(dir, "../evil"); err == nil {
		t.Fatal("invalid name accepted")
	}

	// The running server sees new devices immediately.
	if code := get(t, who, laptop); code != http.StatusOK {
		t.Fatalf("laptop: %d", code)
	}
	if code := get(t, who, phone); code != http.StatusOK {
		t.Fatalf("phone: %d", code)
	}
	if code := get(t, who, laptop+"x"); code != http.StatusUnauthorized {
		t.Fatalf("wrong token: %d", code)
	}

	if err := RevokeDevice(dir, "laptop"); err != nil {
		t.Fatal(err)
	}
	if code := get(t, who, laptop); code != http.StatusUnauthorized {
		t.Fatalf("revoked laptop still accepted: %d", code)
	}
	if code := get(t, who, phone); code != http.StatusOK {
		t.Fatalf("phone broken by revoking laptop: %d", code)
	}
	if err := RevokeDevice(dir, "laptop"); !errors.Is(err, ErrNoSuchDevice) {
		t.Fatalf("double revoke: %v", err)
	}

	// Only hashes are stored, and the file is private.
	b, _ := os.ReadFile(filepath.Join(dir, DevicesFile))
	if strings.Contains(string(b), phone) {
		t.Fatal("raw token stored on disk")
	}
	if fi, _ := os.Stat(filepath.Join(dir, DevicesFile)); fi.Mode().Perm() != 0o600 {
		t.Fatalf("devices.json perms = %v", fi.Mode().Perm())
	}
	devs, err := ListDevices(dir)
	if err != nil || len(devs) != 1 || devs[0].Name != "phone" {
		t.Fatalf("list = %+v, %v", devs, err)
	}
}

func TestNoAuthAndSharedToken(t *testing.T) {
	quiet := slog.New(slog.NewTextHandler(io.Discard, nil))
	open, err := New(Options{DataDir: t.TempDir(), NoAuth: true, Logger: quiet})
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(open.Handler())
	defer ts.Close()
	if code := get(t, ts.URL+"/api/v1/repos", ""); code != http.StatusOK {
		t.Fatalf("no-auth server: %d", code)
	}

	shared, err := New(Options{DataDir: t.TempDir(), Token: "shh", Logger: quiet})
	if err != nil {
		t.Fatal(err)
	}
	ts2 := httptest.NewServer(shared.Handler())
	defer ts2.Close()
	if code := get(t, ts2.URL+"/api/v1/repos", "shh"); code != http.StatusOK {
		t.Fatalf("shared token: %d", code)
	}
	if code := get(t, ts2.URL+"/api/v1/repos", ""); code != http.StatusUnauthorized {
		t.Fatalf("missing token: %d", code)
	}
}

func TestRevokeClosesOpenEventStream(t *testing.T) {
	dir := t.TempDir()
	srv, err := New(Options{DataDir: dir, Heartbeat: 50 * time.Millisecond, Logger: slog.New(slog.NewTextHandler(io.Discard, nil))})
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()
	tok, _ := AddDevice(dir, "laptop")

	req, _ := http.NewRequest(http.MethodPost, ts.URL+"/api/v1/repos/docs", nil)
	req.Header.Set("Authorization", "Bearer "+tok)
	if resp, err := http.DefaultClient.Do(req); err != nil || resp.StatusCode != http.StatusCreated {
		t.Fatalf("create: %v %v", resp, err)
	}

	req, _ = http.NewRequest(http.MethodGet, ts.URL+"/api/v1/repos/docs/events", nil)
	req.Header.Set("Authorization", "Bearer "+tok)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if err := RevokeDevice(dir, "laptop"); err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() { io.Copy(io.Discard, resp.Body); close(done) }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("event stream stayed open after revocation")
	}
}
