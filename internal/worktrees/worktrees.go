// Package worktrees finds git repos and lists their worktrees with the state
// ai-manager shows and `aim prune` decides on: branch and HEAD, lock and
// prune flags, uncommitted and untracked counts, upstream state, unpushed
// commits and the last commit time. It is a library; it decides nothing.
//
// # Discovery
//
// Repos come from two places:
//
//   - A walk of the configured roots (config.Config.Roots). A directory is a
//     candidate when it holds a .git entry (directory or file) or looks like
//     a bare repo (a HEAD file plus objects and refs directories). The walk
//     goes at most MaxDepth levels below each root (default 3, so
//     <root>/org/group/repo is found). It
//     does not follow symlinks, skips SkipDirs (node_modules and similar,
//     .git, and .claude, whose worktrees are listed through their main
//     repo), does not enter bare repos, and keeps descending into ordinary
//     repos, so a repo nested inside another is found too.
//   - Paths the caller passes in, such as session cwds. These can be
//     anywhere (a Claude scratchpad under /private/tmp, a subdirectory of a
//     worktree) and are resolved with git's normal upward search.
//
// Every candidate is resolved with `git rev-parse --git-common-dir`: a linked
// worktree maps to its main repo, and repos are identified by their common
// git directory. Nothing is inferred from directory names. Submodules found
// by the walk are skipped, as part of their superproject.
//
// Bare repos are scanned only when the caller passes one in explicitly (its
// directory, or a path in one of its worktrees). A bare repo that only the
// walk found, directly or through one of its linked worktrees, is skipped
// and reported in Result.Errors as ErrBareSkipped, and no git command runs
// in it. Scanning means naming the repo with --git-dir, which would get
// around safe.bareRepository=explicit; the scanner only does that for repos
// a caller asked for.
//
// A path passed in is resolved by git's own discovery, never by guessing
// from its layout, so a working tree that happens to hold HEAD, objects and
// refs is still a working tree. --git-dir is used for it only when
// safe.bareRepository=explicit refused the path itself as a bare repo (not a
// directory inside one) and it has no .git entry. A bare result is refused
// (ErrEmbeddedBare) when any ancestor of its directory, up to the
// filesystem root, holds a .git entry, or when git says its parent is in a
// working tree, since a repo can commit such a directory. So a home
// directory kept under git makes every bare repo below it refused.
//
// # Listing
//
// Each repo's worktrees come from `git worktree list --porcelain -z` (git
// 2.36 or later), run against the repo, so worktrees outside every root are
// listed too. Upstream state comes from one `git for-each-ref` per repo,
// commit times from one `git rev-list --no-walk`, and remotes from one
// `git config --get-regexp`. Each worktree that exists gets one
// `git status --porcelain=v2 -z` for its dirty counts. Records may appear or
// disappear between the list and the status: a worktree that
// vanishes in between is reported Missing, not as an error.
//
// # SSH host aliases
//
// A remote on an SSH host that isn't GitHub's own, such as
// git@github-work:o/n.git (a Host block in ~/.ssh/config, the usual way to
// use two GitHub accounts), is resolved the way ssh would: `ssh -G -- <host>`,
// which prints the user's ssh config for the host and never connects. Its
// hostname line goes in Repo.HostNames, and the remote counts as GitHub's
// when that is github.com or ssh.github.com (Repo.GitHubRemote). Only a
// host that looks like a host name, on a remote whose path is owner/name,
// is asked about; each answer is kept for hostTTL, so a scan every few
// minutes doesn't run ssh again, and a failed or slow run (sshTimeout)
// leaves the host unresolved, which reads as not GitHub.
//
// # Read-only and hardened
//
// A scan runs only rev-parse, config (reads), worktree list, for-each-ref,
// rev-list and status, plus `ssh -G` for an alias as above. It never
// fetches, prunes, removes, locks or checks out anything. Because nothing
// is fetched, UpstreamOK means only that the local remote-tracking ref
// exists.
//
// For `aim prune` the Scanner also has a few checks (Ignored, ResolveCommit,
// CountNotIn, IsAncestor, RemoteHead, PrunePreview), which read only, and
// three writes, which only `aim prune --apply` calls: RemoveWorktree
// (`git worktree remove`, never --force), DeleteBranch (`git branch -D`,
// only while the branch is where it was checked) and Prune
// (`git worktree prune`). They run through the same hardened runner.
//
// It runs git in repos it merely found on disk, so it doesn't let their
// config run programs:
//   - every call starts with -c core.fsmonitor=false, -c
//     core.hooksPath=/dev/null, -c core.pager=cat, -c protocol.allow=never,
//     -c credential.helper= and --no-optional-locks, and runs with
//     GIT_OPTIONAL_LOCKS=0 (status never rewrites an index),
//     GIT_TERMINAL_PROMPT=0, GIT_NO_LAZY_FETCH=1 and an empty
//     GIT_ALLOW_PROTOCOL. The last three mean no transport can be reached,
//     so a partial clone's lazy fetch, ssh and core.sshCommand never run, on
//     every git version and whatever protocol.<name>.allow a repo sets;
//   - before each status, the worktree's filter drivers are listed with
//     `git config --get-regexp ^filter\.`, and status runs with each
//     driver's clean, smudge and process set to empty, so no filter runs. A
//     driver marked required then makes status fail, and that worktree's
//     counts are reported unknown with the reason. Ignored's status reuses
//     the list its worktree's scan read, so a prune check lists them once;
//   - status runs with --ignore-submodules=dirty, so it doesn't start git in
//     submodules, whose own config could define other filters. Changes inside a
//     submodule's working tree therefore don't count; a changed submodule
//     commit still does.
//
// System and global git config still apply (GIT_CONFIG_NOSYSTEM is left
// alone), so safe.directory and safe.bareRepository keep working.
//
// # Performance and errors
//
// At most Concurrency git processes run at once, across all repos, and
// every call has a Timeout. When git's output pipes stay open after it
// exits or is killed (another process holds a copy), a call waits at most
// 10 seconds more for them, then takes what git wrote and returns git's
// exit status. A failure is recorded on the repo (Repo.Err),
// the worktree (Worktree.Err) or the result (Result.Errors) it belongs to,
// and the rest of the scan carries on.
//
// # Paths
//
// Worktree and repo paths are canonical when they exist: symlinks resolved
// (/tmp is /private/tmp on macOS) and, on macOS, each component spelled as
// it is on disk, because git keeps whatever case a worktree was created
// with. Compare paths with SamePath or PathKey, which ignore case on macOS.
package worktrees

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/gravexa-llc/ai-manager/internal/config"
)

