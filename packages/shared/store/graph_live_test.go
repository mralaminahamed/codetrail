//go:build live

package store

import (
	"context"
	"slices"
	"strings"
	"testing"

	"github.com/mralaminahamed/codetrail/packages/shared/models"
)

// graphIDs derives a repo id from a fixture name, so two tests naming their
// fixtures differently cannot land on one repo id and read each other's rows.
func graphIDs(name string) (remote, commit, repoID string) {
	remote, commit = "https://github.com/graph/"+name, Digest(name)[:40]
	return remote, commit, RepoID(remote, commit)
}

// seedGraphRepo writes the repo and file a symbol has to hang off.
func seedGraphRepo(t *testing.T, s *Store, name string) (repoID, fileID string) {
	t.Helper()
	remote, commit, id := graphIDs(name)
	fileID = FileID(id, "a.go")
	if err := s.PutRepo(context.Background(),
		models.Repo{ID: id, Remote: remote, Ref: "main", Commit: commit},
		[]models.File{{ID: fileID, RepoID: id, Path: "a.go", Blob: "b", Lang: "go", Lines: 40}}); err != nil {
		t.Fatal(err)
	}
	return id, fileID
}

// mkSym and mkEdge derive their ids the way the indexer will, so a fixture
// cannot disagree with SymbolID and EdgeID about what makes a row.
func mkSym(repoID, fileID, path string, start, end int, kind models.SpanKind, name, spanID string) models.Symbol {
	return models.Symbol{
		ID: SymbolID(repoID, path, start, string(kind), name), RepoID: repoID, FileID: fileID,
		Path: path, Name: name, Pkg: "p", Kind: kind,
		StartLine: start, EndLine: end, SpanID: spanID,
	}
}

// The provenance follows the target, because that pairing is what the schema
// enforces; the fixtures that break the pair build their edge by hand.
func mkEdge(repoID, from, path string, offset, line int, toName, toSymbolID string) models.Edge {
	prov := models.ProvenanceSyntactic
	if toSymbolID != "" {
		prov = models.ProvenanceResolved
	}
	return models.Edge{
		ID: EdgeID(repoID, from, path, offset, toName), RepoID: repoID, FromSymbolID: from,
		ToSymbolID: toSymbolID, ToName: toName, Kind: models.EdgeCalls, Provenance: prov,
		Path: path, Line: line,
	}
}

