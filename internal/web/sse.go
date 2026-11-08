package web

import (
	"cmp"
	"encoding/json"
	"fmt"
	"net/http"
	"slices"
	"sync"
	"time"

	"github.com/gravexa-llc/ai-manager/internal/web/api"
)

// DefaultKeepalive is how often an idle stream gets a comment line, so
// dead connections are noticed and proxies don't time it out.
const DefaultKeepalive = 25 * time.Second

// Hub fans change events out to /api/stream clients and counts them. The
// daemon calls Publish after a write; the count drives its poll intervals.
type Hub struct {
	onWatchers func(n int)
	keepalive  time.Duration

	mu      sync.Mutex
	clients map[*client]struct{}
	seq     uint64
	closed  bool
	done    chan struct{}

	cbMu sync.Mutex // serializes onWatchers, so the last call has the latest count
}

// client is one stream. Topics that arrive while it is writing are merged,
// so a slow client gets one event per topic rather than a backlog.
type client struct {
	mu      sync.Mutex
	pending map[string]uint64 // topic → latest seq
	wake    chan struct{}     // capacity 1
}

// NewHub returns a hub. onWatchers, if not nil, is called with the number
// of connected clients each time it changes.
func NewHub(onWatchers func(n int)) *Hub {
	return &Hub{
		onWatchers: onWatchers,
		keepalive:  DefaultKeepalive,
		clients:    map[*client]struct{}{},
		done:       make(chan struct{}),
	}
}

// Watchers returns the number of connected clients.
func (h *Hub) Watchers() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return len(h.clients)
}

// Publish tells every client that topic changed. It never blocks.
func (h *Hub) Publish(topic string) {
	h.mu.Lock()
	h.seq++
	seq := h.seq
	clients := make([]*client, 0, len(h.clients))
	for c := range h.clients {
		clients = append(clients, c)
	}
	h.mu.Unlock()
	for _, c := range clients {
		c.mu.Lock()
		c.pending[topic] = seq
		c.mu.Unlock()
		select {
		case c.wake <- struct{}{}:
		default:
		}
	}
}

// Close ends every stream and refuses new ones. aimd calls it when the
// server shuts down (streams never end on their own).
func (h *Hub) Close() {
	h.mu.Lock()
	defer h.mu.Unlock()
	if !h.closed {
		h.closed = true
		close(h.done)
	}
}

func (h *Hub) add() (*client, bool) {
	c := &client{pending: map[string]uint64{}, wake: make(chan struct{}, 1)}
	h.mu.Lock()
	if h.closed {
		h.mu.Unlock()
		return nil, false
	}
	h.clients[c] = struct{}{}
	h.mu.Unlock()
	h.watchersChanged()
	return c, true
}

func (h *Hub) remove(c *client) {
	h.mu.Lock()
	delete(h.clients, c)
	h.mu.Unlock()
	h.watchersChanged()
}

func (h *Hub) watchersChanged() {
	if h.onWatchers == nil {
		return
	}
	h.cbMu.Lock()
	defer h.cbMu.Unlock()
	h.onWatchers(h.Watchers())
}

// ServeHTTP is GET /api/stream: a text/event-stream that sends a "ready"
// event (data: {"seq": the latest change's, "assets": the static files'
// version}), then a "change" event (data: api.Change) whenever a topic
// changes, and a comment line every keepalive interval.
func (h *Hub) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		writeError(w, http.StatusInternalServerError, "streaming unsupported")
		return
	}
	c, ok := h.add()
	if !ok {
		writeError(w, http.StatusServiceUnavailable, "shutting down")
		return
	}
	defer h.remove(c)

	hd := w.Header()
	hd.Set("Content-Type", "text/event-stream")
	hd.Set("Cache-Control", "no-store")
	hd.Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
	h.mu.Lock()
	seq := h.seq
	h.mu.Unlock()
	// assets is the build's static files version: a page loaded from
	// another build reloads (app-shell.js).
	if _, err := fmt.Fprintf(w, "retry: 3000\nevent: ready\ndata: {\"seq\":%d,\"assets\":%q}\n\n", seq, assetVersion); err != nil {
		return
	}
	flusher.Flush()

	ka := time.NewTicker(h.keepalive)
	defer ka.Stop()
	for {
		select {
		case <-r.Context().Done():
			return
		case <-h.done:
			return
		case <-ka.C:
			if _, err := fmt.Fprint(w, ": keepalive\n\n"); err != nil {
				return
			}
			flusher.Flush()
		case <-c.wake:
			c.mu.Lock()
			pending := c.pending
			c.pending = map[string]uint64{}
			c.mu.Unlock()
			topics := make([]string, 0, len(pending))
			for t := range pending {
				topics = append(topics, t)
			}
			slices.SortFunc(topics, func(a, b string) int { return cmp.Compare(pending[a], pending[b]) })
			now := time.Now().UTC()
			for _, t := range topics {
				data, _ := json.Marshal(api.Change{Topic: t, Seq: pending[t], At: now})
				if _, err := fmt.Fprintf(w, "id: %d\nevent: change\ndata: %s\n\n", pending[t], data); err != nil {
					return
				}
			}
			flusher.Flush()
		}
	}
}
