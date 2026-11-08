package prune

import (
	"fmt"
	"path"
	"strings"
)

// Matcher says whether an ignored path is rebuildable, from the
// [prune] rebuildable list (see config.Prune for the pattern rules).
type Matcher struct {
	pats []pattern
}

type pattern struct {
	comps    []string // path components, each a path.Match pattern
	anchored bool     // must match from the worktree root
	dirOnly  bool     // the last component must be a directory
}

// NewMatcher compiles a rebuildable list. Entries are validated by
// config.Prune.RebuildablePaths; an entry that still doesn't compile is an
// error.
func NewMatcher(entries []string) (*Matcher, error) {
	m := &Matcher{}
	for _, e := range entries {
		p := pattern{dirOnly: strings.HasSuffix(e, "/"), anchored: strings.HasPrefix(e, "/")}
		body := strings.Trim(e, "/")
		if strings.Contains(body, "/") {
			p.anchored = true
		}
		if rest, ok := strings.CutPrefix(body, "**/"); ok {
			p.anchored, body = false, rest
		}
		if body == "" {
			return nil, fmt.Errorf("rebuildable entry %q: empty", e)
		}
		p.comps = strings.Split(body, "/")
		for _, c := range p.comps {
			if _, err := path.Match(c, ""); err != nil {
				return nil, fmt.Errorf("rebuildable entry %q: %w", e, err)
			}
		}
		m.pats = append(m.pats, p)
	}
	return m, nil
}

// Match reports whether p, a path relative to the worktree as
// `git status` names it (with a trailing "/" for a directory), is inside a
// rebuildable path or is one.
func (m *Matcher) Match(p string) bool {
	isDir := strings.HasSuffix(p, "/")
	parts := strings.Split(strings.Trim(p, "/"), "/")
	for _, pat := range m.pats {
		if pat.match(parts, isDir) {
			return true
		}
	}
	return false
}

func (pat pattern) match(parts []string, isDir bool) bool {
	n := len(pat.comps)
	for start := 0; start+n <= len(parts); start++ {
		if pat.anchored && start > 0 {
			return false
		}
		ok := true
		for i, c := range pat.comps {
			if m, _ := path.Match(c, parts[start+i]); !m {
				ok = false
				break
			}
		}
		if !ok {
			continue
		}
		// The matched path is a directory when more components follow it,
		// or when it is the whole path and git listed it as one.
		if pat.dirOnly && start+n == len(parts) && !isDir {
			continue
		}
		return true
	}
	return false
}
