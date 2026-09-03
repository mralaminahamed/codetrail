package store

import (
	"context"
	"crypto/sha1"
	"encoding/hex"
	"fmt"
)

// BlobHash is git's own blob hash of a file's bytes: sha1("blob <n>\x00" || body).
//
// Here rather than in the indexer because two things now derive it — the
// indexer, which writes files.blob before stripping, and the eval, which
// re-hashes the checkout its questions came from and compares. A second
// derivation would agree on the day it is written and drift the day the
// formula moves, and the whole point of that comparison is to catch a harness
// scored against a checkout nobody indexed.
func BlobHash(body []byte) string {
	h := sha1.New()
	fmt.Fprintf(h, "blob %d\x00", len(body))
	h.Write(body)
	return hex.EncodeToString(h.Sum(nil))
}

// SpanRange is one span's identity and its line range, which is all a gold set
// needs. Not models.Span: the text is what makes a whole-corpus read expensive
// and the gold rule never looks at it.
type SpanRange struct {
	ID        string
	Path      string
	StartLine int
	EndLine   int
}

// SpanText is one span's text, for a reader that has to scan the whole corpus.
type SpanText struct {
	ID   string
	Path string
	Text string
}

// FileBlobs is path to blob for one repository.
//
// The blob is hashed from the bytes the indexer read, before stripping, so two
// arms over one commit must agree on it exactly. RepoStats gives counts and
// counts are not the check: a count comparison passes when one arm ran under a
// different MAX_FILE_BYTES and skipped a file the other read.
func (s *Store) FileBlobs(ctx context.Context, repoID string) (map[string]string, error) {
	rows, err := s.pool.Query(ctx, `SELECT path, blob FROM files WHERE repo_id = $1`, repoID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]string{}
	for rows.Next() {
		var path, blob string
		if err := rows.Scan(&path, &blob); err != nil {
			return nil, err
		}
		out[path] = blob
	}
	return out, rows.Err()
}

// SpanRanges is every span of one repository, ordered.
//
// Ordered in SQL rather than in Go because the id set and every gold set are
// derived from this read and both have to be reproducible across runs. P3
// measured that an unordered span read comes back in path order, served from
// spans_path_idx — which is an order, not a guarantee, and the day the planner
// picks another index the eval's artefacts stop being diffable.
func (s *Store) SpanRanges(ctx context.Context, repoID string) ([]SpanRange, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT id, path, start_line, end_line
		FROM spans
		WHERE repo_id = $1
		ORDER BY path, start_line, id`, repoID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []SpanRange
	for rows.Next() {
		var r SpanRange
		if err := rows.Scan(&r.ID, &r.Path, &r.StartLine, &r.EndLine); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// SpanTexts is one keyset page of a repository's span texts, and the cursor
// for the next. An empty cursor back means the last page.
//
// Paged rather than whole-corpus because the caller reads every byte of text a
// repository has: rs/zerolog is 1,303 spans and about a megabyte, and a
// repository ten times that must not be one allocation. Keyed on id, which is
// the primary key, so the walk is total and cannot repeat a row.
func (s *Store) SpanTexts(ctx context.Context, repoID, after string, limit int) ([]SpanText, string, error) {
	if limit < 1 {
		return nil, "", fmt.Errorf("store: span text page size must be positive, got %d", limit)
	}
	rows, err := s.pool.Query(ctx, `
		SELECT id, path, text
		FROM spans
		WHERE repo_id = $1 AND id > $2
		ORDER BY id
		LIMIT $3`, repoID, after, limit)
	if err != nil {
		return nil, "", err
	}
	defer rows.Close()
	var out []SpanText
	for rows.Next() {
		var t SpanText
		if err := rows.Scan(&t.ID, &t.Path, &t.Text); err != nil {
			return nil, "", err
		}
		out = append(out, t)
	}
	if err := rows.Err(); err != nil {
		return nil, "", err
	}
	// A short page is the last page. Returning a cursor here would cost one
	// empty round trip per corpus; returning one on a full page is what makes
	// the walk terminate.
	if len(out) < limit {
		return out, "", nil
	}
	return out, out[len(out)-1].ID, nil
}