// Defaults for the zero Options fields.
const (
	DefaultMaxDepth    = 3
	DefaultConcurrency = 4
	DefaultTimeout     = 10 * time.Second
)

// DefaultSkipDirs are directory names the root walk never enters. .git is
// always skipped, whatever SkipDirs says.
var DefaultSkipDirs = []string{
	".git", ".claude",
	"node_modules", "bower_components", "vendor",
	".build", "Pods", "Carthage", "DerivedData",
	".gradle", ".venv", "venv", "__pycache__", ".tox",
	".terraform", ".next", ".nuxt", ".cache",
}

// Options configures a Scanner. Zero fields take the defaults above.
type Options struct {
	// Roots are the directories searched for repos.
	Roots []string
	// MaxDepth is how many levels below a root a repo can be found.
	MaxDepth int
	// SkipDirs are directory names the walk doesn't enter. Nil means
	// DefaultSkipDirs.
	SkipDirs []string
	// Concurrency is the most git processes run at once.
	Concurrency int
	// Timeout bounds each git call.
	Timeout time.Duration
	// Git is the git binary. Empty means "git" from PATH.
	Git string
	// SSH is the ssh binary that resolves SSH host aliases (`ssh -G`; see
	// the package comment). Empty means "ssh" from PATH. Tests set a fake.
	SSH string
}

// OptionsFromConfig returns Options for cfg's roots, with defaults for the
// rest.
func OptionsFromConfig(cfg config.Config) Options {
	return Options{Roots: slices.Clone(cfg.Roots)}
}

// Scanner discovers repos and scans their worktrees. It is safe for
// concurrent use; the concurrency limit is shared by all its calls.
type Scanner struct {
	opts Options
	sem  chan struct{}
	run  runFunc
	// readDir lists a directory for path canonicalization (tests count
	// the calls).
	readDir func(dir string) []string
	// hosts resolves SSH host aliases, with a cache (sshhost.go).
	hosts *hostResolver
}

func (s *Scanner) newCanon() *canon { return newCanon(s.readDir) }

