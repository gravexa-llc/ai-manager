package live

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/gravexa-llc/ai-manager/internal/events"
	"github.com/gravexa-llc/ai-manager/internal/events/eventstest"
	"github.com/gravexa-llc/ai-manager/internal/store"
	"github.com/gravexa-llc/ai-manager/internal/web"
	"github.com/gravexa-llc/ai-manager/internal/web/api"
)

const (
	testPort  = 7777
	testToken = "test-token-0123456789"
)

// clock is a fake clock the tests move.
type clock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *clock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *clock) set(t time.Time) {
	c.mu.Lock()
	c.t = t
	c.mu.Unlock()
}

// harness is a receiver behind aimd's real HTTP handler, on a temp store.
type harness struct {
	t     *testing.T
	st    *store.Store
	recv  *Receiver
	h     http.Handler
	clock *clock

	mu      sync.Mutex
	modLive []bool
	notes   []string
}

func newHarness(t *testing.T, mod func(*Options)) *harness {
	t.Helper()
	st, err := store.Open(context.Background(), filepath.Join(t.TempDir(), store.FileName))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	return newHarnessOpts(t, st, mod)
}

// newHarnessOn is a harness on another harness's store: aimd after a
// restart.
func newHarnessOn(t *testing.T, st *store.Store) *harness {
	t.Helper()
	return newHarnessOpts(t, st, nil)
}

func newHarnessOpts(t *testing.T, st *store.Store, mod func(*Options)) *harness {
	t.Helper()
	h := &harness{t: t, st: st, clock: &clock{t: time.Date(2026, 11, 6, 5, 0, 0, 0, time.UTC)}}
	opts := Options{
		Store:     st,
		TickEvery: time.Hour, // tests tick by hand
		Now:       h.clock.now,
		SetModLive: func(on bool) {
			h.mu.Lock()
			h.modLive = append(h.modLive, on)
			h.mu.Unlock()
		},
		Notify: func(topic string) {
			h.mu.Lock()
			h.notes = append(h.notes, topic)
			h.mu.Unlock()
		},
	}
	if mod != nil {
		mod(&opts)
	}
	h.recv = New(opts)
	h.h = web.New(web.Options{Port: testPort, Token: testToken, Store: st, Events: h.recv, Waits: h.recv}).Handler()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		h.recv.Run(ctx)
	}()
	t.Cleanup(func() {
		cancel()
		<-done
	})
	return h
}

// request sends a request through aimd's handler with the right Host.
func (h *harness) request(method, target string, body []byte, hdr map[string]string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, target, bytes.NewReader(body))
	req.Host = "127.0.0.1:7777"
	for k, v := range hdr {
		if k == "Host" {
			req.Host = v
			continue
		}
		req.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	h.h.ServeHTTP(rec, req)
	return rec
}

// post sends events as one batch with the token, checks for 200 {}, and
// waits until they are applied. The clock is moved to the last event's
// time first, unless it is already later.
func (h *harness) post(evs ...events.Event) {
	h.t.Helper()
	for _, e := range evs {
		if e.At.After(h.clock.now()) {
			h.clock.set(e.At)
		}
	}
	body, err := json.Marshal(map[string]any{"events": evs})
	if err != nil {
		h.t.Fatal(err)
	}
	rec := h.request(http.MethodPost, "/events", body, map[string]string{web.TokenHeader: testToken})
	if rec.Code != http.StatusOK || string(bytes.TrimSpace(rec.Body.Bytes())) != "{}" {
		h.t.Fatalf("POST /events = %d %s, want 200 {}", rec.Code, rec.Body)
	}
	h.sync()
}

// replay posts events in batches of up to 50, as the mod would.
func (h *harness) replay(evs []events.Event) {
	h.t.Helper()
	for len(evs) > 0 {
		n := min(50, len(evs))
		h.post(evs[:n]...)
		evs = evs[n:]
	}
}

func (h *harness) sync() {
	h.t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := h.recv.Sync(ctx); err != nil {
		h.t.Fatal(err)
	}
}

func (h *harness) tick() {
	h.recv.Tick(context.Background())
}

// session returns the row for a session id (live, else the last ended).
func (h *harness) session(id string) store.Session {
	h.t.Helper()
	s, err := h.st.FindSession(context.Background(), id)
	if err != nil {
		h.t.Fatalf("session %s: %v", id, err)
	}
	return s
}

// agents returns a session's agent rows by agent id ("" is the lead).
func (h *harness) agents(id string) map[string]store.Agent {
	h.t.Helper()
	rows, err := h.st.ListAgents(context.Background(), id)
	if err != nil {
		h.t.Fatal(err)
	}
	out := map[string]store.Agent{}
	for _, a := range rows {
		out[a.AgentID] = a
	}
	return out
}

func (h *harness) events(id string) []store.Event {
	h.t.Helper()
	rows, err := h.st.ListSessionEvents(context.Background(), id, 1000)
	if err != nil {
		h.t.Fatal(err)
	}
	return rows
}

func (h *harness) status() api.Status {
	h.t.Helper()
	rec := h.request(http.MethodGet, "/api/status", nil, nil)
	if rec.Code != http.StatusOK {
		h.t.Fatalf("GET /api/status = %d %s", rec.Code, rec.Body)
	}
	var st api.Status
	if err := json.Unmarshal(rec.Body.Bytes(), &st); err != nil {
		h.t.Fatal(err)
	}
	if st.Events == nil {
		h.t.Fatal("/api/status has no events section")
	}
	return st
}

func (h *harness) modLiveCalls() []bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]bool(nil), h.modLive...)
}

// fixture loads and translates a recorded event log from testdata/events.
func fixture(t *testing.T, name string) []events.Event {
	t.Helper()
	evs, err := eventstest.LoadS2(filepath.Join("..", "..", "testdata", "events", name))
	if err != nil {
		t.Fatal(err)
	}
	return evs
}

// split cuts evs after the first event that matches (it is in the first
// part). It fails the test when none matches.
func split(t *testing.T, evs []events.Event, match func(events.Event) bool) ([]events.Event, []events.Event) {
	t.Helper()
	for i, e := range evs {
		if match(e) {
			return evs[:i+1], evs[i+1:]
		}
	}
	t.Fatal("no event matches")
	return nil, nil
}

func kind(k events.Kind) func(events.Event) bool {
	return func(e events.Event) bool { return e.Kind == k }
}

func notFound(err error) bool { return errors.Is(err, store.ErrNotFound) }

func ptr[T any](v T) *T { return &v }
