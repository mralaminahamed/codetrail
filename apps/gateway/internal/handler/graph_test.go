package handler

import (
	"bytes"
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/labstack/echo/v4"
	"github.com/rs/zerolog"

	"github.com/mralaminahamed/codetrail/packages/shared/models"
	"github.com/mralaminahamed/codetrail/packages/shared/rag"
	"github.com/mralaminahamed/codetrail/packages/shared/store"
)

// The graph fixture, shaped by the four traps this phase's fixtures have to
// avoid:
//
//	Store.Get and Cache.Get share a last segment in two packages, so a reader
//	that binds by name returns both and one that binds by target returns one,
//	and every assertion below names which.
//
//	g calls Store.Get and resolves. f calls something named Get twice and
//	cannot say which, so the approximate set has two rows against the precise
//	set's one — a merged list is 3 and 0 rather than 1 and 2.
//
//	Cache.Get and h have no span, which is Task 2's sub-windowed declaration.
//	Without one, "citation: null" has no fixture and the guess renders forever.
//
//	h has no callers at all, so "nothing calls this" is separable from "no such
//	symbol".
const (
	symStoreGet = "sym-store-get"
	symCacheGet = "sym-cache-get"
	symG        = "sym-g"
	symF        = "sym-f"
	symH        = "sym-h"
)

// Spans the definitions link to. Separate from fixtureSpans, which the
// retrieval tests count row by row: a definition needs a real digest and a
// search must not start returning it.
var fixtureGraphSpans = []models.Span{
	newSpan("span-store-get", "store/store.go", "Store.Get", 10, 20,
		"func (s *Store) Get(k string) (string, bool) { v, ok := s.m[k]; return v, ok }"),
	newSpan("span-g", "store/use.go", "g", 5, 8, "func g() { s.Get(\"k\") }"),
	newSpan("span-f", "cache/use.go", "f", 4, 12, "func f() { c.Get(\"a\"); c.Get(\"b\") }"),
}

func graphSymbols() map[string]models.Symbol {
	sym := func(id, name, pkg, path string, start, end int, spanID string) models.Symbol {
		return models.Symbol{
			ID: id, RepoID: fixtureRepoID, FileID: "file-" + id, Path: path,
			Name: name, Pkg: pkg, Kind: models.KindFunc,
			StartLine: start, EndLine: end, SpanID: spanID,
		}
	}
	return map[string]models.Symbol{
		symStoreGet: sym(symStoreGet, "Store.Get", "store", "store/store.go", 10, 20, "span-store-get"),
		// No span: a declaration longer than one window has none, and this is
		// the only row that can tell a null citation from a rendered guess.
		symCacheGet: sym(symCacheGet, "Cache.Get", "cache", "cache/cache.go", 3, 40, ""),
		symG:        sym(symG, "g", "store", "store/use.go", 5, 8, "span-g"),
		symF:        sym(symF, "f", "cache", "cache/use.go", 4, 12, "span-f"),
		symH:        sym(symH, "h", "cache", "cache/use.go", 14, 18, ""),
	}
}

type defsArgs struct {
	name, pkg string
	suffix    bool
	limit     int
}

type callersArgs struct {
	symbolID     string
	depth, limit int
}

type approxArgs struct {
	name  string
	limit int
}

// graphFake is the graph half of the read fake, embedded in fakeStore. One
// error hook per read, as the rest of that fake has: a store-wide error never
// gets past the first call, and the failures worth covering here are the ones
// behind a call that already succeeded.
type graphFake struct {
	symbols map[string]models.Symbol
	// Keyed by the symbol called, and by the name an edge recorded.
	callers map[string][]store.Caller
	approx  map[string][]store.Approximate

	symbolErr, defsErr, callersErr, approxErr error

	// What the last read was asked for. The depth and the two limits are
	// numbers nothing else can see: this fake answers its fixture whatever it
	// is handed, so every default was free to move until these existed.
	defsCall    defsArgs
	callersCall callersArgs
	approxCall  approxArgs
}

func newGraphFake() *graphFake {
	syms := graphSymbols()
	return &graphFake{
		symbols: syms,
		callers: map[string][]store.Caller{
			symStoreGet: {{Symbol: syms[symG], Depth: 1, CallPath: "store/use.go", CallLine: 6}},
		},
		approx: map[string][]store.Approximate{
			"Get": {
				{Symbol: syms[symF], ToName: "Get", CallPath: "cache/use.go", CallLine: 7},
				{Symbol: syms[symF], ToName: "Get", CallPath: "cache/use.go", CallLine: 9},
			},
		},
	}
}

func (g *graphFake) Definitions(_ context.Context, _, name, pkg string, suffix bool, limit int) ([]models.Symbol, error) {
	g.defsCall = defsArgs{name: name, pkg: pkg, suffix: suffix, limit: limit}
	if g.defsErr != nil {
		return nil, g.defsErr
	}
	var out []models.Symbol
	for _, sy := range g.symbols {
		if sy.Name != name && !(suffix && strings.HasSuffix(sy.Name, "."+name)) {
			continue
		}
		if pkg != "" && sy.Pkg != pkg {
			continue
		}
		out = append(out, sy)
	}
	slices.SortFunc(out, func(a, b models.Symbol) int {
		return cmp.Or(cmp.Compare(a.Path, b.Path), cmp.Compare(a.StartLine, b.StartLine), cmp.Compare(a.ID, b.ID))
	})
	return firstN(out, limit), nil
}