// New returns a Scanner for opts.
func New(opts Options) *Scanner {
	if opts.MaxDepth <= 0 {
		opts.MaxDepth = DefaultMaxDepth
	}
	if opts.SkipDirs == nil {
		opts.SkipDirs = DefaultSkipDirs
	}
	if opts.Concurrency <= 0 {
		opts.Concurrency = DefaultConcurrency
	}
	if opts.Timeout <= 0 {
		opts.Timeout = DefaultTimeout
	}
	if opts.Git == "" {
		opts.Git = "git"
	}
	if opts.SSH == "" {
		opts.SSH = "ssh"
	}
	return &Scanner{
		opts: opts,
		sem:  make(chan struct{}, opts.Concurrency),
		run:  execGit(opts.Git),

		readDir: readDirNames,
		hosts:   newHostResolver(sshHostName(opts.SSH), time.Now),
	}
}

// Result is one scan.
type Result struct {
	// Repos are sorted by Path.
	Repos []Repo
	// Errors are discovery problems and skips, each an *fs.PathError: a
	// root that can't be read, a path passed in that doesn't exist or isn't
	// in a repo (errors.Is(err, ErrNotRepo)), a .git under a root that git
	// rejects, or a bare repo found only by the walk
	// (errors.Is(err, ErrBareSkipped)).
	Errors []error
}

// Repo is one repository and its worktrees.
type Repo struct {
	// Path is the main worktree, or the bare repo's directory. If the
	// listing failed it falls back to GitDir without a trailing /.git.
	Path string
	// GitDir is the repo's common git directory (canonical). It identifies
	// the repo: every worktree of a repo shares it.
	GitDir string
	// Bare is true for a bare repo. Its first worktree is the bare entry.
	Bare bool
	// Remotes maps each remote's name to its URL (the first one, if a
	// remote has several), from remote.<name>.url. Empty when the repo has
	// no remotes or they couldn't be read (see Err).
	Remotes map[string]string
	// HostNames maps each SSH host alias the remotes name (SSHAliasHost),
	// lower-case, to the HostName the user's ssh config gives it, as
	// `ssh -G` printed it (lower-case). A remote on an alias whose HostName
	// is GitHub's counts as a GitHub remote. A host ssh couldn't resolve is
	// left out; nil when no remote is on an alias.
	HostNames map[string]string
	// Worktrees are in git's order: the main worktree first.
	Worktrees []Worktree
	// Err says what couldn't be read. If the listing failed, Worktrees is
	// empty; otherwise the fields it names (upstream state, commit times,
	// remotes) are unknown.
	Err error
}

// Main returns the main worktree (for a bare repo, the bare entry), or nil
// if the listing failed.
func (r *Repo) Main() *Worktree {
	if len(r.Worktrees) == 0 {
		return nil
	}
	return &r.Worktrees[0]
}

// UpstreamState is a branch's upstream as git knows it locally.
type UpstreamState string

const (
	// UpstreamUnknown: not read (a bare entry, or for-each-ref failed).
	UpstreamUnknown UpstreamState = ""
	// UpstreamOK: an upstream is set and its remote-tracking ref exists.
	// aim never fetches, so this says nothing about the remote itself.
	UpstreamOK UpstreamState = "ok"
	// UpstreamGone: an upstream is set but its remote-tracking ref is gone,
	// which happens after `git fetch --prune` once the remote branch is
	// deleted.
	UpstreamGone UpstreamState = "gone"
	// UpstreamNone: no upstream is set, or HEAD is detached or unborn.
	UpstreamNone UpstreamState = "none"
)

