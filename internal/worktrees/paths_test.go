package worktrees

import (
	"context"
	"fmt"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

// fakeDirs is a readDir over a fixed tree that counts the listings of each
// directory.
type fakeDirs struct {
	tree  map[string][]string
	mu    sync.Mutex
	reads map[string]int
}

func (f *fakeDirs) readDir(dir string) []string {
	f.mu.Lock()
	f.reads[dir]++
	f.mu.Unlock()
	time.Sleep(time.Millisecond) // let concurrent callers pile up
	return f.tree[dir]
}

func TestCanonFixCase(t *testing.T) {
	f := &fakeDirs{reads: map[string]int{}, tree: map[string][]string{
		"/":                   {"Users", "tmp"},
		"/Users":              {"Shared", "Sam"},
		"/Users/Sam":          {"Projects", "notes"},
		"/Users/Sam/Projects": {"Foo", "foo", "Bar"},
	}}
	c := newCanon(f.readDir)
	for _, tc := range []struct{ in, want string }{
		{"/Users/Sam/Projects/Bar", "/Users/Sam/Projects/Bar"},
		{"/users/sam/projects/bar", "/Users/Sam/Projects/Bar"},
		// An exact entry wins over one that differs only in case (a
		// case-sensitive volume can hold both); otherwise the first in
		// listing order.
		{"/Users/Sam/Projects/foo", "/Users/Sam/Projects/foo"},
		{"/Users/Sam/Projects/FOO", "/Users/Sam/Projects/Foo"},
		// No matching entry, or a directory that can't be read: kept as given.
		{"/Users/Sam/Projects/Baz/Qux", "/Users/Sam/Projects/Baz/Qux"},
		{"/TMP/x", "/tmp/x"},
	} {
		if got := c.fixCase(tc.in); got != tc.want {
			t.Errorf("fixCase(%s) = %s, want %s", tc.in, got, tc.want)
		}
	}
	for dir, n := range f.reads {
		if n != 1 {
			t.Errorf("%s listed %d times", dir, n)
		}
	}
}

// Many goroutines canonicalizing paths under the same directories at once
// list each directory once: the others wait for that listing.
func TestCanonListsOnceConcurrently(t *testing.T) {
	tree := map[string][]string{"/": {"Crowd"}, "/Crowd": nil}
	for i := range 200 {
		name := fmt.Sprintf("Repo%03d", i)
		tree["/Crowd"] = append(tree["/Crowd"], name)
		tree["/Crowd/"+name] = []string{".git"}
	}
	f := &fakeDirs{tree: tree, reads: map[string]int{}}
	c := newCanon(f.readDir)
	var wg sync.WaitGroup
	for i := range 200 {
		for range 3 {
			wg.Go(func() {
				in := fmt.Sprintf("/crowd/repo%03d/.git", i)
				if got, want := c.fixCase(in), fmt.Sprintf("/Crowd/Repo%03d/.git", i); got != want {
					t.Errorf("fixCase(%s) = %s, want %s", in, got, want)
				}
			})
		}
	}
	wg.Wait()
	if len(f.reads) != len(tree) {
		t.Errorf("%d directories listed, want %d", len(f.reads), len(tree))
	}
	for dir, n := range f.reads {
		if n != 1 {
			t.Errorf("%s listed %d times", dir, n)
		}
	}
}

// One Scan lists each directory above its paths once, though discovery and
// the repo scans both canonicalize them; the next Scan lists them again.
func TestScanListsEachDirectoryOnce(t *testing.T) {
	if !caseInsensitive {
		t.Skip("paths are only respelled on case-insensitive systems")
	}
	parallel(t)
	requireGit(t)
	base := tempDir(t)
	crowd := filepath.Join(base, "Crowd")
	for i := range 50 {
		mkdir(t, filepath.Join(crowd, fmt.Sprintf("sibling-%02d", i)))
	}
	root := filepath.Join(crowd, "Root")
	var repos []string
	for _, name := range []string{"a", "b", "c"} {
		repo := filepath.Join(root, "org", name)
		newRepo(t, repo, d1)
		for _, wt := range []string{"agent-0a", "agent-0b"} {
			gitT(t, repo, "worktree", "add", "-q", "-b", wt, filepath.Join(repo, ".claude", "worktrees", wt))
		}
		repos = append(repos, repo)
	}

	s := New(Options{Roots: []string{root}})
	var (
		mu    sync.Mutex
		reads map[string]int
	)
	s.readDir = func(dir string) []string {
		mu.Lock()
		reads[dir]++
		mu.Unlock()
		return readDirNames(dir)
	}
	for scan := range 2 {
		mu.Lock()
		reads = map[string]int{}
		mu.Unlock()
		// A session cwd inside a worktree as well as the root walk.
		res := s.Scan(context.Background(), filepath.Join(repos[0], ".claude", "worktrees", "agent-0a"))
		if len(res.Repos) != 3 || len(res.Errors) != 0 {
			t.Fatalf("scan %d: repos %v errors %v", scan, repoPaths(res), res.Errors)
		}
		mu.Lock()
		if reads[crowd] != 1 || reads[root] != 1 {
			t.Errorf("scan %d: %s listed %d times, %s %d times", scan, crowd, reads[crowd], root, reads[root])
		}
		for dir, n := range reads {
			if n != 1 {
				t.Errorf("scan %d: %s listed %d times", scan, dir, n)
			}
		}
		mu.Unlock()
	}
}
