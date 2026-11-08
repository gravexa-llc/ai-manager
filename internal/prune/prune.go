// Package prune decides which worktrees `aim prune` may remove, and removes
// them. `aim ls` shows the same verdicts. The rules:
//
// A branch worktree is safe to remove when all of these hold:
//
//  1. git status read it (an unknown status, as in git-lfs repos, is not
//     clean), it has no uncommitted or untracked files, and every ignored
//     path in it is rebuildable (config [prune] rebuildable): `git worktree
//     remove` deletes ignored files without asking;
//  2. no live session has its cwd in it, and it isn't inside a live
//     session's scratchpad;
//  3. its repo has a GitHub remote, no same-repo PR for its branch is open,
//     and the newest one is merged (or closed without merging, which only
//     --include-closed accepts);
//  4. (rule 4′) `git rev-list --count <tip> --not <PR head> <remote>/<base>`
//     is 0. A PR head that isn't in the local repo makes the answer
//     unknown, which is not safe. A missing or stale <remote>/<base> can
//     only make it "not safe".
//
// A detached worktree ("scratch") is safe when rules 1–2 hold, its repo has
// a GitHub remote, and HEAD is reachable from <remote>/<default branch> or
// from the base of a PR on one of the repo's branches.
//
// Main checkouts are never candidates. Locked worktrees are never removed;
// a Claude Code lock is reported as live or stale from its lead's pid and
// start time. Prunable records (git says the folder or its .git file is
// gone) are cleared with `git worktree prune`, repo by repo, and only when
// prune would clear nothing that wasn't listed.
//
// Anything that can't be read (GitHub stale or skipped for the rate-limit
// budget, the session list, a git call) makes the answer unknown, and
// unknown is never safe.
package prune

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/gravexa-llc/ai-manager/internal/github"
	"github.com/gravexa-llc/ai-manager/internal/sessions"
	"github.com/gravexa-llc/ai-manager/internal/worktrees"
)

// Category is where a worktree lands in `aim prune`'s report.
type Category string

// Categories.
const (
	Main       Category = "main"        // main checkout or bare repo: never a candidate
	Safe       Category = "safe"        // branch worktree whose PR merged
	SafeClosed Category = "safe-closed" // as Safe, but the PR was closed without merging
	Scratch    Category = "scratch"     // detached worktree whose HEAD is on the remote
	Locked     Category = "locked"      // never removed
	Prunable   Category = "prunable"    // git record whose folder or .git file is gone
	NotSafe    Category = "not-safe"
)

// Removable reports whether `aim prune --apply` removes worktrees in c.
// SafeClosed needs --include-closed.
func (c Category) Removable(includeClosed bool) bool {
	return c == Safe || c == Scratch || (c == SafeClosed && includeClosed)
}

// PRSource is the part of the GitHub client the checks use.
// *github.Client implements it.
type PRSource interface {
	BranchPRs(ctx context.Context, repos []github.RepoBranches) github.BranchPRsResult
	Budget() github.Budget
}

// SessionLister lists Claude Code sessions. sessions.Lister implements it.
type SessionLister interface {
	List(ctx context.Context) (sessions.Snapshot, error)
}

// prsRead is how many PRs per branch github.Client.BranchPRs reads (newest
// first). A branch with more may have an older PR still open.
const prsRead = 5

// Checker evaluates worktrees against the rules. Scanner is required; a nil
// GitHub, Sessions or Procs makes what it would answer unknown.
type Checker struct {
	Scanner  *worktrees.Scanner
	GitHub   PRSource
	Sessions SessionLister
	Procs    ProcessTable
	// Rebuildable matches ignored paths that are fine to lose.
	Rebuildable *Matcher
}

// Options select what Evaluate looks at.
type Options struct {
	// Repo, when set, limits the report to matching repos: a path in the
	// repo (scanned even when it is outside the roots), the repo
	// directory's name, or the GitHub owner/name (any case).
	Repo string
	// Prior, when set, supplies the GitHub answers instead of asking again
	// (the re-check before --apply removes anything).
	Prior *Report
}

