package testenv

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Setup moves TMPDIR to a fresh directory under /tmp, and done removes it
// with what was left in it and restores TMPDIR.
func TestSetup(t *testing.T) {
	old := os.Getenv("TMPDIR")
	done := Setup()
	tmp := os.TempDir()
	if filepath.Dir(tmp) != tempParent || !strings.HasPrefix(filepath.Base(tmp), "aim-test-") {
		t.Errorf("TMPDIR = %q, want a new directory in %s", tmp, tempParent)
	}
	left := filepath.Join(tmp, "left-behind")
	if err := os.Mkdir(left, 0o700); err != nil {
		t.Fatal(err)
	}
	if code := done(0); code != 0 {
		t.Errorf("done(0) = %d", code)
	}
	if _, err := os.Stat(tmp); !os.IsNotExist(err) {
		t.Errorf("%s still there after done: %v", tmp, err)
	}
	if got := os.Getenv("TMPDIR"); got != old {
		t.Errorf("TMPDIR after done = %q, want %q", got, old)
	}
	if code := Setup()(3); code != 3 {
		t.Errorf("done(3) = %d", code)
	}
}