func (g *graphFake) Symbol(_ context.Context, _, symbolID string) (models.Symbol, error) {
	if g.symbolErr != nil {
		return models.Symbol{}, g.symbolErr
	}
	sy, ok := g.symbols[symbolID]
	if !ok {
		return models.Symbol{}, store.ErrNotFound
	}
	return sy, nil
}

func (g *graphFake) CallersOf(_ context.Context, _, symbolID string, depth, limit int) ([]store.Caller, error) {
	g.callersCall = callersArgs{symbolID: symbolID, depth: depth, limit: limit}
	if g.callersErr != nil {
		return nil, g.callersErr
	}
	return firstN(g.callers[symbolID], limit), nil
}

func (g *graphFake) ApproximateCallersOf(_ context.Context, _, name string, limit int) ([]store.Approximate, error) {
	g.approxCall = approxArgs{name: name, limit: limit}
	if g.approxErr != nil {
		return nil, g.approxErr
	}
	return firstN(g.approx[name], limit), nil
}

// firstN honours the LIMIT the handler asked for, so a handler reading one over
// its bound sees the extra row only when the fixture holds one.
func firstN[T any](rows []T, limit int) []T {
	if len(rows) > limit {
		return rows[:limit]
	}
	return rows
}

func graphHandler(t *testing.T) (*fakeStore, *echo.Echo) {
	t.Helper()
	st := newStore()
	return st, mount(hermeticHandler(st, &fakeRetriever{}))
}

// getPath, not get: read_live_test.go already spells a get for the live suite,
// and both files compile under -tags=live.
func getPath(e *echo.Echo, path string) *httptest.ResponseRecorder {
	return do(e, http.MethodGet, path, "")
}

// graphSpan is the span a graph fixture definition links to.
func graphSpan(id string) models.Span {
	for _, s := range fixtureGraphSpans {
		if s.ID == id {
			return s
		}
	}
	panic("no graph fixture span " + id)
}

// symbolsResponse, symbolResponse and the caller views are decoded into typed
// structs rather than maps wherever a field's absence is the thing under test:
// a map lookup of a missing key and of a null are both nil.
type symbolsResponse struct {
	RepoID    string          `json:"repo_id"`
	Count     int             `json:"count"`
	Matched   string          `json:"matched"`
	Truncated bool            `json:"truncated"`
	Symbols   []symbolPayload `json:"symbols"`
	Staleness rag.Staleness   `json:"staleness"`
}

type symbolPayload struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	Pkg       string `json:"pkg"`
	Kind      string `json:"kind"`
	Path      string `json:"path"`
	StartLine int    `json:"start_line"`
	EndLine   int    `json:"end_line"`
	SpanID    string `json:"span_id"`
}

type callerPayload struct {
	Symbol     symbolPayload `json:"symbol"`
	Depth      int           `json:"depth"`
	Provenance string        `json:"provenance"`
	ToName     string        `json:"to_name"`
	Call       struct {
		Path string `json:"path"`
		Line int    `json:"line"`
	} `json:"call"`
	Citation *rag.Citation `json:"citation"`
}

type callersPayload struct {
	RepoID      string          `json:"repo_id"`
	Symbol      symbolPayload   `json:"symbol"`
	Depth       int             `json:"depth"`
	Truncated   bool            `json:"truncated"`
	Callers     []callerPayload `json:"callers"`
	Approximate struct {
		MatchedOn string          `json:"matched_on"`
		Count     int             `json:"count"`
		Truncated bool            `json:"truncated"`
		Failed    bool            `json:"failed"`
		Callers   []callerPayload `json:"callers"`
	} `json:"approximate"`
}

func decodeAs[T any](t *testing.T, rec *httptest.ResponseRecorder, want int) T {
	t.Helper()
	if rec.Code != want {
		t.Fatalf("status %d, want %d: %s", rec.Code, want, rec.Body)
	}
	var out T
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("unmarshal %q: %v", rec.Body.String(), err)
	}
	return out
}

func symNames(syms []symbolPayload) []string {
	out := make([]string, 0, len(syms))
	for _, s := range syms {
		out = append(out, s.Name)
	}
	return out
}

// Exact is the default and the response says so. A silent fallback to suffix
// matching would answer a different question than the one asked, in a payload
// with no field to say it happened (Open question 11).
func TestDefinitionsReturnsExactMatchesAndSaysWhichMatchItMade(t *testing.T) {
	st, e := graphHandler(t)
	out := decodeAs[symbolsResponse](t, getPath(e, "/api/repos/repo-1/symbols?name=Store.Get"), http.StatusOK)
	if out.RepoID != fixtureRepoID || out.Count != 1 || out.Matched != "exact" {
		t.Fatalf("repo %q count %d matched %q", out.RepoID, out.Count, out.Matched)
	}
	got := out.Symbols[0]
	want := graphSymbols()[symStoreGet]
	if got.ID != want.ID || got.Name != want.Name || got.Pkg != want.Pkg ||
		got.Path != want.Path || got.StartLine != want.StartLine || got.EndLine != want.EndLine ||
		got.SpanID != want.SpanID || got.Kind != string(want.Kind) {
		t.Errorf("symbol %+v, want %+v", got, want)
	}
	// The last segment alone matches nothing without the opt-in, and the
	// fixture holds two definitions ending in it.
	if bare := decodeAs[symbolsResponse](t, getPath(e, "/api/repos/repo-1/symbols?name=Get"), http.StatusOK); bare.Count != 0 {
		t.Errorf("exact match for Get returned %v", symNames(bare.Symbols))
	}
	// The join keys stay off the view, as they do on the span view.
	var raw struct {
		Symbols []map[string]any `json:"symbols"`
	}
	if err := json.Unmarshal(getPath(e, "/api/repos/repo-1/symbols?name=Store.Get").Body.Bytes(), &raw); err != nil {
		t.Fatal(err)
	}
	for _, k := range []string{"repo_id", "file_id"} {
		if _, ok := raw.Symbols[0][k]; ok {
			t.Errorf("the symbol view serves %q", k)
		}
	}
	if st.defsCall.name != "Store.Get" || st.defsCall.suffix {
		t.Errorf("the store was asked %+v", st.defsCall)
	}
}

