package handler

import (
	"context"
	"errors"
	"math"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/labstack/echo/v4"

	"github.com/mralaminahamed/codetrail/packages/shared/admit"
	"github.com/mralaminahamed/codetrail/packages/shared/metrics"
	"github.com/mralaminahamed/codetrail/packages/shared/models"
	"github.com/mralaminahamed/codetrail/packages/shared/rag"
	"github.com/mralaminahamed/codetrail/packages/shared/store"
)

// Reader is the store surface the read endpoints need, as an interface for the
// same reason Enqueuer is one: these tests run without a database, and a
// refusal, a 410 and a failed touch all have to be producible on demand.
type Reader interface {
	ListRepos(ctx context.Context, limit int) ([]store.RepoRow, error)
	GetRepo(ctx context.Context, id string) (models.Repo, error)
	RepoGone(ctx context.Context, id string) (bool, error)
	RepoStats(ctx context.Context, repoID string) (store.Stats, error)
	GetSpan(ctx context.Context, repoID, spanID string) (models.Span, error)
	NewerCommit(ctx context.Context, repoID string) (string, time.Time, error)
	TouchRepo(ctx context.Context, id string) error
	// The graph reads (see graph.go). CallersOf and ApproximateCallersOf are
	// two methods rather than one because §6 refuses to merge their answers,
	// and a single method returning one list would put that decision behind an
	// interface where no test could see it.
	Definitions(ctx context.Context, repoID, name, pkg string, suffix bool, limit int) ([]models.Symbol, error)
	Symbol(ctx context.Context, repoID, symbolID string) (models.Symbol, error)
	CallersOf(ctx context.Context, repoID, symbolID string, depth, limit int) ([]store.Caller, error)
	ApproximateCallersOf(ctx context.Context, repoID, name string, limit int) ([]store.Approximate, error)
}

// Retriever is what search and ask retrieve with: *rag.Retriever in the binary.
// An interface because the outcomes this handler has to tell apart — nothing
// retrieved, a top score under the floor, a top score that is not a number, and
// an arm that failed — are properties of a result, not of a query anyone can
// write.
type Retriever interface {
	Search(ctx context.Context, repoID, q string, limit int) (rag.Result, error)
}

// maxQuestionLen bounds what reaches the embedder and to_tsquery. A question is
// unbounded user input on a public endpoint; this is long enough for any real
// question and short enough to be obviously safe.
const maxQuestionLen = 1000

// maxReadLimit bounds every list this API serves. Outside 1..maxReadLimit is a
// 400 rather than a silent clamp: a caller asking for 5,000 spans has
// misunderstood something, and quietly serving 50 hides it.
const maxReadLimit = 50

const (
	defaultSearchLimit = 10
	defaultRepoLimit   = 20
)

// answeredBy names what produced an answer. There is exactly one answerer until
// P7 and the field exists anyway: spec §8 calls a silent downgrade the failure
// that costs a week, and a client written before the field exists cannot notice
// one.
const answeredBy = "extractive"

type repoView struct {
	ID         string    `json:"id"`
	Remote     string    `json:"remote"`
	Ref        string    `json:"ref"`
	Commit     string    `json:"commit"`
	IndexedAt  time.Time `json:"indexed_at"`
	LastUsedAt time.Time `json:"last_used_at"`
}

// spanView is what a caller may see of a span. models.Span stays whole for
// retrieval; file_id and repo_id are join keys that answer no question a caller
// can ask, and the citation next to this carries the repo. Same projection rule
// as jobView.
type spanView struct {
	ID        string          `json:"id"`
	Path      string          `json:"path"`
	Kind      models.SpanKind `json:"kind"`
	Symbol    string          `json:"symbol"`
	StartLine int             `json:"start_line"`
	EndLine   int             `json:"end_line"`
	Text      string          `json:"text"`
}

func spanOf(s models.Span) spanView {
	return spanView{
		ID: s.ID, Path: s.Path, Kind: s.Kind, Symbol: s.Symbol,
		StartLine: s.StartLine, EndLine: s.EndLine, Text: s.Text,
	}
}

// floorView travels with every answer. Calibrated is the field that stops a
// response looking confidently thresholded: spec:315 puts the number in P6, and
// until then a reader cannot otherwise tell a placeholder from a measurement.
//
// Applicable is false in lexical-only mode, where there is no cosine similarity
// to compare: ts_rank_cd is unbounded and corpus-dependent, so a floor over it
// would be a category error that happens to typecheck.
type floorView struct {
	Value      float64 `json:"value"`
	Calibrated bool    `json:"calibrated"`
	Applicable bool    `json:"applicable"`
}

