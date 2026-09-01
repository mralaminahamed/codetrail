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
// least useful one. Both a query (TouchRepo) and an index (PutRepo) wind that
// column, so "least recently used" covers both ways a repo gets used — without
// the second, runJob's eviction deletes the rows the same job just wrote.
//
// Nothing calls TouchRepo yet, since no endpoint reads a repo before P3, so in
// P1 the column holds each repo's most recent index and the order is in
// practice least-recently-indexed-first.
//
// One column rather than GREATEST(last_queried_at, indexed_at), which would
// rank identically — PutRepo sets indexed_at to now() on the same writes — but
// could not use repos_lru_idx and would make this ordering an expression.
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