func readSymbols(t *testing.T, s *Store, repoID string) []models.Symbol {
	t.Helper()
	rows, err := s.pool.Query(context.Background(), `
		SELECT id, repo_id, file_id, path, name, pkg, kind, start_line, end_line, span_id
		FROM symbols WHERE repo_id = $1 ORDER BY id`, repoID)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var out []models.Symbol
	for rows.Next() {
		var sy models.Symbol
		var span *string
		if err := rows.Scan(&sy.ID, &sy.RepoID, &sy.FileID, &sy.Path, &sy.Name, &sy.Pkg,
			&sy.Kind, &sy.StartLine, &sy.EndLine, &span); err != nil {
			t.Fatal(err)
		}
		if span != nil {
			sy.SpanID = *span
		}
		out = append(out, sy)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return out
}

func readEdges(t *testing.T, s *Store, repoID string) []models.Edge {
	t.Helper()
	rows, err := s.pool.Query(context.Background(), `
		SELECT id, repo_id, from_symbol_id, to_symbol_id, to_name, kind, provenance, path, line
		FROM edges WHERE repo_id = $1 ORDER BY id`, repoID)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var out []models.Edge
	for rows.Next() {
		var e models.Edge
		var to *string
		if err := rows.Scan(&e.ID, &e.RepoID, &e.FromSymbolID, &to, &e.ToName,
			&e.Kind, &e.Provenance, &e.Path, &e.Line); err != nil {
			t.Fatal(err)
		}
		if to != nil {
			e.ToSymbolID = *to
		}
		out = append(out, e)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return out
}

// wantRows compares by id and then whole row, so a failure says which row is
// missing or wrong rather than that two counts differ.
func wantSymbols(t *testing.T, got, want []models.Symbol) {
	t.Helper()
	have := map[string]models.Symbol{}
	for _, sy := range got {
		have[sy.ID] = sy
	}
	for _, w := range want {
		g, ok := have[w.ID]
		if !ok {
			t.Errorf("symbol %s (%s %s:%d) is not in the table", w.ID, w.Name, w.Path, w.StartLine)
			continue
		}
		if g != w {
			t.Errorf("symbol %s read back as %+v, want %+v", w.ID, g, w)
		}
		delete(have, w.ID)
	}
	for id, g := range have {
		t.Errorf("unexpected symbol %s (%s %s:%d)", id, g.Name, g.Path, g.StartLine)
	}
}

func wantEdges(t *testing.T, got, want []models.Edge) {
	t.Helper()
	have := map[string]models.Edge{}
	for _, e := range got {
		have[e.ID] = e
	}
	for _, w := range want {
		g, ok := have[w.ID]
		if !ok {
			t.Errorf("edge to %s at %s:%d (%s) is not in the table", w.ToName, w.Path, w.Line, w.ID)
			continue
		}
		if g != w {
			t.Errorf("edge %s read back as %+v, want %+v", w.ID, g, w)
		}
		delete(have, w.ID)
	}
	for id, g := range have {
		t.Errorf("unexpected edge to %s at %s:%d (%s), provenance %s", g.ToName, g.Path, g.Line, id, g.Provenance)
	}
}

// The round trip everything downstream assumes, with the fixture that makes a
// symbol id say what it is for: two init functions in one file. They share a
// name and a kind and differ only in where they start, so an id that does not
// carry the start line collapses them into one row — and two init functions in
// one file is legal Go, not a contrivance.
//
// One symbol is linked to a span and one edge is resolved, so span_id and
// to_symbol_id are columns this test reads back rather than columns nothing
// observes.
func TestPutGraphWritesSymbolsAndEdgesLive(t *testing.T) {
	ctx := context.Background()
	_, _, repoID := graphIDs("write")
	s := fresh(t, repoID)
	repoID, fileID := seedGraphRepo(t, s, "write")

	span := mkSpan(repoID, fileID, "a.go", 11, 12, "func (s *Store) Get() {}", unit(1))
	if err := s.PutSpans(ctx, repoID, []EmbeddedSpan{span}, fakeModel, EmbeddingDim); err != nil {
		t.Fatal(err)
	}

	first := mkSym(repoID, fileID, "a.go", 3, 5, models.KindFunc, "init", "")
	second := mkSym(repoID, fileID, "a.go", 7, 9, models.KindFunc, "init", "")
	get := mkSym(repoID, fileID, "a.go", 11, 12, models.KindFunc, "Store.Get", span.ID)
	if first.ID == second.ID {
		t.Fatalf("both init definitions hash to %s: a symbol id that drops the start line cannot tell two same-named declarations in one file apart", first.ID)
	}

	syms := []models.Symbol{first, second, get}
	edges := []models.Edge{
		mkEdge(repoID, first.ID, "a.go", 30, 4, "Get", get.ID),
		mkEdge(repoID, second.ID, "a.go", 60, 8, "Println", ""),
	}
	if err := s.PutGraph(ctx, repoID, syms, edges); err != nil {
		t.Fatalf("PutGraph returned %v: a symbol with no span and an edge with no target are ordinary rows, not errors", err)
	}
	wantSymbols(t, readSymbols(t, s, repoID), syms)
	wantEdges(t, readEdges(t, s, repoID), edges)
}

// A re-index that finds fewer call sites has to lose the ones that went. The
// second write keeps both definitions and drops two of three edges, so an
// insert-only writer leaves edges for call sites that no longer exist — and it
// leaves them looking exactly like the ones that do.
func TestPutGraphReplacesAnEarlierGraphForTheSameRepoLive(t *testing.T) {
	ctx := context.Background()
	_, _, repoID := graphIDs("replace")
	s := fresh(t, repoID)
	repoID, fileID := seedGraphRepo(t, s, "replace")

	a := mkSym(repoID, fileID, "a.go", 3, 5, models.KindFunc, "A", "")
	b := mkSym(repoID, fileID, "a.go", 7, 9, models.KindFunc, "B", "")
	before := []models.Edge{
		mkEdge(repoID, a.ID, "a.go", 30, 4, "B", b.ID),
		mkEdge(repoID, a.ID, "a.go", 40, 4, "Close", ""),
		mkEdge(repoID, b.ID, "a.go", 80, 8, "Println", ""),
	}
	if err := s.PutGraph(ctx, repoID, []models.Symbol{a, b}, before); err != nil {
		t.Fatal(err)
	}
	after := []models.Edge{before[0]}
	if err := s.PutGraph(ctx, repoID, []models.Symbol{a, b}, after); err != nil {
		t.Fatal(err)
	}
	wantEdges(t, readEdges(t, s, repoID), after)
	wantSymbols(t, readSymbols(t, s, repoID), []models.Symbol{a, b})
}

// Ids are deterministic, so a retried job has to converge on the same rows
// rather than on rows that merely count the same.
func TestPutGraphIsIdempotentAcrossTwoIdenticalRunsLive(t *testing.T) {
	ctx := context.Background()
	_, _, repoID := graphIDs("idem")
	s := fresh(t, repoID)
	repoID, fileID := seedGraphRepo(t, s, "idem")

	a := mkSym(repoID, fileID, "a.go", 3, 5, models.KindFunc, "A", "")
	b := mkSym(repoID, fileID, "a.go", 7, 9, models.KindFunc, "B", "")
	edges := []models.Edge{
		mkEdge(repoID, a.ID, "a.go", 30, 4, "B", b.ID),
		mkEdge(repoID, b.ID, "a.go", 80, 8, "Close", ""),
	}
	var runs [2][]models.Edge
	for i := range 2 {
		if err := s.PutGraph(ctx, repoID, []models.Symbol{a, b}, edges); err != nil {
			t.Fatalf("run %d: %v", i+1, err)
		}
		wantSymbols(t, readSymbols(t, s, repoID), []models.Symbol{a, b})
		runs[i] = readEdges(t, s, repoID)
	}
	if !slices.Equal(runs[0], runs[1]) {
		t.Errorf("the second run wrote %+v, the first wrote %+v", runs[1], runs[0])
	}
	wantEdges(t, runs[1], edges)
}

// The corpus holds many repositories and each is written independently. B is
// written first and is the better candidate for an unscoped delete to take: it
// holds a symbol with the same name as A's and more edges than A has, so a
// delete that lost its repo scope changes this answer rather than adding a row
// nobody looks at.
func TestPutGraphLeavesAnotherReposGraphAloneLive(t *testing.T) {
	ctx := context.Background()
	_, _, idA := graphIDs("scopeA")
	_, _, idB := graphIDs("scopeB")
	s := fresh(t, idA, idB)
	repoB, fileB := seedGraphRepo(t, s, "scopeB")
	bGet := mkSym(repoB, fileB, "a.go", 3, 5, models.KindFunc, "Store.Get", "")
	bCaller := mkSym(repoB, fileB, "a.go", 7, 9, models.KindFunc, "Caller", "")
	bEdges := []models.Edge{
		mkEdge(repoB, bCaller.ID, "a.go", 30, 8, "Get", bGet.ID),
		mkEdge(repoB, bCaller.ID, "a.go", 44, 8, "Get", bGet.ID),
		mkEdge(repoB, bCaller.ID, "a.go", 58, 8, "Close", ""),
	}
	if err := s.PutGraph(ctx, repoB, []models.Symbol{bGet, bCaller}, bEdges); err != nil {
		t.Fatal(err)
	}

	repoA, fileA := seedGraphRepo(t, s, "scopeA")
	aGet := mkSym(repoA, fileA, "a.go", 3, 5, models.KindFunc, "Store.Get", "")
	if err := s.PutGraph(ctx, repoA, []models.Symbol{aGet},
		[]models.Edge{mkEdge(repoA, aGet.ID, "a.go", 30, 4, "Close", "")}); err != nil {
		t.Fatal(err)
	}

	wantSymbols(t, readSymbols(t, s, repoB), []models.Symbol{bGet, bCaller})
	wantEdges(t, readEdges(t, s, repoB), bEdges)
}

// A row carrying another repo's id is refused rather than relabelled: writing
// it under repoID would produce a row whose id is a hash of one repo sitting
// under another, and nothing later could tell.
func TestPutGraphRefusesARowFromAnotherRepoLive(t *testing.T) {
	ctx := context.Background()
	_, _, repoID := graphIDs("foreign")
	_, _, other := graphIDs("foreignOther")
	s := fresh(t, repoID)
	repoID, fileID := seedGraphRepo(t, s, "foreign")

	stray := mkSym(other, fileID, "a.go", 3, 5, models.KindFunc, "Stray", "")
	err := s.PutGraph(ctx, repoID, []models.Symbol{stray}, nil)
	if err == nil {
		t.Fatal("a symbol from another repo was written")
	}
	for _, want := range []string{"Stray", "a.go", other, repoID} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not name %q", err, want)
		}
	}

	mine := mkSym(repoID, fileID, "a.go", 3, 5, models.KindFunc, "Mine", "")
	strayEdge := mkEdge(other, mine.ID, "a.go", 30, 4, "Close", "")
	err = s.PutGraph(ctx, repoID, []models.Symbol{mine}, []models.Edge{strayEdge})
	if err == nil {
		t.Fatal("an edge from another repo was written")
	}
	for _, want := range []string{"Close", "a.go:4", other} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not name %q", err, want)
		}
	}
	if syms := readSymbols(t, s, repoID); len(syms) != 0 {
		t.Errorf("the refused write left %d symbols behind: %+v", len(syms), syms)
	}
}