// Report is one evaluation.
type Report struct {
	// Repos are sorted by path.
	Repos []*RepoReport
	// SessionsErr is why live sessions couldn't be listed; nil when they
	// were. While it is set, rule 2 is unknown for every candidate.
	SessionsErr error
	// LiveSessions is how many live sessions were checked.
	LiveSessions int
	// GitHubSkipped says why GitHub wasn't read ("" when it was).
	GitHubSkipped string
	// GitHubQueries and GitHubCost are what reading GitHub took.
	GitHubQueries, GitHubCost int
	// Warnings are discovery problems (unreadable roots, skipped bare
	// repos) and other things worth a line.
	Warnings []string

	prs map[string]branchAnswer // prKey → answer, reused by Options.Prior
}

// RepoReport is one repo.
type RepoReport struct {
	Repo *worktrees.Repo
	// Remote is the remote that names the repo on GitHub, "" if none does.
	Remote string
	// GitHub is the repo's owner/name: as GitHub answered when it did,
	// else as the remote names it. "" without a GitHub remote.
	GitHub string
	// DefaultBranch is GitHub's default branch, or the one
	// refs/remotes/<remote>/HEAD names; "" if neither is known.
	DefaultBranch string
	// GitHubErr is why GitHub couldn't answer for this repo.
	GitHubErr error
	Items     []*Item

	owner, name  string
	localDefault string           // the branch refs/remotes/<remote>/HEAD names
	bases        []string         // base branches of the repo's PRs, for detached HEADs
	remoteRefs   map[string]error // remote-tracking ref → ResolveCommit's error (nil: it exists)
}

// Item is one worktree's verdict.
type Item struct {
	Repo     *RepoReport
	Worktree *worktrees.Worktree

	Category Category
	// Reasons explain the verdict: for NotSafe, every rule that failed or
	// couldn't be checked; for the safe categories, one line saying why;
	// for Locked and Prunable, git's state.
	Reasons []string

	// Ignored is set for candidates (not main, locked or prunable).
	Ignored *Ignored
	// PR is the PR the verdict used, or the open one that blocks it; nil
	// when the branch has none or GitHub couldn't say.
	PR *github.PR
	// PRUnknown is true when GitHub couldn't be read for this branch.
	PRUnknown bool
	// Lock is set for locked worktrees.
	Lock *Lock
	// ReachableFrom is the remote-tracking ref holding a detached HEAD,
	// e.g. "origin/main".
	ReachableFrom string
}

// Ignored is what `git worktree remove` would delete without asking.
type Ignored struct {
	// AtRisk are the ignored paths outside every rebuildable path.
	AtRisk []string
	// Rebuildable counts the ignored paths that are fine to lose.
	Rebuildable int
	// Err is why they couldn't be listed.
	Err error
}

// LockState says whether a lock's owner is still running.
type LockState string

// Lock states.
const (
	LockLive    LockState = "live"
	LockStale   LockState = "stale"
	LockUnknown LockState = "unknown" // not a Claude lock, or the pid couldn't be checked
)

// Lock is a locked worktree's owner.
type Lock struct {
	State   LockState
	AgentID string
	PID     int
	// Detail says what was found, e.g. "pid 123 isn't running".
	Detail string
}

// branchAnswer is GitHub's answer for one branch.
type branchAnswer struct {
	prs github.BranchPRs
	err error
}

func prKey(owner, name, branch string) string {
	return strings.ToLower(owner+"/"+name) + "\x00" + branch
}

