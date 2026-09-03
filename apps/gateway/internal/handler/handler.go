// Package handler implements the gateway's REST API.
package handler

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/labstack/echo/v4"
	"github.com/rs/zerolog"

	"github.com/mralaminahamed/codetrail/packages/shared/admit"
	"github.com/mralaminahamed/codetrail/packages/shared/jobs"
	"github.com/mralaminahamed/codetrail/packages/shared/metrics"
	"github.com/mralaminahamed/codetrail/packages/shared/rag"
)

// Enqueuer is the queue surface the API needs. An interface so the handler
// tests run without a database.
type Enqueuer interface {
	Enqueue(ctx context.Context, remote, ref string) (jobs.Job, error)
	Get(ctx context.Context, id string) (jobs.Job, error)
}

type Handler struct {
	Policy admit.Policy
	Jobs   Enqueuer
	// Repos, Rag, Floor and Budget are the read path (see read.go). Floor
	// travels with every answer rather than being applied inside the retriever,
	// because only this handler knows whether a request was a search or an ask.
	Repos  Reader
	Rag    Retriever
	Floor  rag.Floor
	Budget rag.Budget
	// LLM is the bounded loop, or nil when LLM_PROVIDER=none. Nil is not a
	// degraded state: an unconfigured deployment is one that was never asked to
	// do this, and reporting it as degraded would make every offline deploy
	// look broken.
	LLM Answerer
	// AnswerDefault is what a request that names no answerer gets. It defaults
	// to extractive EVEN WHEN A PROVIDER IS CONFIGURED, because spec:230 makes
	// the free path the default and a public demo has to cost nothing per
	// visitor who does not ask for more.
	AnswerDefault string
	// Now dates citations. A field so a test can pin the staleness wording
	// against a fixed clock.
	Now func() time.Time
	Log zerolog.Logger
}

func Mount(e *echo.Echo, h *Handler, mw ...echo.MiddlewareFunc) {
	g := e.Group("/api", mw...)
	g.POST("/repos", h.postRepo)
	g.GET("/jobs/:id", h.getJob)
	g.GET("/repos", h.listRepos)
	g.GET("/repos/:repo", h.getRepo)
	g.GET("/repos/:repo/spans/:span", h.getSpan)
	// POST, with the query in the body: a question in a query string is logged
	// by every proxy, load balancer and access log between the caller and this
	// process, and it is the one string in this phase that must not be.
	g.POST("/repos/:repo/search", h.search)
	g.POST("/repos/:repo/ask", h.ask)
	// GET, unlike the two above: a symbol name is an identifier, not prose, and
	// it is already in the URL of every permalink this product renders. See
	// graph.go.
	g.GET("/repos/:repo/symbols", h.listSymbols)
	g.GET("/repos/:repo/symbols/:symbol", h.getSymbol)
	g.GET("/repos/:repo/symbols/:symbol/callers", h.callersOfSymbol)
}

type repoRequest struct {
	Remote string `json:"remote"`
	Ref    string `json:"ref"`
}

// jobView is what a caller may see of a job. jobs.Job stays whole for the
// indexer; this is the projection the API serves.
//
// attempts is scheduling detail. error is written from the indexer's git
// stderr, which has carried a server filesystem path and a credential-bearing
// URL — the same disclosure the 500 path withholds, and there is no reason for
// a 200 to be less careful than a 500.
//
// Spec §10 wants a terminal failure to carry its reason into the API. That is
// deliberately not honoured yet: no safe vocabulary for a reason exists today,
// and git's stderr is not one. Surfacing nothing beats surfacing a path. Do
// not close this gap by piping the raw column through.
//
// repo_id is the one field added since: every read endpoint is keyed on a repo
// id, and hash(key, commit) is computable only by the indexer, which saw the
// commit. Without it, submit-then-poll cannot name what it produced and the
// only route to an id is the corpus listing. Empty until the job is done.
type jobView struct {
	ID     string      `json:"id"`
	Remote string      `json:"remote"`
	Ref    string      `json:"ref"`
	Status jobs.Status `json:"status"`
	RepoID string      `json:"repo_id,omitempty"`
}

func view(j jobs.Job) jobView {
	return jobView{ID: j.ID, Remote: j.Remote, Ref: j.Ref, Status: j.Status, RepoID: j.RepoID}
}

// maxRefLen bounds what reaches git's argv. Git's own limit is the filesystem's;
// this is short enough to be obviously safe and long enough for any real ref.
const maxRefLen = 255

