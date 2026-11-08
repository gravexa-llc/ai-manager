package state

import (
	"context"
	"fmt"
	"time"

	"github.com/gravexa-llc/ai-manager/internal/config"
	"github.com/gravexa-llc/ai-manager/internal/plans"
	"github.com/gravexa-llc/ai-manager/internal/store"
)

// OptionsFromConfig returns Options with the thresholds in cfg.
func OptionsFromConfig(cfg config.Config) Options {
	return Options{StalledAfter: cfg.State.StalledAfter, QuietAfter: cfg.State.QuietAfter}
}

// StorePackages turns a parsed plan's packages into the rows
// store.SyncPackages takes, in plan order.
func StorePackages(p plans.Plan) []store.Package {
	out := make([]store.Package, len(p.Packages))
	for i, pkg := range p.Packages {
		out[i] = store.Package{
			PkgID: pkg.ID, Seq: i, Title: pkg.Title, Repo: pkg.Repo, Size: pkg.Size,
			Needs: pkg.Needs, NeedsNote: pkg.Note, NeedsYou: pkg.NeedsYou,
			Owner: pkg.Owner, Lane: pkg.Lane, DoneInPlan: pkg.Done,
		}
	}
	return out
}

// Load reads an Input from the store: every plan with its packages, links
// and tombstones, every PR, every repo's default branch, every worktree
// (removed ones too), every session (ended ones too) and every agent. Now
// and RepoNames are left for the caller.
func Load(ctx context.Context, st *store.Store) (Input, error) {
	var in Input
	ps, err := st.ListPlans(ctx, store.PlanFilter{})
	if err != nil {
		return in, fmt.Errorf("state: load plans: %w", err)
	}
	for _, p := range ps {
		pkgs, err := st.ListPackages(ctx, p.ID)
		if err != nil {
			return in, fmt.Errorf("state: load packages of %s: %w", p.Slug, err)
		}
		in.Plans = append(in.Plans, Plan{Plan: p, Packages: pkgs})
		links, err := st.ListLinks(ctx, p.ID)
		if err != nil {
			return in, fmt.Errorf("state: load links of %s: %w", p.Slug, err)
		}
		in.Links = append(in.Links, links...)
		ts, err := st.ListTombstones(ctx, p.ID)
		if err != nil {
			return in, fmt.Errorf("state: load tombstones of %s: %w", p.Slug, err)
		}
		in.Tombstones = append(in.Tombstones, ts...)
	}
	if in.PRs, err = st.ListPRs(ctx, store.PRFilter{}); err != nil {
		return in, fmt.Errorf("state: load PRs: %w", err)
	}
	repos, err := st.ListRepos(ctx)
	if err != nil {
		return in, fmt.Errorf("state: load repos: %w", err)
	}
	for _, r := range repos {
		if r.DefaultBranch != "" {
			if in.DefaultBranches == nil {
				in.DefaultBranches = map[string]string{}
			}
			in.DefaultBranches[r.Name] = r.DefaultBranch
		}
	}
	if in.Worktrees, err = st.ListWorktrees(ctx, store.WorktreeFilter{IncludeRemoved: true}); err != nil {
		return in, fmt.Errorf("state: load worktrees: %w", err)
	}
	if in.Sessions, err = st.ListSessions(ctx, store.SessionFilter{IncludeEnded: true}); err != nil {
		return in, fmt.Errorf("state: load sessions: %w", err)
	}
	if in.Agents, err = st.ListAgents(ctx, ""); err != nil {
		return in, fmt.Errorf("state: load agents: %w", err)
	}
	return in, nil
}

// Run is one pass, for aimd to call after each poll: it loads the store,
// matches, replaces each plan's inferred links with the matcher's answer,
// derives every plan's board, and saves the lifecycles that changed. now
// is the snapshot's time; repoNames is Input.RepoNames. Run one pass at a
// time: two at once could store links from different snapshots.
//
// A pass also keeps the provisional packages: it stores what
// Unknown finds, moves on the last activity of those with work on them,
// and expires those quiet for ProvisionalQuiet. A pass that made or
// expired one starts over from the store, at most twice, so its answer
// has them as the store does.
func Run(ctx context.Context, st *store.Store, now time.Time, repoNames map[string]string, opts Options) ([]PlanState, error) {
	for again := 2; ; again-- {
		in, err := Load(ctx, st)
		if err != nil {
			return nil, err
		}
		in.Now, in.RepoNames = now, repoNames
		m := Match(in)
		if again > 0 {
			made, err := seeProvisional(ctx, st, in, m)
			if err != nil {
				return nil, err
			}
			if made {
				continue // the matcher links to the new packages
			}
		}
		states, err := runPass(ctx, st, in, m, opts)
		if err != nil {
			return nil, err
		}
		if again > 0 {
			expired, err := keepProvisional(ctx, st, in, states)
			if err != nil {
				return nil, err
			}
			if expired {
				continue
			}
		}
		if err := saveStates(ctx, st, states, now); err != nil {
			return nil, err
		}
		return states, nil
	}
}

// runPass stores the matcher's links and derives the boards.
func runPass(ctx context.Context, st *store.Store, in Input, m Matches, opts Options) ([]PlanState, error) {
	for _, p := range in.Plans {
		if _, err := st.ReplaceInferredLinks(ctx, p.ID, m.Links[p.ID]); err != nil {
			return nil, fmt.Errorf("state: %w", err)
		}
	}
	// Reload the links, so the board carries their ids.
	in.Links = nil
	for _, p := range in.Plans {
		links, err := st.ListLinks(ctx, p.ID)
		if err != nil {
			return nil, fmt.Errorf("state: load links of %s: %w", p.Slug, err)
		}
		in.Links = append(in.Links, links...)
	}
	return Derive(in, m, opts), nil
}

// saveStates stores the lifecycles that changed and the alerts.
func saveStates(ctx context.Context, st *store.Store, states []PlanState, now time.Time) error {
	for _, ps := range states {
		var changed []store.PackageLifecycle
		for _, pkg := range ps.Packages {
			if pkg.Changed {
				changed = append(changed, store.PackageLifecycle{PkgID: pkg.ID, Lifecycle: string(pkg.Lifecycle), Prev: string(pkg.PrevLifecycle), At: pkg.Since})
			}
		}
		if len(changed) > 0 {
			if err := st.SetLifecycles(ctx, ps.PlanID, changed); err != nil {
				return fmt.Errorf("state: %w", err)
			}
		}
		if _, err := st.SyncAlerts(ctx, ps.PlanID, storeAlerts(ps.Alerts), now); err != nil {
			return fmt.Errorf("state: %w", err)
		}
	}
	return nil
}

// storeAlerts are the alerts a plan's timeline keeps (store.SyncAlerts):
// stalled packages, by package, and lane collisions, by lane, each raised
// at the pass that first finds it. Unlocked packages are in the lifecycle
// history already (blocked → ready), and off-plan agents aren't the plan's
// work.
func storeAlerts(as []Alert) []store.PlanAlert {
	var out []store.PlanAlert
	for _, a := range as {
		switch a.Kind {
		case AlertStalled:
			for _, id := range a.Packages {
				out = append(out, store.PlanAlert{Kind: string(a.Kind), Key: id, Packages: []string{id}, Message: a.Message})
			}
		case AlertLaneCollision:
			out = append(out, store.PlanAlert{Kind: string(a.Kind), Key: a.Lane, Packages: a.Packages, Message: a.Message})
		}
	}
	return out
}
