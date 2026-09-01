package store

import "context"

// Evict deletes every repo beyond the keep most recently queried and returns
// how many went.
//
// One DELETE, letting the schema's ON DELETE CASCADE take the dependent rows —
// files and spans today, and whatever P3's symbol graph adds by declaring the
// same reference. An orphan sweep would be a second system to disagree with
// this one.
//
// LRU, on last_queried_at, rather than on size: the biggest repo is not the
// least useful one. Nothing calls TouchRepo yet — no endpoint reads a repo
// before P3 — so in P1 that column holds each repo's first insert time and the
// order is effectively oldest-indexed-first.
//
// PutRepo does not move that column either, so re-indexing does not renew a
// repo's lease on the corpus: measured, a re-index of the least recent repo
// writes its rows and the eviction at the end of that same job deletes them
// again. Correct as LRU-on-queries — a re-index is not a query — and harmless
// while nothing reads repos, but it becomes visible in P3 the moment queries
// start moving the clock for real.
//
// A negative keep is rejected by Postgres ("OFFSET must not be negative")
// rather than clamped here, so a caller that computes one gets an error
// instead of an emptied corpus.
func (s *Store) Evict(ctx context.Context, keep int) (int, error) {
	tag, err := s.pool.Exec(ctx, `
		DELETE FROM repos WHERE id IN (
			SELECT id FROM repos
			ORDER BY last_queried_at DESC
			OFFSET $1
		)`, keep)
	if err != nil {
		return 0, err
	}
	return int(tag.RowsAffected()), nil
}

// CountRepos is the corpus size eviction bounds.
func (s *Store) CountRepos(ctx context.Context) (int, error) {
	var n int
	err := s.pool.QueryRow(ctx, `SELECT count(*) FROM repos`).Scan(&n)
	return n, err
}