// Two Get methods in two packages, which is the only shape in which name
// matching is visibly wrong. Both come back and the label says the widening
// happened.
func TestSuffixMatchingIsOptInAndLabelled(t *testing.T) {
	_, e := graphHandler(t)
	out := decodeAs[symbolsResponse](t, getPath(e, "/api/repos/repo-1/symbols?name=Get&suffix=true"), http.StatusOK)
	if want := []string{"Cache.Get", "Store.Get"}; !slices.Equal(symNames(out.Symbols), want) {
		t.Errorf("suffix=true returned %v, want %v", symNames(out.Symbols), want)
	}
	if out.Matched != "suffix" || out.Count != 2 {
		t.Errorf("matched %q count %d, want \"suffix\" and 2", out.Matched, out.Count)
	}
	// pkg narrows the pair to one, which is what a caller who found two
	// definitions does next. A parameter the store ignored would answer both.
	narrowed := decodeAs[symbolsResponse](t, getPath(e, "/api/repos/repo-1/symbols?name=Get&suffix=true&pkg=cache"), http.StatusOK)
	if want := []string{"Cache.Get"}; !slices.Equal(symNames(narrowed.Symbols), want) {
		t.Errorf("pkg=cache returned %v, want %v", symNames(narrowed.Symbols), want)
	}
	if narrowed.Matched != "suffix" {
		t.Errorf("pkg=cache reported matched %q", narrowed.Matched)
	}
}

// ?suffix=yes is not false. P3 shipped an API that accepted mode and ignored
// it; a flag whose unparseable value quietly means "off" is the same defect
// with a different name, and no field in the payload can show it.
func TestAnUnparseableSuffixIsRefusedRatherThanIgnored(t *testing.T) {
	_, e := graphHandler(t)
	rec := getPath(e, "/api/repos/repo-1/symbols?name=Get&suffix=yes")
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("suffix=yes answered %d: %s", rec.Code, rec.Body)
	}
	out := body(t, rec)
	if d, _ := out["error"].(string); !strings.Contains(d, "suffix") {
		t.Errorf("the 400 does not name suffix: %q", d)
	}
	if out["rule"] != "form" {
		t.Errorf("body %s does not name the rule", rec.Body)
	}
}

func TestAnUnknownSymbolIsFourOhFour(t *testing.T) {
	_, e := graphHandler(t)
	for _, path := range []string{
		"/api/repos/repo-1/symbols/nosuchsymbol",
		"/api/repos/repo-1/symbols/nosuchsymbol/callers",
	} {
		rec := getPath(e, path)
		if rec.Code != http.StatusNotFound {
			t.Errorf("%s: want 404, got %d: %s", path, rec.Code, rec.Body)
		}
		if d, _ := body(t, rec)["error"].(string); !strings.Contains(d, "symbol") {
			t.Errorf("%s: the 404 does not say what was missing: %q", path, d)
		}
	}
}

// The other half of the same claim: a symbol that is here and is called by
// nothing is a 200 with an empty list. Without this, a handler answering 404
// for everything passes the test above.
func TestASymbolWithNoCallersIsTwoHundredWithAnEmptyList(t *testing.T) {
	_, e := graphHandler(t)
	out := decodeAs[callersPayload](t, getPath(e, "/api/repos/repo-1/symbols/"+symH+"/callers"), http.StatusOK)
	if len(out.Callers) != 0 || out.Approximate.Count != 0 || len(out.Approximate.Callers) != 0 {
		t.Errorf("h has callers: %+v", out)
	}
	if out.Symbol.Name != "h" || out.Depth != 1 {
		t.Errorf("symbol %q depth %d", out.Symbol.Name, out.Depth)
	}
	// An empty list, not a null one: a client iterating the field must not have
	// to branch on its absence.
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(getPath(e, "/api/repos/repo-1/symbols/"+symH+"/callers").Body.Bytes(), &raw); err != nil {
		t.Fatal(err)
	}
	if string(raw["callers"]) != "[]" {
		t.Errorf("callers serialised as %s, want []", raw["callers"])
	}
}

