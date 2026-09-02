//go:build live

package store

import (
	"context"
	"slices"
	"strings"
	"testing"

	"github.com/mralaminahamed/codetrail/packages/shared/models"
	"github.com/mralaminahamed/codetrail/packages/shared/rag"
)

// The fixture decouples symbol from text, which a corpus written by the indexer
// never is — a declaration's text contains its own name — and which is the only
// way a test can see the A/B weighting do anything at all.
//
//	c_symbol.go  symbol parseConfig, text matching nothing        the A case
//	a_body.go    no symbol, parseConfig twice in ~200 tokens      the B case
//	d_prose.go   no symbol, "parse the config file"               split terms only
//	e_method.go  symbol Store.Get, text matching nothing          the dotted symbol
//	y_cover.go   alpha and beta once each                         ranking function
//	x_repeat.go  alpha six times                                  ranking function
//
// and in a second repository, a_body.go with symbol parseConfig and the term ten
// times over — a better match than anything in the target repo, so a missing
// repo filter shows up as that span at rank 1 rather than as a subtler order.
//
// Two counts are measured rather than chosen. The body span carries the term
// twice because at one occurrence the weighted and unweighted rankings both
// separate it from the symbol span, but the unweighted one separates it 0.1 to
// 0.1 — a tie broken by path, which would score a kill that the weights had
// nothing to do with. At two it is 1.0 against 0.8 weighted and 0.1 against 0.2
// unweighted, so dropping the weights flips the order on the score. At three it
// is 1.2 against 1.0 and the body wins even with the weights on.
type lexSpan struct {
	path, symbol, text string
	line               int
}

const (
	lexTargetRemote = "https://github.com/lexical/target"
	lexOtherRemote  = "https://github.com/lexical/other"
	lexFiller       = "filler word here "
)

func lexFixture(t *testing.T) (*Store, string, string) {
	t.Helper()
	target := RepoID(lexTargetRemote, Digest(lexTargetRemote)[:40])
	other := RepoID(lexOtherRemote, Digest(lexOtherRemote)[:40])
	s := fresh(t, target, other)

	// c_symbol.go goes in last though it ranks first, so a query that lost
	// its ORDER BY cannot pass by returning rows in the order they arrived.
	// y_cover.go before x_repeat.go for the same reason, though measured that
	// is not enough on its own: with the ORDER BY deleted this pair still came
	// back [x_repeat, y_cover] — the expected answer — whichever order they
	// were inserted in. Unordered is not insertion-ordered. So the ranking
	// function test does not detect a missing ORDER BY and TestSymbolOutweighsBody
	// is what does.
	seedLexSpans(t, s, target, lexTargetRemote, []lexSpan{
		{path: "a_body.go", line: 10, text: strings.Repeat(lexFiller, 60) +
			" parseConfig " + strings.Repeat("more filler text ", 60) +
			" parseConfig " + strings.Repeat("tail words go ", 60)},
		{path: "d_prose.go", line: 20, text: "// parse the config file and return it"},
		{path: "e_method.go", line: 30, symbol: "Store.Get",
			text: "func (r *zzz) yyy() error { return nil }"},
		{path: "y_cover.go", line: 40, text: "alpha beta " + strings.Repeat(lexFiller, 30)},
		{path: "x_repeat.go", line: 50, text: strings.Repeat("alpha ", 6) + strings.Repeat(lexFiller, 30)},
		{path: "c_symbol.go", line: 60, symbol: "parseConfig",
			text: "func zzz(a int) error { return nil }"},
	})
	seedLexSpans(t, s, other, lexOtherRemote, []lexSpan{
		{path: "a_body.go", line: 10, symbol: "parseConfig",
			text: strings.Repeat("parseConfig ", 10)},
	})
	return s, target, other
}

// Direct INSERTs rather than PutSpans: every span here has a NULL embedding,
// which is what shows the lexical arm needs none, and PutSpans requires a
// 768-wide vector per span that would only be noise in this fixture.
func seedLexSpans(t *testing.T, s *Store, repoID, remote string, spans []lexSpan) {
	t.Helper()
	ctx := context.Background()
	commit := Digest(remote)[:40]
	files := make([]models.File, 0, len(spans))
	for _, sp := range spans {
		files = append(files, models.File{
			ID: FileID(repoID, sp.path), RepoID: repoID, Path: sp.path,
			Blob: "b", Lang: "go", Lines: 1,
		})
	}
	if err := s.PutRepo(ctx, models.Repo{
		ID: repoID, Remote: remote, Ref: "main", Commit: commit,
	}, files); err != nil {
		t.Fatal(err)
	}
	for _, sp := range spans {
		d := Digest(sp.text)
		if _, err := s.pool.Exec(ctx, `
			INSERT INTO spans (id, repo_id, file_id, path, kind, symbol, start_line,
				end_line, text, digest, embed_model, embed_dim)
			VALUES ($1, $2, $3, $4, 'func', $5, $6, $7, $8, $9, 'fake-768', 768)`,
			SpanID(repoID, sp.path, sp.line, sp.line+1, d), repoID, FileID(repoID, sp.path),
			sp.path, sp.symbol, sp.line, sp.line+1, sp.text, d); err != nil {
			t.Fatal(err)
		}
	}
}

