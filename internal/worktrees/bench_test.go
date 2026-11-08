//go:build unix

package worktrees

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"
)

// The scan benchmarks run on a temp tree of AIM_BENCH_REPOS repos (default
// 60), each with two linked worktrees under .claude/worktrees as
// worktree-isolated agents make them, inside a directory that also holds
// AIM_BENCH_CROWD empty siblings (default 5000), like a busy $TMPDIR or
// home folder. They report git calls, directory listings and this
// process's own CPU time (not git's) per scan.
//
//	go test ./internal/worktrees -run '^$' -bench Scan -benchtime 5x

func benchEnv(name string, def int) int {
	if n, err := strconv.Atoi(os.Getenv(name)); err == nil && n >= 0 {
		return n
	}
	return def
}

// buildBench builds the benchmark tree and returns its root and its number
// of repos.
func buildBench(b *testing.B) (string, int) {
	b.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		b.Skip("git not on PATH")
	}
	repos, crowd := benchEnv("AIM_BENCH_REPOS", 60), benchEnv("AIM_BENCH_CROWD", 5000)
	base := tempDir(b)
	parent := filepath.Join(base, "crowded")
	mkdir(b, parent)
	for i := range crowd {
		if err := os.Mkdir(filepath.Join(parent, fmt.Sprintf("sibling-%05d", i)), 0o755); err != nil {
			b.Fatal(err)
		}
	}
	tmpl := filepath.Join(base, "template")
	newRepo(b, tmpl, d1)
	root := filepath.Join(parent, "root")
	for i := range repos {
		repo := filepath.Join(root, fmt.Sprintf("org%d", i%5), fmt.Sprintf("repo%02d", i))
		mkdir(b, filepath.Dir(repo))
		if out, err := exec.Command("cp", "-R", tmpl, repo).CombinedOutput(); err != nil {
			b.Fatalf("cp: %v\n%s", err, out)
		}
		for j := range 2 {
			wt := filepath.Join(repo, ".claude", "worktrees", fmt.Sprintf("agent-%08x", 2*i+j))
			gitT(b, repo, "worktree", "add", "-q", "-b", fmt.Sprintf("work-%d", j), wt)
		}
	}
	return root, repos
}

// countCalls counts s's git calls and directory listings.
func countCalls(s *Scanner) (gits, lists *atomic.Int64) {
	gits, lists = new(atomic.Int64), new(atomic.Int64)
	run, readDir := s.run, s.readDir
	s.run = func(ctx context.Context, dir string, env []string, args ...string) ([]byte, error) {
		gits.Add(1)
		return run(ctx, dir, env, args...)
	}
	s.readDir = func(dir string) []string {
		lists.Add(1)
		return readDir(dir)
	}
	return gits, lists
}

// selfCPU returns this process's user plus system CPU time so far.
func selfCPU(b *testing.B) time.Duration {
	var ru syscall.Rusage
	if err := syscall.Getrusage(syscall.RUSAGE_SELF, &ru); err != nil {
		b.Fatal(err)
	}
	return time.Duration(ru.Utime.Nano() + ru.Stime.Nano())
}

// report adds the per-scan metrics.
func report(b *testing.B, n int, gits, lists *atomic.Int64, cpu time.Duration) {
	b.ReportMetric(float64(gits.Load())/float64(n), "git/op")
	b.ReportMetric(float64(lists.Load())/float64(n), "readdir/op")
	b.ReportMetric(float64(cpu.Microseconds())/1000/float64(n), "self-cpu-ms/op")
}

func checkBench(b *testing.B, res *Result, repos int) {
	b.Helper()
	if len(res.Repos) != repos || len(res.Errors) != 0 {
		b.Fatalf("%d repos (want %d), errors %v", len(res.Repos), repos, res.Errors)
	}
	for _, r := range res.Repos {
		if r.Err != nil || len(r.Worktrees) != 3 {
			b.Fatalf("%s: %d worktrees, err %v", r.Path, len(r.Worktrees), r.Err)
		}
		for _, w := range r.Worktrees {
			if !w.StatusRead {
				b.Fatalf("%s: status not read: %v", w.Path, w.Err)
			}
		}
	}
}

// BenchmarkScan is one Scan of the tree, as aimd runs it.
func BenchmarkScan(b *testing.B) {
	root, repos := buildBench(b)
	s := New(Options{Roots: []string{root}})
	gits, lists := countCalls(s)
	ctx := context.Background()
	n, cpu := 0, selfCPU(b)
	for b.Loop() {
		checkBench(b, s.Scan(ctx), repos)
		n++
	}
	report(b, n, gits, lists, selfCPU(b)-cpu)
}

// BenchmarkScanIgnored is a Scan plus Ignored on every linked worktree,
// as `aim prune` reads them.
func BenchmarkScanIgnored(b *testing.B) {
	root, repos := buildBench(b)
	s := New(Options{Roots: []string{root}})
	gits, lists := countCalls(s)
	ctx := context.Background()
	n, cpu := 0, selfCPU(b)
	for b.Loop() {
		res := s.Scan(ctx)
		checkBench(b, res, repos)
		var wg sync.WaitGroup
		for i := range res.Repos {
			for j := range res.Repos[i].Worktrees {
				w := &res.Repos[i].Worktrees[j]
				if w.Main {
					continue
				}
				wg.Go(func() {
					if _, err := s.Ignored(ctx, w); err != nil {
						b.Error(err)
					}
				})
			}
		}
		wg.Wait()
		n++
	}
	report(b, n, gits, lists, selfCPU(b)-cpu)
}
