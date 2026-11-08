package github

import (
	"time"
)

// State is a pull request's state.
type State string

// PR states, as GitHub names them.
const (
	StateOpen   State = "OPEN"
	StateMerged State = "MERGED"
	StateClosed State = "CLOSED" // closed without merging
)

// Mergeability is GitHub's `mergeable` field.
//
// GitHub computes it lazily: the first read of an open PR after a change
// often answers UNKNOWN and starts the computation, and the next read a few
// seconds to a minute later has the answer. So UNKNOWN is exposed as
// MergePending, never as "not mergeable": keep the last known value and ask
// again on the next poll. MERGED and CLOSED PRs are always MergePending,
// because GitHub never computes it for them.
type Mergeability string

// Mergeability values. Any value GitHub sends that isn't MERGEABLE or
// CONFLICTING is read as MergePending.
const (
	Mergeable    Mergeability = "MERGEABLE"
	Conflicting  Mergeability = "CONFLICTING"
	MergePending Mergeability = "UNKNOWN"
)

// Pending reports whether GitHub hasn't computed mergeability yet (or never
// will, for a merged or closed PR).
func (m Mergeability) Pending() bool { return m != Mergeable && m != Conflicting }

// MergeState is GitHub's `mergeStateStatus`, which combines mergeability,
// checks, reviews and draft state. MergeStateClean on an open PR is the
// "green and mergeable" signal: no conflict, and checks passing or none
// required.
type MergeState string

// MergeState values, as GitHub names them.
const (
	MergeStateClean    MergeState = "CLEAN"     // mergeable; checks passing or none
	MergeStateDirty    MergeState = "DIRTY"     // merge conflict
	MergeStateUnstable MergeState = "UNSTABLE"  // mergeable, but non-required checks failing
	MergeStateBlocked  MergeState = "BLOCKED"   // a required check or review is missing
	MergeStateBehind   MergeState = "BEHIND"    // head is behind base and the repo requires it up to date
	MergeStateDraft    MergeState = "DRAFT"     // draft PR
	MergeStateHasHooks MergeState = "HAS_HOOKS" // mergeable, with pre-receive hooks
	MergeStateUnknown  MergeState = "UNKNOWN"   // not computed yet; always so for merged and closed PRs
)

// Checks is the state of a PR head's status check rollup.
type Checks string

// Checks values. ChecksNone is not a GitHub value: it stands for a null
// statusCheckRollup, meaning the head commit has no checks at all (the repo
// has no CI). Treat it as fine, not as failing or pending.
const (
	ChecksNone     Checks = "NONE"
	ChecksSuccess  Checks = "SUCCESS"
	ChecksFailure  Checks = "FAILURE"
	ChecksError    Checks = "ERROR"
	ChecksPending  Checks = "PENDING"
	ChecksExpected Checks = "EXPECTED"
)

// ReviewDecision is GitHub's `reviewDecision`. It is empty when the repo has
// no review rule, as on many personal and small-team repos.
type ReviewDecision string

// ReviewDecision values, as GitHub names them.
const (
	ReviewNone             ReviewDecision = ""
	ReviewApproved         ReviewDecision = "APPROVED"
	ReviewChangesRequested ReviewDecision = "CHANGES_REQUESTED"
	ReviewRequired         ReviewDecision = "REVIEW_REQUIRED"
)

// PR is one pull request, as read by BranchPRs or OpenPRs.
//
// The body is fetched only to find the `Plan: <slug> · <ID>` line and is not
// kept: Plan holds what was parsed from it.
type PR struct {
	// Repo is the nameWithOwner GitHub answers with. After a transfer it
	// names the new owner even when the local remote still names the old
	// one, so key stored PRs on it.
	Repo   string
	Number int
	State  State
	Draft  bool

	Mergeable      Mergeability
	MergeState     MergeState
	ReviewDecision ReviewDecision
	Checks         Checks

	HeadRef string // head branch name
	HeadOID string // head commit SHA (headRefOid)
	BaseRef string // base branch name; not always the default branch

	// CrossRepository is true when the head branch lives in a fork. BranchPRs
	// drops such PRs; OpenPRs keeps them.
	CrossRepository bool

	Title  string
	URL    string
	Author string // login; empty for a deleted account

	CreatedAt time.Time
	UpdatedAt time.Time
	MergedAt  time.Time // zero unless merged
	ClosedAt  time.Time // zero while open; set for merged PRs too

	// Plan is the package named by the body's Plan: line; zero if there is
	// none.
	Plan PlanRef
}

