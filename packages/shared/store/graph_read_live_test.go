//go:build live

package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/mralaminahamed/codetrail/packages/shared/models"
)

// The fixture every read below runs against, built so that each control in the
// query is separately visible:
//
//	outer ─▶ main ─▶ a ─▶ b ─▶ target   a chain, so depth is observable
//	         main ─▶ c ─▶ target        a diamond: two routes to target
//	                target ─▶ target    a direct self-call
//	         d ─▶ e ─▶ d, e ─▶ target   a two-node cycle reaching target
//	Store.Get and Cache.Get             two definitions sharing a last segment
//	f ─▶ "Get", f ─▶ "target"           syntactic edges, null target
//	g ─▶ Store.Get, h ─▶ Cache.Get      resolved edges naming Get
//
// outer exists because the diamond puts main at depth 2 by the short route, so
// nothing in the plan's own fixture is reachable only at depth 3 — and min()
// then hides an off-by-one in the depth bound at every depth above 1.
//
// The callers are split across two files whose path order disagrees with their
// depth order (z.go holds the near ones, a.go the far ones), so an answer
// served in path order is not the expected one. TestCallersOfWalksTheChain
// asserts that disagreement rather than trusting it.
//
// Repo B holds a symbol also named target with six callers to repo A's four,
// so a read that lost its repo filter returns a different answer rather than
// an extra row nobody looks at.
type graphFixture struct {
	s     *Store
	repoA string
	repoB string
	sym   map[string]models.Symbol
	symB  map[string]models.Symbol
}

func seedReadGraph(t *testing.T) graphFixture {
	t.Helper()
	ctx := context.Background()
	remoteA, commitA, idA := graphIDs("reads")
	remoteB, commitB, idB := graphIDs("readsOther")
	s := fresh(t, idA, idB)

	putRepo := func(remote, commit, id string, paths ...string) map[string]string {
		files := make([]models.File, 0, len(paths))
		ids := map[string]string{}
		for _, p := range paths {
			ids[p] = FileID(id, p)
			files = append(files, models.File{
				ID: ids[p], RepoID: id, Path: p, Blob: Digest(id + p)[:8], Lang: "go", Lines: 40,
			})
		}
		if err := s.PutRepo(ctx,
			models.Repo{ID: id, Remote: remote, Ref: "main", Commit: commit}, files); err != nil {
			t.Fatal(err)
		}
		return ids
	}

	fa := putRepo(remoteA, commitA, idA, "a.go", "cache.go", "store.go", "z.go")
	sym := map[string]models.Symbol{}
	def := func(path string, start, end int, name string) {
		sym[name] = mkSym(idA, fa[path], path, start, end, models.KindFunc, name, "")
	}
	def("z.go", 3, 5, "b")
	def("z.go", 7, 9, "c")
	def("z.go", 11, 14, "e")
	def("z.go", 15, 17, "target")
	def("a.go", 3, 5, "a")
	def("a.go", 7, 10, "main")
	def("a.go", 11, 13, "d")
	def("a.go", 15, 17, "outer")
	def("store.go", 3, 5, "Store.Get")
	def("store.go", 7, 9, "g")
	def("cache.go", 3, 5, "Cache.Get")
	def("cache.go", 7, 10, "f")
	def("cache.go", 11, 13, "h")

	var syms []models.Symbol
	for _, sy := range sym {
		syms = append(syms, sy)
	}

	// The offset is the edge's key, so every call site gets its own.
	off := 100
	call := func(from, path string, line int, toName, to string) models.Edge {
		off += 10
		return mkEdge(idA, sym[from].ID, path, off, line, toName, to)
	}
	id := func(n string) string { return sym[n].ID }
	edges := []models.Edge{
		call("outer", "a.go", 16, "main", id("main")),
		// main's two hops are on two lines: the diamond's short route is the
		// one whose call site a caller row must cite.
		call("main", "a.go", 8, "a", id("a")),
		call("main", "a.go", 9, "c", id("c")),
		call("a", "a.go", 4, "b", id("b")),
		call("b", "z.go", 4, "target", id("target")),
		call("c", "z.go", 8, "target", id("target")),
		call("target", "z.go", 16, "target", id("target")),
		call("d", "a.go", 12, "e", id("e")),
		call("e", "z.go", 12, "d", id("d")),
		call("e", "z.go", 13, "target", id("target")),
		call("g", "store.go", 8, "Get", id("Store.Get")),
		call("f", "cache.go", 8, "Get", ""),
		call("h", "cache.go", 12, "Get", id("Cache.Get")),
		call("f", "cache.go", 9, "target", ""),
	}
	if err := s.PutGraph(ctx, idA, syms, edges); err != nil {
		t.Fatal(err)
	}

	fb := putRepo(remoteB, commitB, idB, "a.go")
	symB := map[string]models.Symbol{"target": mkSym(idB, fb["a.go"], "a.go", 3, 5, models.KindFunc, "target", "")}
	symsB := []models.Symbol{symB["target"]}
	var edgesB []models.Edge
	for i := range 6 {
		name := fmt.Sprintf("caller%d", i)
		symB[name] = mkSym(idB, fb["a.go"], "a.go", 7+4*i, 9+4*i, models.KindFunc, name, "")
		symsB = append(symsB, symB[name])
		edgesB = append(edgesB,
			mkEdge(idB, symB[name].ID, "a.go", 1000+10*i, 8+4*i, "target", symB["target"].ID))
		// Repo B carries syntactic edges too, so an unscoped approximate read
		// changes its answer as well.
		if i < 3 {
			edgesB = append(edgesB,
				mkEdge(idB, symB[name].ID, "a.go", 2000+10*i, 8+4*i, "target", ""))
		}
	}
	if err := s.PutGraph(ctx, idB, symsB, edgesB); err != nil {
		t.Fatal(err)
	}
	return graphFixture{s: s, repoA: idA, repoB: idB, sym: sym, symB: symB}
}

