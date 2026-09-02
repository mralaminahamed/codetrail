package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/prometheus/client_golang/prometheus/promhttp"

	"github.com/mralaminahamed/codetrail/packages/shared/chunk"
	"github.com/mralaminahamed/codetrail/packages/shared/metrics"
	"github.com/mralaminahamed/codetrail/packages/shared/models"
	"github.com/mralaminahamed/codetrail/packages/shared/symbols"
)

// graphRepo is two packages, and every property this stage claims needs both
// of them.
//
// a.go carries a call that resolves (G, twice on one line, so two edges share
// a line and only an offset tells them apart), a call that cannot resolve
// because it leaves the repository (fmt.Println), and a callee with no name at
// all (fns[0]()). b/util.go carries a call to something no package here
// declares. So one fixture holds both labels, and the assertions below name an
// edge in each package rather than counting them — a count passes under a
// mutant that swaps which package got which label.
var graphRepo = map[string]string{
	"a.go": `package p

import "fmt"

// F reports what G says about n, twice on one line.
func F(n int) int {
	fmt.Println(G(n), G(n))
	return fns[0]()
}

// G doubles n.
func G(n int) int {
	return n * 2
}

var fns = []func() int{}
`,
	"b/util.go": `package b

// Helper calls something no package in this repository declares.
func Helper() int {
	return Missing()
}
`,
}

// gKeys is the two call sites in a.go that a working type-checker names: both
// calls to G, keyed by byte offset into the bytes on disk.
//
// Derived from Parse rather than written as literals, because the point of the
// key is that it is an offset into the file the loader reads — a literal would
// still match if the stage handed Parse a different stream of bytes.
func gKeys(t *testing.T) []symbols.Key {
	t.Helper()
	f, err := symbols.Parse("a.go", []byte(graphRepo["a.go"]))
	if err != nil {
		t.Fatal(err)
	}
	var out []symbols.Key
	for _, c := range f.Calls {
		if c.Name == "G" {
			out = append(out, symbols.Key{Path: c.Path, Offset: c.Offset})
		}
	}
	if len(out) != 2 {
		t.Fatalf("the fixture has %d calls to G, want the two on one line", len(out))
	}
	return out
}

// resolvedG is the answer a working type-checker gives for graphRepo: both
// calls to G point at G's declaration.
//
// Line 12 is the `func G` line and G's definition starts at 11, its doc
// comment. They are deliberately different: a target is the declaration's own
// token and a Def's range starts at the comment, so a stage that matched them
// by equality would resolve nothing here.
func resolvedG(t *testing.T) map[symbols.Key]symbols.Target {
	t.Helper()
	out := map[symbols.Key]symbols.Target{}
	for _, k := range gKeys(t) {
		out[k] = symbols.Target{Path: "a.go", Line: 12}
	}
	return out
}

// edge is one written edge, spelled the way a reviewer can check it against
// the fixture.
type edge struct {
	from, to, at, provenance string
}

func edges(t *testing.T, rec *recorder) []edge {
	t.Helper()
	names := map[string]string{}
	for _, s := range rec.syms {
		names[s.ID] = s.Name
	}
	out := make([]edge, 0, len(rec.edges))
	for _, e := range rec.edges {
		to := ""
		if e.ToSymbolID != "" {
			to = names[e.ToSymbolID]
			if to == "" {
				t.Errorf("edge to %s at %s:%d points at %s, which is not a symbol this job wrote",
					e.ToName, e.Path, e.Line, e.ToSymbolID)
			}
		}
		out = append(out, edge{
			from: names[e.FromSymbolID], to: e.ToName + "->" + to,
			at: fmt.Sprintf("%s:%d", e.Path, e.Line), provenance: string(e.Provenance),
		})
	}
	return out
}

func (e edge) String() string {
	return fmt.Sprintf("%s calls %s at %s (%s)", e.from, e.to, e.at, e.provenance)
}

func edgeStrings(es []edge) []string {
	out := make([]string, 0, len(es))
	for _, e := range es {
		out = append(out, e.String())
	}
	return out
}