// Evaluate scans the worktrees and judges each one.
func (c *Checker) Evaluate(ctx context.Context, opts Options) (*Report, error) {
	rep := &Report{}

	// Live sessions, whose cwds are also repos to scan.
	var live []sessions.Session
	if c.Sessions == nil {
		rep.SessionsErr = errors.New("no session lister")
	} else if snap, err := c.Sessions.List(ctx); err != nil {
		rep.SessionsErr = err
	} else {
		for _, s := range snap.Sessions {
			if s.Active() {
				live = append(live, s)
			}
		}
	}
	rep.LiveSessions = len(live)

	var extra []string
	sessionPaths := map[string]bool{}
	for _, s := range live {
		if s.Cwd != "" {
			extra = append(extra, s.Cwd)
			sessionPaths[worktrees.PathKey(absPath(s.Cwd))] = true
		}
	}
	// Find the repos. --repo given as a path names one repo, wherever it
	// is; given as a directory name it narrows what is scanned (the scan,
	// not discovery, is what costs).
	var dirs []string
	var repoDir string // the repo --repo names by path
	if opts.Repo != "" {
		if _, err := os.Stat(opts.Repo); err == nil {
			gd, err := c.Scanner.CommonDir(ctx, opts.Repo)
			if err != nil {
				return nil, fmt.Errorf("--repo %s: %w", opts.Repo, err)
			}
			repoDir, dirs = gd, []string{gd}
		}
	}
	if repoDir == "" {
		var errs []error
		dirs, errs = c.Scanner.Discover(ctx, extra...)
		for _, err := range errs {
			var pe *fs.PathError
			if errors.As(err, &pe) && sessionPaths[worktrees.PathKey(pe.Path)] {
				continue // a session cwd that isn't a repo
			}
			rep.Warnings = append(rep.Warnings, err.Error())
		}
		if opts.Repo != "" && !strings.Contains(opts.Repo, "/") {
			dirs = slices.DeleteFunc(dirs, func(gd string) bool {
				return !strings.EqualFold(filepath.Base(repoPathOf(gd)), opts.Repo)
			})
		}
	}
	res := &worktrees.Result{Repos: c.Scanner.ScanRepos(ctx, dirs)}

	// Pick the repos.
	for i := range res.Repos {
		repo := &res.Repos[i]
		rr := &RepoReport{Repo: repo}
		if remote, owner, name, ok := repo.GitHubRemote(); ok {
			rr.Remote, rr.owner, rr.name = remote, owner, name
			rr.GitHub = owner + "/" + name
		}
		if opts.Repo != "" && !matchRepo(rr, opts.Repo, repoDir) {
			continue
		}
		rep.Repos = append(rep.Repos, rr)
	}
	if opts.Repo != "" && len(rep.Repos) == 0 {
		return nil, fmt.Errorf("--repo %s: no repo matches", opts.Repo)
	}

	// The local default branch first (GitHub isn't asked about a main
	// checkout on it), then the ignored-file checks run while GitHub is
	// read.
	var wg sync.WaitGroup
	for _, rr := range rep.Repos {
		if rr.Remote != "" {
			wg.Go(func() {
				if b, err := c.Scanner.RemoteHead(ctx, rr.Repo.GitDir, rr.Remote); err == nil {
					rr.localDefault = b
				}
			})
		}
	}
	wg.Wait()
	for _, rr := range rep.Repos {
		for i := range rr.Repo.Worktrees {
			it := &Item{Repo: rr, Worktree: &rr.Repo.Worktrees[i]}
			rr.Items = append(rr.Items, it)
			if candidate(it.Worktree) {
				wg.Go(func() { it.Ignored = c.ignored(ctx, it.Worktree) })
			}
		}
	}
	c.readGitHub(ctx, rep, opts.Prior)
	wg.Wait()

	// In use: live sessions' cwds and scratchpads (rule 2).
	inUse := map[*worktrees.Worktree][]string{}
	for _, s := range live {
		if s.Cwd == "" {
			continue
		}
		paths := []string{s.Cwd}
		if r, err := filepath.EvalSymlinks(s.Cwd); err == nil && r != s.Cwd {
			paths = append(paths, r)
		}
		for _, p := range paths {
			if _, w := res.Lookup(p); w != nil {
				if label := "cwd of " + sessionLabel(s); !slices.Contains(inUse[w], label) {
					inUse[w] = append(inUse[w], label)
				}
				break
			}
		}
	}
	scratchpads := map[string]string{}
	for _, s := range live {
		if s.SessionID != "" {
			scratchpads[strings.ToLower(s.SessionID)] = sessionLabel(s)
		}
	}

	for _, rr := range rep.Repos {
		c.prepareRepo(ctx, rr)
		for _, it := range rr.Items {
			w := it.Worktree
			wg.Go(func() {
				var use []string
				for _, u := range inUse[w] {
					use = append(use, "in use: "+u)
				}
				if s := scratchpads[strings.ToLower(w.ScratchpadSession)]; w.ScratchpadSession != "" && s != "" {
					use = append(use, "in the scratchpad of live "+s)
				}
				c.judge(ctx, rep, it, use)
			})
		}
	}
	wg.Wait()

	sort.SliceStable(rep.Repos, func(i, j int) bool {
		return worktrees.PathKey(rep.Repos[i].Repo.Path) < worktrees.PathKey(rep.Repos[j].Repo.Path)
	})
	return rep, nil
}