// The schema refuses this pair too, so an assertion of err != nil passes under
// a mutant that deletes the Go check. What the Go check is for is the message:
// a constraint violation names a table and a constraint, and the batch it came
// from could be forty thousand edges.
func TestPutGraphNamesTheEdgeWhoseProvenanceAndTargetDisagreeLive(t *testing.T) {
	ctx := context.Background()
	_, _, repoID := graphIDs("pair")
	s := fresh(t, repoID)
	repoID, fileID := seedGraphRepo(t, s, "pair")
	target := mkSym(repoID, fileID, "a.go", 3, 5, models.KindFunc, "Store.Get", "")
	caller := mkSym(repoID, fileID, "a.go", 7, 9, models.KindFunc, "Caller", "")
	syms := []models.Symbol{target, caller}

	resolvedWithNoTarget := mkEdge(repoID, caller.ID, "a.go", 30, 8, "Get", "")
	resolvedWithNoTarget.Provenance = models.ProvenanceResolved
	syntacticWithTarget := mkEdge(repoID, caller.ID, "a.go", 44, 8, "Close", target.ID)
	syntacticWithTarget.Provenance = models.ProvenanceSyntactic

	for _, tc := range []struct {
		name string
		edge models.Edge
		says []string
	}{
		{"resolved with no target", resolvedWithNoTarget, []string{"Get", "a.go:8", "resolved"}},
		{"syntactic with a target", syntacticWithTarget, []string{"Close", "a.go:8", "syntactic"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := s.PutGraph(ctx, repoID, syms, []models.Edge{tc.edge})
			if err == nil {
				t.Fatal("the write was accepted")
			}
			for _, want := range tc.says {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("error %q does not name %q", err, want)
				}
			}
		})
	}
}

