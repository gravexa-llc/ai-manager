package planapi

// What waits on you, and on a plan's lead, plan by plan: the counts a
// summary, a band and the dashboard's menu show.
//
//   - Needs you is the plan's Open items in the Needs you inbox
//     (internal/inbox): the monitored sessions waiting on you, the
//     PRs internal/prwait's rule says need you (a review request, on any
//     PR, or a ready PR you merge), its provisional packages' missing rows
//     and its packages that are your step and ready (Owner you, or a
//     "you:" step, once everything they need is done). They are read from
//     the store at each read, so a summary agrees with the inbox's Open
//     tab as of its last sync pass. The dashboard's handlers run a pass
//     first.
//   - To merge is the plan's open PRs that wait for its lead to merge them:
//     internal/prwait's rule (through monitor.Set.PRWait as the
//     dashboard decides it) says they are ready to merge, the plan says
//     "Merges: lead", and no agent still works on them. It is worked out
//     at each state pass, which runs after every change to the sessions,
//     the PRs and the worktrees.

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/gravexa-llc/ai-manager/internal/inbox"
	"github.com/gravexa-llc/ai-manager/internal/monitor"
	"github.com/gravexa-llc/ai-manager/internal/plans"
	"github.com/gravexa-llc/ai-manager/internal/prwait"
	"github.com/gravexa-llc/ai-manager/internal/store"
	"github.com/gravexa-llc/ai-manager/internal/web/api"
)

// needs is what waits on you and on the leads, by lower-case plan slug.
type needs struct {
	// items are each plan's Open inbox items, oldest first.
	items map[string][]store.InboxItem
	// toMerge are each plan's PRs ready for its lead to merge
	// ("owner/repo#123"), from the last state pass.
	toMerge map[string][]string
}

// needsNow reads the inbox's Open items, and takes the last pass's PRs to
// merge.
func (s *Service) needsNow(ctx context.Context) (needs, error) {
	items, err := s.st.ListInbox(ctx, store.InboxFilter{State: store.InboxOpen})
	if err != nil {
		return needs{}, fmt.Errorf("read the Needs you inbox: %w", err)
	}
	n := needs{items: map[string][]store.InboxItem{}}
	for _, g := range inbox.Groups(items, nil) {
		if g.Plan != "" {
			k := strings.ToLower(g.Plan)
			n.items[k] = append(n.items[k], g.Items...)
		}
	}
	s.mu.Lock()
	n.toMerge = s.toMerge
	s.mu.Unlock()
	return n, nil
}

// summaryNow is a board's summary with what waits now.
func (s *Service) summaryNow(ctx context.Context, b api.Plan, since time.Time) (api.PlanSummary, error) {
	n, err := s.needsNow(ctx)
	if err != nil {
		return api.PlanSummary{}, err
	}
	return summaryOf(b, since, n), nil
}

// withBands returns copies of boards with their bands filled in from what
// waits now.
func (s *Service) withBands(ctx context.Context, boards []api.Plan) ([]api.Plan, error) {
	n, err := s.needsNow(ctx)
	if err != nil {
		return nil, err
	}
	out := slices.Clone(boards)
	for i := range out {
		out[i].Band = summaryOf(out[i], time.Time{}, n).Band
	}
	return out, nil
}

// leadMerges lists, by lower-case plan slug, the open PRs ready for the
// plan's lead to merge (see the file comment). The monitoring is decided
// as the dashboard's monitor.Source does it, from the store, these boards
// and the scanned repos' names. Without a lead-merged plan there is
// nothing to read.
func (s *Service) leadMerges(ctx context.Context, boards []api.Plan) (map[string][]string, error) {
	if !slices.ContainsFunc(boards, func(b api.Plan) bool { return b.Merges == plans.MergesLead }) {
		return nil, nil
	}
	rows, err := s.st.ListSessions(ctx, store.SessionFilter{})
	if err != nil {
		return nil, fmt.Errorf("list sessions: %w", err)
	}
	prs, err := s.st.ListPRs(ctx, store.PRFilter{State: store.PROpen})
	if err != nil {
		return nil, fmt.Errorf("list PRs: %w", err)
	}
	src := &monitor.Source{
		Mode:      s.cfg.Sessions.Monitor,
		Store:     s.st,
		Plans:     func(context.Context) ([]api.Plan, error) { return boards, nil },
		RepoNames: s.repoNames,
	}
	set, err := src.Load(ctx, rows, nil)
	if err != nil {
		return nil, err
	}
	out := map[string][]string{}
	for _, p := range prs {
		v := set.PRWait(p)
		if v.Plan == "" || v.WaitingOn != prwait.WaitingLead || v.Merges != prwait.MergesLead || !prwait.IsReadyToMerge(p) {
			continue
		}
		k := strings.ToLower(v.Plan)
		out[k] = append(out[k], store.PRRef(p.Repo, p.Number))
	}
	return out, nil
}