// validRef allowlists the charset a git ref needs, the way admit.validSegment
// does for a path segment, and refuses "." and ".." as that does. The indexer
// hands ref to `git clone --branch`, so a leading "-" is an option and not a
// name, and ".." is forbidden by git's own ref syntax anyway.
//
// It diverges from validSegment by allowing "/", which a ref needs, and by
// capping the length, which argv makes worth doing; it refuses ".." anywhere
// rather than only as the whole string, which is stricter.
//
// It is a bound on what reaches argv, not a model of git's ref syntax, and it
// is looser than git in several ways: it accepts a leading or trailing "/", a
// doubled "//", a ".lock" suffix, a component starting with "." (".foo",
// "a/.b") and a ref ending with "." ("foo.", "v1."). Nothing downstream makes
// up the difference — no code runs check-ref-format — so each of those reaches
// git, fails the clone, and spends the job's attempts. That is the whole cost:
// none can be read as an option, and the clone is the only place a ref goes.
// The trailing-dot rule is the refname's, not each component's: "foo./bar" is
// legal to git and accepted here.
//
// In one direction it is narrower than git: "+" is legal in a tag, so a semver
// build-metadata tag is refused here. Widening it is cheap and nobody has
// asked; the charset is the thing worth being conservative about.
func validRef(s string) bool {
	if s == "" || s == "." || len(s) > maxRefLen || strings.HasPrefix(s, "-") || strings.Contains(s, "..") {
		return false
	}
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
		case r == '.', r == '_', r == '-', r == '/':
		default:
			return false
		}
	}
	return true
}

func (h *Handler) postRepo(c echo.Context) error {
	var req repoRequest
	if err := c.Bind(&req); err != nil {
		// echo's HTTPError stringifies with the bound Go type appended, which
		// names this package rather than anything the caller sent. A fixed
		// message is the only body that cannot be made to carry server state.
		h.Log.Warn().Err(err).Str("request_id", requestID(c)).Str("op", "bind").Msg("malformed request body")
		metrics.CountAdmission(false, string(admit.RuleForm))
		return c.JSON(http.StatusBadRequest, echo.Map{"error": "malformed request body", "rule": string(admit.RuleForm)})
	}
	remote, err := h.Policy.Check(req.Remote)
	if err != nil {
		var ae *admit.Error
		if errors.As(err, &ae) {
			// Name the rule: a generic 400 tells an operator nothing about
			// what to change. The counter carries the same rule, so
			// codetrail_admission_rejected_total answers the same question a
			// reader of one 400 gets.
			metrics.CountAdmission(false, string(ae.Rule))
			return c.JSON(http.StatusBadRequest, echo.Map{"error": ae.Detail, "rule": string(ae.Rule)})
		}
		metrics.CountAdmission(false, string(admit.RuleForm))
		return c.JSON(http.StatusBadRequest, echo.Map{"error": err.Error(), "rule": string(admit.RuleForm)})
	}
	ref := strings.TrimSpace(req.Ref)
	if ref == "" {
		ref = "HEAD"
	}
	if !validRef(ref) {
		metrics.CountAdmission(false, string(admit.RuleForm))
		return c.JSON(http.StatusBadRequest, echo.Map{
			"error": "ref must be a plain git ref name",
			"rule":  string(admit.RuleForm),
		})
	}
	// Counted here rather than after Enqueue, because what this counts is the
	// admission decision and not the request's outcome: a queue write that
	// fails is a 500 the ratio's denominator must still contain.
	//
	// Counted here rather than in the indexer, which re-checks the same
	// allowlist before it clones: counting both would double every submission,
	// and that re-check failing is a job failure with a reason of its own.
	metrics.CountAdmission(true, "")
	// The normalised URL, not the caller's spelling: the dedupe index would
	// otherwise see a trailing slash and a .git suffix as separate
	// repositories. The index folds case on top of that, which is the one
	// spelling difference normalising here must not erase — the forge decides
	// the display case of an owner and a name.
	job, err := h.Jobs.Enqueue(c.Request().Context(), remote.URL, ref)
	if err != nil {
		return h.fail(c, err, "enqueue")
	}
	return c.JSON(http.StatusAccepted, view(job))
}

func (h *Handler) getJob(c echo.Context) error {
	job, err := h.Jobs.Get(c.Request().Context(), c.Param("id"))
	if errors.Is(err, jobs.ErrNotFound) {
		return c.JSON(http.StatusNotFound, echo.Map{"error": "no such job"})
	}
	if err != nil {
		return h.fail(c, err, "read job")
	}
	return c.JSON(http.StatusOK, view(job))
}

// fail answers a 500 without the underlying error. A pgx message names the
// connection host, the database and the constraint, and this endpoint takes
// URLs from strangers. The operator reads the real error from the log; the
// request id is what ties a caller's report to that line.
func (h *Handler) fail(c echo.Context, err error, op string) error {
	rid := requestID(c)
	h.Log.Error().Err(err).Str("request_id", rid).Str("op", op).Msg("request failed")
	return c.JSON(http.StatusInternalServerError, echo.Map{"error": "internal error", "request_id": rid})
}

// requestID is the id echo's RequestID middleware stamped on the response, and
// what a caller quotes when reporting a failure.
func requestID(c echo.Context) string {
	return c.Response().Header().Get(echo.HeaderXRequestID)
}
