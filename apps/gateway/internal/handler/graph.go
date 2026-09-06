package handler

import (
	"context"
	"errors"
	"net/http"
	"strconv"
	"strings"

	"github.com/labstack/echo/v4"

	"github.com/mralaminahamed/codetrail/packages/shared/models"
	"github.com/mralaminahamed/codetrail/packages/shared/rag"
	"github.com/mralaminahamed/codetrail/packages/shared/store"
)

// GET, not POST. P3 put search and ask behind POST because a question is prose
// that must not reach a proxy's access log; a symbol name is an identifier and
// is already in the URL of every permalink this product renders, so the rule
// does not extend here and a GET is cacheable and linkable.
const (
	defaultSymbolLimit = 20
	defaultCallerLimit = 20
	// Direct callers are what "who calls this" usually means. A default of 3
	// would make the common request expensive for a reason nobody asked for.
	defaultDepth = 1
	// Measured, and lowered from 5. Fan-out in a call graph is multiplicative
	// and the depth bound is the only thing that caps it: EXPLAIN (ANALYZE)
	// over 20 mutually-calling symbols walks 1,494,559 rows at depth 5 before
	// LIMIT sees any of them, and 30 symbols at depth 4 exceeds the statement
	// timeout. At 3 the same graphs answer in 79ms and 1.5s.
	//
	// Three is also what agent.DefaultToolLimits gives the model's callers_of,
	// so the endpoint and the tool now bound the same walk the same way.
	maxDepth = 3
)

// symbolView is what a caller may see of a definition. repo_id and file_id are
// join keys that answer no question a caller can ask — the same projection rule
// jobView and spanView follow.
//
// span_id keeps its key when it is empty rather than being omitted: a client
// cannot otherwise tell "this declaration has no span" from "the field is
// gone", and the first is a real state (a declaration longer than one window).
type symbolView struct {
	ID        string          `json:"id"`
	Name      string          `json:"name"`
	Pkg       string          `json:"pkg"`
	Kind      models.SpanKind `json:"kind"`
	Path      string          `json:"path"`
	StartLine int             `json:"start_line"`
	EndLine   int             `json:"end_line"`
	SpanID    string          `json:"span_id"`
}

func symbolOf(sy models.Symbol) symbolView {
	return symbolView{
		ID: sy.ID, Name: sy.Name, Pkg: sy.Pkg, Kind: sy.Kind, Path: sy.Path,
		StartLine: sy.StartLine, EndLine: sy.EndLine, SpanID: sy.SpanID,
	}
}

// callSite is the file:line of one hop. Spec:5 describes this product in one
// sentence and that sentence is about citing file:line; a caller row without it
// names who calls and cannot say where.
type callSite struct {
	Path string `json:"path"`
	Line int    `json:"line"`
}

// callerView is one definition that reaches the queried one.
//
// Provenance is on every row because §6 makes the label a per-row property: the
// repo view's aggregate says how much of a repository is precise, and a client
// that read the aggregate as the label would be reading a per-repo fact off a
// per-row one, which is the failure the whole phase is written against.
type callerView struct {
	Symbol     symbolView        `json:"symbol"`
	Depth      int               `json:"depth"`
	Provenance models.Provenance `json:"provenance"`
	Call       callSite          `json:"call"`
	// Null when the definition has no span, because a digest is a claim about
	// text and an empty one is a claim that cannot be checked presented as one
	// that can. P3 refused to render a permalink for an unknown forge for the
	// same reason.
	Citation *rag.Citation `json:"citation"`
}

// approximateCallerView is one caller matched by name. It has no depth: the
// match is a guess, and traversing from a guess compounds it.
type approximateCallerView struct {
	Symbol     symbolView        `json:"symbol"`
	ToName     string            `json:"to_name"`
	Provenance models.Provenance `json:"provenance"`
	Call       callSite          `json:"call"`
	Citation   *rag.Citation     `json:"citation"`
}