type hitView struct {
	SpanID    string          `json:"span_id"`
	Path      string          `json:"path"`
	Kind      models.SpanKind `json:"kind"`
	Symbol    string          `json:"symbol"`
	StartLine int             `json:"start_line"`
	EndLine   int             `json:"end_line"`
	Text      string          `json:"text"`
	// Score is the fused score, which orders the list and carries no quality:
	// the top hit of any non-empty result scores 1/(k+1) whether it is perfect
	// or the best of a worthless set. VectorScore is the cosine similarity —
	// the number that does carry quality — and is null when the vector arm did
	// not return this span, because 0 there is a real similarity and would read
	// as one.
	Score       float64  `json:"score"`
	VectorScore *float64 `json:"vector_score"`
	// Per-arm ranks, so an arm's contribution is readable from the data rather
	// than inferred (spec:316). 0 means that arm did not return this span.
	VectorRank  int          `json:"vector_rank"`
	LexicalRank int          `json:"lexical_rank"`
	Citation    rag.Citation `json:"citation"`
}

type searchResponse struct {
	RepoID string   `json:"repo_id"`
	Mode   rag.Mode `json:"mode"`
	// A pointer: json.Marshal fails outright on NaN, and a lexical-only or
	// empty result has no cosine similarity. null is the honest encoding.
	TopScore *float64  `json:"top_score"`
	Count    int       `json:"count"`
	Hits     []hitView `json:"hits"`
}

type answerResponse struct {
	RepoID     string      `json:"repo_id"`
	Refused    bool        `json:"refused"`
	AnsweredBy string      `json:"answered_by"`
	Answer     string      `json:"answer"`
	Citations  []rag.Cited `json:"citations"`
	// How many ranked spans did not fit the budget. An answer built from 2 of 7
	// spans is a different claim from one built from all of them.
	Dropped  int       `json:"dropped"`
	Mode     rag.Mode  `json:"mode"`
	TopScore *float64  `json:"top_score"`
	Floor    floorView `json:"floor"`
}

// refusalResponse is a 200. Spec §10 makes a refusal and an error distinct
// outcomes; expressing "we had nothing to say" as an HTTP error would file it
// inside every error-rate panel in existence, which is the hiding §10 exists to
// prevent.
type refusalResponse struct {
	RepoID   string     `json:"repo_id"`
	Refused  bool       `json:"refused"`
	Reason   rag.Reason `json:"reason"`
	Detail   string     `json:"detail"`
	Mode     rag.Mode   `json:"mode"`
	TopScore *float64   `json:"top_score"`
	Floor    floorView  `json:"floor"`
}

func (h *Handler) listRepos(c echo.Context) error {
	limit, ok := limitOf(c, c.QueryParam("limit"), defaultRepoLimit)
	if !ok {
		return nil
	}
	rows, err := h.Repos.ListRepos(c.Request().Context(), limit)
	if err != nil {
		return h.fail(c, err, "list repos")
	}
	out := make([]repoView, 0, len(rows))
	for _, r := range rows {
		out = append(out, repoView{
			ID: r.ID, Remote: r.Remote, Ref: r.Ref, Commit: r.Commit,
			IndexedAt: r.IndexedAt, LastUsedAt: r.LastUsedAt,
		})
	}
	return c.JSON(http.StatusOK, echo.Map{"repos": out, "count": len(out)})
}

func (h *Handler) getRepo(c echo.Context) error {
	r, ok, err := h.lookupRepo(c)
	if !ok {
		return err
	}
	ctx := c.Request().Context()
	stats, err := h.Repos.RepoStats(ctx, r.ID)
	if err != nil {
		return h.fail(c, err, "repo stats")
	}
	newer, err := h.newer(ctx, r.ID)
	if err != nil {
		return h.fail(c, err, "staleness")
	}
	h.touch(c, r.ID)
	return c.JSON(http.StatusOK, echo.Map{
		"id": r.ID, "remote": r.Remote, "ref": r.Ref, "commit": r.Commit,
		"indexed_at": r.IndexedAt,
		// Three numbers, not a coverage ratio: chunk.Chunks drops token-less
		// regions, so a repo's span count is not derivable from its file count
		// and one figure would invent the relationship.
		"files": stats.Files, "spans": stats.Spans,
		"files_with_spans": stats.FilesWithSpans,
		// The graph, split by provenance. The only place a reader can see how
		// much of a repository's call graph is precise — an aggregate over a
		// per-row column, never a per-repo label, which is why every caller row
		// carries the label too (spec:190).
		"symbols": stats.Symbols, "edges": stats.Edges,
		"edges_resolved": stats.EdgesResolved, "edges_syntactic": stats.EdgesSyntactic,
		"staleness": rag.NewStaleness(r, newer, h.now()),
	})
}

