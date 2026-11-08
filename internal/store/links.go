package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"
)

// Link kinds.
const (
	LinkAgent        = "agent"
	LinkBranch       = "branch"
	LinkPR           = "pr"
	LinkWorktree     = "worktree"
	LinkCloudSession = "cloud_session"
)

// Match methods, most reliable first.
const (
	MatchSpawn    = "spawn"
	MatchPlanLine = "plan_line"
	MatchTitle    = "title"
	MatchBranch   = "branch"
	MatchManual   = "manual"
)

// Confidences.
const (
	ConfidenceExact = "exact"
	ConfidenceHigh  = "high"
	ConfidenceLow   = "low"
)

// BranchRef is the ref a branch link stores: "owner/repo:branch", with the
// repo as GitHub names it (nameWithOwner). When the repo's GitHub name isn't
// known, the main checkout's path stands in for it.
func BranchRef(repo, branch string) string { return repo + ":" + branch }

// AgentRef is the ref an agent link stores for a subagent or a session's
// main loop: "session_id/agent_id", with an empty agent id for the main
// loop ("session_id/").
func AgentRef(sessionID, agentID string) string { return sessionID + "/" + agentID }

// JobRef is the ref an agent link stores for a background session, which
// is keyed on its stable job id rather than its session id: "job:<id>",
// the same form as its session key.
func JobRef(jobID string) string { return "job:" + jobID }

// Link is a row of the links table: one way a package is tied to work.
type Link struct {
	ID     int64 // set by the store
	PlanID int64
	PkgID  string
	Kind   string // LinkAgent, LinkBranch, LinkPR, LinkWorktree, LinkCloudSession

	// An agent link names a subagent by (SessionID, AgentID), or a
	// background session by JobID with no AgentID.
	SessionID string
	JobID     string
	AgentID   string
	// Ref names the target:
	//   - pr: PRRef(repo, number), "owner/repo#123";
	//   - branch: BranchRef(repo, branch), "owner/repo:branch";
	//   - worktree: the worktree's canonical path;
	//   - agent: AgentRef(SessionID, AgentID), or JobRef(JobID) for a
	//     background session with no AgentID. The store fills it in when
	//     it's empty;
	//   - cloud_session: the session URL.
	Ref string

	MatchMethod string    // MatchSpawn … MatchManual
	Confidence  string    // ConfidenceExact, ConfidenceHigh, ConfidenceLow
	CreatedAt   time.Time // zero means now

	// Inferred is true for a link the matcher (internal/state) made, which
	// it rewrites on every run (ReplaceInferredLinks). It is false for a
	// link written with UpsertLink or UpdateLink (the mod's spawn links,
	// your manual links), which the matcher never changes. Set by the
	// store.
	Inferred bool
}

// normalize fills in an agent link's Ref from its ids.
func (l *Link) normalize() {
	if l.Kind == LinkAgent && l.Ref == "" {
		switch {
		case l.SessionID != "":
			l.Ref = AgentRef(l.SessionID, l.AgentID)
		case l.JobID != "" && l.AgentID == "":
			l.Ref = JobRef(l.JobID)
		}
	}
}

func (l Link) validate() error {
	switch l.Kind {
	case LinkAgent:
		if l.SessionID == "" && l.JobID == "" {
			return errors.New("an agent link needs a session id or a job id")
		}
	case LinkBranch, LinkPR, LinkWorktree, LinkCloudSession:
		if l.Ref == "" {
			return fmt.Errorf("a %s link needs a ref", l.Kind)
		}
	default:
		return fmt.Errorf("unknown link kind %q", l.Kind)
	}
	switch l.MatchMethod {
	case MatchSpawn, MatchPlanLine, MatchTitle, MatchBranch, MatchManual:
	default:
		return fmt.Errorf("unknown match method %q", l.MatchMethod)
	}
	switch l.Confidence {
	case ConfidenceExact, ConfidenceHigh, ConfidenceLow:
	default:
		return fmt.Errorf("unknown confidence %q", l.Confidence)
	}
	if l.PkgID == "" {
		return errors.New("empty package id")
	}
	return nil
}

const linkColumns = `id, plan_id, pkg_id, kind, session_id, job_id, agent_id, ref,
	match_method, confidence, created_at, inferred`