// a(b(), b()) is two calls on one line. The edge id is what makes a row, so a
// key built from the line merges them — and the merge is silent: the surviving
// row looks exactly like a correct one.
func TestTwoCallsOnOneLineAreTwoRowsLive(t *testing.T) {
	ctx := context.Background()
	_, _, repoID := graphIDs("oneline")
	s := fresh(t, repoID)
	repoID, fileID := seedGraphRepo(t, s, "oneline")

	a := mkSym(repoID, fileID, "a.go", 3, 5, models.KindFunc, "a", "")
	// Both on line 4, at the two offsets of `a(b(), b())`.
	left := mkEdge(repoID, a.ID, "a.go", 42, 4, "b", "")
	right := mkEdge(repoID, a.ID, "a.go", 48, 4, "b", "")
	if left.ID == right.ID {
		t.Fatalf("both calls to b on line 4 hash to %s: an edge id keyed on the line cannot tell two calls on one line apart", left.ID)
	}
	edges := []models.Edge{left, right}
	if err := s.PutGraph(ctx, repoID, []models.Symbol{a}, edges); err != nil {
		t.Fatal(err)
	}
	wantEdges(t, readEdges(t, s, repoID), edges)
}

// A repository indexed once without the type-checker and again with it must end
// up with the better label. Recorded as a survivor in the plan's own terms: the
// wholesale DELETE means the second write has nothing to conflict with, so this
// test does not by itself reach ON CONFLICT. The duplicate-in-one-call test
// below is the one that does.
func TestARerunUpgradesASyntacticEdgeToResolvedLive(t *testing.T) {
	ctx := context.Background()
	_, _, repoID := graphIDs("upgrade")
	s := fresh(t, repoID)
	repoID, fileID := seedGraphRepo(t, s, "upgrade")

	get := mkSym(repoID, fileID, "a.go", 3, 5, models.KindFunc, "Store.Get", "")
	caller := mkSym(repoID, fileID, "a.go", 7, 9, models.KindFunc, "Caller", "")
	syms := []models.Symbol{get, caller}

	syntactic := mkEdge(repoID, caller.ID, "a.go", 30, 8, "Get", "")
	if err := s.PutGraph(ctx, repoID, syms, []models.Edge{syntactic}); err != nil {
		t.Fatal(err)
	}
	resolved := mkEdge(repoID, caller.ID, "a.go", 30, 8, "Get", get.ID)
	if resolved.ID != syntactic.ID {
		t.Fatalf("the same call site hashed to two ids, %s then %s: this test is not exercising the conflict it exists for", syntactic.ID, resolved.ID)
	}
	if err := s.PutGraph(ctx, repoID, syms, []models.Edge{resolved}); err != nil {
		t.Fatal(err)
	}
	wantEdges(t, readEdges(t, s, repoID), []models.Edge{resolved})
}

