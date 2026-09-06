package store

import (
	"context"
	"fmt"
	"strconv"
	"strings"
)

// embeddingsByDigestSQL reads one vector per digest.
//
// digest = ANY($1), not an IN list with interpolated values: an IN whose text
// changes per call defeats the plan cache, and it is one quoting bug away from
// an injection in a package whose inputs are a stranger's file contents.
//
// ORDER BY digest, id and take the first per digest, so two rows sharing a
// digest resolve DETERMINISTICALLY. Without it the map's contents would depend
// on row order, and a corpus where two embedders wrote the same digest would
// produce different reuse on two runs — which would make
// TestIncrementalAndFullIndexProduceIdenticalRowsLive flaky and its failures
// unreadable.
const embeddingsByDigestSQL = `
	SELECT DISTINCT ON (digest) digest, embedding
	FROM spans
	WHERE digest = ANY($1) AND embed_model = $2 AND embed_dim = $3
	      AND embedding IS NOT NULL
	ORDER BY digest, id`

// EmbeddingsByDigest returns the vector already computed for each of these
// digests under this exact embedder, for the digests that have one.
//
// ONE ROUND TRIP for a whole job, not one per digest.
//
// NOT SCOPED TO A REPOSITORY, and the signature says so by having no repo
// parameter — a repoID argument that callers passed "" to would be a scope that
// looks enforced and is not. The read crosses repositories on purpose: a digest
// is a content hash, so the same declaration in two repositories has the same
// embedding, and a repo-scoped read would miss a fork, a vendored copy and a
// moved file.
//
// The risk that creates is real and is recorded rather than dismissed: one
// repository's indexing now depends on another repository's rows. The
// mitigations are that the caller re-checks the digest before accepting a
// vector, and that an embedding is a RANKING input rather than an
// authorisation one — a poisoned vector degrades a result set, it does not
// grant access.
//
// Filtered on embed_model AND embed_dim. Both, not either: spec §3's invariant
// is that store.CheckDim refuses an embedder of another width rather than
// letting two vector spaces share a table, "where they would rank nonsense
// confidently and nothing about the query would look wrong". A model-blind or
// width-blind reuse read does exactly what that invariant exists to prevent,
// one layer down — and rag.ErrModelMismatch would never fire, because the row's
// own embed_model column would still say the right thing.
func (s *Store) EmbeddingsByDigest(ctx context.Context, digests []string, model string, dim int) (map[string][]float32, error) {
	out := make(map[string][]float32, len(digests))
	if len(digests) == 0 {
		return out, nil
	}
	rows, err := s.pool.Query(ctx, embeddingsByDigestSQL, digests, model, dim)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var digest, raw string
		if err := rows.Scan(&digest, &raw); err != nil {
			return nil, err
		}
		v, err := parseVec(raw)
		if err != nil {
			return nil, fmt.Errorf("digest %s: %w", digest, err)
		}
		// A row whose width does not match what was asked for is a corrupt row
		// — embed_dim is a plain INTEGER with no CHECK tying it to the
		// vector(768) column — and lending its vector would put two vector
		// spaces in one corpus, which is what CheckDim exists to prevent.
		if len(v) != dim {
			return nil, fmt.Errorf("digest %s: row holds a %d-wide vector under embed_dim %d", digest, len(v), dim)
		}
		out[digest] = v
	}
	return out, rows.Err()
}

// parseVec is vecLiteral's inverse. There is no pgvector binding in this build,
// so a vector crosses as pgvector's text form, "[1,-0.5]", in both directions.
// 32-bit parsing, to match the 32-bit formatting that made the write lossless.
func parseVec(s string) ([]float32, error) {
	s = strings.TrimSpace(s)
	if len(s) < 2 || s[0] != '[' || s[len(s)-1] != ']' {
		return nil, fmt.Errorf("store: %q is not a pgvector literal", s)
	}
	s = s[1 : len(s)-1]
	if s == "" {
		return []float32{}, nil
	}
	parts := strings.Split(s, ",")
	out := make([]float32, 0, len(parts))
	for _, p := range parts {
		f, err := strconv.ParseFloat(strings.TrimSpace(p), 32)
		if err != nil {
			return nil, fmt.Errorf("store: %q is not a pgvector literal: %w", p, err)
		}
		out = append(out, float32(f))
	}
	return out, nil
}