// Spec §6: the edge set is the AST's and type information only upgrades rows.
// Run the same fixture with and without a resolver and the edges are the same
// edges — same tails, same names, same call sites — differing only in their
// label and their target.
func TestEveryCallIsAnEdgeBeforeAnythingIsResolved(t *testing.T) {
	ix, unresolved := fakeIndexer(t, withFiles(graphRepo),
		withResolver(nil, symbols.Stats{Reason: symbols.ReasonLoadError}))
	ix.runJob(context.Background(), aJob())

	ix, resolved := fakeIndexer(t, withFiles(graphRepo),
		withResolver(resolvedG(t), symbols.Stats{Reason: symbols.ReasonOK, Resolved: 2, External: 1, Unresolved: 1}))
	ix.runJob(context.Background(), aJob())

	// Where the edges are, not how many: a count is equal under a stage that
	// wrote a different edge for every call site it could not name.
	sites := func(rec *recorder) []string {
		out := []string{}
		for _, e := range edges(t, rec) {
			out = append(out, e.from+" calls "+strings.Split(e.to, "->")[0]+" at "+e.at)
		}
		return out
	}
	if a, b := sites(unresolved), sites(resolved); !slices.Equal(a, b) {
		t.Fatalf("resolution changed the edge set:\n without %v\n with    %v", a, b)
	}
	want := []string{
		"F calls Println at a.go:7",
		"F calls G at a.go:7",
		"F calls G at a.go:7",
		"Helper calls Missing at b/util.go:5",
	}
	if got := sites(unresolved); !slices.Equal(got, want) {
		t.Fatalf("edges:\n got %v\nwant %v", got, want)
	}
	// Two calls on one line are two rows, and the ids are what make them two.
	if a, b := rec2ids(resolved)[1], rec2ids(resolved)[2]; a == b {
		t.Errorf("both calls to G on line 7 share the edge id %s", a)
	}
}

func rec2ids(rec *recorder) []string {
	out := make([]string, 0, len(rec.edges))
	for _, e := range rec.edges {
		out = append(out, e.ID)
	}
	return out
}

// Spec:190, and the mutation this whole phase is written against: the label is
// a property of the call site, never of the package or the job.
//
// Both halves are needed. With reason "ok" a stage that stamped every edge
// resolved would pass anything that only checked the resolved ones; with a
// failed load a stage that stamped every edge syntactic would pass anything
// that only checked those. Each run names the label of an edge in each
// package.
func TestOnlyTheCallSitesTheResolverNamedAreResolved(t *testing.T) {
	for _, tc := range []struct {
		name  string
		stats symbols.Stats
	}{
		{"the load succeeded", symbols.Stats{Reason: symbols.ReasonOK, Resolved: 2, External: 1, Unresolved: 1}},
		// The stronger form, and the one Task 3 measured on a real
		// repository: a package fails to load and calls in the package that
		// did load still resolve.
		{"a package failed to load", symbols.Stats{Reason: symbols.ReasonLoadError, Packages: 2, Loaded: 1, Failed: 1, Resolved: 2}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ix, rec := fakeIndexer(t, withFiles(graphRepo), withResolver(resolvedG(t), tc.stats))
			ix.runJob(context.Background(), aJob())

			want := []string{
				"F calls Println-> at a.go:7 (syntactic)",
				"F calls G->G at a.go:7 (resolved)",
				"F calls G->G at a.go:7 (resolved)",
				"Helper calls Missing-> at b/util.go:5 (syntactic)",
			}
			if got := edgeStrings(edges(t, rec)); !slices.Equal(got, want) {
				t.Fatalf("edges:\n got %s\nwant %s",
					strings.Join(got, "\n      "), strings.Join(want, "\n      "))
			}
		})
	}
}