// Two rows for one call site inside one write — the shape a type-check pass
// produces when it appends its upgrades rather than replacing the row. The
// deletes cannot help here, so this is where ON CONFLICT DO UPDATE is the only
// thing standing between the corpus and an edge that claims less than is known.
func TestADuplicateCallSiteInOneWriteKeepsTheResolvedLabelLive(t *testing.T) {
	ctx := context.Background()
	_, _, repoID := graphIDs("dup")
	s := fresh(t, repoID)
	repoID, fileID := seedGraphRepo(t, s, "dup")

	get := mkSym(repoID, fileID, "a.go", 3, 5, models.KindFunc, "Store.Get", "")
	caller := mkSym(repoID, fileID, "a.go", 7, 9, models.KindFunc, "Caller", "")
	syntactic := mkEdge(repoID, caller.ID, "a.go", 30, 8, "Get", "")
	resolved := mkEdge(repoID, caller.ID, "a.go", 30, 8, "Get", get.ID)
	if err := s.PutGraph(ctx, repoID, []models.Symbol{get, caller},
		[]models.Edge{syntactic, resolved}); err != nil {
		t.Fatal(err)
	}
	wantEdges(t, readEdges(t, s, repoID), []models.Edge{resolved})
}

// The symbol half of the same thing. Two rows for one definition inside one
// write — a file walked twice, or a caller that appends its span links rather
// than replacing the row — has to converge on the later one rather than abort a
// job at its last step, and the columns the id does not determine are the ones
// that have to move.
func TestADuplicateSymbolInOneWriteKeepsTheLastRowLive(t *testing.T) {
	ctx := context.Background()
	_, _, repoID := graphIDs("dupsym")
	s := fresh(t, repoID)
	repoID, fileID := seedGraphRepo(t, s, "dupsym")

	span := mkSpan(repoID, fileID, "a.go", 3, 9, "func A() {}", unit(1))
	if err := s.PutSpans(ctx, repoID, []EmbeddedSpan{span}, fakeModel, EmbeddingDim); err != nil {
		t.Fatal(err)
	}
	unlinked := mkSym(repoID, fileID, "a.go", 3, 5, models.KindFunc, "A", "")
	linked := mkSym(repoID, fileID, "a.go", 3, 9, models.KindFunc, "A", span.ID)
	if unlinked.ID != linked.ID {
		t.Fatalf("the same definition hashed to two ids, %s then %s: this test is not exercising the conflict it exists for", unlinked.ID, linked.ID)
	}
	if err := s.PutGraph(ctx, repoID, []models.Symbol{unlinked, linked}, nil); err != nil {
		t.Fatal(err)
	}
	wantSymbols(t, readSymbols(t, s, repoID), []models.Symbol{linked})
}