// callerNames renders the answer as "name:depth", which is what every depth
// assertion below compares — a set of names alone survives a mutant that only
// moves a caller to the wrong depth.
func callerNames(cs []Caller) []string {
	out := make([]string, 0, len(cs))
	for _, c := range cs {
		out = append(out, fmt.Sprintf("%s:%d", c.Symbol.Name, c.Depth))
	}
	return out
}

func (f graphFixture) callers(t *testing.T, depth, limit int) []Caller {
	t.Helper()
	cs, err := f.s.CallersOf(context.Background(), f.repoA, f.sym["target"].ID, depth, limit)
	if err != nil {
		t.Fatal(err)
	}
	return cs
}

// The whole ordered set with every row's depth. A membership assertion passes
// under a mutant that inverts the traversal's direction, duplicates a diamond's
// caller or reports the longer of two routes.
func TestCallersOfWalksTheChainAndReportsDepthLive(t *testing.T) {
	f := seedReadGraph(t)
	want := []string{"b:1", "c:1", "e:1", "target:1", "a:2", "main:2", "d:2", "outer:3"}
	got := callerNames(f.callers(t, 5, 50))
	if !slices.Equal(got, want) {
		t.Fatalf("callers of target: %v, want %v", got, want)
	}

	// The order has to disagree with the orders an unordered result comes back
	// in, or asserting it proves nothing. P3 measured twice that a result with
	// no ORDER BY arrives in the order an index served it.
	byPath := slices.SortedFunc(slices.Values(f.callers(t, 5, 50)), func(x, y Caller) int {
		if x.Symbol.Path != y.Symbol.Path {
			return strings.Compare(x.Symbol.Path, y.Symbol.Path)
		}
		return x.Symbol.StartLine - y.Symbol.StartLine
	})
	if slices.Equal(callerNames(byPath), want) {
		t.Fatal("fixture: depth order and path order agree, so the ORDER BY asserts nothing")
	}
	byID := slices.SortedFunc(slices.Values(f.callers(t, 5, 50)), func(x, y Caller) int {
		return strings.Compare(x.Symbol.ID, y.Symbol.ID)
	})
	if slices.Equal(callerNames(byID), want) {
		t.Fatal("fixture: depth order and id order agree, so the ORDER BY asserts nothing")
	}

	// With a LIMIT below the result size the order decides membership, not
	// presentation: the four nearest callers, not four arbitrary ones.
	if got, want := callerNames(f.callers(t, 5, 4)), want[:4]; !slices.Equal(got, want) {
		t.Errorf("callers of target limited to 4: %v, want %v", got, want)
	}
}

