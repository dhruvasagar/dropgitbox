package server

import "sync"

// Event announces that a repository's branch moved to a new commit.
type Event struct {
	Repo string `json:"repo"`
	Head string `json:"head"`
}

// hub fans out push events to every client subscribed to a repository.
type hub struct {
	mu   sync.Mutex
	subs map[string]map[chan Event]struct{}
}

func newHub() *hub {
	return &hub{subs: make(map[string]map[chan Event]struct{})}
}

// subscribe registers interest in repo. The returned cancel func must be
// called to release the subscription.
func (h *hub) subscribe(repo string) (<-chan Event, func()) {
	ch := make(chan Event, 8)
	h.mu.Lock()
	if h.subs[repo] == nil {
		h.subs[repo] = make(map[chan Event]struct{})
	}
	h.subs[repo][ch] = struct{}{}
	h.mu.Unlock()

	return ch, func() {
		h.mu.Lock()
		delete(h.subs[repo], ch)
		if len(h.subs[repo]) == 0 {
			delete(h.subs, repo)
		}
		h.mu.Unlock()
	}
}

// publish delivers ev to all subscribers without blocking. A subscriber
// whose buffer is full misses the event; that is harmless because clients
// also poll, and the next event carries the latest head anyway.
func (h *hub) publish(ev Event) {
	h.mu.Lock()
	defer h.mu.Unlock()
	for ch := range h.subs[ev.Repo] {
		select {
		case ch <- ev:
		default:
		}
	}
}
