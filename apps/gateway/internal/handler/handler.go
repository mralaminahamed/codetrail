// Package handler implements the gateway's REST API.
package handler

import (
	"context"
	"errors"
	"net/http"
	"strings"

	"github.com/labstack/echo/v4"
	"github.com/rs/zerolog"

	"github.com/mralaminahamed/codetrail/packages/shared/admit"
	"github.com/mralaminahamed/codetrail/packages/shared/jobs"
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
	Log    zerolog.Logger
}

func Mount(e *echo.Echo, h *Handler, mw ...echo.MiddlewareFunc) {
	g := e.Group("/api", mw...)
	g.POST("/repos", h.postRepo)
	g.GET("/jobs/:id", h.getJob)
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
type jobView struct {
	ID     string      `json:"id"`
	Remote string      `json:"remote"`
	Ref    string      `json:"ref"`
	Status jobs.Status `json:"status"`
}

func view(j jobs.Job) jobView {
	return jobView{ID: j.ID, Remote: j.Remote, Ref: j.Ref, Status: j.Status}
}

// maxRefLen bounds what reaches git's argv. Git's own limit is the filesystem's;
// this is short enough to be obviously safe and long enough for any real ref.
const maxRefLen = 255

// validRef allowlists the charset a git ref needs, the way admit.validSegment
// does for a path segment, and refuses "." and ".." as that does. The indexer
// hands ref to a subprocess, so a leading "-" is an option and not a name, and
// ".." is forbidden by git's own ref syntax anyway. Bound it at the front door
// rather than at a call site that is not written yet.
//
// It diverges from validSegment by allowing "/", which a ref needs, and by
// capping the length, which argv makes worth doing; it refuses ".." anywhere
// rather than only as the whole string, which is stricter.
//
// It is a bound on what reaches argv, not a model of git's ref syntax, and it
// is looser than git in several ways: it accepts a leading or trailing "/", a
// doubled "//", a ".lock" suffix, a component starting with "." (".foo",
// "a/.b") and a ref ending with "." ("foo.", "v1."). Git rejects all of them,
// so they fail the clone rather than doing anything, and none can be read as an
// option. The trailing-dot rule is the refname's, not each component's:
// "foo./bar" is legal to git and accepted here. Task 7 must still run git
// check-ref-format; this does not make that redundant.
//
// In one direction it is narrower than git: "+" is legal in a tag, so a semver
// build-metadata tag is refused here. Widening that is a decision for when
// Task 7 exists and the argv path is real.
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
		return c.JSON(http.StatusBadRequest, echo.Map{"error": "malformed request body", "rule": string(admit.RuleForm)})
	}
	remote, err := h.Policy.Check(req.Remote)
	if err != nil {
		var ae *admit.Error
		if errors.As(err, &ae) {
			// Name the rule: a generic 400 tells an operator nothing about
			// what to change.
			return c.JSON(http.StatusBadRequest, echo.Map{"error": ae.Detail, "rule": string(ae.Rule)})
		}
		return c.JSON(http.StatusBadRequest, echo.Map{"error": err.Error(), "rule": string(admit.RuleForm)})
	}
	ref := strings.TrimSpace(req.Ref)
	if ref == "" {
		ref = "HEAD"
	}
	if !validRef(ref) {
		return c.JSON(http.StatusBadRequest, echo.Map{
			"error": "ref must be a plain git ref name",
			"rule":  string(admit.RuleForm),
		})
	}
	// The normalised URL, not the caller's spelling: the dedupe index would
	// otherwise see three spellings of one repository as three repositories.
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
