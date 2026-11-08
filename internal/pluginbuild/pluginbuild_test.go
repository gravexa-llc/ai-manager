package pluginbuild

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

// fakeDescriber answers DescribeDir from a map, counting calls. block, if
// set, holds each call until it is closed.
type fakeDescriber struct {
	mu     sync.Mutex
	builds map[string]string
	calls  map[string]int
	block  chan struct{}
	called chan struct{}
}

func (f *fakeDescriber) DescribeDir(ctx context.Context, dir string) (string, error) {
	f.mu.Lock()
	f.calls[dir]++
	b, ok := f.builds[dir]
	block, called := f.block, f.called
	f.mu.Unlock()
	if called != nil {
		called <- struct{}{}
	}
	if block != nil {
		<-block
	}
	if !ok {
		return "", errors.New("not a git checkout")
	}
	return b, nil
}

func (f *fakeDescriber) set(dir, build string) {
	f.mu.Lock()
	f.builds[dir] = build
	f.mu.Unlock()
}

func (f *fakeDescriber) count(dir string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls[dir]
}

// pluginDir makes a plugin folder whose manifest has the name.
func pluginDir(t *testing.T, name string) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "plugin")
	if err := os.MkdirAll(filepath.Join(dir, ".claude-plugin"), 0o755); err != nil {
		t.Fatal(err)
	}
	body := `{"name": "` + name + `", "version": "1.0.0"}`
	if err := os.WriteFile(filepath.Join(dir, ".claude-plugin", "plugin.json"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return dir
}

type fakeClock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *fakeClock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *fakeClock) add(d time.Duration) {
	c.mu.Lock()
	c.t = c.t.Add(d)
	c.mu.Unlock()
}

func TestOnDisk(t *testing.T) {
	ctx := context.Background()
	root := pluginDir(t, Name)
	f := &fakeDescriber{builds: map[string]string{root: "v1.0.0-3-gabc1234"}, calls: map[string]int{}}
	clk := &fakeClock{t: time.Date(2026, 11, 8, 9, 0, 0, 0, time.UTC)}
	c := New(f, clk.now)

	if got := c.OnDisk(ctx, root); got != "v1.0.0-3-gabc1234" {
		t.Errorf("OnDisk = %q", got)
	}
	// Cached for TTL, then worked out again.
	f.set(root, "v1.0.0-5-gdef5678")
	clk.add(TTL - time.Second)
	if got := c.OnDisk(ctx, root+"/"); got != "v1.0.0-3-gabc1234" || f.count(root) != 1 {
		t.Errorf("within the TTL: %q after %d calls, want the cached answer after 1", got, f.count(root))
	}
	clk.add(2 * time.Second)
	if got := c.OnDisk(ctx, root); got != "v1.0.0-5-gdef5678" || f.count(root) != 2 {
		t.Errorf("after the TTL: %q after %d calls", got, f.count(root))
	}

	// A failure is "" and cached too.
	bad := pluginDir(t, Name)
	if got := c.OnDisk(ctx, bad); got != "" {
		t.Errorf("not a checkout: %q", got)
	}
	c.OnDisk(ctx, bad)
	if f.count(bad) != 1 {
		t.Errorf("a failure was retried within the TTL: %d calls", f.count(bad))
	}

	// Only this plugin's folder, by absolute path, is ever described.
	other := pluginDir(t, "someone-else")
	f.set(other, "v9")
	noManifest := t.TempDir()
	f.set(noManifest, "v9")
	for _, dir := range []string{other, noManifest, "", "plugin", "relative/plugin"} {
		if got := c.OnDisk(ctx, dir); got != "" {
			t.Errorf("OnDisk(%q) = %q, want \"\"", dir, got)
		}
		if f.count(dir) != 0 {
			t.Errorf("OnDisk(%q) ran git", dir)
		}
	}
	var nilCache *Cache
	if nilCache.OnDisk(ctx, root) != "" {
		t.Error("a nil cache answered")
	}
}

// While one call works a folder out, others get the last answer at once.
func TestOnDiskDoesntWait(t *testing.T) {
	ctx := context.Background()
	root := pluginDir(t, Name)
	f := &fakeDescriber{builds: map[string]string{root: "v1"}, calls: map[string]int{},
		block: make(chan struct{}), called: make(chan struct{}, 1)}
	clk := &fakeClock{t: time.Date(2026, 11, 8, 9, 0, 0, 0, time.UTC)}
	c := New(f, clk.now)

	done := make(chan string)
	go func() { done <- c.OnDisk(ctx, root) }()
	<-f.called
	if got := c.OnDisk(ctx, root); got != "" {
		t.Errorf("during the first answer: %q, want \"\" at once", got)
	}
	close(f.block)
	if got := <-done; got != "v1" {
		t.Errorf("first answer = %q", got)
	}

	f.mu.Lock()
	f.block, f.builds[root] = make(chan struct{}), "v2"
	f.mu.Unlock()
	clk.add(TTL + time.Second)
	go func() { done <- c.OnDisk(ctx, root) }()
	<-f.called
	if got := c.OnDisk(ctx, root); got != "v1" {
		t.Errorf("during a refresh: %q, want the last answer", got)
	}
	close(f.block)
	if got := <-done; got != "v2" {
		t.Errorf("refreshed answer = %q", got)
	}
}

func TestOutdated(t *testing.T) {
	for _, tc := range []struct {
		build, onDisk string
		want          bool
	}{
		{"v1.0.0-3-gabc1234", "v1.0.0-5-gdef5678", true},
		{"v1.0.0-3-gabc1234", "v1.0.0-3-gabc1234", false},
		{"v1.0.0-3-gabc1234", "v1.0.0-3-gabc1234-dirty", true},
		{"", "v1.0.0-5-gdef5678", false}, // not reported
		{"v1.0.0-3-gabc1234", "", false}, // the folder can't be read
	} {
		if got := Outdated(tc.build, tc.onDisk); got != tc.want {
			t.Errorf("Outdated(%q, %q) = %v", tc.build, tc.onDisk, got)
		}
	}
}

func TestIsPluginRoot(t *testing.T) {
	if !IsPluginRoot(pluginDir(t, Name)) {
		t.Error("the plugin's folder")
	}
	broken := pluginDir(t, Name)
	if err := os.WriteFile(filepath.Join(broken, ".claude-plugin", "plugin.json"), []byte("{"), 0o644); err != nil {
		t.Fatal(err)
	}
	if IsPluginRoot(broken) || IsPluginRoot(pluginDir(t, "other")) || IsPluginRoot(t.TempDir()) {
		t.Error("a folder that isn't the plugin's")
	}
}