func lexPaths(cs []models.Cite) []string {
	out := make([]string, len(cs))
	for i, c := range cs {
		out[i] = c.Path
	}
	return out
}

func lexSearch(t *testing.T, s *Store, repoID, q string, split bool) []models.Cite {
	t.Helper()
	cs, err := s.LexicalSearch(context.Background(), repoID, rag.Terms(q, split), 20)
	if err != nil {
		t.Fatal(err)
	}
	return cs
}

// The question the arm exists for: where is parseConfig defined. c_symbol.go
// carries the name in its symbol and nowhere in its text, so it is reachable
// only through the whole identifier the index holds.
func TestLexicalSearchFindsASpanBySymbol(t *testing.T) {
	s, target, _ := lexFixture(t)
	got := lexPaths(lexSearch(t, s, target, "parseConfig", true))
	if !slices.Contains(got, "c_symbol.go") {
		t.Fatalf("c_symbol.go was not retrieved for %q: got %v", "parseConfig", got)
	}
}

// A symbol match is a definition and a body match is a mention. Split off, so
// the only term is the whole identifier and the two spans differ in nothing but
// which column carries it.
func TestSymbolOutweighsBody(t *testing.T) {
	s, target, _ := lexFixture(t)
	got := lexPaths(lexSearch(t, s, target, "parseConfig", false))
	sym, body := slices.Index(got, "c_symbol.go"), slices.Index(got, "a_body.go")
	if sym < 0 || body < 0 {
		t.Fatalf("parseConfig ranked %v, want both the symbol and the body span", got)
	}
	if sym > body {
		t.Fatalf("parseConfig ranked %v, want c_symbol.go before a_body.go", got)
	}
}

// The split's whole and only effect: prose that spells the parts separately.
// d_prose.go never contains parseConfig, so it is unreachable without it — and
// the second half pins that the split is what does it, not the corpus.
func TestSplitTermsReachProseThatNamesThePartsSeparately(t *testing.T) {
	s, target, _ := lexFixture(t)
	if got := lexPaths(lexSearch(t, s, target, "parseConfig", true)); !slices.Contains(got, "d_prose.go") {
		t.Fatalf("d_prose.go was not retrieved with splitting on: got %v", got)
	}
	if got := lexPaths(lexSearch(t, s, target, "parseConfig", false)); slices.Contains(got, "d_prose.go") {
		t.Fatalf("d_prose.go was retrieved with splitting off, so the split proves nothing: got %v", got)
	}
}

// A method's symbol is Store.Get, and Postgres reads that as one token — the
// parser calls it a host — so the A weight reaches it only because 0008 turns
// the dot into a space. e_method.go's text carries neither word.
func TestAMethodSymbolIsReachableByItsParts(t *testing.T) {
	s, target, _ := lexFixture(t)
	got := lexPaths(lexSearch(t, s, target, "Store.Get", false))
	if !slices.Contains(got, "e_method.go") {
		t.Fatalf("e_method.go was not retrieved for %q: got %v", "Store.Get", got)
	}
}

func TestLexicalSearchIsScopedToOneRepo(t *testing.T) {
	s, target, other := lexFixture(t)
	for i, c := range lexSearch(t, s, target, "parseConfig", true) {
		if c.RepoID == other {
			t.Fatalf("span %s from repo %s appeared at rank %d", c.Path, other, i+1)
		}
	}
}

// The arm ships ts_rank_cd, and this is what that costs: six mentions of one
// term outrank one mention of each of two. Under OR the cover-density algorithm
// never runs — measured, two spans with the terms adjacent and 300 tokens apart
// score identically — so the real difference from ts_rank is that ts_rank_cd
// sums per occurrence where ts_rank saturates and rewards distinct terms. The
// choice is unmeasured against any corpus (spec:316); this pins which one ships
// so swapping it is a visible change rather than a silent one.
func TestRepetitionOutranksCoverageUnderTheShippedRankingFunction(t *testing.T) {
	s, target, _ := lexFixture(t)
	got := lexPaths(lexSearch(t, s, target, "alpha beta", false))
	want := []string{"x_repeat.go", "y_cover.go"}
	if !slices.Equal(got, want) {
		t.Fatalf("alpha beta ranked %v, want %v", got, want)
	}
}

