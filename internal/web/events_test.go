package web

import (
	"net/http"
	"strings"
	"sync"
	"testing"

	"github.com/gravexa-llc/ai-manager/internal/events"
	"github.com/gravexa-llc/ai-manager/internal/web/api"
)

// fakeSink records what POST /events hands it.
type fakeSink struct {
	mu      sync.Mutex
	batches []events.Batch
	refused []string
}

func (f *fakeSink) Accept(b events.Batch) {
	f.mu.Lock()
	f.batches = append(f.batches, b)
	f.mu.Unlock()
}

func (f *fakeSink) Refuse(reason string) {
	f.mu.Lock()
	f.refused = append(f.refused, reason)
	f.mu.Unlock()
}

func (f *fakeSink) Stats() api.EventStats {
	f.mu.Lock()
	defer f.mu.Unlock()
	return api.EventStats{Batches: int64(len(f.batches)), RefusedBatches: int64(len(f.refused))}
}

func TestPostEvents(t *testing.T) {
	sink := &fakeSink{}
	srv := New(Options{Port: testPort, Token: testToken, Store: newTestStore(t), Events: sink})
	h := srv.Handler()
	tok := map[string]string{TokenHeader: testToken}

	body := `{"events": [{"kind": "idle", "session_id": "s1", "at": "2026-11-05T12:00:00Z"}, {"kind": "odd", "session_id": "s1", "at": "2026-11-05T12:00:00Z"}]}`
	rec := do(t, h, http.MethodPost, "/events", strings.NewReader(body), tok)
	if rec.Code != http.StatusOK || strings.TrimSpace(rec.Body.String()) != "{}" {
		t.Fatalf("POST /events = %d %s, want 200 {}", rec.Code, rec.Body)
	}
	if len(sink.batches) != 1 || len(sink.batches[0].Events) != 1 || sink.batches[0].Unknown["odd"] != 1 {
		t.Errorf("sink got %+v", sink.batches)
	}

	if rec := do(t, h, http.MethodPost, "/events", strings.NewReader(body), nil); rec.Code != http.StatusUnauthorized {
		t.Errorf("without the token: %d, want 401", rec.Code)
	}
	if rec := do(t, h, http.MethodPost, "/events", strings.NewReader(`{"events": [`), tok); rec.Code != http.StatusBadRequest {
		t.Errorf("broken body: %d, want 400", rec.Code)
	}
	huge := `{"events": [{"kind": "idle", "session_id": "` + strings.Repeat("s", events.MaxBatchBytes) + `"}]}`
	if rec := do(t, h, http.MethodPost, "/events", strings.NewReader(huge), tok); rec.Code != http.StatusRequestEntityTooLarge {
		t.Errorf("over 256 KB: %d, want 413", rec.Code)
	}
	if len(sink.refused) != 2 {
		t.Errorf("refused = %v, want 2", sink.refused)
	}

	var st api.Status
	decode(t, do(t, h, http.MethodGet, "/api/status", nil, nil), &st)
	if st.Events == nil || st.Events.Batches != 1 || st.Events.RefusedBatches != 2 {
		t.Errorf("status events = %+v", st.Events)
	}
}

// With no sink (a server without the receiver), batches are dropped and
// still answered 200 {}, and /api/status has no events section.
func TestPostEventsWithoutSink(t *testing.T) {
	h := New(Options{Port: testPort, Token: testToken, Store: newTestStore(t)}).Handler()
	rec := do(t, h, http.MethodPost, "/events", strings.NewReader(`{"events": []}`), map[string]string{TokenHeader: testToken})
	if rec.Code != http.StatusOK {
		t.Errorf("POST /events = %d", rec.Code)
	}
	var st api.Status
	decode(t, do(t, h, http.MethodGet, "/api/status", nil, nil), &st)
	if st.Events != nil {
		t.Errorf("status events = %+v, want none", st.Events)
	}
}