// approximateView is spec:84's invariant at the API boundary: a syntactic edge
// has a null target, so these callers name something spelled ToName and cannot
// say it is this symbol. A sibling field with its own count, never merged into
// callers — if a console flattens the two it does so knowingly.
//
// MatchedOn is on the wire so no client has to know the convention to interpret
// the block. Failed is Open question 14: the precise half is what was asked
// for, so a failure of this query alone is reported here rather than turned
// into a 500 that discards an answer nothing was wrong with.
type approximateView struct {
	MatchedOn string                  `json:"matched_on"`
	Count     int                     `json:"count"`
	Truncated bool                    `json:"truncated"`
	Failed    bool                    `json:"failed"`
	Callers   []approximateCallerView `json:"callers"`
}

// matchedOnName is the only value MatchedOn takes in P4. A literal rather than
// the zero value, so the claim is visible at the place that makes it.
const matchedOnName = "name"

type callersResponse struct {
	RepoID string     `json:"repo_id"`
	Symbol symbolView `json:"symbol"`
	Depth  int        `json:"depth"`
	// Whether the LIMIT bit. Open question 1 makes the limit the second bound
	// on a traversal whose fan-out is multiplicative; without this a truncated
	// answer and a complete one are the same payload.
	Truncated   bool            `json:"truncated"`
	Callers     []callerView    `json:"callers"`
	Approximate approximateView `json:"approximate"`
}

func (h *Handler) listSymbols(c echo.Context) error {
	name := strings.TrimSpace(c.QueryParam("name"))
	if name == "" {
		badRequest(c, "name must not be empty")
		return nil
	}
	suffix, ok := boolOf(c, "suffix", c.QueryParam("suffix"))
	if !ok {
		return nil
	}
	limit, ok := limitOf(c, c.QueryParam("limit"), defaultSymbolLimit)
	if !ok {
		return nil
	}
	r, ok, err := h.lookupRepo(c)
	if !ok {
		return err
	}
	ctx := c.Request().Context()
	rows, err := h.Repos.Definitions(ctx, r.ID, name,
		strings.TrimSpace(c.QueryParam("pkg")), suffix, limit+1)
	if err != nil {
		return h.fail(c, err, "definitions")
	}
	rows, truncated := clip(rows, limit)
	out := make([]symbolView, 0, len(rows))
	for _, sy := range rows {
		out = append(out, symbolOf(sy))
	}
	// Every row here is a path and a line range at one commit, which is a claim
	// about where something is. It carries no digest, so staleness is the only
	// thing that can qualify it — and a failed staleness query fails the
	// request rather than degrading to "not superseded", which would be a
	// freshness claim made from no evidence.
	newer, err := h.newer(ctx, r.ID)
	if err != nil {
		return h.fail(c, err, "staleness")
	}
	h.touch(c, r.ID)
	// matched is derived from the flag that reached the store, not from the
	// query string: it exists so a widening the caller did not ask for is
	// visible in the payload, and reading it off the request would report the
	// question rather than the answer.
	return c.JSON(http.StatusOK, echo.Map{
		"repo_id": r.ID, "count": len(out), "matched": matchedOf(suffix),
		"truncated": truncated, "symbols": out,
		"staleness": rag.NewStaleness(r, newer, h.now()),
	})
}

func (h *Handler) getSymbol(c echo.Context) error {
	r, ok, err := h.lookupRepo(c)
	if !ok {
		return err
	}
	ctx := c.Request().Context()
	sy, err := h.Repos.Symbol(ctx, r.ID, c.Param("symbol"))
	if errors.Is(err, store.ErrNotFound) {
		return c.JSON(http.StatusNotFound, echo.Map{"error": "no such symbol in this repository"})
	}
	if err != nil {
		return h.fail(c, err, "read symbol")
	}
	newer, err := h.newer(ctx, r.ID)
	if err != nil {
		return h.fail(c, err, "staleness")
	}
	cite, err := h.citer(r, newer).of(ctx, sy)
	if err != nil {
		return h.fail(c, err, "read span")
	}
	h.touch(c, r.ID)
	// staleness beside the citation rather than only inside it: a definition
	// with no span still has a repository whose ref may have moved, and that
	// claim must not disappear with the citation.
	return c.JSON(http.StatusOK, echo.Map{
		"symbol":    symbolOf(sy),
		"citation":  cite,
		"staleness": rag.NewStaleness(r, newer, h.now()),
	})
}

