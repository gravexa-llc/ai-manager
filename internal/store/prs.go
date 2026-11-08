package store

import (
	"context"
	"database/sql"
	"fmt"
	"strconv"
	"strings"
	"time"
)

// PR states as GitHub reports them.
const (
	PROpen   = "OPEN"
	PRClosed = "CLOSED"
	PRMerged = "MERGED"
)

// PR is a row of the prs table: one pull request, keyed on (Repo, Number).
// String fields from GitHub's enums are stored as GitHub sends them.
type PR struct {
	Repo   string // GitHub's nameWithOwner, e.g. "owner/repo"
	Number int

	HeadBranch        string
	HeadRefOID        string
	BaseRef           string
	IsCrossRepository bool
	Title             string
	Author            string
	State             string // PROpen, PRClosed, PRMerged
	Draft             bool
	Mergeable         string // MERGEABLE | CONFLICTING | UNKNOWN
	MergeStateStatus  string // CLEAN | DIRTY | UNSTABLE | BLOCKED | BEHIND | DRAFT | UNKNOWN …
	ReviewDecision    string // "" when GitHub sends null
	ChecksState       string // statusCheckRollup.state; "" when null (no CI)

	// PlanSlug and PlanPkg come from the body's `Plan: <slug> · <ID>` line.
	// The body itself is never stored.
	PlanSlug string
	PlanPkg  string

	URL       string
	CreatedAt time.Time // GitHub's createdAt: when the PR was opened; zero if not stored
	UpdatedAt time.Time // GitHub's updatedAt
	MergedAt  time.Time
	ClosedAt  time.Time

	// FirstSeenAt is when aimd first stored the PR. UpsertPRs sets it on a
	// new row (to the given time, or now) and keeps it afterwards.
	FirstSeenAt time.Time

	// ReviewRequestedAt is set while your review is requested on the PR:
	// when aimd first saw the request (migration 0007). Only
	// SetReviewRequests writes it; UpsertPRs leaves it as stored.
	ReviewRequestedAt time.Time
}

// ReviewRequested reports whether your review is requested on the PR, as
// the last review-request search of its owner found.
func (p PR) ReviewRequested() bool { return !p.ReviewRequestedAt.IsZero() }

// PRKey names a pull request: GitHub's nameWithOwner and its number.
type PRKey struct {
	Repo   string
	Number int
}

// OpenedAt is when the PR was opened: GitHub's CreatedAt, or, for a row
// stored before aimd kept that, when aimd first stored it.
func (p PR) OpenedAt() time.Time {
	if !p.CreatedAt.IsZero() {
		return p.CreatedAt
	}
	return p.FirstSeenAt
}

// PRRef is the ref a pr link stores for a pull request: "owner/repo#123".
func PRRef(repo string, number int) string {
	return repo + "#" + strconv.Itoa(number)
}

// prColumns are the columns UpsertPRs writes; prSelect adds the one it
// leaves alone.
const (
	prColumns = `repo, number, head_branch, head_ref_oid, base_ref, is_cross_repository,
	title, author, state, draft, mergeable, merge_state_status, review_decision,
	checks_state, plan_slug, plan_pkg, url, updated_at, merged_at, closed_at, first_seen_at, created_at`
	prSelect = prColumns + `, review_requested_at`
)

func scanPR(sc scanner) (PR, error) {
	var p PR
	err := sc.Scan(&p.Repo, &p.Number, &p.HeadBranch, &p.HeadRefOID, &p.BaseRef, &p.IsCrossRepository,
		&p.Title, &p.Author, &p.State, &p.Draft, &p.Mergeable, &p.MergeStateStatus, &p.ReviewDecision,
		&p.ChecksState, &p.PlanSlug, &p.PlanPkg, &p.URL, msTime{&p.UpdatedAt}, msTime{&p.MergedAt}, msTime{&p.ClosedAt},
		msTime{&p.FirstSeenAt}, msTime{&p.CreatedAt}, msTime{&p.ReviewRequestedAt})
	return p, err
}

// UpsertPRs writes each pull request, keyed on (Repo, Number), in one
// transaction. Every field is written as given (the caller decides what to
// keep when GitHub answers UNKNOWN), except FirstSeenAt, which a stored PR
// keeps, a zero CreatedAt, which keeps the stored one, and
// ReviewRequestedAt, which only SetReviewRequests writes.
func (s *Store) UpsertPRs(ctx context.Context, prs ...PR) error {
	return s.writeTx(ctx, func(tx *sql.Tx) error {
		for _, p := range prs {
			if p.Repo == "" || p.Number <= 0 {
				return fmt.Errorf("store: upsert PR %q #%d: want a repo and a positive number", p.Repo, p.Number)
			}
			_, err := tx.ExecContext(ctx, `
				INSERT INTO prs (`+prColumns+`)
				VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
				ON CONFLICT (repo, number) DO UPDATE SET
					repo = excluded.repo,
					head_branch = excluded.head_branch,
					head_ref_oid = excluded.head_ref_oid,
					base_ref = excluded.base_ref,
					is_cross_repository = excluded.is_cross_repository,
					title = excluded.title,
					author = excluded.author,
					state = excluded.state,
					draft = excluded.draft,
					mergeable = excluded.mergeable,
					merge_state_status = excluded.merge_state_status,
					review_decision = excluded.review_decision,
					checks_state = excluded.checks_state,
					plan_slug = excluded.plan_slug,
					plan_pkg = excluded.plan_pkg,
					url = excluded.url,
					updated_at = excluded.updated_at,
					merged_at = excluded.merged_at,
					closed_at = excluded.closed_at,
					first_seen_at = coalesce(prs.first_seen_at, excluded.first_seen_at),
					created_at = coalesce(excluded.created_at, prs.created_at)`,
				p.Repo, p.Number, p.HeadBranch, p.HeadRefOID, p.BaseRef, p.IsCrossRepository,
				p.Title, p.Author, p.State, p.Draft, p.Mergeable, p.MergeStateStatus, p.ReviewDecision,
				p.ChecksState, p.PlanSlug, p.PlanPkg, p.URL, ms(p.UpdatedAt), ms(p.MergedAt), ms(p.ClosedAt),
				s.stamp(p.FirstSeenAt).UnixMilli(), ms(p.CreatedAt))
			if err != nil {
				return fmt.Errorf("store: upsert PR %s: %w", PRRef(p.Repo, p.Number), err)
			}
		}
		return nil
	})
}

