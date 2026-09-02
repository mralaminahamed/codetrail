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

// ListRepos is the corpus, most recently used first — the same order Evict
// reads, so the listing's tail is what eviction takes next.
//
// The id tiebreak is not decoration: PutRepo stamps last_queried_at from now()
// once per transaction, so a batch of repos indexed inside one clock tick ties,
// and two listings of an unchanged corpus would otherwise differ.
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

// Stats is what a repo view can say about coverage. Three numbers rather than a
// ratio: chunk.Chunks drops token-less regions, so a repo's span count is not
// derivable from its file count and any single "coverage" figure would be
// inventing the relationship between them.
type Stats struct {
	Files          int
	Spans          int
	FilesWithSpans int
}

// RepoStats counts what one repository actually holds, rather than reading
// repos.file_count: PutRepo upserts and never deletes, so after a re-index that
// produced fewer files the column disagrees with the rows. The rows are what
// retrieval searches.
func (s *Store) RepoStats(ctx context.Context, repoID string) (Stats, error) {
	var st Stats
	err := s.pool.QueryRow(ctx, `
		SELECT (SELECT count(*) FROM files WHERE repo_id = $1),
		       (SELECT count(*) FROM spans WHERE repo_id = $1),
		       (SELECT count(DISTINCT file_id) FROM spans WHERE repo_id = $1)`,
		repoID).Scan(&st.Files, &st.Spans, &st.FilesWithSpans)
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
