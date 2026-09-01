package store

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strconv"
	"strings"

	"github.com/jackc/pgx/v5"

	"github.com/mralaminahamed/codetrail/packages/shared/models"
)

// SpanID is hash(repo, path, start, end, digest), spec §3. Content-addressed
// for the same reason RepoID is: a retried job converges on the same rows, and
// a re-index of the same commit rewrites them instead of doubling the corpus.
//
// The line numbers go through hash's NUL separator as decimal text, so (1, 23)
// and (12, 3) cannot hash alike.
//
// Nothing in it names a chunking strategy, so one commit chunked two ways
// produces the same ids. That is why the eval's two arms get a database each —
// the settled decision — and why PutSpans scopes its rewrite to one repo_id
// rather than to the table.
func SpanID(repoID, path string, start, end int, digest string) string {
	return hash(repoID, path, strconv.Itoa(start), strconv.Itoa(end), digest)
}

// Digest is the full SHA-256 of a span's text, where the ids keep the first 16
// bytes. An id only has to be unique within a repo; the digest is what spec §8
// makes the proof that retrieved text is indexed text, and truncating it
// weakens that proof for no gain.
func Digest(text string) string {
	sum := sha256.Sum256([]byte(text))
	return hex.EncodeToString(sum[:])
}

// EmbeddedSpan is a span together with the vector computed from its Text. The
// two travel as one value because a span written without its embedding is
// invisible to every query the product has.
type EmbeddedSpan struct {
	models.Span
	Embedding []float32
}

// spanBatchSize bounds how many inserts go over the wire in one round trip. A
// repo is tens of thousands of spans and each carries a 768-component literal,
// so the whole set in one batch is a multi-megabyte message held in memory on
// both ends.
const spanBatchSize = 500

// PutSpans is authoritative for repoID: it deletes that repo's spans and writes
// the given set, in one transaction. Passing no spans therefore clears them.
//
// PutRepo only upserts, and recorded the consequence — a second index producing
// fewer rows leaves the first run's behind. For spans that is worse than
// untidy: re-chunking the same commit under a different window size would leave
// both runs' spans in one table and a retrieval would silently mix them.
//
// Widths are checked before the transaction opens. Rollback would undo the
// DELETE anyway, so this is not what keeps the previous spans safe — the
// transaction is; it is that a mis-configured embedder fails without a round
// trip, and with a message naming the span rather than a column.
//
// repo_id is written from the argument, and a span carrying a different RepoID
// is refused rather than relabelled: silently rewriting it would produce a row
// whose id is a hash of one repo sitting under another.
func (s *Store) PutSpans(ctx context.Context, repoID string, spans []EmbeddedSpan, model string, dim int) error {
	// The same guard the caller ran at startup, on the write path this time:
	// an embedder of another width must not reach the table by handing its own
	// dim to a batch of matching vectors.
	if err := CheckDim(dim); err != nil {
		return fmt.Errorf("model %s: %w", model, err)
	}
	for _, sp := range spans {
		if sp.RepoID != repoID {
			return fmt.Errorf("span %s (%s:%d-%d) belongs to repo %s, not %s",
				sp.ID, sp.Path, sp.StartLine, sp.EndLine, sp.RepoID, repoID)
		}
		if len(sp.Embedding) != dim {
			return fmt.Errorf("%w: span %s (%s:%d-%d) has %d components, model %s produces %d",
				ErrDimMismatch, sp.ID, sp.Path, sp.StartLine, sp.EndLine, len(sp.Embedding), model, dim)
		}
	}

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)

	if _, err := tx.Exec(ctx, `DELETE FROM spans WHERE repo_id = $1`, repoID); err != nil {
		return fmt.Errorf("clearing spans for %s: %w", repoID, err)
	}
	for lo := 0; lo < len(spans); lo += spanBatchSize {
		hi := min(lo+spanBatchSize, len(spans))
		var b pgx.Batch
		for _, sp := range spans[lo:hi] {
			// The DELETE above means a retry never conflicts. The upsert is for
			// a duplicate inside one call, which has to converge rather than
			// abort a job at its last step.
			b.Queue(`
				INSERT INTO spans (id, repo_id, file_id, path, kind, symbol,
					start_line, end_line, text, digest, embed_model, embed_dim, embedding)
				VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13::vector)
				ON CONFLICT (id) DO UPDATE SET
					file_id = EXCLUDED.file_id, kind = EXCLUDED.kind,
					symbol = EXCLUDED.symbol, text = EXCLUDED.text,
					embed_model = EXCLUDED.embed_model, embed_dim = EXCLUDED.embed_dim,
					embedding = EXCLUDED.embedding`,
				sp.ID, repoID, sp.FileID, sp.Path, string(sp.Kind), sp.Symbol,
				sp.StartLine, sp.EndLine, sp.Text, sp.Digest, model, dim, vecLiteral(sp.Embedding))
		}
		if err := tx.SendBatch(ctx, &b).Close(); err != nil {
			return fmt.Errorf("spans %d-%d: %w", lo, hi-1, err)
		}
	}
	return tx.Commit(ctx)
}

// CountSpans is how many spans one repo has — the number PutSpans's
// replacement contract is stated in.
func (s *Store) CountSpans(ctx context.Context, repoID string) (int, error) {
	var n int
	err := s.pool.QueryRow(ctx, `SELECT count(*) FROM spans WHERE repo_id = $1`, repoID).Scan(&n)
	return n, err
}

// vecLiteral renders a vector in pgvector's text form, "[1,-0.5]". There is no
// pgvector binding in this build, so the value crosses as a Go string; 'f' with
// precision -1 is the shortest decimal that parses back to the same float32,
// which is what makes that crossing lossless.
//
// The INSERT's $13::vector is documentation, not machinery. Measured: without
// it every test here still passes, because Postgres infers the parameter's type
// from the column it is being written to. The cast is kept so a reader does not
// have to know that, and so moving the parameter somewhere with nothing to
// infer from does not quietly become a text comparison.
func vecLiteral(v []float32) string {
	var b strings.Builder
	b.Grow(len(v)*10 + 2)
	b.WriteByte('[')
	for i, f := range v {
		if i > 0 {
			b.WriteByte(',')
		}
		b.WriteString(strconv.FormatFloat(float64(f), 'f', -1, 32))
	}
	b.WriteByte(']')
	return b.String()
}