// A target names the line of the declaration itself; a definition's range
// starts at its doc comment. Matching the two by equality resolves nothing for
// a documented declaration, which is most of them, and nothing about the
// output says why.
func TestATargetIsMatchedToTheDefinitionThatContainsIt(t *testing.T) {
	ix, rec := fakeIndexer(t, withFiles(graphRepo),
		withResolver(resolvedG(t), symbols.Stats{Reason: symbols.ReasonOK}))
	ix.runJob(context.Background(), aJob())

	var g models.Symbol
	for _, s := range rec.syms {
		if s.Name == "G" {
			g = s
		}
	}
	if g.StartLine != 11 || g.EndLine != 14 {
		t.Fatalf("G spans %d..%d, want 11..14 — the fixture no longer separates the two lines", g.StartLine, g.EndLine)
	}
	for _, e := range rec.edges {
		if e.ToName != "G" {
			continue
		}
		if e.ToSymbolID != g.ID {
			t.Fatalf("the edge to G at %s:%d points at %q, want G's own id %q: the target's line 12 is inside G's range and is not its first line",
				e.Path, e.Line, e.ToSymbolID, g.ID)
		}
	}
}

// The tail of an edge is the declaration the call is written inside. A wrong
// tail is a wrong answer to "who calls this" that looks exactly like a right
// one.
func TestTheEdgeTailIsTheDeclarationTheCallIsIn(t *testing.T) {
	ix, rec := fakeIndexer(t, withFiles(graphRepo),
		withResolver(resolvedG(t), symbols.Stats{Reason: symbols.ReasonOK}))
	ix.runJob(context.Background(), aJob())

	for _, e := range edges(t, rec) {
		want := "F"
		if strings.HasPrefix(e.at, "b/util.go") {
			want = "Helper"
		}
		if e.from != want {
			t.Errorf("%s: the edge leaves %s, want %s", e, e.from, want)
		}
	}
}

// Spec:196: a type-check that does not happen is an outcome. Every reason
// leaves a complete graph of syntactic edges, a counted outcome and a done
// job — and each reason is distinct, because "we have no toolchain" and "this
// repository has no in-repo calls" are otherwise the same silence.
func TestAResolverFailureLeavesEveryEdgeSyntacticAndTheJobIntact(t *testing.T) {
	for _, tc := range []struct {
		name string
		opts []fixtureOpt
		set  func(ix *indexer)
		want string
	}{
		{name: "the load failed", opts: []fixtureOpt{withResolver(nil, symbols.Stats{Reason: symbols.ReasonLoadError})},
			want: symbols.ReasonLoadError},
		{name: "the budget was spent", opts: []fixtureOpt{withResolver(nil, symbols.Stats{Reason: symbols.ReasonDeadline})},
			want: symbols.ReasonDeadline},
		{name: "there is no module", opts: []fixtureOpt{withResolver(nil, symbols.Stats{Reason: symbols.ReasonNoModule})},
			want: symbols.ReasonNoModule},
		// The knob, proven by a resolver that panics if it is called at all:
		// asserting that the edges are syntactic also passes when the
		// resolver ran and failed.
		{name: "typechecking is off", opts: []fixtureOpt{typechecking(false), withPanickingResolver()},
			want: symbols.ReasonDisabled},
		// The real loader with no go binary. Nothing is forked: Validate
		// refuses a GoBin the parent's PATH does not resolve to.
		{name: "there is no toolchain", set: func(ix *indexer) { ix.graph, ix.lim.goBin = symbols.Resolve, "" },
			want: symbols.ReasonNoToolchain},
	} {
		t.Run(tc.name, func(t *testing.T) {
			before := graphCounters(t)
			ix, rec := fakeIndexer(t, append([]fixtureOpt{withFiles(graphRepo)}, tc.opts...)...)
			if tc.set != nil {
				tc.set(ix)
			}
			ix.runJob(context.Background(), aJob())

			if rec.failReason() != "" {
				t.Fatalf("the job failed: %s", rec.failReason())
			}
			if len(rec.completed) != 1 {
				t.Fatalf("the job was not completed: %v", rec.completed)
			}
			want := []string{
				"F calls Println-> at a.go:7 (syntactic)",
				"F calls G-> at a.go:7 (syntactic)",
				"F calls G-> at a.go:7 (syntactic)",
				"Helper calls Missing-> at b/util.go:5 (syntactic)",
			}
			if got := edgeStrings(edges(t, rec)); !slices.Equal(got, want) {
				t.Fatalf("edges:\n got %s\nwant %s",
					strings.Join(got, "\n      "), strings.Join(want, "\n      "))
			}
			if len(rec.syms) != 4 {
				t.Errorf("the graph has %d symbols, want 4: a failed type-check costs labels, not definitions", len(rec.syms))
			}
			if got, want := logField(t, rec, "reason"), tc.want; got != want {
				t.Errorf("the job log says reason %v, want %q", got, want)
			}
			moved := movedGraphCounters(t, before)
			wantMoved := map[string]float64{
				`codetrail_graph_edges_total{provenance="syntactic"}`: 4,
				`codetrail_typecheck_total{reason="` + tc.want + `"}`: 1,
			}
			if !maps.Equal(moved, wantMoved) {
				t.Errorf("counters moved %v, want %v", moved, wantMoved)
			}
		})
	}
}

