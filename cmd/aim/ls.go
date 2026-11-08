package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/gravexa-llc/ai-manager/internal/prune"
)

const lsUsage = `usage: aim ls [--repo R] [--json]

Lists every worktree of every repo under the configured roots (and of the
repos live sessions are in), one line each: path, branch, uncommitted and
untracked files, ignored files at risk, upstream, PR, and whether
aim prune would remove it, with the reason. Read-only.

  --repo R   only the repo R: a path in it, its directory name, or owner/name
  --json     print JSON instead of a table
`

// cmdLs runs `aim ls`.
func cmdLs(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("ls", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.Usage = func() { fmt.Fprint(stderr, lsUsage) }
	repo := fs.String("repo", "", "")
	asJSON := fs.Bool("json", false, "")
	if err := fs.Parse(args); err != nil {
		if err == flag.ErrHelp {
			return 0
		}
		return 2
	}
	if fs.NArg() > 0 {
		fmt.Fprintf(stderr, "aim ls: unexpected argument %q\n\n%s", fs.Arg(0), lsUsage)
		return 2
	}

	ctx, stop := signalContext()
	defer stop()
	_, _, rep := evaluate(ctx, "ls", *repo, stderr)
	if rep == nil {
		return 1
	}
	if *asJSON {
		enc := json.NewEncoder(stdout)
		enc.SetIndent("", "  ")
		if err := enc.Encode(lsJSONReport(rep)); err != nil {
			fmt.Fprintf(stderr, "aim ls: %v\n", err)
			return 1
		}
		return 0
	}
	printWarnings(stderr, rep)
	printLs(stdout, rep)
	return 0
}

// printLs writes the table: a header line per repo, then one line per
// worktree.
func printLs(w io.Writer, rep *prune.Report) {
	for i, rr := range rep.Repos {
		if i > 0 {
			fmt.Fprintln(w)
		}
		header := displayPath(rr.Repo.Path)
		switch {
		case rr.GitHub != "":
			header += "  (" + rr.GitHub + ")"
		default:
			header += "  (no GitHub remote)"
		}
		fmt.Fprintln(w, header)
		if rr.Repo.Err != nil {
			fmt.Fprintf(w, "  error: %v\n", rr.Repo.Err)
		}
		tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
		fmt.Fprintln(tw, "  PATH\tBRANCH\tUNCOMMITTED\tUNTRACKED\tIGNORED AT RISK\tUPSTREAM\tPR\tVERDICT")
		for _, it := range rr.Items {
			wt := it.Worktree
			fmt.Fprintf(tw, "  %s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\n",
				displayPath(wt.Path), branchLabel(wt), count(it, wt.Uncommitted), count(it, wt.Untracked),
				atRisk(it), upstreamLabel(wt), prLabel(it), verdict(it, false))
		}
		tw.Flush()
	}
}

// count shows a status count: "?" when status wasn't read, "-" when there
// is no working tree to read.
func count(it *prune.Item, n int) string {
	wt := it.Worktree
	switch {
	case wt.Bare || wt.Missing || wt.Prunable:
		return "-"
	case !wt.StatusRead:
		return "?"
	default:
		return strconv.Itoa(n)
	}
}

// atRisk shows how many ignored paths would be lost, "?" if they couldn't
// be listed, "-" if they weren't checked (main, locked, prunable).
func atRisk(it *prune.Item) string {
	switch {
	case it.Ignored == nil:
		return "-"
	case it.Ignored.Err != nil:
		return "?"
	default:
		return strconv.Itoa(len(it.Ignored.AtRisk))
	}
}

// The JSON form of aim ls.
type (
	lsJSON struct {
		Repos         []repoJSON `json:"repos"`
		LiveSessions  int        `json:"live_sessions"`
		SessionsError string     `json:"sessions_error,omitempty"`
		GitHubSkipped string     `json:"github_skipped,omitempty"`
		Warnings      []string   `json:"warnings,omitempty"`
	}
	repoJSON struct {
		Path          string         `json:"path"`
		GitDir        string         `json:"git_dir"`
		GitHub        string         `json:"github,omitempty"`
		Remote        string         `json:"remote,omitempty"`
		DefaultBranch string         `json:"default_branch,omitempty"`
		Error         string         `json:"error,omitempty"`
		GitHubError   string         `json:"github_error,omitempty"`
		Worktrees     []worktreeJSON `json:"worktrees"`
	}
	worktreeJSON struct {
		Path              string       `json:"path"`
		RecordedPath      string       `json:"recorded_path,omitempty"`
		Main              bool         `json:"main,omitempty"`
		Bare              bool         `json:"bare,omitempty"`
		Branch            string       `json:"branch,omitempty"`
		Detached          bool         `json:"detached,omitempty"`
		Head              string       `json:"head,omitempty"`
		Locked            bool         `json:"locked,omitempty"`
		LockReason        string       `json:"lock_reason,omitempty"`
		Lock              *lockJSON    `json:"lock,omitempty"`
		Prunable          bool         `json:"prunable,omitempty"`
		PrunableReason    string       `json:"prunable_reason,omitempty"`
		Missing           bool         `json:"missing,omitempty"`
		AgentID           string       `json:"agent_id,omitempty"`
		ScratchpadSession string       `json:"scratchpad_session,omitempty"`
		StatusKnown       bool         `json:"status_known"`
		Uncommitted       int          `json:"uncommitted"`
		Untracked         int          `json:"untracked"`
		Ignored           *ignoredJSON `json:"ignored,omitempty"`
		Upstream          string       `json:"upstream,omitempty"`
		UpstreamRef       string       `json:"upstream_ref,omitempty"`
		Unpushed          int          `json:"unpushed"`
		Behind            int          `json:"behind"`
		LastCommitAt      *time.Time   `json:"last_commit_at,omitempty"`
		PR                *prJSON      `json:"pr,omitempty"`
		PRUnknown         bool         `json:"pr_unknown,omitempty"`
		ReachableFrom     string       `json:"reachable_from,omitempty"`
		Verdict           verdictJSON  `json:"verdict"`
		Error             string       `json:"error,omitempty"`
	}
	lockJSON struct {
		State   string `json:"state"`
		AgentID string `json:"agent_id,omitempty"`
		PID     int    `json:"pid,omitempty"`
		Detail  string `json:"detail"`
	}
	ignoredJSON struct {
		AtRisk      []string `json:"at_risk"`
		Rebuildable int      `json:"rebuildable"`
		Error       string   `json:"error,omitempty"`
	}
	prJSON struct {
		Number   int        `json:"number"`
		State    string     `json:"state"`
		Draft    bool       `json:"draft,omitempty"`
		URL      string     `json:"url,omitempty"`
		HeadRef  string     `json:"head_ref"`
		HeadOID  string     `json:"head_oid"`
		BaseRef  string     `json:"base_ref"`
		MergedAt *time.Time `json:"merged_at,omitempty"`
		ClosedAt *time.Time `json:"closed_at,omitempty"`
	}
	verdictJSON struct {
		Category string   `json:"category"`
		Safe     bool     `json:"safe"`
		Reasons  []string `json:"reasons"`
	}
)

func lsJSONReport(rep *prune.Report) lsJSON {
	out := lsJSON{Repos: []repoJSON{}, LiveSessions: rep.LiveSessions, GitHubSkipped: rep.GitHubSkipped, Warnings: rep.Warnings}
	if rep.SessionsErr != nil {
		out.SessionsError = rep.SessionsErr.Error()
	}
	for _, rr := range rep.Repos {
		r := repoJSON{
			Path: rr.Repo.Path, GitDir: rr.Repo.GitDir, GitHub: rr.GitHub, Remote: rr.Remote,
			DefaultBranch: rr.DefaultBranch, Error: errText(rr.Repo.Err), GitHubError: errText(rr.GitHubErr),
			Worktrees: []worktreeJSON{},
		}
		for _, it := range rr.Items {
			r.Worktrees = append(r.Worktrees, worktreeJSONOf(it))
		}
		out.Repos = append(out.Repos, r)
	}
	return out
}

func worktreeJSONOf(it *prune.Item) worktreeJSON {
	w := it.Worktree
	j := worktreeJSON{
		Path: w.Path, Main: w.Main, Bare: w.Bare, Branch: w.Branch, Detached: w.Detached, Head: w.Head,
		Locked: w.Locked, LockReason: w.LockReason, Prunable: w.Prunable, PrunableReason: w.PrunableReason,
		Missing: w.Missing, AgentID: w.AgentID, ScratchpadSession: w.ScratchpadSession,
		StatusKnown: w.StatusRead, Uncommitted: w.Uncommitted, Untracked: w.Untracked,
		Upstream: string(w.Upstream), UpstreamRef: w.UpstreamRef, Unpushed: w.Unpushed, Behind: w.Behind,
		PRUnknown: it.PRUnknown, ReachableFrom: it.ReachableFrom, Error: errText(w.Err),
		Verdict: verdictJSON{Category: string(it.Category), Safe: it.Category.Removable(false), Reasons: it.Reasons},
	}
	if w.RecordedPath != w.Path {
		j.RecordedPath = w.RecordedPath
	}
	if !w.LastCommitAt.IsZero() {
		t := w.LastCommitAt
		j.LastCommitAt = &t
	}
	if l := it.Lock; l != nil {
		j.Lock = &lockJSON{State: string(l.State), AgentID: l.AgentID, PID: l.PID, Detail: l.Detail}
	}
	if ig := it.Ignored; ig != nil {
		j.Ignored = &ignoredJSON{AtRisk: ig.AtRisk, Rebuildable: ig.Rebuildable, Error: errText(ig.Err)}
		if j.Ignored.AtRisk == nil {
			j.Ignored.AtRisk = []string{}
		}
	}
	if pr := it.PR; pr != nil {
		j.PR = &prJSON{
			Number: pr.Number, State: strings.ToLower(string(pr.State)), Draft: pr.Draft, URL: pr.URL,
			HeadRef: pr.HeadRef, HeadOID: pr.HeadOID, BaseRef: pr.BaseRef,
		}
		if !pr.MergedAt.IsZero() {
			t := pr.MergedAt
			j.PR.MergedAt = &t
		}
		if !pr.ClosedAt.IsZero() {
			t := pr.ClosedAt
			j.PR.ClosedAt = &t
		}
	}
	return j
}

func errText(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}
