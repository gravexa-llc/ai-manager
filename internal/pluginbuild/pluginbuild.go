// Package pluginbuild tells which build of the ai-manager plugin is on disk
// in a plugin folder now, the way the plugin's mod names the build it
// loaded (internal/events PluginBuild), so the dashboard can flag a session
// that still runs an older one.
//
// A session loads the plugin's mod once, when it starts, from the folder
// Claude Code reads the plugin from (a checkout's plugin/ folder). After a
// `git pull` there, sessions already running keep the old mod until they
// restart. The mod reports its build and folder on session_start; Cache
// works out what that folder holds now.
package pluginbuild

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// Name is the plugin's name in its plugin.json. Cache only describes a
// folder whose manifest says it, so aimd runs git in no other folder a
// session names.
const Name = "ai-manager"

// Describer names the build of the files under a folder: the worktree
// scanner's DescribeDir.
type Describer interface {
	DescribeDir(ctx context.Context, dir string) (string, error)
}

// Defaults.
const (
	// TTL is how long an answer is kept, a failure included.
	TTL = time.Minute
	// Timeout bounds one folder's git calls.
	Timeout = 5 * time.Second
	// maxRoots caps the folders remembered (a few in practice: the
	// checkout's plugin/ and a worktree's, for --plugin-dir).
	maxRoots = 32
)

// Cache answers OnDisk, remembering each folder's answer for TTL. It is
// safe for concurrent use.
type Cache struct {
	d   Describer
	now func() time.Time

	mu sync.Mutex
	m  map[string]*entry
}

type entry struct {
	build string
	at    time.Time // when build was worked out; zero before the first answer
	busy  bool      // a call is working it out
}

// New returns a cache over d. now is the clock; time.Now if nil.
func New(d Describer, now func() time.Time) *Cache {
	if now == nil {
		now = time.Now
	}
	return &Cache{d: d, now: now, m: map[string]*entry{}}
}

// OnDisk returns the build in the plugin folder root now, or "" when it
// can't tell: root isn't an absolute path to this plugin's folder (one
// whose .claude-plugin/plugin.json names it), or git couldn't describe it.
//
// An answer is kept for TTL. The call that finds none, or finds it stale,
// works it out (at most Timeout); calls for the same folder meanwhile get
// the last answer, or "", without waiting.
func (c *Cache) OnDisk(ctx context.Context, root string) string {
	if c == nil || c.d == nil || root == "" || !filepath.IsAbs(root) {
		return ""
	}
	root = filepath.Clean(root)
	now := c.now()
	c.mu.Lock()
	e := c.m[root]
	if e != nil && (e.busy || (!e.at.IsZero() && now.Sub(e.at) < TTL && !now.Before(e.at))) {
		b := e.build
		c.mu.Unlock()
		return b
	}
	if e == nil {
		c.evictLocked()
		e = &entry{}
		c.m[root] = e
	}
	e.busy = true
	c.mu.Unlock()

	build := c.describe(ctx, root)

	c.mu.Lock()
	e.build, e.at, e.busy = build, c.now(), false
	c.mu.Unlock()
	return build
}

// describe works out root's build, "" when it isn't this plugin's folder
// or git fails.
func (c *Cache) describe(ctx context.Context, root string) string {
	if !IsPluginRoot(root) {
		return ""
	}
	ctx, cancel := context.WithTimeout(ctx, Timeout)
	defer cancel()
	build, err := c.d.DescribeDir(ctx, root)
	if err != nil {
		return ""
	}
	return build
}

// evictLocked drops the oldest answer when the cache is full. c.mu is
// held.
func (c *Cache) evictLocked() {
	if len(c.m) < maxRoots {
		return
	}
	var oldest string
	var at time.Time
	for k, e := range c.m {
		if e.busy {
			continue
		}
		if oldest == "" || e.at.Before(at) {
			oldest, at = k, e.at
		}
	}
	if oldest != "" {
		delete(c.m, oldest)
	}
}

// IsPluginRoot reports whether dir holds this plugin: a
// .claude-plugin/plugin.json whose name is Name.
func IsPluginRoot(dir string) bool {
	f, err := os.Open(filepath.Join(dir, ".claude-plugin", "plugin.json"))
	if err != nil {
		return false
	}
	defer f.Close()
	var m struct {
		Name string `json:"name"`
	}
	if err := json.NewDecoder(f).Decode(&m); err != nil {
		return false
	}
	return m.Name == Name
}

// Outdated reports whether a session that loaded build runs an older
// plugin than onDisk, the build its folder holds now: both are known and
// they differ.
func Outdated(build, onDisk string) bool {
	return build != "" && onDisk != "" && build != onDisk
}