func (h *Handler) callersOfSymbol(c echo.Context) error {
	depth, ok := depthOf(c, c.QueryParam("depth"))
	if !ok {
		return nil
	}
	limit, ok := limitOf(c, c.QueryParam("limit"), defaultCallerLimit)
	if !ok {
		return nil
	}
	r, ok, err := h.lookupRepo(c)
	if !ok {
		return err
	}
	ctx := c.Request().Context()
	sy, err := h.Repos.Symbol(ctx, r.ID, c.Param("symbol"))
	if errors.Is(err, store.ErrNotFound) {
		// A symbol that is not here is a 404. An empty caller list is a 200:
		// "nothing calls this" is an answer, and serving it for a symbol that
		// does not exist would make the two indistinguishable.
		return c.JSON(http.StatusNotFound, echo.Map{"error": "no such symbol in this repository"})
	}
	if err != nil {
		return h.fail(c, err, "read symbol")
	}
	// One over the bound, so "the limit was reached" and "the limit bit" are
	// two different facts rather than one number a client has to compare.
	rows, err := h.Repos.CallersOf(ctx, r.ID, sy.ID, depth, limit+1)
	if err != nil {
		return h.fail(c, err, "callers")
	}
	rows, truncated := clip(rows, limit)
	newer, err := h.newer(ctx, r.ID)
	if err != nil {
		return h.fail(c, err, "staleness")
	}
	ci := h.citer(r, newer)

	callers := make([]callerView, 0, len(rows))
	for _, cl := range rows {
		cite, err := ci.of(ctx, cl.Symbol)
		if err != nil {
			return h.fail(c, err, "read span")
		}
		callers = append(callers, callerView{
			Symbol: symbolOf(cl.Symbol), Depth: cl.Depth,
			// CallersOf traverses to_symbol_id, and the schema's CHECK pairs a
			// non-null target with 'resolved'. So the label is a property of
			// the row rather than a guess about it.
			Provenance: models.ProvenanceResolved,
			Call:       callSite{Path: cl.CallPath, Line: cl.CallLine},
			Citation:   cite,
		})
	}

	approx, err := h.approximate(ctx, ci, r.ID, sy, limit)
	if err != nil {
		return h.fail(c, err, "read span")
	}
	h.touch(c, r.ID)
	return c.JSON(http.StatusOK, callersResponse{
		RepoID: r.ID, Symbol: symbolOf(sy), Depth: depth,
		Truncated: truncated, Callers: callers, Approximate: approx,
	})
}

// approximate builds the name-matched block. Its own query's failure is
// reported in the block and never returned: the precise callers are what was
// asked for, and discarding a correct answer because the guess beside it failed
// trades the answer for the guess (Open question 14). The error returned here
// is the span read's, which belongs to the citation of a row that did come back.
func (h *Handler) approximate(ctx context.Context, ci *citer, repoID string, sy models.Symbol, limit int) (approximateView, error) {
	out := approximateView{MatchedOn: matchedOnName, Callers: []approximateCallerView{}}
	rows, err := h.Repos.ApproximateCallersOf(ctx, repoID, approxName(sy.Name), limit+1)
	if err != nil {
		h.Log.Error().Err(err).Str("op", "approximate callers").Str("repo_id", repoID).
			Msg("the approximate caller set failed; the precise answer stands")
		out.Failed = true
		return out, nil
	}
	rows, out.Truncated = clip(rows, limit)
	for _, a := range rows {
		cite, cerr := ci.of(ctx, a.Symbol)
		if cerr != nil {
			return approximateView{}, cerr
		}
		out.Callers = append(out.Callers, approximateCallerView{
			Symbol: symbolOf(a.Symbol), ToName: a.ToName,
			// The query's own predicate is to_symbol_id IS NULL, which the
			// schema's CHECK pairs with 'syntactic'.
			Provenance: models.ProvenanceSyntactic,
			Call:       callSite{Path: a.CallPath, Line: a.CallLine},
			Citation:   cite,
		})
	}
	out.Count = len(out.Callers)
	return out, nil
}

