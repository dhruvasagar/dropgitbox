package server

import (
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strings"
	"testing"
	"time"
)

func newTestServer(t *testing.T, opts Options) (*Server, *httptest.Server, string) {
	t.Helper()
	opts.DataDir = t.TempDir()
	opts.Logger = slog.New(slog.NewTextHandler(io.Discard, nil))
	srv, err := New(opts)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { srv.Close() })
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)
	tok, err := AddDevice(opts.DataDir, "laptop")
	if err != nil {
		t.Fatal(err)
	}
	return srv, ts, tok
}

func do(t *testing.T, method, url, token string, hdr map[string]string) *http.Response {
	t.Helper()
	req, _ := http.NewRequest(method, url, nil)
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	return resp
}

func TestUnauthenticatedRequestsRevealNothing(t *testing.T) {
	_, ts, tok := newTestServer(t, Options{})
	if r := do(t, "POST", ts.URL+"/api/v1/repos/docs", tok, nil); r.StatusCode != http.StatusCreated {
		t.Fatalf("create: %d", r.StatusCode)
	}
	paths := []string{
		"/api/v1/whoami", "/api/v1/repos", "/api/v1/repos/docs", "/api/v1/repos/docs/events",
		"/api/v1/repos/nonexistent", "/git/docs.git/info/refs?service=git-upload-pack",
		"/git/nonexistent.git/info/refs?service=git-upload-pack", "/git/docs.git/HEAD",
	}
	for _, p := range paths {
		for _, token := range []string{"", "dgb_wrong", "not-even-a-token"} {
			r := do(t, "GET", ts.URL+p, token, nil)
			// Existing and missing repos look identical without a token.
			if r.StatusCode != http.StatusUnauthorized {
				t.Errorf("GET %s token=%q: %d, want 401", p, token, r.StatusCode)
			}
		}
	}
	if r := do(t, "POST", ts.URL+"/api/v1/repos/evil", "", nil); r.StatusCode != http.StatusUnauthorized {
		t.Errorf("unauthenticated create: %d", r.StatusCode)
	}
	if r := do(t, "GET", ts.URL+"/healthz", "", nil); r.StatusCode != http.StatusOK {
		t.Errorf("healthz: %d", r.StatusCode)
	}
}

func TestOnlySmartHTTPEndpointsAreServed(t *testing.T) {
	_, ts, tok := newTestServer(t, Options{})
	do(t, "POST", ts.URL+"/api/v1/repos/docs", tok, nil)

	ok := do(t, "GET", ts.URL+"/git/docs.git/info/refs?service=git-upload-pack", tok, nil)
	if ok.StatusCode != http.StatusOK {
		t.Fatalf("smart info/refs: %d", ok.StatusCode)
	}
	for _, p := range []string{
		"/git/docs.git/HEAD", "/git/docs.git/config", "/git/docs.git/info/refs",
		"/git/docs.git/objects/info/packs", "/git/docs.git/info/refs?service=git-evil",
		"/git/docs.git/../devices.json", "/git/docs.git/hooks/pre-receive",
		"/git/..%2fdevices.json", "/git/docs.git/%2e%2e/%2e%2e/etc/passwd",
	} {
		r := do(t, "GET", ts.URL+p, tok, nil)
		if r.StatusCode == http.StatusOK {
			t.Errorf("GET %s served (200)", p)
		}
	}
	if r := do(t, "GET", ts.URL+"/git/docs.git/git-receive-pack", tok, nil); r.StatusCode != http.StatusNotFound {
		t.Errorf("GET on receive-pack: %d", r.StatusCode)
	}
}

func TestBruteForceIsBlocked(t *testing.T) {
	srv, ts, tok := newTestServer(t, Options{AuthMaxFailures: 5, AuthWindow: time.Minute, AuthBlock: time.Hour})
	who := ts.URL + "/api/v1/whoami"

	// Missing credentials (e.g. git's first anonymous probe) don't count.
	for range 10 {
		do(t, "GET", who, "", nil)
	}
	if r := do(t, "GET", who, tok, nil); r.StatusCode != http.StatusOK {
		t.Fatalf("anonymous probes caused a block: %d", r.StatusCode)
	}

	for i := range 5 {
		if r := do(t, "GET", who, "dgb_guess", nil); r.StatusCode != http.StatusUnauthorized {
			t.Fatalf("attempt %d: %d", i, r.StatusCode)
		}
	}
	r := do(t, "GET", who, tok, nil) // even a valid token is refused while blocked
	if r.StatusCode != http.StatusTooManyRequests || r.Header.Get("Retry-After") == "" {
		t.Fatalf("blocked client: %d retry-after=%q", r.StatusCode, r.Header.Get("Retry-After"))
	}

	// The block expires.
	srv.limiter.now = func() time.Time { return time.Now().Add(2 * time.Hour) }
	if r := do(t, "GET", who, tok, nil); r.StatusCode != http.StatusOK {
		t.Fatalf("after block expiry: %d", r.StatusCode)
	}
}