func scanLink(sc scanner) (Link, error) {
	var l Link
	err := sc.Scan(&l.ID, &l.PlanID, &l.PkgID, &l.Kind, &l.SessionID, &l.JobID, &l.AgentID, &l.Ref,
		&l.MatchMethod, &l.Confidence, msTime{&l.CreatedAt}, &l.Inferred)
	return l, err
}

// UpsertLink stores a link and returns its id. A link to the same package,
// of the same kind, with the same target (SessionID, JobID, AgentID, Ref)
// is the same link: its MatchMethod and Confidence are updated and its id
// and CreatedAt kept. Either way the link is not Inferred afterwards, so the
// matcher leaves it alone. The package must exist (ErrNotFound otherwise);
// the link takes the package ID as the package spells it.
//
// A manual link (MatchManual) also clears the tombstone of the same link,
// if you had unlinked it before.
func (s *Store) UpsertLink(ctx context.Context, l Link) (int64, error) {
	l.normalize()
	if err := l.validate(); err != nil {
		return 0, fmt.Errorf("store: upsert link: %w", err)
	}
	var id int64
	err := s.writeTx(ctx, func(tx *sql.Tx) error {
		err := tx.QueryRowContext(ctx, `
			INSERT INTO links (plan_id, pkg_id, kind, session_id, job_id, agent_id, ref, match_method, confidence, created_at, inferred)
			SELECT p.plan_id, p.pkg_id, ?, ?, ?, ?, ?, ?, ?, ?, 0
			FROM packages AS p WHERE p.plan_id = ? AND p.pkg_id = ?
			ON CONFLICT (plan_id, pkg_id, kind, session_id, job_id, agent_id, ref) DO UPDATE SET
				match_method = excluded.match_method,
				confidence = excluded.confidence,
				inferred = 0
			RETURNING id`,
			l.Kind, l.SessionID, l.JobID, l.AgentID, l.Ref, l.MatchMethod, l.Confidence,
			s.stamp(l.CreatedAt).UnixMilli(), l.PlanID, l.PkgID).Scan(&id)
		if err != nil {
			return notFound(err, "package %d/%s", l.PlanID, l.PkgID)
		}
		if l.MatchMethod == MatchManual {
			if _, err := tx.ExecContext(ctx, `
				DELETE FROM link_tombstones
				WHERE plan_id = ? AND pkg_id = ? AND kind = ? AND session_id = ? AND job_id = ? AND agent_id = ? AND ref = ?`,
				l.PlanID, l.PkgID, l.Kind, l.SessionID, l.JobID, l.AgentID, l.Ref); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return 0, fmt.Errorf("store: upsert link %d/%s %s: %w", l.PlanID, l.PkgID, l.Kind, err)
	}
	return id, nil
}

// UpdateLink rewrites the link with l.ID: its package (within the same
// plan, spelled as the package spells it), kind, target, method and
// confidence. PlanID and CreatedAt are kept, and the link is no longer
// Inferred.
func (s *Store) UpdateLink(ctx context.Context, l Link) error {
	l.normalize()
	if err := l.validate(); err != nil {
		return fmt.Errorf("store: update link %d: %w", l.ID, err)
	}
	res, err := s.w.ExecContext(ctx, `
		UPDATE links SET
			pkg_id = coalesce((SELECT p.pkg_id FROM packages AS p WHERE p.plan_id = links.plan_id AND p.pkg_id = ?), ?),
			kind = ?, session_id = ?, job_id = ?, agent_id = ?, ref = ?,
			match_method = ?, confidence = ?, inferred = 0
		WHERE id = ?`,
		l.PkgID, l.PkgID, l.Kind, l.SessionID, l.JobID, l.AgentID, l.Ref, l.MatchMethod, l.Confidence, l.ID)
	if err != nil {
		return fmt.Errorf("store: update link %d: %w", l.ID, err)
	}
	return mustAffect(res, "link %d", l.ID)
}

// DeleteLink deletes the link with the given id. It leaves no tombstone, so
// the matcher may make the link again; use Unlink for "you removed this".
func (s *Store) DeleteLink(ctx context.Context, id int64) error {
	res, err := s.w.ExecContext(ctx, `DELETE FROM links WHERE id = ?`, id)
	if err != nil {
		return fmt.Errorf("store: delete link %d: %w", id, err)
	}
	return mustAffect(res, "link %d", id)
}

// ListLinks returns a plan's links by package, then id.
func (s *Store) ListLinks(ctx context.Context, planID int64) ([]Link, error) {
	rows, err := s.r.QueryContext(ctx, `SELECT `+linkColumns+` FROM links WHERE plan_id = ? ORDER BY pkg_id, id`, planID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Link
	for rows.Next() {
		l, err := scanLink(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, l)
	}
	return out, rows.Err()
}

// Tombstone records that you removed a link: the matcher never makes that
// link again. Its fields are the link's identity.
type Tombstone struct {
	PlanID    int64
	PkgID     string
	Kind      string
	SessionID string
	JobID     string
	AgentID   string
	Ref       string    // as Link.Ref; filled in for an agent the same way
	CreatedAt time.Time // zero means now
}

func (t Tombstone) link() Link {
	return Link{PlanID: t.PlanID, PkgID: t.PkgID, Kind: t.Kind, SessionID: t.SessionID, JobID: t.JobID,
		AgentID: t.AgentID, Ref: t.Ref, MatchMethod: MatchManual, Confidence: ConfidenceExact}
}

// Unlink removes the link the tombstone names, if it is stored, and records
// the tombstone, in one transaction. The link doesn't have to be stored: a
// link the board shows because it was reached through another one (an
// agent in a linked worktree, say) is unlinked the same way. Unlinking
// twice is not an error. The package must exist (ErrNotFound otherwise).
func (s *Store) Unlink(ctx context.Context, t Tombstone) error {
	l := t.link()
	l.normalize()
	if err := l.validate(); err != nil {
		return fmt.Errorf("store: unlink: %w", err)
	}
	err := s.writeTx(ctx, func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx, `
			INSERT INTO link_tombstones (plan_id, pkg_id, kind, session_id, job_id, agent_id, ref, created_at)
			SELECT p.plan_id, p.pkg_id, ?, ?, ?, ?, ?, ?
			FROM packages AS p WHERE p.plan_id = ? AND p.pkg_id = ?
			ON CONFLICT (plan_id, pkg_id, kind, session_id, job_id, agent_id, ref) DO NOTHING`,
			l.Kind, l.SessionID, l.JobID, l.AgentID, l.Ref, s.stamp(t.CreatedAt).UnixMilli(), l.PlanID, l.PkgID)
		if err != nil {
			return err
		}
		if n, err := res.RowsAffected(); err != nil {
			return err
		} else if n == 0 {
			var one int
			if err := tx.QueryRowContext(ctx, `SELECT 1 FROM packages WHERE plan_id = ? AND pkg_id = ?`, l.PlanID, l.PkgID).Scan(&one); err != nil {
				return notFound(err, "package %d/%s", l.PlanID, l.PkgID)
			}
		}
		_, err = tx.ExecContext(ctx, `
			DELETE FROM links
			WHERE plan_id = ? AND pkg_id = ? AND kind = ? AND session_id = ? AND job_id = ? AND agent_id = ? AND ref = ?`,
			l.PlanID, l.PkgID, l.Kind, l.SessionID, l.JobID, l.AgentID, l.Ref)
		return err
	})
	if err != nil {
		return fmt.Errorf("store: unlink %d/%s %s %s: %w", l.PlanID, l.PkgID, l.Kind, l.Ref, err)
	}
	return nil
}

// ListTombstones returns a plan's tombstones by package, then kind and
// ref.
func (s *Store) ListTombstones(ctx context.Context, planID int64) ([]Tombstone, error) {
	rows, err := s.r.QueryContext(ctx, `
		SELECT plan_id, pkg_id, kind, session_id, job_id, agent_id, ref, created_at
		FROM link_tombstones WHERE plan_id = ?
		ORDER BY pkg_id, kind, ref, session_id, job_id, agent_id`, planID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Tombstone
	for rows.Next() {
		var t Tombstone
		if err := rows.Scan(&t.PlanID, &t.PkgID, &t.Kind, &t.SessionID, &t.JobID, &t.AgentID, &t.Ref, msTime{&t.CreatedAt}); err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

// LinkChanges counts what ReplaceInferredLinks did.
type LinkChanges struct {
	Added, Updated, Removed int
}

// linkTarget is a link's identity within a plan: package (upper case),
// kind and target.
type linkTarget struct {
	pkg, kind, session, job, agent, ref string
}

func targetOf(l Link) linkTarget {
	return linkTarget{strings.ToUpper(l.PkgID), l.Kind, l.SessionID, l.JobID, l.AgentID, l.Ref}
}

// ReplaceInferredLinks makes the plan's inferred links exactly links, in
// one transaction: the matcher's whole answer for the plan. Each link is
// inserted (Inferred, keeping its CreatedAt or now), or updated when its
// method or confidence changed, and every other inferred link of the plan
// is deleted. A link is skipped when it has a tombstone, when the same
// link is already stored as not inferred (a spawn or manual link wins), or
// when its package doesn't exist. Links are validated first; an invalid one
// fails the whole call.
func (s *Store) ReplaceInferredLinks(ctx context.Context, planID int64, links []Link) (LinkChanges, error) {
	var ch LinkChanges
	want := make([]Link, 0, len(links))
	for _, l := range links {
		l.PlanID = planID
		l.normalize()
		if err := l.validate(); err != nil {
			return ch, fmt.Errorf("store: replace inferred links of plan %d: %w", planID, err)
		}
		want = append(want, l)
	}
	err := s.writeTx(ctx, func(tx *sql.Tx) error {
		have := map[linkTarget]Link{}
		rows, err := tx.QueryContext(ctx, `SELECT `+linkColumns+` FROM links WHERE plan_id = ?`, planID)
		if err != nil {
			return err
		}
		for rows.Next() {
			l, err := scanLink(rows)
			if err != nil {
				rows.Close()
				return err
			}
			have[targetOf(l)] = l
		}
		if err := rows.Close(); err != nil {
			return err
		}
		dead := map[linkTarget]bool{}
		rows, err = tx.QueryContext(ctx, `SELECT pkg_id, kind, session_id, job_id, agent_id, ref FROM link_tombstones WHERE plan_id = ?`, planID)
		if err != nil {
			return err
		}
		for rows.Next() {
			var t linkTarget
			if err := rows.Scan(&t.pkg, &t.kind, &t.session, &t.job, &t.agent, &t.ref); err != nil {
				rows.Close()
				return err
			}
			t.pkg = strings.ToUpper(t.pkg)
			dead[t] = true
		}
		if err := rows.Close(); err != nil {
			return err
		}

		keep := map[int64]bool{}
		for _, l := range want {
			k := targetOf(l)
			if dead[k] {
				continue
			}
			if old, ok := have[k]; ok {
				keep[old.ID] = true
				if !old.Inferred || (old.MatchMethod == l.MatchMethod && old.Confidence == l.Confidence) {
					continue
				}
				if _, err := tx.ExecContext(ctx, `UPDATE links SET match_method = ?, confidence = ? WHERE id = ?`,
					l.MatchMethod, l.Confidence, old.ID); err != nil {
					return err
				}
				ch.Updated++
				continue
			}
			var id int64
			err := tx.QueryRowContext(ctx, `
				INSERT INTO links (plan_id, pkg_id, kind, session_id, job_id, agent_id, ref, match_method, confidence, created_at, inferred)
				SELECT p.plan_id, p.pkg_id, ?, ?, ?, ?, ?, ?, ?, ?, 1
				FROM packages AS p WHERE p.plan_id = ? AND p.pkg_id = ?
				RETURNING id`,
				l.Kind, l.SessionID, l.JobID, l.AgentID, l.Ref, l.MatchMethod, l.Confidence,
				s.stamp(l.CreatedAt).UnixMilli(), planID, l.PkgID).Scan(&id)
			if errors.Is(err, sql.ErrNoRows) {
				continue // the package is gone
			}
			if err != nil {
				return err
			}
			l.ID, l.Inferred = id, true
			have[k] = l
			keep[id] = true
			ch.Added++
		}
		for _, old := range have {
			if old.Inferred && !keep[old.ID] {
				if _, err := tx.ExecContext(ctx, `DELETE FROM links WHERE id = ?`, old.ID); err != nil {
					return err
				}
				ch.Removed++
			}
		}
		return nil
	})
	if err != nil {
		return LinkChanges{}, fmt.Errorf("store: replace inferred links of plan %d: %w", planID, err)
	}
	return ch, nil
}
