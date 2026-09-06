package store

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/mralaminahamed/codetrail/packages/shared/models"
)

// Caller is one definition that reaches the queried one, at its shortest
// distance, with the call site of its own hop.
//
// Depth is hops, not rows: a caller reachable by two paths is one caller. The
// call site is the one on the shortest path, because a citation for a longer
// route is a citation for a fact the row is not reporting.
type Caller struct {
	Symbol   models.Symbol
	Depth    int
	CallPath string
	CallLine int
}

// Approximate is one caller matched by name rather than by target: it calls
// something spelled ToName and nothing knows which one (spec:84).
//
// A separate type from Caller because it answers a different question, and
// merging the two would produce a list whose members are partly facts and
// partly coincidences with no field to tell them apart. It has no Depth for
// the same reason: traversing from a guess compounds it.
type Approximate struct {
	Symbol   models.Symbol
	ToName   string
	CallPath string
	CallLine int
}

// symbolCols is the one spelling of the symbols column list, aliased s in every
// read below. Two spellings would swap path and name silently — both are text
// — and the scan would still succeed.
const symbolCols = `s.id, s.repo_id, s.file_id, s.path, s.name, s.pkg, s.kind,
	s.start_line, s.end_line, s.span_id`

type scanner interface{ Scan(dest ...any) error }

// scanSymbol reads symbolCols and whatever the query appended after it.
// span_id is nullable and models.Symbol has no pointers, so the conversion
// happens here rather than at four call sites.
func scanSymbol(sc scanner, sy *models.Symbol, rest ...any) error {
	var span *string
	dest := []any{&sy.ID, &sy.RepoID, &sy.FileID, &sy.Path, &sy.Name, &sy.Pkg,
		&sy.Kind, &sy.StartLine, &sy.EndLine, &span}
	if err := sc.Scan(append(dest, rest...)...); err != nil {
		return err
	}
	if span != nil {
		sy.SpanID = *span
	}
	return nil
}

// callersSQL is "who calls this" (spec §8). A named constant so a test can
// EXPLAIN the statement that ships rather than a copy of it that can drift.
//
// It traverses to_symbol_id and nothing else. A join from a syntactic edge's
// to_name to a symbols.name is the guess spec:84 forbids, made at read time
// where no column records that it happened — and it is invisible in any corpus
// whose names are unique, which is most fixtures and no real repository.
//
// Two independent controls stop it, and they stop different things:
//
//	c.depth < $3 bounds the work to what the caller asked for. It is the one
//	that shows up in the answer.
//
//	NOT from_symbol_id = ANY(c.path) stops a cycle re-entering. Recursion is
//	ordinary Go — f calls f, two helpers call each other, every tree walker —
//	and without the guard the traversal explores every *walk* rather than every
//	*path*, which is exponential in the fan-in. Measured at depth 5 over the
//	live fixture's eight definitions: 19 rows guarded, 46 unguarded.
//
// The guard changes the cost and, on a cycle, one row of the answer. For every
// symbol other than the queried one it changes nothing: min(depth) is the BFS
// distance and a walk that revisits a node is never shorter than the path that
// does not. The exception is the queried symbol itself, because the anchor
// seeds path with it — so a symbol that indirectly calls itself is absent from
// its own caller list while a *direct* self-call, which the anchor produces, is
// present at depth 1. Measured on rs/zerolog's mutually recursive CBOR decoder
// in P4's Task 7; the plan's earlier claim that the guard is answer-neutral is
// true only of a fixture whose target is off the cycle.
//
// So "who calls this" reads as "who else calls this" once a cycle is involved.
// Nothing in a fixture's result set reads the guard back, which is why
// TestTheCycleGuardBoundsTheTraversalItselfLive asserts the work instead.
//
// depth < 1 is not refused here: the anchor is unconditional, so depth 0 still
// returns the direct callers. The bound is the caller's and the endpoint is
// where an out-of-range one is a 400 — a store that clamped would hide it.
//
// The diamond is why min() and the GROUP BY exist. Two paths of different
// lengths to one caller are one caller; without the aggregation it is two rows,
// which is not a loop and not an error but a plausible-looking wrong answer.
const callersSQL = `
	WITH RECURSIVE callers AS (
		SELECT e.from_symbol_id AS sym, 1 AS depth,
		       ARRAY[e.to_symbol_id, e.from_symbol_id] AS path,
		       e.path AS call_path, e.line AS call_line
		FROM edges e
		WHERE e.repo_id = $1 AND e.to_symbol_id = $2
	  UNION ALL
		SELECT e.from_symbol_id, c.depth + 1,
		       c.path || e.from_symbol_id, e.path, e.line
		FROM edges e
		JOIN callers c ON e.to_symbol_id = c.sym
		WHERE e.repo_id = $1
		  AND c.depth < $3
		  AND NOT e.from_symbol_id = ANY(c.path)
	)
	SELECT ` + symbolCols + `,
	       min(c.depth) AS depth,
	       (array_agg(c.call_path ORDER BY c.depth, c.call_path, c.call_line))[1] AS call_path,
	       (array_agg(c.call_line ORDER BY c.depth, c.call_path, c.call_line))[1] AS call_line
	FROM callers c JOIN symbols s ON s.id = c.sym
	GROUP BY s.id
	ORDER BY depth, s.path, s.start_line, s.id
	LIMIT $4`

