package worktrees

import (
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"sync"
)

// caseInsensitive is true where paths are compared without regard to case.
// macOS volumes are case-insensitive by default (APFS, HFS+): git can record
// ~/projects/x for a repo that is ~/Projects/x on disk.
var caseInsensitive = runtime.GOOS == "darwin" || runtime.GOOS == "ios" || runtime.GOOS == "windows"

// PathKey returns p in the form used to compare paths: cleaned, and folded
// to lower case on case-insensitive systems (macOS). Use it for map keys.
func PathKey(p string) string {
	p = filepath.Clean(p)
	if caseInsensitive {
		return strings.ToLower(p)
	}
	return p
}

// SamePath reports whether a and b name the same path, ignoring case on
// macOS. It is purely lexical: it doesn't resolve symlinks.
func SamePath(a, b string) bool { return PathKey(a) == PathKey(b) }

// within reports whether p is dir or inside it, by PathKey.
func within(p, dir string) bool {
	p, dir = PathKey(p), PathKey(dir)
	if p == dir {
		return true
	}
	if !strings.HasSuffix(dir, string(filepath.Separator)) {
		dir += string(filepath.Separator)
	}
	return strings.HasPrefix(p, dir)
}

// canon turns existing paths into their canonical form: symlinks resolved
// and, on case-insensitive systems, every component spelled the way it is on
// disk. filepath.EvalSymlinks alone keeps the caller's spelling (checked on
// macOS: it returns .../proj/x for .../Proj/x), so each directory is read to
// find the stored name.
//
// One canon covers one scan (Scan shares one between discovery and the
// repo scans). It lists each directory at most once, however many paths
// pass through it and however many goroutines ask at the same time, and
// looks names up in a set: a crowded parent such as a busy $TMPDIR is read
// once per scan, not once per path. Safe for concurrent use.
type canon struct {
	readDir func(dir string) []string
	mu      sync.Mutex
	dirs    map[string]*dirNames
}

// dirNames is one directory's listing, read on first use.
type dirNames struct {
	once  sync.Once
	names []string // in listing order
	set   map[string]struct{}
}

func newCanon(readDir func(string) []string) *canon {
	return &canon{readDir: readDir, dirs: map[string]*dirNames{}}
}

// readDirNames lists dir's entry names, or nil if it can't be read.
func readDirNames(dir string) []string {
	f, err := os.Open(dir)
	if err != nil {
		return nil
	}
	defer f.Close()
	names, _ := f.Readdirnames(-1)
	return names
}

// path returns the canonical form of p, or an error (fs.ErrNotExist when p
// doesn't exist) from resolving it.
func (c *canon) path(p string) (string, error) {
	r, err := filepath.EvalSymlinks(p)
	if err != nil {
		return "", err
	}
	if !caseInsensitive {
		return r, nil
	}
	return c.fixCase(r), nil
}

// fixCase respells each component of the absolute path p with its on-disk
// name, keeping a component as given when its directory can't be read or
// has no matching entry.
func (c *canon) fixCase(p string) string {
	vol := filepath.VolumeName(p)
	sep := string(filepath.Separator)
	cur := vol + sep
	for _, part := range strings.Split(strings.TrimPrefix(p[len(vol):], sep), sep) {
		if part == "" {
			continue
		}
		cur = filepath.Join(cur, c.list(cur).spelling(part))
	}
	return cur
}

// list returns dir's listing, reading it if no one has yet. Callers that
// ask while it is being read wait for that read.
func (c *canon) list(dir string) *dirNames {
	c.mu.Lock()
	d := c.dirs[dir]
	if d == nil {
		d = &dirNames{}
		c.dirs[dir] = d
	}
	c.mu.Unlock()
	d.once.Do(func() {
		d.names = c.readDir(dir)
		d.set = make(map[string]struct{}, len(d.names))
		for _, n := range d.names {
			d.set[n] = struct{}{}
		}
	})
	return d
}

// spelling returns how part is stored in the directory: part itself when
// an entry has exactly that name, else the first entry equal to it ignoring
// case, else part unchanged.
func (d *dirNames) spelling(part string) string {
	if _, ok := d.set[part]; ok {
		return part
	}
	for _, n := range d.names {
		if strings.EqualFold(n, part) {
			return n
		}
	}
	return part
}

// scratchpadRE matches a Claude Code session scratchpad:
// <tmp>/claude-<uid>/<munged project>/<sessionId>/scratchpad/...
var scratchpadRE = regexp.MustCompile(`/claude-[0-9]+/[^/]+/([0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12})/scratchpad(?:/|$)`)

// scratchpadSession returns the session id in a Claude scratchpad path, or "".
func scratchpadSession(p string) string {
	if m := scratchpadRE.FindStringSubmatch(filepath.ToSlash(p)); m != nil {
		return m[1]
	}
	return ""
}

// agentDirRE matches the folder name of a worktree-isolated agent's worktree.
var agentDirRE = regexp.MustCompile(`^agent-([0-9a-f]+)$`)

// agentFromPath returns <agentId> when p is <repo>/.claude/worktrees/agent-<agentId>,
// the layout Claude Code uses for worktree-isolated agents (the folder
// name carries the same agentId the spawn returns), or "".
func agentFromPath(p string) string {
	p = filepath.Clean(p)
	parent := filepath.Dir(p)
	if filepath.Base(parent) != "worktrees" || filepath.Base(filepath.Dir(parent)) != ".claude" {
		return ""
	}
	if m := agentDirRE.FindStringSubmatch(filepath.Base(p)); m != nil {
		return m[1]
	}
	return ""
}
