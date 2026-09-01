package store

import (
	"context"
	"errors"
	"fmt"

	"github.com/mralaminahamed/codetrail/packages/shared/models"
)

// ErrMixedEmbedders is a repo whose spans were not all written by one
// embedder. It is a corpus that cannot be queried coherently: half of it is in
// a different vector space from the query, and every score is meaningless
// rather than low.
var ErrMixedEmbedders = errors.New("store: repo has spans from more than one embedder")

// VectorSearch ranks one repo's spans by cosine similarity to q (spec §8).
//
// The ORDER BY runs on the distance operator rather than on the returned
// similarity: only that form can use spans_embedding_idx, and sorting on the
// computed similarity is the same ordering by way of a sequential scan.
//
// What leaves this package is the similarity, `1 - distance`. The number is
// compared against a score floor and put in a response, and a floor on a
// quantity where lower is better is a filter whose sense is inverted and whose
// tests still pass.
//
// No kind filter and no kind weighting: kind=file does not say which chunking
// arm produced a row (P2 measured that the AST strategy emits it too), so a
// heuristic over it is a heuristic over a label that means two things.
func (s *Store) VectorSearch(ctx context.Context, repoID string, q []float32, limit int) ([]models.Cite, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT id, repo_id, file_id, path, kind, symbol, start_line, end_line,
		       text, digest, 1 - (embedding <=> $2::vector) AS score
		FROM spans
		WHERE repo_id = $1 AND embedding IS NOT NULL
		ORDER BY embedding <=> $2::vector
		LIMIT $3`, repoID, vecLiteral(q), limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []models.Cite
	for rows.Next() {
		var c models.Cite
		// embedding is nullable, so the similarity of a row written without one
		// is NULL and has no float destination. The WHERE clause above is what
		// keeps that off this scan; PutSpans cannot produce such a row, so the
		// guard is for a row written by something else.
		var score float64
		if err := rows.Scan(&c.ID, &c.RepoID, &c.FileID, &c.Path, &c.Kind, &c.Symbol,
			&c.StartLine, &c.EndLine, &c.Text, &c.Digest, &score); err != nil {
			return nil, err
		}
		c.Score = float32(score)
		out = append(out, c)
	}
	return out, rows.Err()
}

// SpanEmbedder names the embedder a repo's spans were actually written by, so
// a caller can refuse to rank them against a query embedded by another one.
//
// PutSpans replaces a repo's spans wholesale, so a repo is single-model by
// construction; this reads what is there rather than trusting that, because
// the failure it prevents — a query and a corpus in different vector spaces —
// produces confident nonsense and nothing about the query looks wrong. Spec §3
// says that of vector widths; it is equally true of models at one width.
//
// No spans is ErrNotFound: an unindexed or emptied repo has no embedder, which
// is a different thing from disagreeing about one.
func (s *Store) SpanEmbedder(ctx context.Context, repoID string) (string, int, error) {
	rows, err := s.pool.Query(ctx,
		`SELECT DISTINCT embed_model, embed_dim FROM spans WHERE repo_id = $1`, repoID)
	if err != nil {
		return "", 0, err
	}
	defer rows.Close()

	var model string
	var dim, n int
	for rows.Next() {
		if n++; n > 1 {
			return "", 0, fmt.Errorf("%w: %s", ErrMixedEmbedders, repoID)
		}
		if err := rows.Scan(&model, &dim); err != nil {
			return "", 0, err
		}
	}
	if err := rows.Err(); err != nil {
		return "", 0, err
	}
	if n == 0 {
		return "", 0, fmt.Errorf("%w: repo %s has no spans", ErrNotFound, repoID)
	}
	return model, dim, nil
}