// The database write is the one thing in this stage that may still fail a job:
// a graph that was not written is not a downgraded label, it is a missing row.
func TestAGraphWriteThatFailsFailsTheJob(t *testing.T) {
	ix, rec := fakeIndexer(t, withFiles(graphRepo),
		withResolver(nil, symbols.Stats{Reason: symbols.ReasonOK}),
		withGraphWriteError(errors.New("edges exploded")))
	ix.runJob(context.Background(), aJob())

	if rec.failReason() != "edges exploded" {
		t.Fatalf("the job failed with %q, want the write's own error", rec.failReason())
	}
	if len(rec.completed) != 0 {
		t.Errorf("the job was completed anyway: %v", rec.completed)
	}
}

// The policy is built from the job and from the boot-time settings, not from
// defaults inside the loader: a root that is not the checkout type-checks
// somebody else's code, and a cache directory inside the checkout is a
// directory `./...` walks.
func TestTheTypeCheckerIsGivenTheCheckoutAndTheJobsOwnCaches(t *testing.T) {
	ix, rec := fakeIndexer(t, withFiles(graphRepo),
		withResolver(nil, symbols.Stats{Reason: symbols.ReasonOK}))
	ix.runJob(context.Background(), aJob())

	dir := filepath.Join(ix.scratch, ix.id, "job1")
	want := symbols.Policy{Root: dir, Home: dir + ".gohome", GoBin: ix.lim.goBin, Proxy: ix.lim.goProxy}
	if rec.policy != want {
		t.Fatalf("policy %+v, want %+v", rec.policy, want)
	}
	if strings.HasPrefix(rec.policy.Home, rec.policy.Root+string(os.PathSeparator)) {
		t.Errorf("the go caches live inside the checkout at %s: ./... would walk them", rec.policy.Home)
	}
}

// Task 3's carry-forward, and the one that fails silently: Key is a byte
// offset into the bytes on disk, and StripDocs removes the prose bytes while
// keeping their line terminators. Hand the graph pass stripped source and
// every lookup misses, with no error anywhere and a resolution rate of zero
// that looks like a repository with no in-repo calls.
func TestTheGraphPassParsesTheRawBodyAndNotTheStrippedSource(t *testing.T) {
	raw, err := symbols.Parse("a.go", []byte(graphRepo["a.go"]))
	if err != nil {
		t.Fatal(err)
	}
	strippedSrc, err := chunk.StripDocs("a.go", []byte(graphRepo["a.go"]))
	if err != nil {
		t.Fatal(err)
	}
	stripped, err := symbols.Parse("a.go", strippedSrc)
	if err != nil {
		t.Fatal(err)
	}
	// What would make this test fail to fail: if stripping preserved offsets,
	// both streams would key alike and the assertion below would pass under a
	// stage that used either.
	var moved bool
	for i := range raw.Calls {
		if raw.Calls[i].Offset != stripped.Calls[i].Offset {
			moved = true
		}
	}
	if !moved {
		t.Fatal("stripping no longer moves offsets, so this fixture cannot tell the two streams apart")
	}

	ix, rec := fakeIndexer(t, withFiles(graphRepo), stripping(),
		withResolver(resolvedG(t), symbols.Stats{Reason: symbols.ReasonOK}))
	ix.runJob(context.Background(), aJob())

	var got int
	for _, e := range rec.edges {
		if e.Provenance == models.ProvenanceResolved {
			got++
		}
	}
	if got != 2 {
		t.Fatalf("%d of the two calls to G resolved under STRIP_DOC_COMMENTS=true: the keys are offsets into bytes nothing else holds", got)
	}
}

