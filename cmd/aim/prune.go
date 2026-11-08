package main

import (
	"bufio"
	"flag"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"
	"text/tabwriter"

	"github.com/gravexa-llc/ai-manager/internal/prune"
	"github.com/gravexa-llc/ai-manager/internal/worktrees"
)

const pruneUsage = `usage: aim prune [--repo R] [--include-closed] [--apply] [--yes]

Shows which worktrees are safe to remove: branch worktrees whose PR merged,
with no local changes, no ignored files outside the rebuildable list, no
live session in them, and no commit missing from both the PR and its base;
detached ("scratch") worktrees whose HEAD is on the remote; and git records
whose folder is gone. Locked worktrees and main checkouts are never removed.
A dry run unless --apply is given.

  --repo R           only the repo R: a path in it, its directory name, or owner/name
  --include-closed   also remove worktrees whose PR was closed without merging
  --apply            remove them: git worktree remove (never --force), then
                     git branch -D, and git worktree prune for gone folders
  --yes              don't ask for confirmation with --apply
`

// stdin is where --apply reads its confirmation. Tests replace it.
var stdin io.Reader = os.Stdin

// cmdPrune runs `aim prune`.
func cmdPrune(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("prune", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.Usage = func() { fmt.Fprint(stderr, pruneUsage) }
	repo := fs.String("repo", "", "")
	includeClosed := fs.Bool("include-closed", false, "")
	apply := fs.Bool("apply", false, "")
	yes := fs.Bool("yes", false, "")
	if err := fs.Parse(args); err != nil {
		if err == flag.ErrHelp {
			return 0
		}
		return 2
	}
	if fs.NArg() > 0 {
		fmt.Fprintf(stderr, "aim prune: unexpected argument %q\n\n%s", fs.Arg(0), pruneUsage)
		return 2
	}
	if *yes && !*apply {
		fmt.Fprintf(stderr, "aim prune: --yes only makes sense with --apply\n")
		return 2
	}

	ctx, stop := signalContext()
	defer stop()
	checker, opts, rep := evaluate(ctx, "prune", *repo, stderr)
	if rep == nil {
		return 1
	}
	printWarnings(stderr, rep)
	printPruneReport(stdout, rep, *includeClosed)
	plan := rep.Plan(*includeClosed)
	summary := planSummary(plan)

	if !*apply {
		if plan.Empty() {
			fmt.Fprintln(stdout, "\nDry run: nothing to remove.")
		} else {
			fmt.Fprintf(stdout, "\nDry run: nothing was removed. `aim prune --apply` would %s.\n", summary)
		}
		return 0
	}
	if plan.Empty() {
		fmt.Fprintln(stdout, "\nNothing to remove.")
		return 0
	}
	if !*yes {
		fmt.Fprintf(stdout, "\nThis will %s. Continue? [y/N] ", summary)
		line, _ := bufio.NewReader(stdin).ReadString('\n')
		if a := strings.ToLower(strings.TrimSpace(line)); a != "y" && a != "yes" {
			fmt.Fprintln(stdout)
			fmt.Fprintln(stderr, "aim prune: not confirmed; nothing was removed")
			return 1
		}
	}
	fmt.Fprintln(stdout)
	res := checker.Apply(ctx, plan, opts, rep)
	printApplyResult(stdout, res)
	if res.Failed() {
		return 1
	}
	return 0
}

// group is one section of the dry-run report.
type group struct {
	title string
	items []*prune.Item
}

// printPruneReport writes the dry run, grouped by category.
func printPruneReport(w io.Writer, rep *prune.Report, includeClosed bool) {
	closedTitle := "Safe to remove, PR closed without merging (only with --include-closed)"
	if includeClosed {
		closedTitle = "Safe to remove, PR closed without merging"
	}
	groups := []*group{
		{title: "Safe to remove, PR merged"},
		{title: closedTitle},
		{title: "Scratch: detached, HEAD on the remote"},
		{title: "Locked: never removed"},
		{title: "Prunable records: folder or its .git gone"},
		{title: "Not safe"},
	}
	byCat := map[prune.Category]*group{
		prune.Safe: groups[0], prune.SafeClosed: groups[1], prune.Scratch: groups[2],
		prune.Locked: groups[3], prune.Prunable: groups[4], prune.NotSafe: groups[5],
	}
	mains := 0
	for _, rr := range rep.Repos {
		for _, it := range rr.Items {
			if g := byCat[it.Category]; g != nil {
				g.items = append(g.items, it)
			} else {
				mains++
			}
		}
	}
	first := true
	for _, g := range groups {
		if len(g.items) == 0 {
			continue
		}
		if !first {
			fmt.Fprintln(w)
		}
		first = false
		sort.SliceStable(g.items, func(i, j int) bool {
			return worktrees.PathKey(g.items[i].Worktree.Path) < worktrees.PathKey(g.items[j].Worktree.Path)
		})
		fmt.Fprintf(w, "%s (%d):\n", g.title, len(g.items))
		tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
		for _, it := range g.items {
			fmt.Fprintf(tw, "  %s\t%s\t%s\n", displayPath(it.Worktree.Path), branchLabel(it.Worktree), strings.Join(it.Reasons, "; "))
		}
		tw.Flush()
	}
	if !first {
		fmt.Fprintln(w)
	}
	fmt.Fprintf(w, "Main checkouts (%d) are never candidates.\n", mains)
}

// planSummary says what a plan does, for the dry run and the prompt.
func planSummary(p prune.Plan) string {
	branches := 0
	for _, it := range p.Remove {
		if it.Worktree.Branch != "" {
			branches++
		}
	}
	records := 0
	for _, rr := range p.Prune {
		for _, it := range rr.Items {
			if it.Category == prune.Prunable {
				records++
			}
		}
	}
	var parts []string
	if len(p.Remove) > 0 {
		s := "remove " + nouns(len(p.Remove), "worktree")
		if branches > 0 {
			s += " and delete " + nouns(branches, "branch")
		}
		parts = append(parts, s)
	}
	if len(p.Prune) > 0 {
		parts = append(parts, fmt.Sprintf("run git worktree prune in %s for %s",
			nouns(len(p.Prune), "repo"), nouns(records, "record")))
	}
	return strings.Join(parts, ", and ")
}

// printApplyResult reports exactly what --apply did.
func printApplyResult(w io.Writer, res prune.Result) {
	if res.RecheckErr != nil {
		fmt.Fprintf(w, "re-check failed, nothing was removed: %v\n", res.RecheckErr)
		return
	}
	removed, branches, skipped, failed, pruned := 0, 0, 0, 0, 0
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	for _, r := range res.Removals {
		path := displayPath(r.Item.Worktree.Path)
		switch {
		case r.Skipped != "":
			skipped++
			fmt.Fprintf(tw, "skipped\t%s\t%s\n", path, r.Skipped)
		case r.Removed:
			removed++
			note := "detached"
			if r.BranchDeleted {
				branches++
				note = "branch " + r.Item.Worktree.Branch + " deleted"
			}
			if r.Err != nil {
				failed++
				note = r.Err.Error()
			}
			fmt.Fprintf(tw, "removed\t%s\t%s\n", path, note)
		default:
			failed++
			fmt.Fprintf(tw, "failed\t%s\t%v\n", path, r.Err)
		}
	}
	for _, p := range res.Prunes {
		path := displayPath(p.Repo.Repo.Path)
		switch {
		case p.Err != nil:
			failed++
			fmt.Fprintf(tw, "failed\t%s\tgit worktree prune: %v\n", path, p.Err)
		case p.Skipped != "":
			skipped++
			fmt.Fprintf(tw, "not pruned\t%s\t%s\n", path, p.Skipped)
		default:
			pruned += len(p.Pruned)
			var names []string
			for _, e := range p.Pruned {
				names = append(names, displayPath(e.Path))
			}
			fmt.Fprintf(tw, "pruned\t%s\t%s: %s\n", path, nouns(len(p.Pruned), "record"), strings.Join(names, ", "))
		}
	}
	tw.Flush()
	fmt.Fprintf(w, "\nRemoved %s, deleted %s, pruned %s; %d skipped, %d failed.\n",
		nouns(removed, "worktree"), nouns(branches, "branch"),
		nouns(pruned, "record"), skipped, failed)
}