// The two direct inserts. Nothing that goes through PutGraph can see the
// constraint, because the Go check refuses first — which is why both exist.
func TestTheSchemaRefusesAResolvedEdgeWithNoTargetLive(t *testing.T) {
	insertRawEdge(t, "noTarget", nil, "resolved", `violates check constraint "edges_provenance_target"`)
}

func TestTheSchemaRefusesASyntacticEdgeWithATargetLive(t *testing.T) {
	insertRawEdge(t, "withTarget", &selfTarget, "syntactic", `violates check constraint "edges_provenance_target"`)
}

// The hole the plan expected not to exist. The constraint's left side is never
// NULL, but its right side is NULL when provenance is, the equality is then
// NULL, and a CHECK evaluating to NULL passes: measured on pg17, (NULL, NULL)
// and ('x', NULL) are both accepted without the column's NOT NULL.
func TestTheSchemaRefusesAnEdgeWithNoProvenanceLive(t *testing.T) {
	insertRawEdge(t, "noProvenance", nil, "", `null value in column "provenance"`)
}

// §3's kind and provenance are closed sets, and a value outside either is a row
// every later query would have to carry a case for. The writer only ever spells
// them from models' constants, so this is the half of the guarantee that holds
// against a hand-run INSERT.
func TestTheSchemaRefusesAKindOrProvenanceOutsideTheSetLive(t *testing.T) {
	ctx := context.Background()
	_, _, repoID := graphIDs("enum")
	s := fresh(t, repoID)
	repoID, fileID := seedGraphRepo(t, s, "enum")
	sym := mkSym(repoID, fileID, "a.go", 3, 5, models.KindFunc, "Store.Get", "")
	if err := s.PutGraph(ctx, repoID, []models.Symbol{sym}, nil); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct{ name, kind, provenance, want string }{
		{"kind", "invokes", "syntactic", "edges_kind_check"},
		{"provenance", "calls", "guessed", "edges_provenance_check"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := s.pool.Exec(ctx, `
				INSERT INTO edges (id, repo_id, from_symbol_id, to_symbol_id, to_name, kind, provenance, path, line)
				VALUES ('raw-'||$1, $2, $3, NULL, 'Get', $1, $4, 'a.go', 8)`,
				tc.kind, repoID, sym.ID, tc.provenance)
			if err == nil {
				t.Fatalf("the INSERT succeeded, want %s", tc.want)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("the INSERT failed with %q, want %s", err, tc.want)
			}
		})
	}
}

// selfTarget is the sentinel insertRawEdge swaps for the symbol it just wrote.
var selfTarget = "self"

func insertRawEdge(t *testing.T, name string, target *string, provenance, wantErr string) {
	t.Helper()
	ctx := context.Background()
	_, _, repoID := graphIDs(name)
	s := fresh(t, repoID)
	repoID, fileID := seedGraphRepo(t, s, name)
	sym := mkSym(repoID, fileID, "a.go", 3, 5, models.KindFunc, "Store.Get", "")
	if err := s.PutGraph(ctx, repoID, []models.Symbol{sym}, nil); err != nil {
		t.Fatal(err)
	}
	if target != nil && *target == selfTarget {
		target = &sym.ID
	}
	var prov any = provenance
	if provenance == "" {
		prov = nil
	}
	_, err := s.pool.Exec(ctx, `
		INSERT INTO edges (id, repo_id, from_symbol_id, to_symbol_id, to_name, kind, provenance, path, line)
		VALUES ('raw', $1, $2, $3, 'Get', 'calls', $4, 'a.go', 8)`,
		repoID, sym.ID, target, prov)
	if err == nil {
		t.Fatalf("the INSERT succeeded, want %s", wantErr)
	}
	if !strings.Contains(err.Error(), wantErr) {
		t.Errorf("the INSERT failed with %q, want %s", err, wantErr)
	}
}