// A declaration longer than CHUNK_MAX_DECL_LINES is sub-windowed, so it has no
// span with its own range — and those are the declarations most worth asking
// who calls them. The link is by containment for exactly that reason.
func TestASymbolLinksToTheSpanThatContainsIt(t *testing.T) {
	var b strings.Builder
	b.WriteString("package p\n\n// Table is longer than one declaration may be.\nvar Table = []string{\n")
	for i := 0; i < 210; i++ {
		fmt.Fprintf(&b, "\t\"row%03d\",\n", i)
	}
	b.WriteString("}\n")

	ix, rec := fakeIndexer(t, withFiles(map[string]string{"big.go": b.String()}),
		withResolver(nil, symbols.Stats{Reason: symbols.ReasonOK}))
	// The fixture is a declaration longer than MAX_FILE_BYTES' 1KB default in
	// this harness, and a file the walk skips for size has no spans to be
	// contained by.
	ix.lim.walk.MaxFileBytes = 1 << 16
	ix.runJob(context.Background(), aJob())

	if len(rec.syms) != 1 || rec.syms[0].Name != "Table" {
		t.Fatalf("symbols %+v, want one named Table", rec.syms)
	}
	sym := rec.syms[0]
	if sym.StartLine != 3 || sym.EndLine != 215 {
		t.Fatalf("Table spans %d..%d, want 3..215", sym.StartLine, sym.EndLine)
	}
	for _, sp := range rec.spans {
		if sp.StartLine == sym.StartLine && sp.EndLine == sym.EndLine {
			t.Fatal("a span has the declaration's own range, so this fixture cannot tell containment from equality")
		}
	}
	want := spanAt(t, rec, 3, 42)
	if sym.SpanID != want {
		t.Fatalf("Table links to %q, want the span 3..42 (%s)", sym.SpanID, want)
	}
}

// Under the window strategy the spans overlap, so a definition's first line is
// inside two of them and the link has to be deterministic. The AST strategy
// cannot see this rule at all: there the declaration's own span is the only
// container.
func TestASymbolLinksToTheMostSpecificOfTwoOverlappingWindows(t *testing.T) {
	var b strings.Builder
	b.WriteString("package p\n")
	for i := 2; i <= 32; i++ {
		fmt.Fprintf(&b, "// filler %d\n", i)
	}
	// The blank line matters: without it every filler comment above joins the
	// declaration's doc group and the definition starts at line 2.
	b.WriteString("\n// Parse is declared where two windows overlap.\nfunc Parse() {}\n")
	for i := 36; i <= 80; i++ {
		fmt.Fprintf(&b, "// filler %d\n", i)
	}

	ix, rec := fakeIndexer(t, windowed(), withFiles(map[string]string{"a.go": b.String()}),
		withResolver(nil, symbols.Stats{Reason: symbols.ReasonOK}))
	ix.lim.walk.MaxFileBytes = 1 << 16
	ix.runJob(context.Background(), aJob())

	if len(rec.syms) != 1 || rec.syms[0].StartLine != 34 {
		t.Fatalf("symbols %+v, want one starting at line 34", rec.syms)
	}
	// Both windows hold line 34, which is what makes the tie-break visible.
	if first, second := spanAt(t, rec, 1, 40), spanAt(t, rec, 31, 70); first == "" || second == "" {
		t.Fatalf("the fixture produced %v, want overlapping windows 1..40 and 31..70", rec.spans)
	} else if rec.syms[0].SpanID != second {
		t.Fatalf("Parse links to %q, want the window that starts closest to it, 31..70 (%s) and not 1..40 (%s)",
			rec.syms[0].SpanID, second, first)
	}
}

