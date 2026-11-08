package store

import (
	"context"
	"database/sql"
	"fmt"
	"time"
)

// Repo is a row of the repos table: what GitHub said about one repository.
type Repo struct {
	// Name is the nameWithOwner GitHub answered with, the key PR rows use.
	// It compares case-insensitively.
	Name string
	// DefaultBranch is the repo's default branch; "" if GitHub didn't say.
	DefaultBranch string
	// UpdatedAt is when the row was last written; zero means now.
	UpdatedAt time.Time
}

// UpsertRepos writes each repo, keyed on Name (any case), in one
// transaction. Every field is written as given, the name's case included.
func (s *Store) UpsertRepos(ctx context.Context, repos ...Repo) error {
	return s.writeTx(ctx, func(tx *sql.Tx) error {
		for _, r := range repos {
			if r.Name == "" {
				return fmt.Errorf("store: upsert repo: want a name")
			}
			_, err := tx.ExecContext(ctx, `
				INSERT INTO repos (name, default_branch, updated_at) VALUES (?, ?, ?)
				ON CONFLICT (name) DO UPDATE SET
					name = excluded.name,
					default_branch = excluded.default_branch,
					updated_at = excluded.updated_at`,
				r.Name, r.DefaultBranch, s.stamp(r.UpdatedAt).UnixMilli())
			if err != nil {
				return fmt.Errorf("store: upsert repo %s: %w", r.Name, err)
			}
		}
		return nil
	})
}

// ListRepos returns every repo, by name.
func (s *Store) ListRepos(ctx context.Context) ([]Repo, error) {
	rows, err := s.r.QueryContext(ctx, `SELECT name, default_branch, updated_at FROM repos ORDER BY name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Repo
	for rows.Next() {
		var r Repo
		if err := rows.Scan(&r.Name, &r.DefaultBranch, msTime{&r.UpdatedAt}); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}