// Worktree is one entry of a repo's worktree list, with its state.
type Worktree struct {
	// Path is the worktree's directory: canonical when it exists (see the
	// package doc), else RecordedPath cleaned.
	Path string
	// RecordedPath is the path exactly as git recorded it, possibly in a
	// different case from the disk. Pass this one back to git.
	RecordedPath string

	// Main is true for the repo's main worktree (git lists it first). It is
	// never a prune candidate.
	Main bool
	// Bare is true for a bare repo's own entry, which has no working tree.
	Bare bool
	// Head is the full SHA of HEAD; empty for a bare entry, all zeros on an
	// unborn branch.
	Head string
	// Branch is the short name of the checked-out branch; empty when
	// detached or bare.
	Branch   string
	Detached bool

	Locked bool
	// LockReason is the raw lock reason; it can be empty on a locked
	// worktree.
	LockReason string
	// Lock is set when LockReason is a Claude Code lock.
	Lock *ClaudeLock

	// Prunable is git's verdict that the worktree's admin data is stale;
	// clearing it takes `git worktree prune`, not `git worktree remove`. git
	// never reports a locked worktree as prunable, even when its directory
	// is gone: check Missing.
	Prunable       bool
	PrunableReason string
	// Missing is true when Path doesn't exist: prunable, being removed, or a
	// temp dir that was wiped.
	Missing bool

	// AgentID is the Claude agent the worktree belongs to: from Lock, or
	// else from a <repo>/.claude/worktrees/agent-<agentId> folder (the lock
	// is released when the agent finishes).
	AgentID string
	// ScratchpadSession is the Claude session id when Path is inside that
	// session's scratchpad (<tmp>/claude-<uid>/<project>/<sessionId>/scratchpad/).
	ScratchpadSession string

	// StatusRead is true when git status ran, so Uncommitted and Untracked
	// are real counts. It is false for bare, missing and prunable entries
	// and when status failed (see Err): treat those as unknown, not clean.
	StatusRead bool
	// Uncommitted counts tracked files with staged or unstaged changes,
	// including renames, conflicts and submodules whose commit changed.
	// Changes inside a submodule's working tree are not counted (status
	// doesn't start git in submodules; see the package doc).
	Uncommitted int
	// Untracked counts untracked entries as git status shows them: an
	// untracked directory counts once. Ignored files don't count.
	Untracked int

	// Upstream is the checked-out branch's upstream state.
	Upstream UpstreamState
	// UpstreamRef is the upstream's short name, e.g. origin/feature.
	UpstreamRef string
	// Unpushed is how many commits the branch is ahead of its upstream, and
	// Behind how many it is behind. Both are set only when Upstream is
	// UpstreamOK.
	Unpushed int
	Behind   int

	// LastCommitAt is the committer date of Head; zero if unknown.
	LastCommitAt time.Time

	// Err says what couldn't be read for this worktree.
	Err error

	// filters are the -c options that disabled the worktree's filter
	// drivers for the scan's status, kept (when filtersRead) so Ignored
	// doesn't run git config again for a worktree of the same scan.
	filters     []string
	filtersRead bool
}

// Dirty is the uncommitted plus untracked count. It is meaningful only when
// StatusRead is true.
func (w *Worktree) Dirty() int { return w.Uncommitted + w.Untracked }

// Scan discovers repos (see Discover) and scans each one. Discovery and
// the repo scans share one cache of directory listings, so each directory
// above a path is listed once per Scan.
func (s *Scanner) Scan(ctx context.Context, extra ...string) *Result {
	c := s.newCanon()
	dirs, errs := s.discover(ctx, c, extra)
	repos := s.scanRepos(ctx, c, dirs)
	sort.SliceStable(repos, func(i, j int) bool { return PathKey(repos[i].Path) < PathKey(repos[j].Path) })
	return &Result{Repos: repos, Errors: errs}
}

// ScanRepos scans the repos with the given common git directories (as
// Discover returns them) and returns them in the same order. aimd calls it
// with one directory when that repo's WatchPaths change. Each repo is named
// with --git-dir, which is explicit use: pass only repos that Discover
// returned or that you chose, never a bare repo that was merely found.
func (s *Scanner) ScanRepos(ctx context.Context, gitDirs []string) []Repo {
	return s.scanRepos(ctx, s.newCanon(), gitDirs)
}

// scanRepos is ScanRepos with the scan's canon.
func (s *Scanner) scanRepos(ctx context.Context, c *canon, gitDirs []string) []Repo {
	repos := make([]Repo, len(gitDirs))
	jobs := make(chan int)
	var wg sync.WaitGroup
	for range min(s.opts.Concurrency, len(gitDirs)) {
		wg.Go(func() {
			for i := range jobs {
				repos[i] = s.scanRepo(ctx, c, gitDirs[i])
			}
		})
	}
	for i := range gitDirs {
		jobs <- i
	}
	close(jobs)
	wg.Wait()
	return repos
}