// The link is nullable and this is the shape that needs it: with
// STRIP_DOC_COMMENTS=true the chunker sees a file whose doc comments are gone,
// so its spans start at the `func` keyword — while a definition's range comes
// from the raw bytes and starts at the comment. The definition's first line is
// then inside no span at all.
//
// Measured rather than assumed, and it is the eval corpus that runs this way.
func TestADeclarationWithNoSpanStillGetsASymbolWithANullLink(t *testing.T) {
	ix, rec := fakeIndexer(t, stripping(), withFiles(graphRepo),
		withResolver(nil, symbols.Stats{Reason: symbols.ReasonOK}))
	ix.runJob(context.Background(), aJob())

	links := map[string]string{}
	for _, s := range rec.syms {
		links[s.Name] = s.SpanID
	}
	if links["F"] != "" {
		t.Errorf("F links to a span (%s); with the doc comment stripped its first line is in none", links["F"])
	}
	// And the undocumented declaration in the same file still links, so the
	// null above is a property of the range and not of the whole job.
	if links["fns"] == "" {
		t.Errorf("fns links to no span, yet it has no doc comment for stripping to remove")
	}
	if len(rec.syms) != 4 {
		t.Errorf("the graph has %d symbols, want 4: a missing span costs a link, not a definition", len(rec.syms))
	}
}

// The log line is the only place an operator learns why a repository's edges
// are all syntactic. Every field is read here, because a field nothing reads
// back is a side effect a mutation can delete for free.
func TestTheJobLogNamesWhatTheGraphStageProducedAndWhyItStopped(t *testing.T) {
	ix, rec := fakeIndexer(t, withFiles(graphRepo),
		withResolver(resolvedG(t), symbols.Stats{
			Reason: symbols.ReasonOK, Packages: 2, Loaded: 2, Resolved: 2, External: 1, Unresolved: 1,
		}))
	ix.runJob(context.Background(), aJob())

	line := logLine(t, rec.logged.String(), "symbol graph")
	want := map[string]any{
		"level": "info", "message": "symbol graph",
		"symbols": 4.0, "edges": 4.0, "resolved": 2.0, "syntactic": 2.0,
		"external": 1.0, "unnameable": 1.0, "reason": "ok",
	}
	for k, v := range want {
		if got, ok := line[k]; !ok {
			t.Errorf("the log line has no %q field: %v", k, line)
		} else if got != v {
			t.Errorf("the log line says %s=%v, want %v", k, got, v)
		}
	}
}

// And the same line at warn when something was lost, because an operator
// greps for the downgrade rather than reading every job's info line.
func TestALostTypeCheckIsLoggedAtWarn(t *testing.T) {
	ix, rec := fakeIndexer(t, withFiles(graphRepo),
		withResolver(nil, symbols.Stats{Reason: symbols.ReasonLoadError}))
	ix.runJob(context.Background(), aJob())

	if got := logLine(t, rec.logged.String(), "symbol graph")["level"]; got != "warn" {
		t.Errorf("a failed type-check is logged at %v, want warn", got)
	}
}

// The whole delta vector of both counter families, because a test that reads
// only the series it expects to move passes under a mutant that moves both.
func TestTheGraphCountersMoveExactlyOnce(t *testing.T) {
	before := graphCounters(t)
	ix, _ := fakeIndexer(t, withFiles(graphRepo),
		withResolver(resolvedG(t), symbols.Stats{Reason: symbols.ReasonOK}))
	ix.runJob(context.Background(), aJob())

	want := map[string]float64{
		`codetrail_graph_edges_total{provenance="resolved"}`:  2,
		`codetrail_graph_edges_total{provenance="syntactic"}`: 2,
		`codetrail_typecheck_total{reason="ok"}`:              1,
	}
	if moved := movedGraphCounters(t, before); !maps.Equal(moved, want) {
		t.Fatalf("counters moved %v, want %v", moved, want)
	}
}