// f calls f is ordinary Go. The self-call makes target its own caller once, at
// depth 1 — not twice, and not at every depth the bound allows.
func TestCallersOfTerminatesOnASelfCallLive(t *testing.T) {
	f := seedReadGraph(t)
	var self []Caller
	for _, c := range f.callers(t, 5, 50) {
		if c.Symbol.Name == "target" {
			self = append(self, c)
		}
	}
	if len(self) != 1 {
		t.Fatalf("target is its own caller %d times: %v", len(self), callerNames(self))
	}
	if self[0].Depth != 1 || self[0].CallPath != "z.go" || self[0].CallLine != 16 {
		t.Errorf("the self-call is depth %d at %s:%d, want depth 1 at z.go:16",
			self[0].Depth, self[0].CallPath, self[0].CallLine)
	}
}

// d calls e calls d, and e calls target. Both are callers, each once, at the
// depth of the route that does not go round the cycle.
func TestCallersOfTerminatesOnATwoNodeCycleLive(t *testing.T) {
	f := seedReadGraph(t)
	seen := map[string][]int{}
	for _, c := range f.callers(t, 5, 50) {
		seen[c.Symbol.Name] = append(seen[c.Symbol.Name], c.Depth)
	}
	for name, wantDepth := range map[string]int{"e": 1, "d": 2} {
		got := seen[name]
		if len(got) != 1 {
			t.Errorf("%s appears %d times in the answer, at depths %v", name, len(got), got)
			continue
		}
		if got[0] != wantDepth {
			t.Errorf("%s is at depth %d, want %d", name, got[0], wantDepth)
		}
	}
}

// The cycle guard cannot change the rows this query returns — min(depth) is the
// BFS distance and a walk that revisits a node is never shorter than the path
// that does not — so nothing in the result set can read it back. What it
// changes is how much work the traversal does, and that is a number.
//
// Asserted as an absolute first, so a traversal that stopped bounding itself
// fails with a count rather than with a comparison against a second query the
// same mutation would also have changed. The counterfactual follows, because
// 19 is only evidence next to what the unguarded form actually costs.
func TestTheCycleGuardBoundsTheTraversalItselfLive(t *testing.T) {
	f := seedReadGraph(t)
	ctx := context.Background()

	rowsOf := func(sql string) float64 {
		t.Helper()
		var raw []byte
		if err := f.s.pool.QueryRow(ctx, "EXPLAIN (ANALYZE, TIMING OFF, FORMAT JSON) "+sql,
			f.repoA, f.sym["target"].ID, 5, 50).Scan(&raw); err != nil {
			t.Fatal(err)
		}
		var plans []struct {
			Plan map[string]any `json:"Plan"`
		}
		if err := json.Unmarshal(raw, &plans); err != nil {
			t.Fatal(err)
		}
		n, ok := findPlanRows(plans[0].Plan, "Recursive Union")
		if !ok {
			t.Fatalf("no Recursive Union in the plan:\n%s", raw)
		}
		return n
	}

	const wantRows = 19
	if got := rowsOf(callersSQL); got != wantRows {
		t.Errorf("the traversal produced %v rows at depth 5, want %v", got, wantRows)
	}

	const guard = "AND NOT e.from_symbol_id = ANY(c.path)"
	unguarded := strings.Replace(callersSQL, guard, "", 1)
	if unguarded == callersSQL {
		t.Errorf("the counterfactual rewrote nothing: callersSQL no longer contains %q", guard)
		return
	}
	if got := rowsOf(unguarded); got <= wantRows {
		t.Errorf("without the guard the traversal produced %v rows, not more than %v: "+
			"the fixture has no cycle for the guard to stop", got, wantRows)
	}
}