// scanRepo lists one repo's worktrees and reads their state.
func (s *Scanner) scanRepo(ctx context.Context, c *canon, gitDir string) Repo {
	repo := Repo{GitDir: gitDir}
	out, err := s.git(ctx, gitDir, nil, gitDirArg(gitDir), "worktree", "list", "--porcelain", "-z")
	var records []record
	if err == nil {
		records, err = parsePorcelain(out, true)
	}
	if err == nil && len(records) == 0 {
		err = errors.New("git worktree list: no worktrees")
	}
	if err != nil {
		repo.Path = fallbackRepoPath(gitDir)
		repo.Err = err
		return repo
	}

	repo.Bare = records[0].Bare
	repo.Worktrees = make([]Worktree, len(records))
	for i, rec := range records {
		w := newWorktree(rec, i == 0)
		if p, err := c.path(w.RecordedPath); err == nil {
			w.Path = p
		} else if errors.Is(err, os.ErrNotExist) {
			w.Missing = true
		} else {
			w.Err = err
		}
		repo.Worktrees[i] = w
	}
	repo.Path = repo.Worktrees[0].Path

	// The repo-wide calls return maps, applied once every goroutine is done;
	// only the status goroutines write to worktrees, each to its own.
	heads := make([]string, len(records))
	for i, rec := range records {
		heads[i] = rec.Head
	}
	var (
		wg                            sync.WaitGroup
		branches                      map[string]branchInfo
		times                         map[string]time.Time
		remotes                       [][2]string
		refsErr, timesErr, remotesErr error
	)
	wg.Go(func() { branches, refsErr = s.branches(ctx, gitDir) })
	wg.Go(func() { times, timesErr = s.commitTimes(ctx, gitDir, heads) })
	wg.Go(func() {
		remotes, remotesErr = s.configEntries(ctx, gitDir, gitDir, nil, `^remote\..*\.url$`)
	})
	for i := range repo.Worktrees {
		w := &repo.Worktrees[i]
		if w.Bare || w.Missing || w.Prunable || w.Err != nil {
			continue
		}
		wg.Go(func() { s.readStatus(ctx, w) })
	}
	wg.Wait()

	for i := range repo.Worktrees {
		w := &repo.Worktrees[i]
		w.LastCommitAt = times[w.Head]
		if refsErr == nil {
			setUpstream(w, branches)
		}
	}
	repo.Remotes = remotesFrom(remotes)
	repo.HostNames = s.hostNames(ctx, repo.Remotes)
	repo.Err = errors.Join(refsErr, timesErr, remotesErr)
	return repo
}

// newWorktree builds a Worktree from a listing record, without touching the
// disk.
func newWorktree(rec record, main bool) Worktree {
	w := Worktree{
		Path:              filepath.Clean(rec.Path),
		RecordedPath:      rec.Path,
		Main:              main,
		Bare:              rec.Bare,
		Head:              rec.Head,
		Branch:            strings.TrimPrefix(rec.Branch, "refs/heads/"),
		Detached:          rec.Detached,
		Locked:            rec.Locked,
		LockReason:        rec.LockReason,
		Prunable:          rec.Prunable,
		PrunableReason:    rec.PrunableReason,
		ScratchpadSession: scratchpadSession(rec.Path),
	}
	if l, ok := ParseLockReason(rec.LockReason); ok && rec.Locked {
		w.Lock = &l
		w.AgentID = l.AgentID
	}
	if w.AgentID == "" {
		w.AgentID = agentFromPath(rec.Path)
	}
	return w
}

// fallbackRepoPath labels a repo whose listing failed.
func fallbackRepoPath(gitDir string) string {
	if filepath.Base(gitDir) == ".git" {
		return filepath.Dir(gitDir)
	}
	return gitDir
}

// branches reads every local branch's upstream with one for-each-ref.
func (s *Scanner) branches(ctx context.Context, gitDir string) (map[string]branchInfo, error) {
	out, err := s.git(ctx, gitDir, nil, gitDirArg(gitDir), "for-each-ref", refsFormat, "refs/heads")
	if err != nil {
		return nil, err
	}
	return parseRefs(out)
}

// setUpstream fills in w's upstream fields from its branch.
func setUpstream(w *Worktree, branches map[string]branchInfo) {
	switch {
	case w.Bare:
	case w.Branch == "":
		w.Upstream = UpstreamNone
	default:
		b, ok := branches[w.Branch]
		if !ok { // unborn, or deleted since the listing
			w.Upstream = UpstreamNone
			return
		}
		w.Upstream, w.UpstreamRef = b.State, b.Upstream
		if b.State == UpstreamOK {
			w.Unpushed, w.Behind = b.Ahead, b.Behind
		}
	}
}