// repoPathOf guesses a repo's directory from its common git directory, the
// way the scanner labels it before listing: <repo>/.git → <repo>, a bare
// repo's directory as is. Only used to narrow a scan by name; matchRepo
// checks the real path afterwards.
func repoPathOf(gitDir string) string {
	if filepath.Base(gitDir) == ".git" {
		return filepath.Dir(gitDir)
	}
	return gitDir
}

// matchRepo reports whether rr is the repo --repo names.
func matchRepo(rr *RepoReport, sel, gitDir string) bool {
	if gitDir != "" {
		return worktrees.SamePath(rr.Repo.GitDir, gitDir)
	}
	if strings.Contains(sel, "/") {
		return rr.GitHub != "" && strings.EqualFold(rr.GitHub, sel)
	}
	return strings.EqualFold(filepath.Base(rr.Repo.Path), sel)
}

// readGitHub asks GitHub for the PRs of every branch with a worktree in a
// repo with a GitHub remote, in one BranchPRs call, unless the budget says
// stop. With prior set it reuses that report's answers instead.
func (c *Checker) readGitHub(ctx context.Context, rep *Report, prior *Report) {
	if prior != nil {
		rep.prs = prior.prs
		rep.GitHubSkipped = prior.GitHubSkipped
		for _, rr := range rep.Repos {
			for _, p := range prior.Repos {
				if worktrees.SamePath(p.Repo.GitDir, rr.Repo.GitDir) {
					rr.GitHub, rr.DefaultBranch, rr.GitHubErr = p.GitHub, p.DefaultBranch, p.GitHubErr
					rr.bases = p.bases
				}
			}
		}
		return
	}
	rep.prs = map[string]branchAnswer{}
	var reqs []github.RepoBranches
	for _, rr := range rep.Repos {
		if rr.Remote == "" {
			continue
		}
		req := github.RepoBranches{Owner: rr.owner, Name: rr.name}
		for _, w := range rr.Repo.Worktrees {
			// A main checkout on the default branch has no PR worth a
			// query slot.
			if w.Branch != "" && !(w.Main && w.Branch == rr.localDefault) {
				req.Branches = append(req.Branches, w.Branch)
			}
		}
		reqs = append(reqs, req)
	}
	if len(reqs) == 0 {
		return
	}
	switch {
	case c.GitHub == nil:
		rep.GitHubSkipped = "no GitHub client"
		return
	default:
		if b := c.GitHub.Budget(); b.Verdict == github.BudgetStop {
			rep.GitHubSkipped = fmt.Sprintf("the GitHub rate-limit budget is low (%d left; resumes %s)",
				b.Remaining, b.ResumeAt.Local().Format("15:04"))
			return
		}
	}
	res := c.GitHub.BranchPRs(ctx, reqs)
	rep.GitHubQueries, rep.GitHubCost = res.Queries, res.Cost
	byKey := map[string]*github.RepoPRs{}
	for i := range res.Repos {
		r := &res.Repos[i]
		byKey[strings.ToLower(r.Owner+"/"+r.Name)] = r
		for _, b := range r.Branches {
			rep.prs[prKey(r.Owner, r.Name, b.Branch)] = branchAnswer{prs: b, err: b.Err}
		}
	}
	for _, rr := range rep.Repos {
		r := byKey[strings.ToLower(rr.owner+"/"+rr.name)]
		if r == nil {
			continue
		}
		if r.Repo != "" {
			rr.GitHub = r.Repo
		}
		rr.DefaultBranch = r.DefaultBranch
		rr.GitHubErr = r.Err
		for _, b := range r.Branches {
			for _, pr := range b.PRs {
				if pr.BaseRef != "" && !slices.Contains(rr.bases, pr.BaseRef) {
					rr.bases = append(rr.bases, pr.BaseRef)
				}
			}
		}
	}
}