// The label sets are closed and they are written out in two packages, because
// metrics cannot import symbols — go/packages would be linked into the
// gateway. This is what keeps the copy honest.
func TestTheGraphCounterVocabulariesAreTheClosedSetsTheyName(t *testing.T) {
	reasons := slices.Clone(symbols.Reasons)
	slices.Sort(reasons)
	got := slices.Clone(metrics.TypecheckReasons)
	slices.Sort(got)
	if !slices.Equal(got, reasons) {
		t.Errorf("metrics.TypecheckReasons is %v, want symbols' own set %v", got, reasons)
	}
	var provenances []string
	for _, p := range models.Provenances {
		provenances = append(provenances, string(p))
	}
	slices.Sort(provenances)
	got = slices.Clone(metrics.GraphProvenances)
	slices.Sort(got)
	if !slices.Equal(got, provenances) {
		t.Errorf("metrics.GraphProvenances is %v, want models' own set %v", got, provenances)
	}
}

// Open question 9: a worker with no toolchain still indexes, and says so once
// rather than producing a repository of syntactic edges that looks like a
// property of the code.
func TestTheBootLineSaysWhetherThisWorkerCanTypeCheck(t *testing.T) {
	for _, tc := range []struct {
		name string
		set  func(l *limits)
		want string
	}{
		{"no toolchain", func(l *limits) { l.goBin = "" }, `"level":"warn"`},
		{"switched off", func(l *limits) { l.typecheck = false }, "TYPECHECK is off"},
		{"ready", func(l *limits) {}, `"go":"/opt/codetrail-test/go"`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ix, logged := testIndexer(t, &fakeQueue{})
			tc.set(&ix.lim)
			ix.logToolchain()
			if out := logged.String(); !strings.Contains(out, tc.want) {
				t.Fatalf("the boot line is %q, want it to carry %q", out, tc.want)
			}
		})
	}
}

// The two graph knobs are operator settings, and a value that never became one
// must not boot. TYPECHECK=no reading as enabled, or a GOPROXY of direct
// reaching the loader, are both settings someone believes are in force.
func TestTheGraphKnobsAreRefusedAtBoot(t *testing.T) {
	t.Run("TYPECHECK", func(t *testing.T) {
		t.Setenv("TYPECHECK", "sometimes")
		if _, err := limitsFrom(); err == nil || !strings.Contains(err.Error(), "TYPECHECK") {
			t.Fatalf("want an error naming TYPECHECK, got %v", err)
		}
	})
	for _, proxy := range []string{"direct", "off,direct", "https://p.example|direct", "http://p.example"} {
		t.Run("TYPECHECK_GOPROXY="+proxy, func(t *testing.T) {
			t.Setenv("TYPECHECK_GOPROXY", proxy)
			if _, err := limitsFrom(); err == nil || !strings.Contains(err.Error(), proxy) {
				t.Fatalf("want an error naming %q, got %v", proxy, err)
			}
		})
	}
}

// Task 3's other carry-forward. With a proxy configured the go command writes
// its module cache read-only, and os.RemoveAll over the tree then fails — so
// the job's whole checkout leaks, not merely its cache.
func TestAScratchTreeTheGoCommandLeftReadOnlyIsStillRemoved(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root ignores the permissions this test is about")
	}
	build := func() string {
		dir := filepath.Join(t.TempDir(), "job")
		cache := filepath.Join(dir, "go", "modcache", "example.com", "mod@v1.0.0")
		if err := os.MkdirAll(cache, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(cache, "go.mod"), []byte("module m\n"), 0o400); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(cache, 0o555); err != nil {
			t.Fatal(err)
		}
		return dir
	}
	dir := build()
	// The control: without it, a passing removeScratch would prove only that
	// removing a directory works.
	if err := os.RemoveAll(dir); err == nil {
		t.Fatal("os.RemoveAll removed a read-only cache, so this fixture proves nothing")
	}
	if err := removeScratch(dir); err != nil {
		t.Fatalf("removeScratch: %v", err)
	}
	if _, err := os.Stat(dir); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("the scratch tree is still there: %v", err)
	}
}