// approxName is the name a syntactic edge could have recorded for this symbol.
//
// symbols.name spells a method Store.Get; an edge's to_name is the callee's
// last identifier — spec:84's "it calls something named Close" — because
// without type information nothing can tell a receiver from a package
// qualifier. Asking for "Store.Get" matches no syntactic edge in any corpus, so
// the approximate set would be silently empty for every method in the corpus.
func approxName(name string) string {
	if i := strings.LastIndex(name, "."); i >= 0 {
		return name[i+1:]
	}
	return name
}

// citer renders a definition's citation from the span it links to, remembering
// each span it reads: a caller list is up to twice the limit in rows and two
// definitions in one long declaration share a span.
type citer struct {
	h     *Handler
	repo  models.Repo
	newer rag.Newer
	seen  map[string]*rag.Citation
}

func (h *Handler) citer(r models.Repo, newer rag.Newer) *citer {
	return &citer{h: h, repo: r, newer: newer, seen: map[string]*rag.Citation{}}
}

// of is nil, nil for a definition with no span. The location is already in the
// symbol; what is missing is the digest, and a citation carrying an empty one
// claims text nobody can check against.
//
// A span_id that reads nothing is not treated as "no span": the column is
// ON DELETE SET NULL, so a non-null id pointing at no row is a broken invariant
// rather than a missing digest, and answering null would hide it.
func (ci *citer) of(ctx context.Context, sy models.Symbol) (*rag.Citation, error) {
	if sy.SpanID == "" {
		return nil, nil
	}
	if cit, ok := ci.seen[sy.SpanID]; ok {
		return cit, nil
	}
	sp, err := ci.h.Repos.GetSpan(ctx, ci.repo.ID, sy.SpanID)
	if err != nil {
		return nil, err
	}
	cit := rag.NewCitation(ci.repo, sp, ci.newer, ci.h.now())
	ci.seen[sy.SpanID] = &cit
	return &cit, nil
}

// depthOf reads ?depth=. Out of range is a 400 naming the rule rather than a
// silent clamp, the decision P3 made for limit and for the same reason: a
// caller asking for depth 40 has misunderstood the endpoint, and quietly
// serving 5 hides it.
func depthOf(c echo.Context, raw string) (int, bool) {
	if raw == "" {
		return defaultDepth, true
	}
	n, err := strconv.Atoi(raw)
	if err != nil || n < 1 || n > maxDepth {
		badRequest(c, "depth must be between 1 and "+strconv.Itoa(maxDepth))
		return 0, false
	}
	return n, true
}

// boolOf reads an opt-in flag. An unparseable value is a 400 rather than false:
// ?suffix=yes quietly meaning "exact" is the decorative-parameter defect P3
// shipped with mode, where the API accepted a field and ignored it.
func boolOf(c echo.Context, name, raw string) (bool, bool) {
	if raw == "" {
		return false, true
	}
	v, err := strconv.ParseBool(raw)
	if err != nil {
		badRequest(c, name+" must be true or false")
		return false, false
	}
	return v, true
}

func matchedOf(suffix bool) string {
	if suffix {
		return "suffix"
	}
	return "exact"
}

// clip trims a list read one over its bound and says whether it bit.
func clip[T any](rows []T, limit int) ([]T, bool) {
	if len(rows) > limit {
		return rows[:limit], true
	}
	return rows, false
}