// prepareRepo reads what every worktree of the repo shares, once: the
// default branch from refs/remotes/<remote>/HEAD when GitHub didn't name
// one (detached worktrees need it), and which of <remote>/<default> and
// <remote>/<PR base> exist locally.
func (c *Checker) prepareRepo(ctx context.Context, rr *RepoReport) {
	if rr.Remote == "" {
		return
	}
	if rr.DefaultBranch == "" {
		rr.DefaultBranch = rr.localDefault
	}
	rr.remoteRefs = map[string]error{}
	for _, b := range append([]string{rr.DefaultBranch}, rr.bases...) {
		if b == "" {
			continue
		}
		ref := "refs/remotes/" + rr.Remote + "/" + b
		if _, done := rr.remoteRefs[ref]; !done {
			_, rr.remoteRefs[ref] = c.Scanner.ResolveCommit(ctx, rr.Repo.GitDir, ref)
		}
	}
}

// resolveRemoteRef says whether a remote-tracking ref exists, from the
// repo's cache when prepareRepo read it.
func (c *Checker) resolveRemoteRef(ctx context.Context, rr *RepoReport, ref string) error {
	if err, ok := rr.remoteRefs[ref]; ok {
		return err
	}
	_, err := c.Scanner.ResolveCommit(ctx, rr.Repo.GitDir, ref)
	return err
}

// judge sets it's category and reasons.
func (c *Checker) judge(ctx context.Context, rep *Report, it *Item, inUse []string) {
	w, rr := it.Worktree, it.Repo
	if w.Branch != "" && rr.Remote != "" {
		c.attachPR(rep, it)
	}
	switch {
	case w.Main || w.Bare:
		it.Category = Main
		if w.Bare {
			it.Reasons = []string{"bare repo"}
		} else {
			it.Reasons = []string{"main checkout"}
		}
		return
	case w.Locked:
		it.Category = Locked
		it.Lock = c.checkLock(ctx, w)
		it.Reasons = []string{it.Lock.Detail}
		return
	case w.Prunable:
		it.Category = Prunable
		it.Reasons = []string{w.PrunableReason}
		if !w.Missing {
			it.Reasons = append(it.Reasons, "the folder stays; prune clears only git's record of it")
		}
		return
	case w.Missing:
		it.Category = NotSafe
		it.Reasons = []string{"folder missing, but git doesn't call it prunable"}
		return
	}

	var reasons []string
	// Rule 1.
	if !w.StatusRead {
		r := "status unknown"
		if w.Err != nil {
			r += ": " + oneLine(w.Err)
		}
		reasons = append(reasons, r)
	} else {
		if w.Uncommitted > 0 {
			reasons = append(reasons, plural(w.Uncommitted, "uncommitted file"))
		}
		if w.Untracked > 0 {
			reasons = append(reasons, plural(w.Untracked, "untracked file"))
		}
	}
	if it.Ignored == nil { // Evaluate reads it ahead, while GitHub is read
		it.Ignored = c.ignored(ctx, w)
	}
	switch {
	case it.Ignored.Err != nil && !w.StatusRead:
		// Failed for the same reason as status; said once above.
	case it.Ignored.Err != nil:
		reasons = append(reasons, "ignored files unknown: "+oneLine(it.Ignored.Err))
	case len(it.Ignored.AtRisk) > 0:
		reasons = append(reasons, plural(len(it.Ignored.AtRisk), "ignored path")+" at risk: "+listSome(it.Ignored.AtRisk, 3))
	}
	// Rule 2.
	if rep.SessionsErr != nil {
		reasons = append(reasons, "live sessions unknown")
	}
	reasons = append(reasons, inUse...)
	// Rules 3 and 4, or the detached HEAD's reachability.
	var category Category
	var why string
	if rr.Remote == "" {
		reasons = append(reasons, "no GitHub remote")
	} else if w.Detached {
		var r string
		why, r = c.checkDetached(ctx, it)
		if r != "" {
			reasons = append(reasons, r)
		}
		category = Scratch
	} else {
		category, why, reasons = c.checkBranch(ctx, rep, it, reasons)
	}

	if len(reasons) > 0 {
		it.Category = NotSafe
		it.Reasons = reasons
		return
	}
	it.Category = category
	it.Reasons = []string{why}
}