func (h *Handler) getSpan(c echo.Context) error {
	r, ok, err := h.lookupRepo(c)
	if !ok {
		return err
	}
	ctx := c.Request().Context()
	sp, err := h.Repos.GetSpan(ctx, r.ID, c.Param("span"))
	if errors.Is(err, store.ErrNotFound) {
		return c.JSON(http.StatusNotFound, echo.Map{"error": "no such span in this repository"})
	}
	if err != nil {
		return h.fail(c, err, "read span")
	}
	newer, err := h.newer(ctx, r.ID)
	if err != nil {
		return h.fail(c, err, "staleness")
	}
	h.touch(c, r.ID)
	return c.JSON(http.StatusOK, echo.Map{
		"span":     spanOf(sp),
		"citation": rag.NewCitation(r, sp, newer, h.now()),
	})
}

// searchRequest carries the query in a body, not a query string: a question is
// logged by every proxy, load balancer and access log between the caller and
// this process, and it is the one string in this phase that must not be.
//
// Limit is a pointer so an explicit 0 is a 400 rather than the default. A
// caller who asked for zero hits has misunderstood something, and answering ten
// hides it.
type searchRequest struct {
	Q     string `json:"q"`
	Limit *int   `json:"limit"`
	// Mode is declared only so it can be refused, and is a *string rather than
	// a rag.Mode because nothing here parses it. Until it existed, echo's
	// binder dropped the key and the response's own mode read as an echo of
	// what was asked for. nil is absence, an explicit null included: neither
	// names a mode.
	Mode *string `json:"mode"`
}

func (h *Handler) search(c echo.Context) error {
	var req searchRequest
	q, limit, ok := h.query(c, &req, defaultSearchLimit)
	if !ok {
		return nil
	}
	r, ok, err := h.lookupRepo(c)
	if !ok {
		return err
	}
	ctx := c.Request().Context()
	out, err := h.Rag.Search(ctx, r.ID, q, limit)
	// An empty corpus ranks nothing, which is an empty list rather than a
	// failure. No answer counter here either: those three outcomes are spec
	// §10's for a question that was asked; a search is a ranked list, and
	// counting its failures as answer errors would put two populations in one
	// rate.
	if err != nil && !noSpans(err) {
		return h.fail(c, err, "search")
	}
	newer, err := h.newer(ctx, r.ID)
	if err != nil {
		return h.fail(c, err, "staleness")
	}

	hits := make([]hitView, 0, len(out.Hits))
	for _, f := range out.Hits {
		sp, ok := out.Spans[f.SpanID]
		if !ok {
			// No text, no lines, no digest: a citation for it would name
			// #L0-L0 of an empty path, which is a wrong claim rather than a
			// missing one.
			continue
		}
		hits = append(hits, hitView{
			SpanID: f.SpanID, Path: sp.Path, Kind: sp.Kind, Symbol: sp.Symbol,
			StartLine: sp.StartLine, EndLine: sp.EndLine, Text: sp.Text,
			Score: f.Score, VectorScore: vectorScore(f),
			VectorRank: f.VectorRank, LexicalRank: f.LexicalRank,
			Citation: rag.NewCitation(r, sp, newer, h.now()),
		})
	}
	h.touch(c, r.ID)
	// No floor on this route. Search ranks; the floor is the answer's decision,
	// and reporting one here would imply a filter that did not run.
	return c.JSON(http.StatusOK, searchResponse{
		RepoID: r.ID, Mode: out.Mode, TopScore: number(out.TopScore),
		Count: len(hits), Hits: hits,
	})
}

