package server

import (
	"net"
	"net/http"
	"net/netip"
	"strings"
	"sync"
	"time"
)

// DefaultTrustedProxies are the proxies whose X-Forwarded-For header is
// believed by default: only the local machine (a reverse proxy on the same
// host).
var DefaultTrustedProxies = []netip.Prefix{
	netip.MustParsePrefix("127.0.0.0/8"),
	netip.MustParsePrefix("::1/128"),
}

// clientIP returns the address of the client that made r. X-Forwarded-For
// is honoured only when the direct peer is a trusted proxy; the rightmost
// address not belonging to a trusted proxy is the client (earlier entries
// are client-controlled and cannot be believed).
func clientIP(r *http.Request, trusted []netip.Prefix) netip.Addr {
	peer := remoteAddr(r)
	if !isTrusted(peer, trusted) {
		return peer
	}
	hops := r.Header.Values("X-Forwarded-For")
	var parts []string
	for _, h := range hops {
		parts = append(parts, strings.Split(h, ",")...)
	}
	for i := len(parts) - 1; i >= 0; i-- {
		a, err := netip.ParseAddr(strings.TrimSpace(parts[i]))
		if err != nil {
			return peer // malformed header: fall back to the proxy itself
		}
		a = a.Unmap()
		if !isTrusted(a, trusted) {
			return a
		}
	}
	if xr, err := netip.ParseAddr(strings.TrimSpace(r.Header.Get("X-Real-IP"))); err == nil {
		return xr.Unmap()
	}
	return peer
}

func remoteAddr(r *http.Request) netip.Addr {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		host = r.RemoteAddr
	}
	a, err := netip.ParseAddr(host)
	if err != nil {
		return netip.Addr{}
	}
	return a.Unmap()
}

func isTrusted(a netip.Addr, trusted []netip.Prefix) bool {
	if !a.IsValid() {
		return false
	}
	for _, p := range trusted {
		if p.Contains(a) {
			return true
		}
	}
	return false
}

// limitKey groups addresses for rate limiting: IPv4 per address, IPv6 per
// /64 (one end-user allocation), so rotating addresses within a subnet does
// not evade the limit.
func limitKey(a netip.Addr) string {
	if a.Is6() {
		p, _ := a.Prefix(64)
		return p.String()
	}
	return a.String()
}

// authLimiter blocks clients that present too many invalid tokens. Tokens
// are 256-bit random values, so guessing is already infeasible; this keeps
// scanners from hammering the server and makes attacks visible in the log.
type authLimiter struct {
	maxFailures int
	window      time.Duration
	block       time.Duration
	now         func() time.Time

	mu      sync.Mutex
	clients map[string]*limitEntry
}

type limitEntry struct {
	failures     int
	windowStart  time.Time
	blockedUntil time.Time
}

// maxTracked bounds the limiter's memory; expired entries are purged when
// it is reached.
const maxTracked = 10000

func newAuthLimiter(maxFailures int, window, block time.Duration) *authLimiter {
	return &authLimiter{maxFailures: maxFailures, window: window, block: block, now: time.Now,
		clients: make(map[string]*limitEntry)}
}

// blocked reports whether key is currently blocked and until when.
func (l *authLimiter) blocked(key string) (time.Time, bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	e := l.clients[key]
	if e == nil || !l.now().Before(e.blockedUntil) {
		return time.Time{}, false
	}
	return e.blockedUntil, true
}

// fail records an invalid token from key and reports whether that made it
// blocked.
func (l *authLimiter) fail(key string) (blockedNow bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.now()
	e := l.clients[key]
	if e == nil {
		if len(l.clients) >= maxTracked {
			l.purge(now)
		}
		e = &limitEntry{windowStart: now}
		l.clients[key] = e
	}
	if now.Sub(e.windowStart) > l.window {
		e.failures, e.windowStart = 0, now
	}
	e.failures++
	if e.failures >= l.maxFailures {
		e.blockedUntil = now.Add(l.block)
		e.failures, e.windowStart = 0, now
		return true
	}
	return false
}

func (l *authLimiter) purge(now time.Time) {
	for k, e := range l.clients {
		if now.Sub(e.windowStart) > l.window && !now.Before(e.blockedUntil) {
			delete(l.clients, k)
		}
	}
	if len(l.clients) >= maxTracked {
		// Under a distributed flood, forget everyone rather than grow
		// without bound. Tokens stay unguessable regardless.
		clear(l.clients)
	}
}

// streamLimiter caps concurrent event streams per device.
type streamLimiter struct {
	max int
	mu  sync.Mutex
	n   map[string]int
}

func (l *streamLimiter) acquire(device string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.n == nil {
		l.n = make(map[string]int)
	}
	if l.n[device] >= l.max {
		return false
	}
	l.n[device]++
	return true
}

func (l *streamLimiter) release(device string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.n[device]--; l.n[device] <= 0 {
		delete(l.n, device)
	}
}

// securityHeaders sets conservative response headers. Nothing here is
// meant for browsers, so content is never sniffed, framed or cached.
func securityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("X-Frame-Options", "DENY")
		h.Set("Referrer-Policy", "no-referrer")
		h.Set("Content-Security-Policy", "default-src 'none'; frame-ancestors 'none'")
		h.Set("Cache-Control", "no-store")
		if r.TLS != nil {
			h.Set("Strict-Transport-Security", "max-age=31536000")
		}
		next.ServeHTTP(w, r)
	})
}

// gitRoute validates a smart-HTTP request below /git/<repo>.git/. Only
// the three endpoints of git's smart protocol are allowed; the legacy
// "dumb" protocol (raw object and ref file access) is not served.
func gitRoute(method, rest, service string) bool {
	switch rest {
	case "info/refs":
		return (method == http.MethodGet || method == http.MethodHead) &&
			(service == "git-upload-pack" || service == "git-receive-pack")
	case "git-upload-pack", "git-receive-pack":
		return method == http.MethodPost
	}
	return false
}