// candidate reports whether the rules apply to w at all: not a main
// checkout, bare entry, locked, prunable or missing worktree.
func candidate(w *worktrees.Worktree) bool {
	return !w.Main && !w.Bare && !w.Locked && !w.Prunable && !w.Missing
}

// attachPR sets it.PR for display: the open PR if there is one, else the
// newest merged one, else the newest closed one.
func (c *Checker) attachPR(rep *Report, it *Item) {
	w := it.Worktree
	ans, ok := rep.prs[prKey(it.Repo.owner, it.Repo.name, w.Branch)]
	if !ok && rep.GitHubSkipped == "" && w.Main && w.Branch == it.Repo.localDefault {
		return // not asked: a main checkout on the default branch
	}
	if !ok || ans.err != nil {
		it.PRUnknown = true
		return
	}
	open, merged, closed := pickPRs(ans.prs.PRs)
	for _, pr := range []*github.PR{open, merged, closed} {
		if pr != nil {
			it.PR = pr
			return
		}
	}
}

// pickPRs returns the newest open, merged and closed-unmerged PRs (PRs are
// newest first).
func pickPRs(prs []github.PR) (open, merged, closed *github.PR) {
	for i := range prs {
		pr := &prs[i]
		if pr.CrossRepository {
			continue
		}
		switch pr.State {
		case github.StateOpen:
			if open == nil {
				open = pr
			}
		case github.StateMerged:
			if merged == nil {
				merged = pr
			}
		case github.StateClosed:
			if closed == nil {
				closed = pr
			}
		}
	}
	return open, merged, closed
}

// checkBranch applies rules 3 and 4′ to a branch worktree, adding to
// reasons. It returns Safe or SafeClosed with the reason it would be safe.
func (c *Checker) checkBranch(ctx context.Context, rep *Report, it *Item, reasons []string) (Category, string, []string) {
	rr, w := it.Repo, it.Worktree
	if rep.GitHubSkipped != "" {
		return NotSafe, "", append(reasons, "PR unknown: GitHub not read ("+rep.GitHubSkipped+")")
	}
	ans, ok := rep.prs[prKey(rr.owner, rr.name, w.Branch)]
	switch {
	case !ok && rr.GitHubErr != nil:
		return NotSafe, "", append(reasons, "PR unknown: "+oneLine(rr.GitHubErr))
	case !ok:
		return NotSafe, "", append(reasons, "PR unknown: GitHub wasn't asked about this branch")
	case ans.err != nil:
		return NotSafe, "", append(reasons, "PR unknown: "+oneLine(ans.err))
	}
	open, merged, closed := pickPRs(ans.prs.PRs)
	var pr *github.PR
	category := Safe
	switch {
	case open != nil:
		return NotSafe, "", append(reasons, fmt.Sprintf("PR #%d is open", open.Number))
	case ans.prs.TotalCount > prsRead:
		return NotSafe, "", append(reasons, fmt.Sprintf("%d PRs on this branch; only the newest %d were read", ans.prs.TotalCount, prsRead))
	case merged != nil:
		pr = merged
	case closed != nil:
		pr, category = closed, SafeClosed
	default:
		return NotSafe, "", append(reasons, "no PR for this branch")
	}
	if r := c.rule4(ctx, rr, w, pr); r != "" {
		reasons = append(reasons, r)
	}
	why := fmt.Sprintf("PR #%d merged into %s", pr.Number, pr.BaseRef)
	if category == SafeClosed {
		why = fmt.Sprintf("PR #%d closed without merging", pr.Number)
	}
	return category, why, reasons
}

