package planapi

import (
	"context"

	"github.com/gravexa-llc/ai-manager/internal/store"
	"github.com/gravexa-llc/ai-manager/internal/web/api"
)

// HideUnmatched hides a PR or branch from a plan's Unmatched lane ("Not
// part of this plan" on the plan's page), or restores it, and runs a state
// pass so the board says so. It is the plan's alone: other plans still
// list the target, and nothing is linked or unlinked. The target needn't be
// in the lane now: one that is linked stays hidden for when it isn't.
// Hiding twice, or restoring what isn't hidden, changes nothing.
func (s *Service) HideUnmatched(ctx context.Context, req api.HideRequest) (api.HideResult, error) {
	switch req.Kind {
	case store.LinkPR, store.LinkBranch:
	case "":
		return api.HideResult{}, badRequest("kind is required: pr or branch")
	default:
		return api.HideResult{}, badRequest("only a pr or a branch is listed as unmatched, not %q", req.Kind)
	}
	l, err := s.parseTarget(ctx, req.Kind, req.Ref)
	if err != nil {
		return api.HideResult{}, err
	}
	p, err := s.planBySlug(ctx, req.Plan)
	if err != nil {
		return api.HideResult{}, err
	}
	res := api.HideResult{Plan: p.Slug, Target: api.Target{Kind: l.Kind, Ref: l.Ref}, Hidden: req.Hidden}
	hidden, err := s.st.ListHiddenUnmatched(ctx, p.ID)
	if err != nil {
		return api.HideResult{}, err
	}
	was := false
	for _, h := range hidden {
		was = was || (h.Kind == l.Kind && h.Ref == l.Ref)
	}
	if req.Hidden {
		if err := s.st.HideUnmatched(ctx, store.HiddenUnmatched{PlanID: p.ID, Kind: l.Kind, Ref: l.Ref, HiddenAt: s.now()}); err != nil {
			return api.HideResult{}, orNotFound(err, "no plan %q is registered", req.Plan)
		}
		res.Changed = !was
	} else {
		if res.Changed, err = s.st.UnhideUnmatched(ctx, p.ID, l.Kind, l.Ref); err != nil {
			return api.HideResult{}, err
		}
	}
	s.log.Info("unmatched", "plan", p.Slug, "kind", l.Kind, "ref", l.Ref, "hidden", req.Hidden, "changed", res.Changed)
	if err := s.runState(ctx); err != nil {
		return api.HideResult{}, err
	}
	return res, nil
}