// Spec:5 describes this product in one sentence and that sentence is about
// citing file:line. A caller that names who and cannot say where has not
// answered.
func TestCallersCarryTheirCallSiteAndACitation(t *testing.T) {
	_, e := graphHandler(t)
	out := decodeAs[callersPayload](t, getPath(e, "/api/repos/repo-1/symbols/"+symStoreGet+"/callers"), http.StatusOK)
	if len(out.Callers) != 1 {
		t.Fatalf("callers %+v, want one", out.Callers)
	}
	// The answer names the corpus it came from. Nothing else in this response
	// carries a repo id — the citations do, but a caller row with no span has
	// none — so without this a client cannot tie the answer back to what it
	// asked about.
	if out.RepoID != fixtureRepoID {
		t.Errorf("repo_id %q, want %q", out.RepoID, fixtureRepoID)
	}
	got := out.Callers[0]
	if got.Symbol.Name != "g" || got.Depth != 1 {
		t.Errorf("caller %q at depth %d, want g at 1", got.Symbol.Name, got.Depth)
	}
	// The call site is the edge's, not the caller's own declaration: g starts
	// at line 5 and calls at line 6.
	if got.Call.Path != "store/use.go" || got.Call.Line != 6 {
		t.Errorf("call site %s:%d, want store/use.go:6", got.Call.Path, got.Call.Line)
	}
	if got.Provenance != string(models.ProvenanceResolved) {
		t.Errorf("provenance %q, want %q", got.Provenance, models.ProvenanceResolved)
	}
	if got.Citation == nil {
		t.Fatalf("caller g carries no citation: %s", getPath(e, "/api/repos/repo-1/symbols/"+symStoreGet+"/callers").Body)
	}
	// The digest is the span's real hash, so the citation covers text a reader
	// can check rather than a range nobody hashed.
	span := graphSpan("span-g")
	if got.Citation.Digest != span.Digest || got.Citation.Path != span.Path ||
		got.Citation.StartLine != span.StartLine || got.Citation.EndLine != span.EndLine {
		t.Errorf("citation %+v, want the span's %s %d-%d %s",
			got.Citation, span.Path, span.StartLine, span.EndLine, span.Digest)
	}
	if got.Citation.Permalink == "" || got.Citation.Commit != fixtureCommit {
		t.Errorf("citation is not permalink-backed: %+v", got.Citation)
	}
}

