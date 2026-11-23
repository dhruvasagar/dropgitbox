package client

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/dhruvasagar/dropgitbox/internal/server"
)

// subscribe listens to the server's push notifications for the folder and
// calls onHead with the server's branch head on connect and after every
// push. It reconnects with backoff until ctx is cancelled.
func subscribe(ctx context.Context, cfg Config, idleTimeout time.Duration, log *slog.Logger, onHead func(string)) {
	backoff := time.Second
	for {
		connected, err := streamEvents(ctx, cfg, idleTimeout, onHead)
		if ctx.Err() != nil {
			return
		}
		if connected {
			backoff = time.Second
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
	if cfg.Token != "" {
		req.Header.Set("Authorization", "Bearer "+cfg.Token)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return false, err
	}
	defer resp.Body.Close()
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