func (h *Handler) ask(c echo.Context) error {
	var req searchRequest
	// The span budget is the default retrieval depth, so an ask that says
	// nothing retrieves exactly what an answer can hold. A caller may ask for
	// more, and then the assembler drops what does not fit and says how many.
	q, limit, ok := h.query(c, &req, h.Budget.MaxSpans)
	if !ok {
		// Not counted. A rejected request never reached the answerer, and
		// filing a caller's malformed question as an answer error would inflate
		// exactly the rate spec §10 wants readable.
		return nil
	}
	r, ok, err := h.lookupRepo(c)
	if !ok {
		return err
	}
	ctx := c.Request().Context()
	out, err := h.Rag.Search(ctx, r.ID, q, limit)
	// A repository with no spans has nothing to rank, so out carries no hits
	// and Decide reaches no_spans below. Anything else is ours.
	if err != nil && !noSpans(err) {
		metrics.CountAnswer("error")
		return h.fail(c, err, "ask")
	}

	floor := floorView{Value: h.Floor.Value, Calibrated: h.Floor.Calibrated, Applicable: out.VectorRan}
	outcome, reason := rag.Decide(h.Floor, len(out.Hits), out.TopScore, out.VectorRan)
	if outcome == rag.OutcomeRefused {
		metrics.CountAnswer("refused")
		metrics.CountRefusal(string(reason))
		h.touch(c, r.ID)
		// 200: a refusal is an outcome, not an error.
		return c.JSON(http.StatusOK, refusalResponse{
			RepoID: r.ID, Refused: true, Reason: reason, Detail: detail(reason, h.Floor),
			Mode: out.Mode, TopScore: number(out.TopScore), Floor: floor,
		})
	}

	newer, err := h.newer(ctx, r.ID)
	if err != nil {
		metrics.CountAnswer("error")
		return h.fail(c, err, "ask")
	}
	a := rag.Assemble(r, out.Hits, out.Spans, newer, h.now(), h.Budget)
	metrics.CountAnswer("answered")
	h.touch(c, r.ID)
	return c.JSON(http.StatusOK, answerResponse{
		RepoID: r.ID, Refused: false, AnsweredBy: answeredBy,
		Answer: a.Text, Citations: a.Citations, Dropped: a.Dropped,
		Mode: out.Mode, TopScore: number(out.TopScore), Floor: floor,
	})
}

// noSpans is the retriever's report that this repository holds nothing to rank:
// SpanEmbedder answers store.ErrNotFound for a corpus with no spans in it.
//
// An outcome, not a failure — the same reading the repo and span routes above
// already give ErrNotFound. Filing it as an error would move the error counter
// for a corpus that is merely empty and make spec §10's no_spans refusal
// unreachable in every mode that runs the vector arm, which is the default.
func noSpans(err error) bool { return errors.Is(err, store.ErrNotFound) }

// detail says what the reason means in words, from the closed set of reasons
// and the configured floor — never from anything the caller sent. Spec §10
// forbids a generic refusal, and "refused" with a bare label is one.
func detail(r rag.Reason, f rag.Floor) string {
	switch r {
	case rag.ReasonNoSpans:
		return "Nothing in this repository's index matched the question."
	case rag.ReasonBelowFloor:
		return "The best match scored under the configured floor of " +
			strconv.FormatFloat(f.Value, 'g', -1, 64) +
			". That floor is not calibrated; its value is measured in P6."
	case rag.ReasonUnscored:
		return "The best match has no usable similarity score, so there is nothing to judge it by."
	}
	return ""
}

// lookupRepo resolves the repository a route names, and is the only place the
// 404/410 distinction is made. ok=false means the returned error is the
// finished response.
//
// repos is read first and RepoGone only when it holds nothing. A repo id is
// hash(key, commit), so a repository re-indexed at the same commit re-uses the
// id its tombstone was written under: asking "is it gone" first answers yes for
// a repository that is right there, serving 410 for a live corpus. RepoGone
// guards that from its own side with a NOT EXISTS; this order is what stops the
// handler needing it to.
func (h *Handler) lookupRepo(c echo.Context) (models.Repo, bool, error) {
	ctx := c.Request().Context()
	id := c.Param("repo")
	r, err := h.Repos.GetRepo(ctx, id)
	if err == nil {
		return r, true, nil
	}
	if !errors.Is(err, store.ErrNotFound) {
		return models.Repo{}, false, h.fail(c, err, "read repo")
	}
	gone, err := h.Repos.RepoGone(ctx, id)
	if err != nil {
		return models.Repo{}, false, h.fail(c, err, "read tombstone")
	}
	if gone {
		// 410, not 404: it existed, and that is a different fact (spec §10).
		// Past KEEP_TOMBSTONES it goes back to 404, which is honest — we no
		// longer remember.
		return models.Repo{}, false, c.JSON(http.StatusGone, echo.Map{
			"error": "this repository was indexed and has since been evicted",
		})
	}
	return models.Repo{}, false, c.JSON(http.StatusNotFound, echo.Map{"error": "no such repository"})
}