// findPlanRows walks an EXPLAIN JSON tree for the first node of a kind and
// returns the rows it actually produced.
func findPlanRows(node map[string]any, kind string) (float64, bool) {
	if node["Node Type"] == kind {
		rows, _ := node["Actual Rows"].(float64)
		loops, _ := node["Actual Loops"].(float64)
		return rows * loops, true
	}
	kids, _ := node["Plans"].([]any)
	for _, k := range kids {
		if m, ok := k.(map[string]any); ok {
			if n, found := findPlanRows(m, kind); found {
				return n, true
			}
		}
	}
	return 0, false
}

// main reaches target two ways: main → c → target and main → a → b → target.
// That is one caller at its shortest distance, and the call site it cites is
// the short route's — a row reporting depth 2 beside the long route's line
// would be citing a fact it is not reporting.
func TestADiamondYieldsOneCallerAtItsShortestDepthLive(t *testing.T) {
	f := seedReadGraph(t)
	var mains []Caller
	for _, c := range f.callers(t, 5, 50) {
		if c.Symbol.Name == "main" {
			mains = append(mains, c)
		}
	}
	if len(mains) != 1 {
		t.Fatalf("main is in the answer %d times, at depths %v: two routes to one caller are one caller",
			len(mains), callerNames(mains))
	}
	if mains[0].Depth != 2 {
		t.Errorf("main at depth %d, want depth 2 (main → c → target, not main → a → b → target)", mains[0].Depth)
	}
	if mains[0].CallPath != "a.go" || mains[0].CallLine != 9 {
		t.Errorf("main cites %s:%d, want a.go:9 — the hop on the shortest route, not a.go:8",
			mains[0].CallPath, mains[0].CallLine)
	}
}

// The bound is the caller's. Every depth is asserted because min(depth) hides
// an extra level wherever a node is already reachable sooner: at depth 2 the
// only row an off-by-one adds is outer's, which exists for that reason.
func TestCallersOfStopsAtTheRequestedDepthLive(t *testing.T) {
	f := seedReadGraph(t)
	for _, tc := range []struct {
		depth int
		want  []string
	}{
		{1, []string{"b:1", "c:1", "e:1", "target:1"}},
		{2, []string{"b:1", "c:1", "e:1", "target:1", "a:2", "main:2", "d:2"}},
		{3, []string{"b:1", "c:1", "e:1", "target:1", "a:2", "main:2", "d:2", "outer:3"}},
		{5, []string{"b:1", "c:1", "e:1", "target:1", "a:2", "main:2", "d:2", "outer:3"}},
	} {
		if got := callerNames(f.callers(t, tc.depth, 50)); !slices.Equal(got, tc.want) {
			t.Errorf("depth=%d returned %v, want %v", tc.depth, got, tc.want)
		}
	}
}

// spec:84: a syntactic edge knows it calls something named target and cannot
// say which one. Binding that name to a symbol at read time re-introduces the
// guess in the one place no column records that it happened.
//
// f is the proof. It calls something named target and something named Get, both
// with a null target, and it calls neither of the definitions that carry those
// names. Two definitions share the last segment Get so that a reader binding by
// name returns a caller of the other one.
func TestCallersOfNeverIncludesASyntacticEdgeLive(t *testing.T) {
	f := seedReadGraph(t)
	for _, c := range f.callers(t, 5, 50) {
		if c.Symbol.Name == "f" {
			t.Errorf("callers of target include f, which calls something named target with a null target (%s:%d)",
				c.CallPath, c.CallLine)
		}
	}

	got, err := f.s.CallersOf(context.Background(), f.repoA, f.sym["Store.Get"].ID, 5, 50)
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"g:1"}; !slices.Equal(callerNames(got), want) {
		t.Errorf("callers of Store.Get: %v, want %v — f calls a null target named Get and h calls Cache.Get",
			callerNames(got), want)
	}
}