// rule4 returns why rule 4′ fails or can't be checked, or "" when nothing
// on the branch is missing from both the PR head and <remote>/<base>.
func (c *Checker) rule4(ctx context.Context, rr *RepoReport, w *worktrees.Worktree, pr *github.PR) string {
	gitDir := rr.Repo.GitDir
	if !isSHA(w.Head) {
		return "HEAD unknown"
	}
	if !isSHA(pr.HeadOID) {
		return fmt.Sprintf("PR #%d head unknown", pr.Number)
	}
	if _, err := c.Scanner.ResolveCommit(ctx, gitDir, pr.HeadOID); err != nil {
		if errors.Is(err, worktrees.ErrNoCommit) {
			return fmt.Sprintf("PR #%d head %s isn't in the local repo (unknown until fetched)", pr.Number, short(pr.HeadOID))
		}
		return fmt.Sprintf("can't check PR #%d head: %s", pr.Number, oneLine(err))
	}
	not := []string{pr.HeadOID}
	base, note := "", ""
	if pr.BaseRef != "" {
		base = rr.Remote + "/" + pr.BaseRef
		ref := "refs/remotes/" + base
		switch err := c.resolveRemoteRef(ctx, rr, ref); {
		case err == nil:
			not = append(not, ref)
		case errors.Is(err, worktrees.ErrNoCommit):
			note = " (" + base + " isn't fetched)"
		default:
			return fmt.Sprintf("can't read %s: %s", base, oneLine(err))
		}
	}
	n, err := c.Scanner.CountNotIn(ctx, gitDir, w.Head, not...)
	if err != nil {
		return "can't compare with the PR: " + oneLine(err)
	}
	if n == 0 {
		return ""
	}
	where := fmt.Sprintf("PR #%d", pr.Number)
	if base != "" {
		where += " or " + base
	}
	return fmt.Sprintf("%s not in %s%s", plural(n, "commit"), where, note)
}

// checkDetached says whether a detached HEAD is on the remote's default
// branch or a PR base. It returns why it is safe, or why not.
func (c *Checker) checkDetached(ctx context.Context, it *Item) (why, reason string) {
	rr, w := it.Repo, it.Worktree
	if !isSHA(w.Head) {
		return "", "HEAD unknown"
	}
	var branches []string
	for _, b := range append([]string{rr.DefaultBranch}, rr.bases...) {
		if b != "" && !slices.Contains(branches, b) {
			branches = append(branches, b)
		}
	}
	var tried []string
	for _, b := range branches {
		name := rr.Remote + "/" + b
		ref := "refs/remotes/" + name
		if err := c.resolveRemoteRef(ctx, rr, ref); err != nil {
			continue
		}
		tried = append(tried, name)
		ok, err := c.Scanner.IsAncestor(ctx, rr.Repo.GitDir, w.Head, ref)
		if err != nil {
			return "", "can't check HEAD: " + oneLine(err)
		}
		if ok {
			it.ReachableFrom = name
			return fmt.Sprintf("detached at %s, on %s", short(w.Head), name), ""
		}
	}
	if len(tried) == 0 {
		return "", "detached, and no " + rr.Remote + "/<default branch> ref to check it against"
	}
	return "", fmt.Sprintf("detached HEAD %s isn't on %s", short(w.Head), strings.Join(tried, " or "))
}

// ignored lists the worktree's ignored paths and splits off the
// rebuildable ones.
//
// git lists a symlink without a trailing "/" even when it points at a
// directory, such as a node_modules linked from another worktree. Removing
// the worktree deletes only the link, never its target, so a symlink is
// matched as if it were a directory: "node_modules/" covers it.
func (c *Checker) ignored(ctx context.Context, w *worktrees.Worktree) *Ignored {
	paths, err := c.Scanner.Ignored(ctx, w)
	if err != nil {
		return &Ignored{Err: err}
	}
	ig := &Ignored{}
	for _, p := range paths {
		match := p
		if !strings.HasSuffix(p, "/") {
			if fi, err := os.Lstat(filepath.Join(w.Path, p)); err == nil && fi.Mode()&os.ModeSymlink != 0 {
				match = p + "/"
			}
		}
		if c.Rebuildable != nil && c.Rebuildable.Match(match) {
			ig.Rebuildable++
		} else {
			ig.AtRisk = append(ig.AtRisk, p)
		}
	}
	sort.Strings(ig.AtRisk)
	return ig
}

