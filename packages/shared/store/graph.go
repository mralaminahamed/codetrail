package store

import (
	"context"
	"fmt"
	"strconv"

	"github.com/jackc/pgx/v5"

	"github.com/mralaminahamed/codetrail/packages/shared/models"
)

// SymbolID is hash(repo, path, start, kind, name), content-addressed for the
// reason SpanID is: a retried job converges on the same rows.
//
// It holds what makes the row unique and nothing else. A file has one
// declaration starting at a given line in the ordinary case, but not always —
// `type A int; type B int` on one line is legal, and so are two `init`
// functions in one file — so kind and name are in the key too. The end line is
// not: it changes when a body grows, and a definition whose identity changed
// with its implementation would look like a different definition to every edge
// pointing at it.
//
// Nothing in it names a chunking strategy, so it does not tie a symbol to a
// span the way sharing SpanID would.
func SymbolID(repoID, path string, start int, kind, name string) string {
	return hash(repoID, path, strconv.Itoa(start), kind, name)
}

// EdgeID is hash(repo, from, path, offset, to_name).
//
// The offset, not the line: `a(b(), b())` is two calls on one line, and since
// the id is what makes a row, a line key would merge them into one edge and
// lose a call site that exists.
func EdgeID(repoID, fromSymbolID, path string, offset int, toName string) string {
	return hash(repoID, fromSymbolID, path, strconv.Itoa(offset), toName)
}

// graphBatchSize bounds how many inserts go over the wire in one round trip.
// Larger than spanBatchSize because these rows carry no vector: the widest
// column is a source path.
const graphBatchSize = 1000

// PutGraph is authoritative for repoID: it deletes that repo's graph and writes
// the given one, in one transaction. Passing nothing therefore clears it.
//
// Edges first, then symbols, then the inserts in the other order. The edge
// delete **is** redundant under this writer, and the comment here used to claim
// it was not: edges.from_symbol_id cascades from symbols, every edge's tail is
// a symbol of the same repo, and the branch-wide sweep removed the statement
// with no test anywhere noticing. It is kept as the statement of order rather
// than as a guarantee — what makes the replacement whole is the symbols delete,
// and that is where a reader should look.
//
// The provenance/target pair is checked here as well as by the schema, and the
// two are not duplicates. The constraint is the guarantee — it holds against a
// future writer and a hand-run UPDATE. This is the error message: it names the
// offending call site instead of surfacing as edges_provenance_target on row
// 4,000 of a batch, with nothing to say which row.
//
// repo_id is written from the argument and a row carrying a different one is
// refused rather than relabelled, as PutSpans does: rewriting it would produce
// a row whose id is a hash of one repo sitting under another.
func (s *Store) PutGraph(ctx context.Context, repoID string, syms []models.Symbol, edges []models.Edge) error {
	for _, sy := range syms {
		if sy.RepoID != repoID {
			return fmt.Errorf("symbol %s (%s:%d %s) belongs to repo %s, not %s",
				sy.ID, sy.Path, sy.StartLine, sy.Name, sy.RepoID, repoID)
		}
	}
	for _, e := range edges {
		if e.RepoID != repoID {
			return fmt.Errorf("edge to %s (%s:%d) belongs to repo %s, not %s",
				e.ToName, e.Path, e.Line, e.RepoID, repoID)
		}
		if (e.ToSymbolID != "") != (e.Provenance == models.ProvenanceResolved) {
			return fmt.Errorf(
				"edge to %s at %s:%d is %q with target %q: a syntactic edge has a null target and a resolved edge points at a symbol row",
				e.ToName, e.Path, e.Line, e.Provenance, e.ToSymbolID)
		}
	}

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)

	if _, err := tx.Exec(ctx, `DELETE FROM edges WHERE repo_id = $1`, repoID); err != nil {
		return fmt.Errorf("clearing edges for %s: %w", repoID, err)
	}
	if _, err := tx.Exec(ctx, `DELETE FROM symbols WHERE repo_id = $1`, repoID); err != nil {
		return fmt.Errorf("clearing symbols for %s: %w", repoID, err)
	}

	for lo := 0; lo < len(syms); lo += graphBatchSize {
		hi := min(lo+graphBatchSize, len(syms))
		var b pgx.Batch
		for _, sy := range syms[lo:hi] {
			// DO UPDATE, not DO NOTHING, and only for the columns the id does
			// not already determine. The deletes above mean a re-run never
			// conflicts; this is for a duplicate inside one call, which has to
			// converge rather than abort a job at its last step.
			b.Queue(`
				INSERT INTO symbols (id, repo_id, file_id, path, name, pkg, kind,
					start_line, end_line, span_id)
				VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10)
				ON CONFLICT (id) DO UPDATE SET
					file_id = EXCLUDED.file_id, pkg = EXCLUDED.pkg,
					end_line = EXCLUDED.end_line, span_id = EXCLUDED.span_id`,
				sy.ID, repoID, sy.FileID, sy.Path, sy.Name, sy.Pkg, string(sy.Kind),
				sy.StartLine, sy.EndLine, nullable(sy.SpanID))
		}
		if err := tx.SendBatch(ctx, &b).Close(); err != nil {
			return fmt.Errorf("symbols %d-%d: %w", lo, hi-1, err)
		}
	}

	for lo := 0; lo < len(edges); lo += graphBatchSize {
		hi := min(lo+graphBatchSize, len(edges))
		var b pgx.Batch
		for _, e := range edges[lo:hi] {
			// DO NOTHING here would be the quiet bug: the ids are
			// deterministic, so a repository indexed once without the
			// type-checker and again with it would keep every edge's first,
			// syntactic label and nothing would look wrong.
			b.Queue(`
				INSERT INTO edges (id, repo_id, from_symbol_id, to_symbol_id, to_name,
					kind, provenance, path, line)
				VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9)
				ON CONFLICT (id) DO UPDATE SET
					to_symbol_id = EXCLUDED.to_symbol_id, kind = EXCLUDED.kind,
					provenance = EXCLUDED.provenance, line = EXCLUDED.line`,
				e.ID, repoID, e.FromSymbolID, nullable(e.ToSymbolID), e.ToName,
				string(e.Kind), string(e.Provenance), e.Path, e.Line)
		}
		if err := tx.SendBatch(ctx, &b).Close(); err != nil {
			return fmt.Errorf("edges %d-%d: %w", lo, hi-1, err)
		}
	}
	return tx.Commit(ctx)
}

// nullable maps the empty string to SQL NULL. A symbol with no span and an edge
// with no target both carry "" in Go — models has no pointers and a nil check
// at every read is a worse trade than one conversion here — and "" is not a
// value either column can legitimately hold, since both are hashes.
func nullable(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

// CountGraph is how many symbols and edges one repo has — the numbers
// PutGraph's replacement contract and eviction's cascade are stated in.
func (s *Store) CountGraph(ctx context.Context, repoID string) (symbols, edges int, err error) {
	err = s.pool.QueryRow(ctx, `
		SELECT (SELECT count(*) FROM symbols WHERE repo_id = $1),
		       (SELECT count(*) FROM edges WHERE repo_id = $1)`, repoID).Scan(&symbols, &edges)
	return symbols, edges, err
}