// Eviction is one DELETE that cascades (spec §3), and Evict's doc comment now
// names symbols and edges as rows it takes. Without the reference, eviction
// fails with a foreign key violation rather than orphaning — a different bug
// with a different message — so both the error and the counts are asserted.
//
// The kept repo holds a graph too, so "the cascade reached the right repo" is
// read back rather than assumed.
func TestEvictingARepoTakesItsGraphWithItLive(t *testing.T) {
	ctx := context.Background()
	s := evictFresh(t)

	older := seed(t, s, "graphGone")
	newer := seed(t, s, "graphKept")
	touch(t, s, older, newer)

	for _, id := range []string{older, newer} {
		sym := mkSym(id, FileID(id, "a.go"), "a.go", 3, 5, models.KindFunc, "Store.Get", "")
		caller := mkSym(id, FileID(id, "a.go"), "a.go", 7, 9, models.KindFunc, "Caller", "")
		if err := s.PutGraph(ctx, id, []models.Symbol{sym, caller}, []models.Edge{
			mkEdge(id, caller.ID, "a.go", 30, 8, "Get", sym.ID),
			mkEdge(id, caller.ID, "a.go", 44, 8, "Close", ""),
		}); err != nil {
			t.Fatal(err)
		}
	}

	n, err := s.Evict(ctx, 1, 10)
	if err != nil {
		t.Fatalf("Evict returned %v: eviction is one DELETE and the graph has to go with it, not block it", err)
	}
	if n != 1 {
		t.Fatalf("Evict removed %d repos, want 1", n)
	}
	gotSyms, gotEdges, err := s.CountGraph(ctx, older)
	if err != nil {
		t.Fatal(err)
	}
	if gotSyms != 0 || gotEdges != 0 {
		t.Errorf("the evicted repo left %d symbols and %d edges", gotSyms, gotEdges)
	}
	keptSyms, keptEdges, err := s.CountGraph(ctx, newer)
	if err != nil {
		t.Fatal(err)
	}
	if keptSyms != 2 || keptEdges != 2 {
		t.Errorf("the kept repo has %d symbols and %d edges, want 2 and 2", keptSyms, keptEdges)
	}
}

// PutSpans deletes a repo's spans on every re-index. A cascade from span_id
// would therefore delete the repo's symbols as a side effect of re-chunking,
// and the graph would vanish between two writes with no error anywhere.
func TestReplacingASpanLeavesTheSymbolWithANullLinkLive(t *testing.T) {
	ctx := context.Background()
	_, _, repoID := graphIDs("relink")
	s := fresh(t, repoID)
	repoID, fileID := seedGraphRepo(t, s, "relink")

	span := mkSpan(repoID, fileID, "a.go", 3, 5, "func A() {}", unit(1))
	if err := s.PutSpans(ctx, repoID, []EmbeddedSpan{span}, fakeModel, EmbeddingDim); err != nil {
		t.Fatal(err)
	}
	linked := mkSym(repoID, fileID, "a.go", 3, 5, models.KindFunc, "A", span.ID)
	unlinked := mkSym(repoID, fileID, "a.go", 7, 9, models.KindFunc, "B", "")
	if err := s.PutGraph(ctx, repoID, []models.Symbol{linked, unlinked}, nil); err != nil {
		t.Fatal(err)
	}

	// A re-chunk: same repo, a different span, so the linked one is deleted.
	next := mkSpan(repoID, fileID, "a.go", 3, 6, "func A() { return }", unit(2))
	if err := s.PutSpans(ctx, repoID, []EmbeddedSpan{next}, fakeModel, EmbeddingDim); err != nil {
		t.Fatal(err)
	}
	linked.SpanID = ""
	wantSymbols(t, readSymbols(t, s, repoID), []models.Symbol{linked, unlinked})
}