// Task 2's sub-windowed declaration: a definition longer than a window has no
// span, so there is no digest. A citation carrying an empty one is a claim
// nobody can check dressed as one they can.
func TestASymbolWithNoSpanCarriesANullCitationRatherThanAGuess(t *testing.T) {
	_, e := graphHandler(t)
	rec := getPath(e, "/api/repos/repo-1/symbols/"+symCacheGet)
	var out struct {
		Symbol    symbolPayload `json:"symbol"`
		Citation  *rag.Citation `json:"citation"`
		Staleness rag.Staleness `json:"staleness"`
	}
	if rec.Code != http.StatusOK {
		t.Fatalf("want 200, got %d: %s", rec.Code, rec.Body)
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	if out.Citation != nil {
		t.Errorf("citation %+v, want null", out.Citation)
	}
	// Present and null, not absent: a client cannot otherwise tell "no span"
	// from "the field moved".
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(rec.Body.Bytes(), &raw); err != nil {
		t.Fatal(err)
	}
	if string(raw["citation"]) != "null" {
		t.Errorf("citation serialised as %s, want null", raw["citation"])
	}
	// The location survives the missing digest, and so does the repository's
	// own staleness claim: the definition is still at cache/cache.go:3-40.
	if out.Symbol.Path != "cache/cache.go" || out.Symbol.StartLine != 3 || out.Symbol.EndLine != 40 {
		t.Errorf("symbol %+v", out.Symbol)
	}
	if out.Staleness.State == "" || out.Staleness.Note == "" {
		t.Errorf("staleness %+v", out.Staleness)
	}

	// And the same definition, reached as a caller, is null there too rather
	// than only on the single-symbol route.
	st2 := newStore()
	st2.callers[symStoreGet] = append(st2.callers[symStoreGet],
		store.Caller{Symbol: graphSymbols()[symCacheGet], Depth: 2, CallPath: "cache/cache.go", CallLine: 12})
	e2 := mount(hermeticHandler(st2, &fakeRetriever{}))
	cs := decodeAs[callersPayload](t, getPath(e2, "/api/repos/repo-1/symbols/"+symStoreGet+"/callers?depth=2"), http.StatusOK)
	if len(cs.Callers) != 2 || cs.Callers[1].Citation != nil {
		t.Errorf("the spanless caller carries %+v", cs.Callers[1].Citation)
	}
}

// Spec:84 at the boundary a client sees. The precise set has one member and the
// approximate set has two; a merged list is three and a zero, and both numbers
// are asserted because either alone survives half the mutation.
func TestApproximateCallersAreASeparateFieldWithTheirOwnCount(t *testing.T) {
	_, e := graphHandler(t)
	out := decodeAs[callersPayload](t, getPath(e, "/api/repos/repo-1/symbols/"+symStoreGet+"/callers"), http.StatusOK)
	if len(out.Callers) != 1 || out.Approximate.Count != 2 {
		t.Fatalf("callers %d, approximate.count %d, want 1 and 2: %+v",
			len(out.Callers), out.Approximate.Count, out)
	}
	if len(out.Approximate.Callers) != out.Approximate.Count {
		t.Errorf("approximate says %d and carries %d", out.Approximate.Count, len(out.Approximate.Callers))
	}
	// The field they arrive in, not the label they carry: a client that read
	// provenance off a merged list would still see two values, and the whole
	// point of §6 is that the two answers are not one list.
	for _, c := range out.Callers {
		if c.Symbol.Name == "f" {
			t.Errorf("the name-matched caller f is in the precise list: %+v", out.Callers)
		}
	}
	if out.Approximate.MatchedOn != "name" {
		t.Errorf("matched_on %q, want \"name\"", out.Approximate.MatchedOn)
	}
	// Two call sites in one function are two rows, and each says which name it
	// matched and that it is a guess.
	var sites []string
	for _, a := range out.Approximate.Callers {
		if a.Provenance != string(models.ProvenanceSyntactic) || a.ToName != "Get" {
			t.Errorf("approximate row %+v", a)
		}
		if a.Depth != 0 {
			t.Errorf("an approximate caller reports depth %d; traversing a guess compounds it", a.Depth)
		}
		sites = append(sites, a.Symbol.Name+"@"+a.Call.Path+":"+strconv.Itoa(a.Call.Line))
	}
	if want := []string{"f@cache/use.go:7", "f@cache/use.go:9"}; !slices.Equal(sites, want) {
		t.Errorf("approximate call sites %v, want %v", sites, want)
	}
	// Each row cites its own definition's span. The precise caller above is in
	// store/use.go and these are in cache/use.go, so a citation memoised under
	// anything but the span id hands one of them the other's digest — and every
	// other assertion in this file still passes.
	span := graphSpan("span-f")
	for _, a := range out.Approximate.Callers {
		if a.Citation == nil || a.Citation.Digest != span.Digest || a.Citation.Path != span.Path {
			t.Errorf("approximate caller f cites %+v, want %s %s", a.Citation, span.Path, span.Digest)
		}
	}
}

// symbols.name spells a method Store.Get; an edge's to_name is the callee's
// last identifier. Asking the approximate query for "Store.Get" matches nothing
// in any corpus, so every method's approximate set would be silently empty —
// and an empty set is exactly what "nothing else calls this" looks like.
func TestTheApproximateSetIsMatchedOnTheCalleesLastIdentifier(t *testing.T) {
	st, e := graphHandler(t)
	out := decodeAs[callersPayload](t, getPath(e, "/api/repos/repo-1/symbols/"+symStoreGet+"/callers"), http.StatusOK)
	if st.approxCall.name != "Get" {
		t.Errorf("the approximate query was asked for %q, want %q", st.approxCall.name, "Get")
	}
	if out.Approximate.Count != 2 {
		t.Errorf("approximate.count %d for Store.Get, want 2", out.Approximate.Count)
	}
	// A plain function's name has no last segment to take, so the same code
	// path must not truncate it.
	getPath(e, "/api/repos/repo-1/symbols/"+symG+"/callers")
	if st.approxCall.name != "g" {
		t.Errorf("a plain function was asked for as %q, want %q", st.approxCall.name, "g")
	}
}

// Open question 14: the precise half is what was asked for. A failure of the
// guess beside it is reported in its own block rather than turned into a 500
// that discards an answer nothing was wrong with.
func TestAFailedApproximateQueryDoesNotCostThePreciseAnswer(t *testing.T) {
	st := newStore()
	st.approxErr = errors.New("relation \"edges\" does not exist")
	var logged bytes.Buffer
	h := hermeticHandler(st, &fakeRetriever{})
	h.Log = zerolog.New(&logged).Level(zerolog.InfoLevel)
	e := mount(h)
	out := decodeAs[callersPayload](t, getPath(e, "/api/repos/repo-1/symbols/"+symStoreGet+"/callers"), http.StatusOK)
	if len(out.Callers) != 1 || out.Callers[0].Symbol.Name != "g" {
		t.Errorf("the precise answer was lost with the approximate one: %+v", out.Callers)
	}
	if !out.Approximate.Failed || out.Approximate.Count != 0 {
		t.Errorf("approximate %+v, want failed with no members", out.Approximate)
	}
	if !strings.Contains(logged.String(), "does not exist") {
		t.Errorf("the failure was dropped silently: %s", logged.String())
	}
	// And the corpus it happened in. This block is the one place a degraded
	// answer is recorded at all — the request is a 200 — so a line that names
	// only the driver's error leaves an operator with nothing to look at.
	if !strings.Contains(logged.String(), `"repo_id":"`+fixtureRepoID+`"`) {
		t.Errorf("the failure names no repository: %s", logged.String())
	}
	// The block still says what it would have matched on, so a client reading
	// it does not have to guess why it is empty.
	if out.Approximate.MatchedOn != "name" {
		t.Errorf("matched_on %q", out.Approximate.MatchedOn)
	}
}

// The citation memo cannot change a response — the same span renders the same
// citation — so nothing in the payload can read it back and only the work it
// saves can. The two approximate rows are two call sites inside one declaration
// and share one span; the precise caller has another. Three rows, two reads.
func TestTwoCallerRowsSharingOneSpanReadItOnce(t *testing.T) {
	st, e := graphHandler(t)
	out := decodeAs[callersPayload](t, getPath(e, "/api/repos/repo-1/symbols/"+symStoreGet+"/callers"), http.StatusOK)
	if len(out.Callers) != 1 || len(out.Approximate.Callers) != 2 {
		t.Fatalf("%d precise and %d approximate rows, want 1 and 2", len(out.Callers), len(out.Approximate.Callers))
	}
	if a, b := out.Approximate.Callers[0].Symbol, out.Approximate.Callers[1].Symbol; a.SpanID != b.SpanID || a.SpanID == "" {
		t.Fatalf("the fixture no longer has two rows sharing one span: %q and %q", a.SpanID, b.SpanID)
	}
	if st.spanReads != 2 {
		t.Errorf("the handler read %d spans for 3 caller rows over 2 distinct spans, want 2", st.spanReads)
	}
}

// Out of range is a 400 naming the rule, not a clamp: a caller asking for depth
// 40 has misunderstood the endpoint and quietly serving 5 hides it. Both ends,
// because a clamp-to-max and a clamp-to-min are different mutants.
func TestDepthOutsideOneToThreeIsFourHundredNamingTheRule(t *testing.T) {
	st, e := graphHandler(t)
	for _, raw := range []string{"40", "0", "-1", "4", "five"} {
		rec := getPath(e, "/api/repos/repo-1/symbols/"+symStoreGet+"/callers?depth="+raw)
		if rec.Code != http.StatusBadRequest {
			// The depth it served, not the whole body: a clamp answers 200 and
			// the number it clamped to is the fact that names the mutant.
			t.Errorf("depth=%s answered %d with depth %v, want 400",
				raw, rec.Code, body(t, rec)["depth"])
			continue
		}
		out := body(t, rec)
		d, _ := out["error"].(string)
		// Which rule, not merely that a rule fired: a 400 naming the wrong
		// field is what §10's "never a generic refusal" is about.
		if !strings.Contains(d, "depth") || !strings.Contains(d, "1") || !strings.Contains(d, strconv.Itoa(maxDepth)) {
			t.Errorf("depth=%s: the 400 says %q, want it to name depth and its bounds", raw, d)
		}
		if out["rule"] != "form" {
			t.Errorf("depth=%s: body %s does not name the rule", raw, rec.Body)
		}
	}
	// A validation failure never reached the store, so nothing was traversed
	// on the way to refusing.
	if st.callersCall.depth != 0 {
		t.Errorf("a refused depth reached the store as %d", st.callersCall.depth)
	}
	// Both ends of the accepted range, from the accepted side: without this,
	// maxDepth could be any number above the range and the refusals above
	// would still pass.
	for _, raw := range []string{"1", "3"} {
		out := decodeAs[callersPayload](t,
			getPath(e, "/api/repos/repo-1/symbols/"+symStoreGet+"/callers?depth="+raw), http.StatusOK)
		if out.Depth != mustAtoi(t, raw) || st.callersCall.depth != mustAtoi(t, raw) {
			t.Errorf("depth=%s answered depth %d and asked the store for %d", raw, out.Depth, st.callersCall.depth)
		}
	}
}

func TestLimitOutsideOneToFiftyIsFourHundredNamingTheRule(t *testing.T) {
	_, e := graphHandler(t)
	for _, path := range []string{
		"/api/repos/repo-1/symbols?name=Get&limit=51",
		"/api/repos/repo-1/symbols?name=Get&limit=0",
		"/api/repos/repo-1/symbols?name=Get&limit=all",
		"/api/repos/repo-1/symbols/" + symStoreGet + "/callers?limit=51",
		"/api/repos/repo-1/symbols/" + symStoreGet + "/callers?limit=0",
	} {
		rec := getPath(e, path)
		if rec.Code != http.StatusBadRequest {
			t.Errorf("%s answered %d: %s", path, rec.Code, rec.Body)
			continue
		}
		out := body(t, rec)
		if d, _ := out["error"].(string); !strings.Contains(d, "limit") || !strings.Contains(d, "50") {
			t.Errorf("%s: the 400 says %q, want it to name limit and its bound", path, d)
		}
		if out["rule"] != "form" {
			t.Errorf("%s: body %s does not name the rule", path, rec.Body)
		}
	}
}

func TestAnEmptyNameIsFourHundredNamingTheRule(t *testing.T) {
	_, e := graphHandler(t)
	for _, path := range []string{
		"/api/repos/repo-1/symbols",
		"/api/repos/repo-1/symbols?name=",
		"/api/repos/repo-1/symbols?name=%20%20",
	} {
		rec := getPath(e, path)
		if rec.Code != http.StatusBadRequest {
			t.Errorf("%s answered %d: %s", path, rec.Code, rec.Body)
			continue
		}
		out := body(t, rec)
		if d, _ := out["error"].(string); !strings.Contains(d, "name") {
			t.Errorf("%s: the 400 says %q, want it to name the name rule", path, d)
		}
		if out["rule"] != "form" {
			t.Errorf("%s: body %s does not name the rule", path, rec.Body)
		}
	}
}

// The 404/410 distinction on the graph routes, and the order that produces it.
// A repo id is hash(key, commit), so a repository re-indexed at the same commit
// re-uses the id its tombstone was written under: a handler that asks "is it
// gone" before it reads repos answers 410 for a repository that is right there.
func TestAnUnknownRepoIsFourOhFourAndAnEvictedOneIsFourTen(t *testing.T) {
	paths := func(repo string) []string {
		return []string{
			"/api/repos/" + repo + "/symbols?name=Get",
			"/api/repos/" + repo + "/symbols/" + symStoreGet,
			"/api/repos/" + repo + "/symbols/" + symStoreGet + "/callers",
		}
	}
	st, e := graphHandler(t)
	st.gone["evicted-1"] = true
	for _, p := range paths("nobody") {
		if rec := getPath(e, p); rec.Code != http.StatusNotFound {
			t.Errorf("%s: want 404, got %d: %s", p, rec.Code, rec.Body)
		}
	}
	for _, p := range paths("evicted-1") {
		rec := getPath(e, p)
		if rec.Code != http.StatusGone {
			t.Errorf("%s: want 410, got %d: %s", p, rec.Code, rec.Body)
			continue
		}
		if d, _ := body(t, rec)["error"].(string); !strings.Contains(d, "evicted") {
			t.Errorf("%s: the 410 does not say what happened: %q", p, d)
		}
	}
	// And a live repository carrying a stale tombstone answers. This fake's
	// RepoGone has no NOT EXISTS guard, so the read order is the only thing
	// that can save it.
	st.gone[fixtureRepoID] = true
	for _, p := range paths(fixtureRepoID) {
		if rec := getPath(e, p); rec.Code != http.StatusOK {
			t.Errorf("%s: a live repository with a tombstone answered %d: %s", p, rec.Code, rec.Body)
		}
	}
}

// last_queried_at is the LRU clock. A repository whose only traffic is graph
// queries would be evicted as cold while it is being read, and nothing about a
// response changes when the touch stops running.
func TestAGraphReadWindsTheLRUClock(t *testing.T) {
	for _, path := range []string{
		"/api/repos/repo-1/symbols?name=Get&suffix=true",
		"/api/repos/repo-1/symbols/" + symStoreGet,
		"/api/repos/repo-1/symbols/" + symStoreGet + "/callers",
	} {
		st, e := graphHandler(t)
		if rec := getPath(e, path); rec.Code != http.StatusOK {
			t.Fatalf("%s: want 200, got %d: %s", path, rec.Code, rec.Body)
		}
		// With the repo id: a call count alone passes under a mutant that
		// winds the clock of the wrong repository.
		if !slices.Equal(st.touched, []string{fixtureRepoID}) {
			t.Errorf("%s: touched %v, want [%s]", path, st.touched, fixtureRepoID)
		}
	}
}

// The touch is a hint for a future eviction, not part of the answer. Failing a
// correct read because a bookkeeping write failed trades the answer for the
// hint.
func TestATouchFailureIsLoggedAndNotReturned(t *testing.T) {
	st := newStore()
	st.touchErr = errors.New("deadlock detected")
	var logged bytes.Buffer
	h := hermeticHandler(st, &fakeRetriever{})
	h.Log = zerolog.New(&logged).Level(zerolog.InfoLevel)
	e := mount(h)
	out := decodeAs[callersPayload](t, getPath(e, "/api/repos/repo-1/symbols/"+symStoreGet+"/callers"), http.StatusOK)
	if len(out.Callers) != 1 {
		t.Errorf("no answer behind the 200: %+v", out)
	}
	if !strings.Contains(logged.String(), "deadlock detected") {
		t.Errorf("a lost touch was dropped silently: %s", logged.String())
	}
}

// A pgx error names tables, columns and sometimes values. The operator reads it
// from the log; the caller gets a request id to quote.
func TestAStoreErrorIsFiveHundredWithARequestIdAndNoDetail(t *testing.T) {
	const detail = "graph: could not read the edges table"
	for _, tc := range []struct {
		name string
		set  func(*fakeStore)
		path string
	}{
		{"definitions", func(f *fakeStore) { f.defsErr = errors.New(detail) },
			"/api/repos/repo-1/symbols?name=Get"},
		{"symbol", func(f *fakeStore) { f.symbolErr = errors.New(detail) },
			"/api/repos/repo-1/symbols/" + symStoreGet},
		{"callers", func(f *fakeStore) { f.callersErr = errors.New(detail) },
			"/api/repos/repo-1/symbols/" + symStoreGet + "/callers"},
		{"span behind a citation", func(f *fakeStore) { f.spanErr = errors.New(detail) },
			"/api/repos/repo-1/symbols/" + symStoreGet + "/callers"},
		{"staleness", func(f *fakeStore) { f.newerErr = errors.New(detail) },
			"/api/repos/repo-1/symbols/" + symStoreGet},
		{"staleness behind the listing", func(f *fakeStore) { f.newerErr = errors.New(detail) },
			"/api/repos/repo-1/symbols?name=Get&suffix=true"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			st := newStore()
			tc.set(st)
			var logged bytes.Buffer
			h := hermeticHandler(st, &fakeRetriever{})
			h.Log = zerolog.New(&logged).Level(zerolog.InfoLevel)
			e := mount(h)
			got := getPath(e, tc.path)
			if got.Code != http.StatusInternalServerError {
				t.Fatalf("want 500, got %d: %s", got.Code, got.Body)
			}
			out := body(t, got)
			if out["error"] != "internal error" {
				t.Errorf("body %s carries the store's own words", got.Body)
			}
			if rid, _ := out["request_id"].(string); rid == "" {
				t.Errorf("no request id to quote: %s", got.Body)
			}
			if strings.Contains(got.Body.String(), "edges") {
				t.Errorf("the response names a table: %s", got.Body)
			}
			// The operator's copy exists, or the opacity above is just a
			// dropped error.
			if !strings.Contains(logged.String(), detail) {
				t.Errorf("the real error was not logged: %s", logged.String())
			}
		})
	}
}