// query binds and validates a search or ask body. ok=false means a 400 has been
// written.
//
// The rules are named in the body (spec §10's "never a generic refusal"), and
// the last one is not cosmetic: without it "???" reaches the embedder, which
// refuses a text it can hash no token from, and a user's punctuation becomes a
// 500 from three layers down.
func (h *Handler) query(c echo.Context, req *searchRequest, defLimit int) (string, int, bool) {
	if err := c.Bind(req); err != nil {
		h.Log.Warn().Err(err).Str("request_id", requestID(c)).Str("op", "bind").Msg("malformed request body")
		badRequest(c, "malformed request body")
		return "", 0, false
	}
	// Refused rather than honoured: retrieval mode is a process setting
	// (RETRIEVAL_MODE) and Search takes no argument for it, so no value here
	// could select anything. Every mode is refused, including the configured
	// one, because which mode that is is not something a caller can know.
	if req.Mode != nil {
		badRequest(c, "mode is not a request field: it is configured per process and reported in the response")
		return "", 0, false
	}
	q := strings.TrimSpace(req.Q)
	switch {
	case q == "":
		badRequest(c, "q must not be empty")
		return "", 0, false
	case len(q) > maxQuestionLen:
		badRequest(c, "q must be at most "+strconv.Itoa(maxQuestionLen)+" characters")
		return "", 0, false
	// Terms is the same tokeniser the lexical arm queries with, so "has a
	// usable term" means the same thing at the edge as it does inside.
	case len(rag.Terms(q, false)) == 0:
		badRequest(c, "q must contain at least one letter or digit")
		return "", 0, false
	}
	limit := defLimit
	if req.Limit != nil {
		limit = *req.Limit
	}
	if limit < 1 || limit > maxReadLimit {
		badRequest(c, "limit must be between 1 and "+strconv.Itoa(maxReadLimit))
		return "", 0, false
	}
	return q, limit, true
}

// limitOf reads a bounded ?limit=. ok=false means a 400 has been written.
func limitOf(c echo.Context, raw string, def int) (int, bool) {
	if raw == "" {
		return def, true
	}
	n, err := strconv.Atoi(raw)
	if err != nil || n < 1 || n > maxReadLimit {
		badRequest(c, "limit must be between 1 and "+strconv.Itoa(maxReadLimit))
		return 0, false
	}
	return n, true
}

// badRequest names the rule that fired. The question itself is never echoed:
// what the caller sent is what they already have, and this body ends up in the
// same places a log line does.
func badRequest(c echo.Context, detail string) {
	//nolint:errcheck // the caller returns nil; echo has already written.
	_ = c.JSON(http.StatusBadRequest, echo.Map{"error": detail, "rule": string(admit.RuleForm)})
}

// newer reads the one staleness claim the corpus can prove, once per request:
// it is a property of the repository, not of a span, so every citation in one
// response says the same thing about the same ref.
//
// An error fails the request rather than degrading to "not superseded". A
// citation that says nothing moved because the query that would have noticed
// failed is a freshness claim made from no evidence, which is the overclaim
// this whole path exists to avoid.
func (h *Handler) newer(ctx context.Context, repoID string) (rag.Newer, error) {
	commit, at, err := h.Repos.NewerCommit(ctx, repoID)
	if err != nil {
		return rag.Newer{}, err
	}
	return rag.Newer{Commit: commit, IndexedAt: at}, nil
}

// touch winds the LRU clock spec §4 designed and P1 shipped with nothing to
// wind it. Without a reader winding it, "least recently used" means "least
// recently indexed" and a popular repository is evicted while it is being read.
//
// A failure is logged and dropped: the touch is a hint for a future eviction,
// not part of the caller's answer, and failing a correct response on a
// bookkeeping write trades the answer for the hint. A refusal winds it too —
// someone used this repository, whatever the corpus had to say.
func (h *Handler) touch(c echo.Context, id string) {
	if err := h.Repos.TouchRepo(c.Request().Context(), id); err != nil {
		h.Log.Warn().Err(err).Str("request_id", requestID(c)).Str("op", "touch").
			Msg("could not wind the LRU clock")
	}
}

// now is the clock citations are dated against. A zero Handler must answer
// rather than panic, which is what the fallback is for.
func (h *Handler) now() time.Time {
	if h.Now != nil {
		return h.Now()
	}
	return time.Now()
}

// vectorScore is the cosine similarity of a hit, or nil when the vector arm did
// not return it. Rank 0 is how Fuse spells "this arm did not have it", and the
// zero VectorScore beside it is not a similarity of zero.
func vectorScore(f rag.Fused) *float64 {
	if f.VectorRank == 0 {
		return nil
	}
	return number(f.VectorScore)
}

// number is the honest encoding of a score that may not be one. json.Marshal
// fails outright on NaN — the response would be a 500 with a half-written body
// — and a lexical-only or empty result genuinely has no cosine similarity.
func number(v float64) *float64 {
	if math.IsNaN(v) {
		return nil
	}
	return &v
}
