package store

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/mralaminahamed/codetrail/packages/shared/models"
)

// RepoRow is a repo as the corpus listing shows it: the row plus the LRU clock
// eviction reads. LastUsedAt is on the wire because it is the only thing that
// explains why a repository a caller had a link to is gone.
type RepoRow struct {
	models.Repo
	LastUsedAt time.Time
}

// ListRepos is the corpus, most recently used first — on the same column Evict
// reads, so the listing's tail is what eviction takes next.
//
// The same column, not the same ORDER BY: the id tiebreak is this query's own
// and Evict has none. PutRepo stamps last_queried_at from now() once per
// transaction, so a batch of repos indexed inside one clock tick ties, and two
// listings of an unchanged corpus would otherwise differ. Eviction needs no
// such tiebreak — a tie spanning the keep boundary means two equally stale
// repositories and either may go — so the listing's tail names what goes next
// as a set, and which of two tied rows leads it is not a promise about which
// one eviction takes.
func (s *Store) ListRepos(ctx context.Context, limit int) ([]RepoRow, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT id, remote, ref, commit_sha, size_bytes, indexed_at, last_queried_at
		FROM repos
		ORDER BY last_queried_at DESC, id
		LIMIT $1`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []RepoRow
	for rows.Next() {
		var r RepoRow
		if err := rows.Scan(&r.ID, &r.Remote, &r.Ref, &r.Commit, &r.SizeBytes,
			&r.IndexedAt, &r.LastUsedAt); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// Stats is what a repo view can say about coverage. Separate numbers rather
// than a ratio: chunk.Chunks drops token-less regions, so a repo's span count
// is not derivable from its file count and any single "coverage" figure would
// be inventing the relationship between them.
//
// The provenance split is an aggregate over a per-row column, not a per-repo
// label (spec:190). It is the only place a user can see that three packages
// resolved and two did not; the label itself stays on the edge, and every
// caller row carries it too so a client cannot mistake the summary for it.
type Stats struct {
	Files          int
	Spans          int
	FilesWithSpans int
	Symbols        int
	Edges          int
	EdgesResolved  int
	EdgesSyntactic int
}

// RepoStats counts what one repository actually holds, rather than reading
// repos.file_count: PutRepo upserts and never deletes, so after a re-index that
// produced fewer files the column disagrees with the rows. The rows are what
// retrieval searches.
// Each provenance is counted by its own predicate rather than one as the
// total minus the other: spec §6's open question 8 wants a third label for a
// call that resolves outside the corpus, and a subtraction would file it under
// syntactic without anything looking wrong.
func (s *Store) RepoStats(ctx context.Context, repoID string) (Stats, error) {
	var st Stats
	err := s.pool.QueryRow(ctx, `
		SELECT (SELECT count(*) FROM files WHERE repo_id = $1),
		       (SELECT count(*) FROM spans WHERE repo_id = $1),
		       (SELECT count(DISTINCT file_id) FROM spans WHERE repo_id = $1),
		       (SELECT count(*) FROM symbols WHERE repo_id = $1),
		       (SELECT count(*) FROM edges WHERE repo_id = $1),
		       (SELECT count(*) FROM edges WHERE repo_id = $1 AND provenance = 'resolved'),
		       (SELECT count(*) FROM edges WHERE repo_id = $1 AND provenance = 'syntactic')`,
		repoID).Scan(&st.Files, &st.Spans, &st.FilesWithSpans,
		&st.Symbols, &st.Edges, &st.EdgesResolved, &st.EdgesSyntactic)
	return st, err
}

// GetSpan reads one span of one repository.
//
// Scoped by repo_id even though the id is a primary key: a span id from another
// repository must be a 404 for this one, not a read across a corpus boundary
// the caller never named. Every other retrieval query is repo-scoped for the
// same reason.
//
// The embedding is not selected. It is 768 floats a caller cannot check
// anything with, and the digest is what makes the span checkable.
func (s *Store) GetSpan(ctx context.Context, repoID, spanID string) (models.Span, error) {
	var sp models.Span
	var kind string
	err := s.pool.QueryRow(ctx, `
		SELECT id, repo_id, file_id, path, kind, symbol, start_line, end_line, text, digest
		FROM spans
		WHERE repo_id = $1 AND id = $2`, repoID, spanID).
		Scan(&sp.ID, &sp.RepoID, &sp.FileID, &sp.Path, &kind, &sp.Symbol,
			&sp.StartLine, &sp.EndLine, &sp.Text, &sp.Digest)
	if errors.Is(err, pgx.ErrNoRows) {
		return models.Span{}, ErrNotFound
	}
	sp.Kind = models.SpanKind(kind)
	return sp, err
}