// §10's answer counters describe the ask path. A graph read is neither an
// answer nor a refusal, and mixing them makes the answer-outcome ratio
// meaningless the moment a console starts walking the graph.
//
// The whole vector of both counters, before and after: a test that checked one
// series passes under a mutant that moves another.
func TestTheGraphRoutesTouchNoAnswerOrRefusalCounter(t *testing.T) {
	before := counters(t)
	st, e := graphHandler(t)
	st.gone["evicted-1"] = true
	for _, path := range []string{
		"/api/repos/repo-1/symbols?name=Get&suffix=true",
		"/api/repos/repo-1/symbols/" + symStoreGet,
		"/api/repos/repo-1/symbols/" + symStoreGet + "/callers?depth=3",
		"/api/repos/repo-1/symbols/nosuchsymbol",
		"/api/repos/repo-1/symbols/" + symStoreGet + "/callers?depth=40",
		"/api/repos/nobody/symbols?name=Get",
		"/api/repos/evicted-1/symbols?name=Get",
	} {
		getPath(e, path)
	}
	// And a 500, which is the outcome most likely to be filed as an answer
	// error by a handler that counted anything at all.
	failing := newStore()
	failing.callersErr = errors.New("boom")
	e = mount(hermeticHandler(failing, &fakeRetriever{}))
	getPath(e, "/api/repos/repo-1/symbols/"+symStoreGet+"/callers")

	if moved := movedSince(t, before); len(moved) != 0 {
		t.Errorf("the graph routes moved %v", moved)
	}
}