// Every hop cites its own call site, not the target's and not the caller's
// declaration. Without this the answer is a list of names in a product whose
// first promise is file:line.
func TestCallersOfReportsTheCallSiteOfEachHopLive(t *testing.T) {
	f := seedReadGraph(t)
	want := map[string]string{
		"b": "z.go:4", "c": "z.go:8", "e": "z.go:13", "target": "z.go:16",
		"a": "a.go:4", "main": "a.go:9", "d": "a.go:12", "outer": "a.go:16",
	}
	got := map[string]string{}
	for _, c := range f.callers(t, 5, 50) {
		got[c.Symbol.Name] = fmt.Sprintf("%s:%d", c.CallPath, c.CallLine)
	}
	for name, w := range want {
		if got[name] != w {
			t.Errorf("%s cites %q, want %q", name, got[name], w)
		}
	}
}

// The approximate set answers a different question — what else here calls
// something with this name — and it is returned separately so a caller can see
// which half of the answer is a fact.
func TestApproximateCallersAreNameMatchedAndSeparateLive(t *testing.T) {
	f := seedReadGraph(t)
	ctx := context.Background()
	got, err := f.s.ApproximateCallersOf(ctx, f.repoA, "Get", 50)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 {
		t.Fatalf("approximate callers of Get: %d rows %v, want 1 (f)", len(got), approxNames(got))
	}
	a := got[0]
	if a.Symbol.Name != "f" || a.ToName != "Get" || a.CallPath != "cache.go" || a.CallLine != 8 {
		t.Errorf("approximate caller %s calls %q at %s:%d, want f calls \"Get\" at cache.go:8",
			a.Symbol.Name, a.ToName, a.CallPath, a.CallLine)
	}

	// Separate, not merged: f is in neither precise answer, and the precise
	// caller g is not in this one.
	precise, err := f.s.CallersOf(ctx, f.repoA, f.sym["Store.Get"].ID, 5, 50)
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range precise {
		if c.Symbol.Name == "f" {
			t.Error("f is in the precise callers of Store.Get")
		}
	}

	// Scoped: repo B has three syntactic edges naming target and repo A has one.
	scoped, err := f.s.ApproximateCallersOf(ctx, f.repoA, "target", 50)
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"f"}; !slices.Equal(approxNames(scoped), want) {
		t.Errorf("approximate callers of target in repo A: %v, want %v", approxNames(scoped), want)
	}
}

func approxNames(as []Approximate) []string {
	out := make([]string, 0, len(as))
	for _, a := range as {
		out = append(out, a.Symbol.Name)
	}
	return out
}

// A resolved edge is already in the precise answer. Counting it here too would
// present a caller the query knows as one it had to guess at.
func TestApproximateCallersExcludeResolvedEdgesLive(t *testing.T) {
	f := seedReadGraph(t)
	got, err := f.s.ApproximateCallersOf(context.Background(), f.repoA, "Get", 50)
	if err != nil {
		t.Fatal(err)
	}
	// g calls Store.Get and h calls Cache.Get, both resolved, both naming Get.
	if want := []string{"f"}; !slices.Equal(approxNames(got), want) {
		t.Errorf("approximate callers of Get: %v, want %v (g and h resolve)", approxNames(got), want)
	}
}

// Exact by default. P3's lexical arm reaches Store.Get through its parts, so a
// caller who found it by searching types Get — and answering that with every
// Get in the repository answers a question they did not ask, in a payload that
// cannot say so.
func TestDefinitionsMatchesExactlyUnlessSuffixIsAskedLive(t *testing.T) {
	f := seedReadGraph(t)
	ctx := context.Background()
	defs := func(name string, suffix bool) []string {
		t.Helper()
		got, err := f.s.Definitions(ctx, f.repoA, name, suffix, 50)
		if err != nil {
			t.Fatal(err)
		}
		out := make([]string, 0, len(got))
		for _, sy := range got {
			out = append(out, sy.Name)
		}
		return out
	}
	if got := defs("Get", false); len(got) != 0 {
		t.Errorf("exact match for \"Get\" returned %d definitions %v, want 0", len(got), got)
	}
	if got, want := defs("Get", true), []string{"Cache.Get", "Store.Get"}; !slices.Equal(got, want) {
		t.Errorf("suffix match for \"Get\": %v, want %v", got, want)
	}
	if got, want := defs("Store.Get", false), []string{"Store.Get"}; !slices.Equal(got, want) {
		t.Errorf("exact match for \"Store.Get\": %v, want %v", got, want)
	}
	// The opt-in widens and never narrows: an exact name still matches itself.
	if got, want := defs("Store.Get", true), []string{"Store.Get"}; !slices.Equal(got, want) {
		t.Errorf("suffix match for \"Store.Get\": %v, want %v", got, want)
	}
}

