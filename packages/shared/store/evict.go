package store

import (
	"context"
	"fmt"
)

// Evict deletes every repo beyond the keep most recently queried, tombstones
// each one, and returns how many went.
//
// One DELETE, letting the schema's ON DELETE CASCADE take the dependent rows —
// files and spans today, and whatever P3's symbol graph adds by declaring the
// same reference. An orphan sweep would be a second system to disagree with
// this one.
//
// The tombstone is written by that same statement, through the DELETE's
// RETURNING, because spec §10 wants an evicted repo to answer 410 rather than
// 404 and the cascade leaves nothing else to tell the two apart. Two
// statements would leave a window: a crash between them commits the delete and
// loses the only record that the repo was ever here, which is the 404 this
// table exists to prevent. That is a counterfactual, not a behaviour
// difference — no test in this suite can distinguish it.
//
// The count is the INSERT's, since the INSERT is the outer statement. ON
// CONFLICT DO UPDATE rather than DO NOTHING for that reason as well as to
// refresh the row: a repo evicted, re-indexed at the same commit and evicted
// again hashes to the same id, and DO NOTHING would report a deletion that
// happened as 0.
//
// LRU, on last_queried_at, rather than on size: the biggest repo is not the
// least useful one. Both a query (TouchRepo) and an index (PutRepo) wind that
// column, so "least recently used" covers both ways a repo gets used — without
// the second, runJob's eviction deletes the rows the same job just wrote.
//
// Nothing calls TouchRepo yet, since no endpoint reads a repo before P3, so
// the column holds each repo's most recent index and the order is in practice
// least-recently-indexed-first.
//
// One column rather than GREATEST(last_queried_at, indexed_at), which would
// rank identically — PutRepo sets indexed_at to now() on the same writes — but
// could not use repos_lru_idx and would make this ordering an expression.
//
// A negative keep is rejected by Postgres ("OFFSET must not be negative")
// rather than clamped here, so a caller that computes one gets an error
// instead of an emptied corpus.
func (s *Store) Evict(ctx context.Context, keep, keepTombstones int) (int, error) {
	tag, err := s.pool.Exec(ctx, `
		WITH gone AS (
			DELETE FROM repos WHERE id IN (
				SELECT id FROM repos
				ORDER BY last_queried_at DESC
				OFFSET $1
			)
			RETURNING id, remote, ref, commit_sha, indexed_at
		)
		INSERT INTO evicted_repos (id, remote, ref, commit_sha, indexed_at, evicted_at)
		SELECT id, remote, ref, commit_sha, indexed_at, now() FROM gone
		ON CONFLICT (id) DO UPDATE SET
			remote = EXCLUDED.remote, ref = EXCLUDED.ref,
			commit_sha = EXCLUDED.commit_sha, indexed_at = EXCLUDED.indexed_at,
			evicted_at = EXCLUDED.evicted_at`, keep)
	if err != nil {
		return 0, err
	}
	n := int(tag.RowsAffected())

	// A second statement, unlike the tombstone itself. Measured on pg17: a trim
	// in the outer statement above reads the snapshot the CTE started from, so
	// asked to keep the 2 newest of 2 old rows plus 1 new one it deleted
	// nothing and left 3. And a crash between the two is recoverable — extra
	// tombstones, which the next eviction trims — where a lost one is not.
	if _, err := s.pool.Exec(ctx, `
		DELETE FROM evicted_repos WHERE id IN (
			SELECT id FROM evicted_repos
			ORDER BY evicted_at DESC, id DESC
			OFFSET $1
		)`, keepTombstones); err != nil {
		return n, fmt.Errorf("trimming tombstones: %w", err)
	}
	return n, nil
}

// RepoGone reports whether this id names a repo the corpus held and evicted,
// which is what lets a read answer 410 instead of 404 (spec §10). Past
// KEEP_TOMBSTONES it goes back to false, which is honest: we no longer
// remember.
//
// A repo id is hash(key, commit), so re-indexing the same commit re-uses the
// id and its tombstone outlives the eviction it recorded. The NOT EXISTS is
// what stops a repo that is back reading as gone, in whatever order a caller
// checks.
func (s *Store) RepoGone(ctx context.Context, id string) (bool, error) {
	var gone bool
	err := s.pool.QueryRow(ctx, `
		SELECT EXISTS (
			SELECT 1 FROM evicted_repos e
			WHERE e.id = $1 AND NOT EXISTS (SELECT 1 FROM repos r WHERE r.id = e.id)
		)`, id).Scan(&gone)
	return gone, err
}

// CountRepos is the corpus size eviction bounds.
func (s *Store) CountRepos(ctx context.Context) (int, error) {
	var n int
	err := s.pool.QueryRow(ctx, `SELECT count(*) FROM repos`).Scan(&n)
	return n, err
}