// A question with no words in it is a result, not a failure, and it must not
// reach to_tsquery: to_tsquery('simple', ”) is a syntax error.
func TestNoUsableTermsReturnsNothingRatherThanErroring(t *testing.T) {
	s, target, _ := lexFixture(t)
	if terms := rag.Terms("??? &|!", true); len(terms) != 0 {
		t.Fatalf("Terms found %v in a question with no words", terms)
	}
	got, err := s.LexicalSearch(context.Background(), target, nil, 20)
	if err != nil {
		t.Fatalf("no terms returned an error: %v", err)
	}
	if got != nil {
		t.Fatalf("no terms returned %d rows", len(got))
	}
}

// Most questions about code contain a character to_tsquery reads as an
// operator. This one contains four.
func TestAQuestionWithPunctuationIsNotASyntaxError(t *testing.T) {
	s, target, _ := lexFixture(t)
	const q = "where is parseConfig() defined? (a & b)"
	got, err := s.LexicalSearch(context.Background(), target, rag.Terms(q, true), 20)
	if err != nil {
		t.Fatalf("%q: %v", q, err)
	}
	if !slices.Contains(lexPaths(got), "c_symbol.go") {
		t.Fatalf("%q retrieved %v, want c_symbol.go among them", q, lexPaths(got))
	}
}

// The column has to be generated and the index has to be GIN over it: an
// ordinary column would hold whatever the last writer put there, and an
// unindexed tsvector works, slowly, and silently.
func TestLexColumnIsGeneratedAndIndexedLive(t *testing.T) {
	ctx := context.Background()
	s, err := New(ctx, dsn(t))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	var typ, generated string
	if err := s.pool.QueryRow(ctx, `
		SELECT data_type, is_generated FROM information_schema.columns
		WHERE table_name = 'spans' AND column_name = 'lex'`).Scan(&typ, &generated); err != nil {
		t.Fatalf("spans.lex is missing: %v", err)
	}
	if typ != "tsvector" || generated != "ALWAYS" {
		t.Fatalf("spans.lex is %s, is_generated=%s; want a tsvector generated ALWAYS", typ, generated)
	}

	var idx string
	if err := s.pool.QueryRow(ctx, `
		SELECT indexdef FROM pg_indexes
		WHERE tablename = 'spans' AND indexname = 'spans_lex_idx'`).Scan(&idx); err != nil {
		t.Fatalf("the lexical index is missing: %v", err)
	}
	if !strings.Contains(idx, "gin") || !strings.Contains(idx, "lex") {
		t.Fatalf("want a gin index over lex, got: %s", idx)
	}
}

// migrate() records a name and skips the body, so the idempotency test in
// store_live_test.go proves the ledger works and nothing about this file. The
// body is re-run here instead, against a spans table that already holds rows —
// the case a deployed database is always in and a fresh one never is.
//
// In one rolled-back transaction, because the column it drops and rebuilds is
// the one every other test in this binary queries.
func TestMigration0008AppliesToAPopulatedTableTwiceLive(t *testing.T) {
	ctx := context.Background()
	s, err := New(ctx, dsn(t))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	body, err := migrationFS.ReadFile("migrations/0008_span_lexical.sql")
	if err != nil {
		t.Fatal(err)
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx)

	// Back to before 0008, then populated, so the ALTER runs against rows.
	for _, stmt := range []string{
		`ALTER TABLE spans DROP COLUMN IF EXISTS lex`,
		`INSERT INTO repos (id, remote, ref, commit_sha) VALUES ('m8', 'https://github.com/a/m8', 'main', 'c0ffee')`,
		`INSERT INTO files (id, repo_id, path, blob, lang, lines) VALUES ('m8-f', 'm8', 'a.go', '', 'go', 1)`,
		`INSERT INTO spans (id, repo_id, file_id, path, kind, symbol, start_line, end_line,
			text, digest, embed_model, embed_dim)
			VALUES ('m8-s', 'm8', 'm8-f', 'a.go', 'func', 'Store.Get', 1, 1, 'body words', 'd', 'm', 768)`,
		// The text carries neither store nor get, so the AND below can only be
		// satisfied by the symbol half of the expression.
	} {
		if _, err := tx.Exec(ctx, stmt); err != nil {
			t.Fatal(err)
		}
	}

	for i := range 2 {
		if _, err := tx.Exec(ctx, string(body)); err != nil {
			t.Fatalf("run %d: %v", i+1, err)
		}
		var matches bool
		if err := tx.QueryRow(ctx, `
			SELECT lex @@ to_tsquery('simple', 'store & get')
			FROM spans WHERE id = 'm8-s'`).Scan(&matches); err != nil {
			t.Fatalf("run %d: %v", i+1, err)
		}
		if !matches {
			t.Errorf("run %d: the backfilled row does not match its own symbol", i+1)
		}
	}
}