// migrate() records a name and skips the body, so the idempotency test in
// store_live_test.go proves the ledger works and nothing about this file. The
// body is re-run here instead, against repos, files and spans that already hold
// rows — the case a deployed database is always in and a fresh one never is —
// and with graph rows written between the two runs, which is what a migration
// that dropped and re-created its tables would erase while still applying
// cleanly.
//
// In one rolled-back transaction, because it drops the two tables the rest of
// this binary reads.
func TestMigration0010AppliesToAPopulatedDatabaseTwiceLive(t *testing.T) {
	ctx := context.Background()
	s, err := New(ctx, dsn(t))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	body, err := migrationFS.ReadFile("migrations/0010_symbol_graph.sql")
	if err != nil {
		t.Fatal(err)
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx)

	// Back to before 0010, then populated, so the CREATEs run against a
	// database whose parents already hold rows and take locks on them.
	for _, stmt := range []string{
		`DROP TABLE IF EXISTS edges`,
		`DROP TABLE IF EXISTS symbols`,
		`INSERT INTO repos (id, remote, ref, commit_sha) VALUES ('m10', 'https://github.com/a/m10', 'main', 'c0ffee')`,
		`INSERT INTO files (id, repo_id, path, blob, lang, lines) VALUES ('m10-f', 'm10', 'a.go', '', 'go', 9)`,
		`INSERT INTO spans (id, repo_id, file_id, path, kind, symbol, start_line, end_line,
			text, digest, embed_model, embed_dim)
			VALUES ('m10-s', 'm10', 'm10-f', 'a.go', 'func', 'Store.Get', 3, 5, 'body', 'd', 'm', 768)`,
	} {
		if _, err := tx.Exec(ctx, stmt); err != nil {
			t.Fatal(err)
		}
	}

	for i := range 2 {
		if _, err := tx.Exec(ctx, string(body)); err != nil {
			t.Fatalf("run %d: %v", i+1, err)
		}
		if i == 0 {
			// Written between the two runs: a migration that dropped and
			// re-created its tables would erase these and still apply cleanly.
			for _, stmt := range []string{
				`INSERT INTO symbols (id, repo_id, file_id, path, name, pkg, kind, start_line, end_line, span_id)
					VALUES ('m10-y', 'm10', 'm10-f', 'a.go', 'Store.Get', 'p', 'func', 3, 5, 'm10-s')`,
				`INSERT INTO edges (id, repo_id, from_symbol_id, to_symbol_id, to_name, kind, provenance, path, line)
					VALUES ('m10-e', 'm10', 'm10-y', 'm10-y', 'Get', 'calls', 'resolved', 'a.go', 4)`,
			} {
				if _, err := tx.Exec(ctx, stmt); err != nil {
					t.Fatalf("run 1: %v", err)
				}
			}
		}

		// The invariant has to hold after both runs, not only the first. In a
		// savepoint, because the failure it asserts would otherwise abort the
		// transaction the rest of this test runs in.
		sp, err := tx.Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		_, err = sp.Exec(ctx, `
			INSERT INTO edges (id, repo_id, from_symbol_id, to_symbol_id, to_name, kind, provenance, path, line)
			VALUES ('m10-bad', 'm10', 'm10-y', NULL, 'Get', 'calls', 'resolved', 'a.go', 4)`)
		if err == nil {
			t.Errorf("run %d: a resolved edge with no target was accepted", i+1)
		} else if !strings.Contains(err.Error(), "edges_provenance_target") {
			t.Errorf("run %d: the INSERT failed with %q, want edges_provenance_target", i+1, err)
		}
		if err := sp.Rollback(ctx); err != nil {
			t.Fatal(err)
		}

		var span *string
		if err := tx.QueryRow(ctx, `SELECT span_id FROM symbols WHERE id = 'm10-y'`).Scan(&span); err != nil {
			t.Fatalf("run %d: the symbol is gone: %v", i+1, err)
		}
		if span == nil || *span != "m10-s" {
			t.Errorf("run %d: the symbol's span link reads %v, want m10-s", i+1, span)
		}
		var to string
		if err := tx.QueryRow(ctx, `SELECT to_symbol_id FROM edges WHERE id = 'm10-e'`).Scan(&to); err != nil {
			t.Fatalf("run %d: the edge is gone: %v", i+1, err)
		}
		if to != "m10-y" {
			t.Errorf("run %d: the edge points at %q, want m10-y", i+1, to)
		}
	}
}