// What a caller who names nothing gets, and the ceiling from the accepted side.
// Each of these is a number nothing read back: the fake answers its fixture
// whatever it is handed.
func TestTheGraphDefaultsAndTheirCeilingReachTheStore(t *testing.T) {
	st, e := graphHandler(t)
	getPath(e, "/api/repos/repo-1/symbols?name=Get&suffix=true")
	// One over the bound, so a truncated list is distinguishable from a full
	// one; the default itself is 20.
	if st.defsCall.limit != defaultSymbolLimit+1 {
		t.Errorf("a listing with no ?limit asked for %d, want %d", st.defsCall.limit, defaultSymbolLimit+1)
	}
	getPath(e, "/api/repos/repo-1/symbols/"+symStoreGet+"/callers")
	if st.callersCall.depth != defaultDepth {
		t.Errorf("callers with no ?depth asked for depth %d, want %d", st.callersCall.depth, defaultDepth)
	}
	if st.callersCall.limit != defaultCallerLimit+1 || st.approxCall.limit != defaultCallerLimit+1 {
		t.Errorf("callers with no ?limit asked for %d and %d, want %d",
			st.callersCall.limit, st.approxCall.limit, defaultCallerLimit+1)
	}
	getPath(e, "/api/repos/repo-1/symbols?name=Get&suffix=true&limit=50")
	if st.defsCall.limit != 51 {
		t.Errorf("?limit=50 asked for %d", st.defsCall.limit)
	}
}