// commitTimes reads the committer date of each head with one rev-list.
// --ignore-missing keeps one unreadable HEAD from failing the rest.
func (s *Scanner) commitTimes(ctx context.Context, gitDir string, heads []string) (map[string]time.Time, error) {
	args := []string{gitDirArg(gitDir), "rev-list", "--no-walk=unsorted", "--timestamp", "--ignore-missing"}
	seen := map[string]bool{}
	for _, h := range heads {
		if isSHA(h) && !seen[h] {
			seen[h] = true
			args = append(args, h)
		}
	}
	if len(seen) == 0 {
		return nil, nil
	}
	out, err := s.git(ctx, gitDir, nil, args...)
	if err != nil {
		return nil, err
	}
	return parseRevListTimestamps(out)
}

// isSHA reports whether s is a hex object name other than the all-zeros
// HEAD of an unborn branch.
func isSHA(s string) bool {
	if len(s) != 40 && len(s) != 64 {
		return false
	}
	zero := true
	for _, r := range s {
		switch {
		case r == '0':
		case r >= '1' && r <= '9', r >= 'a' && r <= 'f':
			zero = false
		default:
			return false
		}
	}
	return !zero
}

// readStatus fills in the dirty counts from git status in the worktree,
// with its filter drivers disabled (see the package doc). Any failure,
// including a required filter that can't run, leaves the counts unknown
// with the reason in Err.
func (s *Scanner) readStatus(ctx context.Context, w *Worktree) {
	env := ceilingEnv(w.Path)
	filters, err := s.filterOverrides(ctx, w.Path, env)
	var out []byte
	if err == nil {
		w.filters, w.filtersRead = filters, true
		args := append(slices.Clip(filters), "status", "--porcelain=v2", "-z",
			"--untracked-files=normal", "--ignore-submodules=dirty")
		out, err = s.git(ctx, w.Path, env, args...)
	}
	if err == nil {
		w.Uncommitted, w.Untracked, err = parseStatus(out, true)
	}
	if err != nil {
		// Removed since the listing: that is churn, not an error.
		if _, statErr := os.Stat(w.Path); errors.Is(statErr, os.ErrNotExist) {
			w.Missing = true
			return
		}
		w.Err = err
		return
	}
	w.StatusRead = true
}

// filterOverrides lists the filter drivers configured for the worktree at
// dir (system, global, repo and worktree config) and returns -c options that
// disable them for one git call.
func (s *Scanner) filterOverrides(ctx context.Context, dir string, env []string) ([]string, error) {
	entries, err := s.configEntries(ctx, dir, "", env, `^filter\.`)
	if err != nil {
		return nil, err
	}
	return filterOverrideArgs(filterDrivers(entries))
}

// Lookup returns the repo and worktree that contain path: the worktree with
// the longest Path (or RecordedPath) that is path or one of its parents, so
// a linked worktree under <repo>/.claude/worktrees wins over the main one.
// Paths compare by PathKey and are not resolved, so pass a canonical path
// (session cwds are). It returns nils when no worktree contains path.
func (r *Result) Lookup(path string) (*Repo, *Worktree) {
	var (
		bestRepo *Repo
		bestWt   *Worktree
		bestLen  = -1
	)
	for i := range r.Repos {
		repo := &r.Repos[i]
		for j := range repo.Worktrees {
			w := &repo.Worktrees[j]
			for _, p := range []string{w.Path, w.RecordedPath} {
				if p != "" && len(p) > bestLen && within(path, p) {
					bestRepo, bestWt, bestLen = repo, w, len(p)
				}
			}
		}
	}
	return bestRepo, bestWt
}

// WatchPaths returns the directories whose changes mean the repo's worktree
// list may have changed: <GitDir>/worktrees when it exists, else GitDir
// itself, where git creates worktrees/ for the first linked worktree. Lock
// and HEAD changes happen in subdirectories of worktrees/, and commits in
// refs/, so a periodic rescan is still needed for those.
func (r *Repo) WatchPaths() []string {
	if r.GitDir == "" {
		return nil
	}
	wt := filepath.Join(r.GitDir, "worktrees")
	if fi, err := os.Stat(wt); err == nil && fi.IsDir() {
		return []string{wt}
	}
	return []string{r.GitDir}
}

// WatchPaths returns every repo's WatchPaths.
func (r *Result) WatchPaths() []string {
	var out []string
	for i := range r.Repos {
		out = append(out, r.Repos[i].WatchPaths()...)
	}
	return out
}