// rawPR is the PRFields fragment as GitHub sends it.
type rawPR struct {
	Repository *struct {
		NameWithOwner string `json:"nameWithOwner"`
	} `json:"repository"`
	Number            int     `json:"number"`
	State             string  `json:"state"`
	IsDraft           bool    `json:"isDraft"`
	Mergeable         string  `json:"mergeable"`
	MergeStateStatus  string  `json:"mergeStateStatus"`
	ReviewDecision    *string `json:"reviewDecision"`
	StatusCheckRollup *struct {
		State string `json:"state"`
	} `json:"statusCheckRollup"`
	HeadRefName       string `json:"headRefName"`
	HeadRefOid        string `json:"headRefOid"`
	BaseRefName       string `json:"baseRefName"`
	IsCrossRepository bool   `json:"isCrossRepository"`
	Title             string `json:"title"`
	URL               string `json:"url"`
	Author            *struct {
		Login string `json:"login"`
	} `json:"author"`
	Body      string     `json:"body"`
	CreatedAt time.Time  `json:"createdAt"`
	UpdatedAt time.Time  `json:"updatedAt"`
	MergedAt  *time.Time `json:"mergedAt"`
	ClosedAt  *time.Time `json:"closedAt"`
}

// toPR converts a raw node. repo is used when the node has no repository
// field (the per-branch query, where the repo is the parent).
func (r rawPR) toPR(repo string) PR {
	if r.Repository != nil && r.Repository.NameWithOwner != "" {
		repo = r.Repository.NameWithOwner
	}
	pr := PR{
		Repo:            repo,
		Number:          r.Number,
		State:           State(r.State),
		Draft:           r.IsDraft,
		Mergeable:       Mergeability(r.Mergeable),
		MergeState:      MergeState(r.MergeStateStatus),
		Checks:          ChecksNone,
		HeadRef:         r.HeadRefName,
		HeadOID:         r.HeadRefOid,
		BaseRef:         r.BaseRefName,
		CrossRepository: r.IsCrossRepository,
		Title:           r.Title,
		URL:             r.URL,
		CreatedAt:       r.CreatedAt,
		UpdatedAt:       r.UpdatedAt,
	}
	if pr.Mergeable.Pending() {
		pr.Mergeable = MergePending
	}
	if pr.MergeState == "" {
		pr.MergeState = MergeStateUnknown
	}
	if r.ReviewDecision != nil {
		pr.ReviewDecision = ReviewDecision(*r.ReviewDecision)
	}
	if r.StatusCheckRollup != nil && r.StatusCheckRollup.State != "" {
		pr.Checks = Checks(r.StatusCheckRollup.State)
	}
	if r.Author != nil {
		pr.Author = r.Author.Login
	}
	if r.MergedAt != nil {
		pr.MergedAt = *r.MergedAt
	}
	if r.ClosedAt != nil {
		pr.ClosedAt = *r.ClosedAt
	}
	pr.Plan, _ = ParsePlanLine(r.Body)
	return pr
}

// prFields is the fragment every query uses: the PR's own fields plus
// body, which is read only for the Plan: line. No nested connections: they
// are what raises a batched query above 1 point.
const prFields = `fragment PRFields on PullRequest {
  number state isDraft mergeable mergeStateStatus reviewDecision
  statusCheckRollup { state }
  headRefName headRefOid baseRefName isCrossRepository
  title url author { login } body
  createdAt updatedAt mergedAt closedAt
}
`