// Open question 1 makes the LIMIT the second bound on a traversal whose fan-out
// is multiplicative. Without a flag, a truncated answer and a complete one are
// the same payload, and a client cannot tell "two callers" from "the first two
// of many".
func TestATruncatedListSaysSo(t *testing.T) {
	st, e := graphHandler(t)
	out := decodeAs[symbolsResponse](t, getPath(e, "/api/repos/repo-1/symbols?name=Get&suffix=true&limit=1"), http.StatusOK)
	if !out.Truncated || out.Count != 1 {
		t.Errorf("limit=1 over two definitions: truncated %v count %d", out.Truncated, out.Count)
	}
	full := decodeAs[symbolsResponse](t, getPath(e, "/api/repos/repo-1/symbols?name=Get&suffix=true&limit=2"), http.StatusOK)
	if full.Truncated || full.Count != 2 {
		t.Errorf("limit=2 over two definitions: truncated %v count %d", full.Truncated, full.Count)
	}
	// The approximate set has its own bound and its own flag: f's two call
	// sites are two rows.
	cs := decodeAs[callersPayload](t, getPath(e, "/api/repos/repo-1/symbols/"+symStoreGet+"/callers?limit=1"), http.StatusOK)
	if !cs.Approximate.Truncated || cs.Approximate.Count != 1 {
		t.Errorf("approximate at limit=1: truncated %v count %d", cs.Approximate.Truncated, cs.Approximate.Count)
	}
	// One precise caller under a limit of one is not truncated, so the two
	// flags are not one flag.
	if cs.Truncated {
		t.Errorf("the precise list claims truncation with one caller and a limit of one")
	}
	// The bound reached the store as one over what was asked for, or the flag
	// above is a coincidence of the fixture's size.
	if st.approxCall.limit != 2 {
		t.Errorf("?limit=1 asked the approximate query for %d", st.approxCall.limit)
	}
}

func mustAtoi(t *testing.T, s string) int {
	t.Helper()
	n, err := strconv.Atoi(s)
	if err != nil {
		t.Fatal(err)
	}
	return n
}