// spanAt is the id of the span with that exact range, or "" when there is
// none.
func spanAt(t *testing.T, rec *recorder, start, end int) string {
	t.Helper()
	for _, sp := range rec.spans {
		if sp.StartLine == start && sp.EndLine == end {
			return sp.ID
		}
	}
	return ""
}

// logLine is the one JSON log line carrying that message.
func logLine(t *testing.T, out, message string) map[string]any {
	t.Helper()
	for _, raw := range strings.Split(out, "\n") {
		if raw == "" {
			continue
		}
		var line map[string]any
		if err := json.Unmarshal([]byte(raw), &line); err != nil {
			t.Fatalf("unparseable log line %q: %v", raw, err)
		}
		if line["message"] == message {
			return line
		}
	}
	t.Fatalf("no %q line in %q", message, out)
	return nil
}

func logField(t *testing.T, rec *recorder, field string) any {
	t.Helper()
	return logLine(t, rec.logged.String(), "symbol graph")[field]
}

// graphCounters reads this process's own metrics the way Prometheus does, so
// what is asserted is what an operator sees.
func graphCounters(t *testing.T) map[string]float64 {
	t.Helper()
	srv := httptest.NewServer(promhttp.Handler())
	defer srv.Close()
	resp, err := http.Get(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]float64{}
	for _, line := range strings.Split(string(raw), "\n") {
		if !strings.HasPrefix(line, "codetrail_graph_edges_total{") &&
			!strings.HasPrefix(line, "codetrail_typecheck_total{") {
			continue
		}
		series, value, ok := strings.Cut(line, "} ")
		if !ok {
			t.Fatalf("unparseable metric line %q", line)
		}
		v, err := strconv.ParseFloat(value, 64)
		if err != nil {
			t.Fatalf("metric %q: %v", line, err)
		}
		out[series+"}"] = v
	}
	// Every series is initialised at startup, so an empty read means the
	// counters are not registered and every assertion below would be vacuous.
	if len(out) != len(metrics.GraphProvenances)+len(metrics.TypecheckReasons) {
		t.Fatalf("read %d graph series, want %d", len(out), len(metrics.GraphProvenances)+len(metrics.TypecheckReasons))
	}
	return out
}

// movedGraphCounters returns only the series that changed, so an assertion
// names the whole movement rather than the one series it expected.
func movedGraphCounters(t *testing.T, before map[string]float64) map[string]float64 {
	t.Helper()
	moved := map[string]float64{}
	for series, now := range graphCounters(t) {
		if d := now - before[series]; d != 0 {
			moved[series] = d
		}
	}
	return moved
}

// A file that does not parse costs its own definitions and nothing else, and
// the count says so — a repository of unparseable Go otherwise looks like a
// repository with no declarations.
func TestAFileThatDoesNotParseIsCountedRatherThanLosingTheGraph(t *testing.T) {
	ix, rec := fakeIndexer(t, withResolver(nil, symbols.Stats{Reason: symbols.ReasonOK}))
	ix.runJob(context.Background(), aJob())

	// fixtureRepo's broken.go is the one: package p, `func (`.
	if out := rec.logged.String(); !strings.Contains(out, `"unparsed":1`) {
		t.Errorf("the unparsed count is not in the log: %q", out)
	}
	names := []string{}
	for _, s := range rec.syms {
		names = append(names, s.Path+":"+s.Name)
	}
	if want := []string{"a.go:F", "b/util.go:Helper"}; !slices.Equal(names, want) {
		t.Fatalf("symbols %v, want %v: the parseable files still produced theirs", names, want)
	}
}