// parseConfig exists in every second Go repository. Repo B's target has six
// callers to repo A's four, so an unscoped read is a different answer rather
// than a longer one.
func TestDefinitionsIsScopedToOneRepoLive(t *testing.T) {
	f := seedReadGraph(t)
	ctx := context.Background()
	got, err := f.s.Definitions(ctx, f.repoA, "target", false, 50)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 {
		t.Fatalf("definitions of target: %d, want 1 (the extra is from repo B): %+v", len(got), got)
	}
	if got[0].ID != f.sym["target"].ID || got[0].RepoID != f.repoA {
		t.Errorf("definitions of target returned %s in repo %s, want %s in %s",
			got[0].ID, got[0].RepoID, f.sym["target"].ID, f.repoA)
	}
	// Repo B's own answer is its own, and the two do not collide.
	other, err := f.s.Definitions(ctx, f.repoB, "target", false, 50)
	if err != nil {
		t.Fatal(err)
	}
	if len(other) != 1 || other[0].ID != f.symB["target"].ID {
		t.Errorf("definitions of target in repo B: %+v, want just %s", other, f.symB["target"].ID)
	}
}

// A symbol id is a primary key, so an unscoped read would answer a caller who
// named this repository with another one's definition.
func TestSymbolIsScopedToItsRepositoryLive(t *testing.T) {
	f := seedReadGraph(t)
	ctx := context.Background()
	got, err := f.s.Symbol(ctx, f.repoA, f.sym["target"].ID)
	if err != nil {
		t.Fatal(err)
	}
	if got != f.sym["target"] {
		t.Errorf("read back %+v, want %+v", got, f.sym["target"])
	}
	if _, err := f.s.Symbol(ctx, f.repoB, f.sym["target"].ID); !errors.Is(err, ErrNotFound) {
		t.Errorf("repo A's target read from repo B returned %v, want ErrNotFound", err)
	}
	if _, err := f.s.Symbol(ctx, f.repoA, "nosuchsymbol"); !errors.Is(err, ErrNotFound) {
		t.Errorf("an unknown symbol returned %v, want ErrNotFound", err)
	}
}

// The split is what makes "three packages resolved and two did not" visible at
// all. Repo A carries both labels; a repo whose edges all resolve makes the two
// counts equal and a summary that read one total twice would look right.
func TestRepoStatsSplitsEdgesByProvenanceLive(t *testing.T) {
	f := seedReadGraph(t)
	ctx := context.Background()
	got, err := f.s.RepoStats(ctx, f.repoA)
	if err != nil {
		t.Fatal(err)
	}
	want := Stats{
		Files: 4, Spans: 0, FilesWithSpans: 0,
		Symbols: 13, Edges: 14, EdgesResolved: 12, EdgesSyntactic: 2,
	}
	if got != want {
		t.Errorf("repo A stats %+v, want %+v", got, want)
	}
	// Repo B is bigger on every count, so a lost repo filter is a changed
	// answer rather than an extra row.
	otherWant := Stats{Files: 1, Symbols: 7, Edges: 9, EdgesResolved: 6, EdgesSyntactic: 3}
	otherGot, err := f.s.RepoStats(ctx, f.repoB)
	if err != nil {
		t.Fatal(err)
	}
	if otherGot != otherWant {
		t.Errorf("repo B stats %+v, want %+v", otherGot, otherWant)
	}
}