func TestBlockingUsesRealClientBehindProxy(t *testing.T) {
	// httptest connections come from 127.0.0.1, a trusted proxy by default.
	_, ts, tok := newTestServer(t, Options{AuthMaxFailures: 3, AuthWindow: time.Minute, AuthBlock: time.Hour})
	who := ts.URL + "/api/v1/whoami"
	attacker := map[string]string{"X-Forwarded-For": "203.0.113.7"}
	for range 3 {
		do(t, "GET", who, "dgb_guess", attacker)
	}
	if r := do(t, "GET", who, tok, attacker); r.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("attacker not blocked: %d", r.StatusCode)
	}
	// Another client behind the same proxy is unaffected.
	if r := do(t, "GET", who, tok, map[string]string{"X-Forwarded-For": "198.51.100.1"}); r.StatusCode != http.StatusOK {
		t.Fatalf("innocent client blocked: %d", r.StatusCode)
	}
	// A spoofed leftmost entry doesn't help: the proxy appends the real IP.
	spoof := map[string]string{"X-Forwarded-For": "198.51.100.1, 203.0.113.7"}
	if r := do(t, "GET", who, tok, spoof); r.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("spoofed XFF evaded block: %d", r.StatusCode)
	}
}

func TestClientIP(t *testing.T) {
	trusted := DefaultTrustedProxies
	mk := func(remote string, xff ...string) *http.Request {
		r := httptest.NewRequest("GET", "/", nil)
		r.RemoteAddr = remote
		for _, x := range xff {
			r.Header.Add("X-Forwarded-For", x)
		}
		return r
	}
	cases := []struct {
		req  *http.Request
		want string
	}{
		{mk("203.0.113.9:5555", "1.2.3.4"), "203.0.113.9"},        // untrusted peer: XFF ignored
		{mk("127.0.0.1:5555", "1.2.3.4"), "1.2.3.4"},              // trusted proxy
		{mk("127.0.0.1:5555", "9.9.9.9, 1.2.3.4"), "1.2.3.4"},     // rightmost untrusted wins
		{mk("127.0.0.1:5555", "1.2.3.4", "127.0.0.1"), "1.2.3.4"}, // skip trusted hops
		{mk("127.0.0.1:5555", "garbage"), "127.0.0.1"},            // malformed
		{mk("[::1]:5555", "2001:db8::1"), "2001:db8::1"},
	}
	for _, c := range cases {
		if got := clientIP(c.req, trusted); got.String() != c.want {
			t.Errorf("RemoteAddr=%s XFF=%v: got %s want %s", c.req.RemoteAddr, c.req.Header.Values("X-Forwarded-For"), got, c.want)
		}
	}
	a, b := netip.MustParseAddr("2001:db8::1"), netip.MustParseAddr("2001:db8::ffff")
	if limitKey(a) != limitKey(b) {
		t.Error("IPv6 addresses in one /64 should share a limit")
	}
}

func TestSecurityHeadersAndWeakSharedToken(t *testing.T) {
	_, ts, _ := newTestServer(t, Options{})
	r := do(t, "GET", ts.URL+"/healthz", "", nil)
	for _, h := range []string{"X-Content-Type-Options", "X-Frame-Options", "Content-Security-Policy", "Cache-Control"} {
		if r.Header.Get(h) == "" {
			t.Errorf("missing %s", h)
		}
	}
	_, err := New(Options{DataDir: t.TempDir(), Token: "short"})
	if err == nil || !strings.Contains(err.Error(), "at least") {
		t.Fatalf("weak shared token accepted: %v", err)
	}
}

func TestEventStreamsPerDeviceAreCapped(t *testing.T) {
	_, ts, tok := newTestServer(t, Options{MaxStreamsPerDevice: 2})
	do(t, "POST", ts.URL+"/api/v1/repos/docs", tok, nil)
	var open []*http.Response
	for range 2 {
		req, _ := http.NewRequest("GET", ts.URL+"/api/v1/repos/docs/events", nil)
		req.Header.Set("Authorization", "Bearer "+tok)
		resp, err := http.DefaultClient.Do(req)
		if err != nil || resp.StatusCode != http.StatusOK {
			t.Fatalf("stream: %v %v", resp, err)
		}
		open = append(open, resp)
	}
	if r := do(t, "GET", ts.URL+"/api/v1/repos/docs/events", tok, nil); r.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("third stream: %d", r.StatusCode)
	}
	for _, r := range open {
		r.Body.Close()
	}
}
