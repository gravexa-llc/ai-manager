package web

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gravexa-llc/ai-manager/internal/store"
	"github.com/gravexa-llc/ai-manager/internal/web/api"
)

const (
	testPort  = 7777
	testToken = "test-token-0123456789"
)

// t0 is the tests' fixed "now".
var t0 = time.Date(2026, 11, 5, 12, 0, 0, 0, time.UTC)

// fakeStatus is a StatusSource with a fixed answer.
type fakeStatus struct{ st api.Status }

func (f fakeStatus) Status() api.Status { return f.st }

// newTestStore opens a store on a temp file.
func newTestStore(t *testing.T) *store.Store {
	t.Helper()
	st, err := store.Open(context.Background(), filepath.Join(t.TempDir(), store.FileName))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	return st
}

// newTestServer returns a server on testPort over st.
func newTestServer(t *testing.T, st Store, status StatusSource) *Server {
	t.Helper()
	return New(Options{
		Port:   testPort,
		Token:  testToken,
		Store:  st,
		Status: status,
		Now:    func() time.Time { return t0 },
	})
}

// do sends a request through h with the right Host unless the request
// sets one, and returns the recorder.
func do(t *testing.T, h http.Handler, method, target string, body io.Reader, hdr map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, target, body)
	req.Host = "127.0.0.1:7777"
	for k, v := range hdr {
		if k == "Host" {
			req.Host = v
			continue
		}
		req.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

// decode reads a JSON body into v.
func decode(t *testing.T, rec *httptest.ResponseRecorder, v any) {
	t.Helper()
	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
		t.Fatalf("Content-Type = %q, want JSON; body %s", ct, rec.Body)
	}
	if err := json.Unmarshal(rec.Body.Bytes(), v); err != nil {
		t.Fatalf("decode %s: %v", rec.Body, err)
	}
}

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}