// callersTimeout is how long the SERVER will spend on one caller walk before
// cancelling it.
//
// The two controls in callersSQL bound cycles and hops. Neither bounds FAN-IN,
// which is what multiplies, and LIMIT applies only after the whole CTE has
// materialised. Measured with EXPLAIN (ANALYZE) over definitions that all call
// each other: 20 of them produce 6,175 rows at depth 3 and 1,494,559 at depth
// 5; 30 of them do not finish inside 20 seconds at depth 4. A package where
// everything calls everything is an ordinary internal util package.
//
// Nothing else bounds it. There is no rate limit, no request-timeout
// middleware and no statement_timeout on the server, and pgxpool's default
// MaxConns is max(4, NumCPU) — so a handful of such GETs hold every connection
// in the pool and /ready cannot get one to Ping with.
//
// 5s because it separates the two populations rather than splitting either:
// the widest bounded walk measured here is 1.4s and the unbounded ones need
// 5.7s and up.
const callersTimeout = 5 * time.Second

// readTx begins a read whose statements the server itself will cancel after
// timeout.
//
// SET LOCAL inside a transaction, not statement_timeout in the DSN: one
// DATABASE_URL serves this process's read path AND the indexer's bulk writes,
// so a number on the connection string is either too long to bound a read or
// short enough to kill an index.
//
// set_config's third argument is is_local, and it takes a bind parameter where
// SET LOCAL does not — the habit rather than the risk, since the value here is
// a constant. is_local buys nothing measurable while this transaction only ever
// rolls back, because a rollback undoes a session SET too: no test in this
// suite can tell the two apart. It is written for the day one of these commits,
// when a session SET would ride the pooled connection into whatever borrows it
// next.
func (s *Store) readTx(ctx context.Context, timeout time.Duration) (pgx.Tx, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	if _, err := tx.Exec(ctx, `SELECT set_config('statement_timeout', $1, true)`, timeout.String()); err != nil {
		_ = tx.Rollback(ctx)
		return nil, err
	}
	return tx, nil
}

