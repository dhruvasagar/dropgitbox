package client

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/dhruvasagar/dropgitbox/internal/server"
)

var errStreamUnauthorized = errors.New("server rejected this device's token")

// subscribe listens to the server's push notifications for the folder and
// calls onHead with the server's branch head on connect and after every
// push. It reconnects with backoff until ctx is cancelled. onRejected is
// called when the server rejects the token (e.g. the device was revoked),
// so the daemon can surface that promptly instead of at its next sync.
func subscribe(ctx context.Context, cfg Config, idleTimeout time.Duration, log *slog.Logger, onHead func(string), onRejected func()) {
	backoff := time.Second
	for {
		connected, err := streamEvents(ctx, cfg, idleTimeout, onHead)
		if ctx.Err() != nil {
			return
		}
		if connected {
			backoff = time.Second
		}
		if errors.Is(err, errStreamUnauthorized) {
			// A revoked or wrong token won't start working by retrying
			// quickly; don't count towards the server's lockout.
			backoff = 5 * time.Minute
			onRejected()
		}
		log.Warn("event stream disconnected; retrying", "err", err, "in", backoff)
		select {
		case <-ctx.Done():
			return
		case <-time.After(backoff):
		}
		backoff = min(backoff*2, 30*time.Second)
	}
}

// streamEvents holds one event-stream connection open. connected reports
// whether the server accepted the connection.
func streamEvents(ctx context.Context, cfg Config, idleTimeout time.Duration, onHead func(string)) (connected bool, err error) {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, cfg.EventsURL(), nil)
	if err != nil {
		return false, err
	}
	req.Header.Set("Accept", "text/event-stream")
	req.Header.Set("User-Agent", userAgent())
	if cfg.Token != "" {
		req.Header.Set("Authorization", "Bearer "+cfg.Token)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return false, err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusUnauthorized {
		return false, errStreamUnauthorized
	}
	if resp.StatusCode != http.StatusOK {
		return false, fmt.Errorf("server returned %s", resp.Status)
	}

	// The server sends a heartbeat regularly; if the line goes silent the
	// connection is dead (e.g. laptop suspended), so drop it and redial.
	idle := time.AfterFunc(idleTimeout, cancel)
	defer idle.Stop()

	sc := bufio.NewScanner(resp.Body)
	for sc.Scan() {
		idle.Reset(idleTimeout)
		data, ok := strings.CutPrefix(sc.Text(), "data:")
		if !ok {
			continue
		}
		var ev server.Event
		if err := json.Unmarshal([]byte(strings.TrimSpace(data)), &ev); err != nil {
			continue
		}
		onHead(ev.Head)
	}
	if err := sc.Err(); err != nil {
		return true, err
	}
	return true, fmt.Errorf("stream closed by server")
}
