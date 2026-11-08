package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"strings"

	"github.com/gravexa-llc/ai-manager/internal/config"
	"github.com/gravexa-llc/ai-manager/internal/github"
	"github.com/gravexa-llc/ai-manager/internal/prune"
	"github.com/gravexa-llc/ai-manager/internal/sessions"
	"github.com/gravexa-llc/ai-manager/internal/worktrees"
)

// Shared by aim ls and aim prune.

// newChecker builds the checker aim ls and aim prune use. Tests replace it
// to swap in fake GitHub, session and process sources.
var newChecker = defaultChecker

func defaultChecker(cfg config.Config) (*prune.Checker, error) {
	paths, err := cfg.Prune.RebuildablePaths()
	if err != nil {
		return nil, err
	}
	m, err := prune.NewMatcher(paths)
	if err != nil {
		return nil, err
	}
	return &prune.Checker{
		Scanner:     worktrees.New(worktrees.OptionsFromConfig(cfg)),
		GitHub:      github.New(cfg, github.Options{}),
		Sessions:    sessions.Lister{},
		Procs:       prune.PS{},
		Rebuildable: m,
	}, nil
}

// evaluate loads the config and evaluates the worktrees for a command. It
// prints its own errors and returns nil on failure.
func evaluate(ctx context.Context, cmd, repo string, stderr io.Writer) (*prune.Checker, prune.Options, *prune.Report) {
	home, err := config.Home()
	if err == nil {
		var cfg config.Config
		if cfg, err = config.Load(home); err == nil {
			var c *prune.Checker
			if c, err = newChecker(cfg); err == nil {
				opts := prune.Options{Repo: expandTilde(repo)}
				var rep *prune.Report
				if rep, err = c.Evaluate(ctx, opts); err == nil {
					return c, opts, rep
				}
			}
		}
	}
	fmt.Fprintf(stderr, "aim %s: %v\n", cmd, err)
	return nil, prune.Options{}, nil
}

// signalContext is cancelled by an interrupt.
func signalContext() (context.Context, context.CancelFunc) {
	return signal.NotifyContext(context.Background(), os.Interrupt)
}

// printWarnings writes what limited the report to stderr.
func printWarnings(w io.Writer, rep *prune.Report) {
	for _, s := range rep.Warnings {
		fmt.Fprintf(w, "aim: warning: %s\n", s)
	}
	if rep.SessionsErr != nil {
		fmt.Fprintf(w, "aim: warning: live sessions unknown, so nothing counts as safe: %v\n", rep.SessionsErr)
	}
	if rep.GitHubSkipped != "" {
		fmt.Fprintf(w, "aim: warning: GitHub not read: %s; PR states are unknown\n", rep.GitHubSkipped)
	}
}

// verdict is an item's verdict on one line.
func verdict(it *prune.Item, includeClosed bool) string {
	reasons := strings.Join(it.Reasons, "; ")
	switch it.Category {
	case prune.Main, prune.Locked:
		return reasons
	case prune.Safe:
		return "safe: " + reasons
	case prune.SafeClosed:
		if includeClosed {
			return "safe: " + reasons
		}
		return "safe with --include-closed: " + reasons
	case prune.Scratch:
		return "scratch: " + reasons
	case prune.Prunable:
		return "prunable: " + reasons
	default:
		return "not safe: " + reasons
	}
}

// branchLabel is the branch, or what stands in for one.
func branchLabel(w *worktrees.Worktree) string {
	switch {
	case w.Bare:
		return "(bare)"
	case w.Detached:
		return "(detached " + shortSHA(w.Head) + ")"
	case w.Branch == "":
		return "-"
	default:
		return w.Branch
	}
}

// prLabel is the PR's number and state, "?" when GitHub couldn't say, "-"
// when there is none.
func prLabel(it *prune.Item) string {
	switch {
	case it.PR != nil:
		state := strings.ToLower(string(it.PR.State))
		if it.PR.State == github.StateOpen && it.PR.Draft {
			state = "draft"
		}
		return fmt.Sprintf("#%d %s", it.PR.Number, state)
	case it.PRUnknown:
		return "?"
	default:
		return "-"
	}
}

// upstreamLabel is the upstream state with ahead/behind counts.
func upstreamLabel(w *worktrees.Worktree) string {
	switch w.Upstream {
	case worktrees.UpstreamOK:
		s := "ok"
		if w.Unpushed > 0 {
			s += fmt.Sprintf(" +%d", w.Unpushed)
		}
		if w.Behind > 0 {
			s += fmt.Sprintf(" -%d", w.Behind)
		}
		return s
	case worktrees.UpstreamUnknown:
		return "-"
	default:
		return string(w.Upstream)
	}
}

// displayPath abbreviates the home directory to ~.
func displayPath(p string) string {
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return p
	}
	if worktrees.SamePath(p, home) {
		return "~"
	}
	if rel, err := filepath.Rel(home, p); err == nil && !strings.HasPrefix(rel, "..") && !filepath.IsAbs(rel) {
		return filepath.Join("~", rel)
	}
	return p
}

// expandTilde expands a leading ~/ in a flag value the shell didn't
// expand (--repo=~/x).
func expandTilde(p string) string {
	if p != "~" && !strings.HasPrefix(p, "~/") {
		return p
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return p
	}
	return filepath.Join(home, strings.TrimPrefix(p, "~"))
}

func shortSHA(s string) string {
	if len(s) > 7 {
		return s[:7]
	}
	return s
}

// nouns writes "1 worktree" or "3 worktrees" ("branch" → "branches"),
// with plural from status.go.
func nouns(n int, noun string) string {
	if strings.HasSuffix(noun, "ch") {
		return plural(n, noun, noun+"es")
	}
	return plural(n, noun, noun+"s")
}
