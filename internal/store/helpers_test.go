package store

import (
	"context"
	"path/filepath"
	"reflect"
	"testing"
	"time"
)

// t0 is a fixed instant, whole milliseconds, in UTC (what reads return).
var t0 = time.Date(2026, 11, 5, 12, 0, 0, 0, time.UTC)

// at returns t0 plus d.
func at(d time.Duration) time.Time { return t0.Add(d) }

// newStore opens a fresh store in a temp dir with its clock fixed at t0.
func newStore(t *testing.T) *Store {
	t.Helper()
	s, err := Open(context.Background(), filepath.Join(t.TempDir(), FileName))
	if err != nil {
		t.Fatal(err)
	}
	s.now = func() time.Time { return t0 }
	t.Cleanup(func() {
		if err := s.Close(); err != nil {
			t.Error(err)
		}
	})
	return s
}

// equal fails the test when got and want differ.
func equal[T any](t *testing.T, what string, got, want T) {
	t.Helper()
	if !reflect.DeepEqual(got, want) {
		t.Errorf("%s:\n got %+v\nwant %+v", what, got, want)
	}
}

// must fails the test on a non-nil error.
func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}