// CallersOf walks edges backwards from one symbol, nearest first.
//
// repo_id is in both terms although to_symbol_id already pins the repository —
// SymbolID hashes the repo id, so an id cannot collide across two of them. The
// clause is what stops a cross-repo traversal the day that stops being true,
// and it costs nothing today.
//
// The transaction is here only to scope callersTimeout, so it rolls back:
// nothing in it writes, and a Commit would be a claim about work that was not
// done.
func (s *Store) CallersOf(ctx context.Context, repoID, symbolID string, depth, limit int) ([]Caller, error) {
	tx, err := s.readTx(ctx, callersTimeout)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback(ctx)

	rows, err := tx.Query(ctx, callersSQL, repoID, symbolID, depth, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []Caller
	for rows.Next() {
		var c Caller
		if err := scanSymbol(rows, &c.Symbol, &c.Depth, &c.CallPath, &c.CallLine); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// ApproximateCallersOf is the set CallersOf refuses to merge: callers whose
// edge names this symbol and cannot say it is this one.
//
// Deliberately dull, and depth 1 by construction rather than by a parameter.
// to_symbol_id IS NULL is what keeps a resolved edge out — it is already in the
// precise answer, and counting it twice would overstate what is not known.
//
// One caller can hold two such call sites, so the call site is in the ORDER BY
// too: two rows for one symbol must not come back in an arbitrary order.
func (s *Store) ApproximateCallersOf(ctx context.Context, repoID, name string, limit int) ([]Approximate, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT `+symbolCols+`, e.to_name, e.path, e.line
		FROM edges e JOIN symbols s ON s.id = e.from_symbol_id
		WHERE e.repo_id = $1 AND e.to_symbol_id IS NULL AND e.to_name = $2
		ORDER BY s.path, s.start_line, s.id, e.path, e.line, e.id
		LIMIT $3`, repoID, name, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []Approximate
	for rows.Next() {
		var a Approximate
		if err := scanSymbol(rows, &a.Symbol, &a.ToName, &a.CallPath, &a.CallLine); err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

// Definitions finds a symbol by name in one repository, optionally within one
// package.
//
// Exact by default. The corpus spells a method Store.Get and P3's lexical arm
// reaches it through its parts, so a caller who types Get is likely to have
// come from there — but widening silently answers a different question than
// the one asked, and the payload cannot say it happened. suffix is the opt-in.
//
// pkg narrows to one package clause and empty means every package. It is a
// predicate rather than a filter over the rows this returns because LIMIT
// applies here: filtering afterwards would drop rows the limit had already
// spent, and answer 3 of 5 while reporting a bound of 20.
//
// right(), not LIKE: a name carrying % or _ would turn a LIKE pattern into a
// wildcard, which is the silent widening this signature exists to refuse.
func (s *Store) Definitions(ctx context.Context, repoID, name, pkg string, suffix bool, limit int) ([]models.Symbol, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT `+symbolCols+`
		FROM symbols s
		WHERE s.repo_id = $1
		  AND (s.name = $2 OR ($3 AND right(s.name, char_length($2) + 1) = '.' || $2))
		  AND ($4 = '' OR s.pkg = $4)
		ORDER BY s.path, s.start_line, s.id
		LIMIT $5`, repoID, name, suffix, pkg, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []models.Symbol
	for rows.Next() {
		var sy models.Symbol
		if err := scanSymbol(rows, &sy); err != nil {
			return nil, err
		}
		out = append(out, sy)
	}
	return out, rows.Err()
}

// Symbol reads one definition of one repository.
//
// Scoped by repo_id even though the id is a primary key, for GetSpan's reason:
// a symbol id from another repository is a 404 for this one, not a read across
// a corpus boundary the caller never named.
func (s *Store) Symbol(ctx context.Context, repoID, symbolID string) (models.Symbol, error) {
	var sy models.Symbol
	err := scanSymbol(s.pool.QueryRow(ctx, `
		SELECT `+symbolCols+`
		FROM symbols s
		WHERE s.repo_id = $1 AND s.id = $2`, repoID, symbolID), &sy)
	if errors.Is(err, pgx.ErrNoRows) {
		return models.Symbol{}, ErrNotFound
	}
	return sy, err
}