// lockStartSlack is how far a running process's start time may be from the
// one in its lock: both are whole seconds.
const lockStartSlack = time.Second

// checkLock says who holds a lock and whether they're still running.
func (c *Checker) checkLock(ctx context.Context, w *worktrees.Worktree) *Lock {
	l := w.Lock
	if l == nil {
		detail := "locked (no reason given)"
		if w.LockReason != "" {
			detail = "locked: " + w.LockReason
		}
		return &Lock{State: LockUnknown, Detail: detail}
	}
	out := &Lock{AgentID: l.AgentID, PID: l.PID}
	owner := fmt.Sprintf("claude %s %s of lead pid %d", l.Kind, l.Name, l.PID)
	if l.AgentID != "" {
		owner = fmt.Sprintf("agent %s of lead pid %d", l.AgentID, l.PID)
	}
	if c.Procs == nil {
		out.State, out.Detail = LockUnknown, "locked by "+owner+" (liveness not checked)"
		return out
	}
	start, running, err := c.Procs.StartTime(ctx, l.PID)
	switch {
	case err != nil:
		out.State, out.Detail = LockUnknown, "locked by "+owner+" (can't check the pid: "+oneLine(err)+")"
	case !running:
		out.State, out.Detail = LockStale, fmt.Sprintf("stale lock: %s; pid %d isn't running", owner, l.PID)
	case l.Start.IsZero():
		out.State, out.Detail = LockLive, "locked by "+owner+" (start time in the lock unreadable)"
	case start.Sub(l.Start).Abs() > lockStartSlack:
		out.State = LockStale
		out.Detail = fmt.Sprintf("stale lock: %s; pid %d is now another process (started %s, the lock says %s)",
			owner, l.PID, start.UTC().Format(time.ANSIC), l.Start.UTC().Format(time.ANSIC))
	default:
		out.State, out.Detail = LockLive, "locked by "+owner
	}
	return out
}

// sessionLabel names a session in a reason.
func sessionLabel(s sessions.Session) string {
	kind := string(s.Kind)
	if kind == "" {
		kind = "session"
	} else {
		kind += " session"
	}
	id := s.SessionID
	if len(id) > 8 {
		id = id[:8]
	}
	if s.JobID != "" {
		id = "job " + s.JobID
	}
	label := strings.TrimSpace(kind + " " + id)
	if s.PID > 0 {
		label += fmt.Sprintf(" (pid %d)", s.PID)
	}
	return label
}

func absPath(p string) string {
	if a, err := filepath.Abs(p); err == nil {
		return a
	}
	return filepath.Clean(p)
}

// isSHA reports whether s is a full, non-zero hex object name.
func isSHA(s string) bool {
	if len(s) != 40 && len(s) != 64 {
		return false
	}
	return strings.Trim(s, "0123456789abcdef") == "" && strings.Trim(s, "0") != ""
}

func short(sha string) string {
	if len(sha) > 7 {
		return sha[:7]
	}
	return sha
}

func plural(n int, what string) string {
	if n == 1 {
		return "1 " + what
	}
	return fmt.Sprintf("%d %ss", n, what)
}

// listSome joins the first n items, saying how many more there are.
func listSome(items []string, n int) string {
	if len(items) <= n {
		return strings.Join(items, ", ")
	}
	return strings.Join(items[:n], ", ") + fmt.Sprintf(" (+%d more)", len(items)-n)
}

// oneLine is err's first line. For a failed git call it is what git said
// (without the command line), which is what a reader needs.
func oneLine(err error) string {
	msg := err.Error()
	var ge *worktrees.GitError
	if errors.As(err, &ge) {
		if line, _, _ := strings.Cut(ge.Stderr, "\n"); line != "" {
			msg = "git " + firstArg(ge.Args) + ": " + strings.TrimPrefix(line, "fatal: ")
		}
	}
	s, _, _ := strings.Cut(msg, "\n")
	return s
}

// firstArg is the git subcommand in args, skipping options.
func firstArg(args []string) string {
	for i := 0; i < len(args); i++ {
		switch a := args[i]; {
		case a == "-c":
			i++
		case strings.HasPrefix(a, "-"):
		default:
			return a
		}
	}
	return ""
}