// GetPR returns one pull request.
func (s *Store) GetPR(ctx context.Context, repo string, number int) (PR, error) {
	p, err := scanPR(s.r.QueryRowContext(ctx, `SELECT `+prSelect+` FROM prs WHERE repo = ? AND number = ?`, repo, number))
	if err != nil {
		return PR{}, notFound(err, "PR %s", PRRef(repo, number))
	}
	return p, nil
}

// PRFilter narrows ListPRs. Empty fields don't filter.
type PRFilter struct {
	Repo       string
	HeadBranch string // case-sensitive; indexed together with Repo
	State      string // PROpen, PRClosed, PRMerged
	PlanSlug   string
}

// ListPRs returns pull requests by repo, then number.
func (s *Store) ListPRs(ctx context.Context, f PRFilter) ([]PR, error) {
	q := `SELECT ` + prSelect + ` FROM prs WHERE 1`
	var args []any
	for _, c := range []struct{ col, val string }{
		{"repo", f.Repo},
		{"head_branch", f.HeadBranch},
		{"state", f.State},
		{"plan_slug", f.PlanSlug},
	} {
		if c.val != "" {
			q += ` AND ` + c.col + ` = ?`
			args = append(args, c.val)
		}
	}
	q += ` ORDER BY repo, number`
	rows, err := s.r.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []PR
	for rows.Next() {
		p, err := scanPR(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// SetReviewRequests records what one review-request search found
// (migration 0007). owners are the owners whose search succeeded, and
// found the PRs it returned. Each PR in found gets ReviewRequestedAt = at
// (now if zero), unless it has one already; every other PR in a repo of
// one of owners loses it. PRs of other owners keep theirs, so a failed
// search changes nothing. A PR in found must be stored already (UpsertPRs
// it first); one that isn't, or whose owner isn't in owners, is skipped.
// It returns how many rows changed.
func (s *Store) SetReviewRequests(ctx context.Context, owners []string, found []PRKey, at time.Time) (int64, error) {
	if len(owners) == 0 {
		return 0, nil
	}
	at = s.stamp(at)
	searched := map[string]bool{}
	for _, o := range owners {
		searched[strings.ToLower(o)] = true
	}
	ownerSearched := func(repo string) bool {
		owner, _, _ := strings.Cut(repo, "/")
		return searched[strings.ToLower(owner)]
	}
	want := map[string]bool{}
	for _, k := range found {
		want[strings.ToLower(PRRef(k.Repo, k.Number))] = true
	}
	var changed int64
	err := s.writeTx(ctx, func(tx *sql.Tx) error {
		changed = 0
		exec := func(what string, k PRKey, q string, args ...any) error {
			res, err := tx.ExecContext(ctx, q, args...)
			if err != nil {
				return fmt.Errorf("store: %s review request on %s: %w", what, PRRef(k.Repo, k.Number), err)
			}
			n, _ := res.RowsAffected()
			changed += n
			return nil
		}
		for _, k := range found {
			if !ownerSearched(k.Repo) {
				continue
			}
			if err := exec("set", k, `UPDATE prs SET review_requested_at = ?
				WHERE repo = ? AND number = ? AND review_requested_at IS NULL`, at.UnixMilli(), k.Repo, k.Number); err != nil {
				return err
			}
		}
		rows, err := tx.QueryContext(ctx, `SELECT repo, number FROM prs WHERE review_requested_at IS NOT NULL`)
		if err != nil {
			return err
		}
		var gone []PRKey
		for rows.Next() {
			var k PRKey
			if err := rows.Scan(&k.Repo, &k.Number); err != nil {
				rows.Close()
				return err
			}
			if ownerSearched(k.Repo) && !want[strings.ToLower(PRRef(k.Repo, k.Number))] {
				gone = append(gone, k)
			}
		}
		if err := rows.Err(); err != nil {
			rows.Close()
			return err
		}
		if err := rows.Close(); err != nil {
			return err
		}
		for _, k := range gone {
			if err := exec("clear", k, `UPDATE prs SET review_requested_at = NULL WHERE repo = ? AND number = ?`, k.Repo, k.Number); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return 0, err
	}
	return changed, nil
}
