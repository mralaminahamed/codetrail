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

func (h *Handler) postRepo(c echo.Context) error {
	var req repoRequest
	if err := c.Bind(&req); err != nil {
		return c.JSON(http.StatusBadRequest, echo.Map{"error": err.Error(), "rule": string(admit.RuleForm)})
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
	// The normalised URL, not the caller's spelling: the dedupe index would
	// otherwise see three spellings of one repository as three repositories.
	job, err := h.Jobs.Enqueue(c.Request().Context(), remote.URL, ref)
	if err != nil {
		return h.fail(c, err, "enqueue")
	}
	return c.JSON(http.StatusAccepted, job)
}

func (h *Handler) getJob(c echo.Context) error {
	job, err := h.Jobs.Get(c.Request().Context(), c.Param("id"))
	if errors.Is(err, jobs.ErrNotFound) {
		return c.JSON(http.StatusNotFound, echo.Map{"error": "no such job"})
	}
	if err != nil {
		return h.fail(c, err, "read job")
	}
	return c.JSON(http.StatusOK, job)
}

// fail answers a 500 without the underlying error. A pgx message names the
// connection host, the database and the constraint, and this endpoint takes
// URLs from strangers. The operator reads the real error from the log; the
// request id is what ties a caller's report to that line.
func (h *Handler) fail(c echo.Context, err error, op string) error {
	rid := c.Response().Header().Get(echo.HeaderXRequestID)
	h.Log.Error().Err(err).Str("request_id", rid).Str("op", op).Msg("request failed")
	return c.JSON(http.StatusInternalServerError, echo.Map{"error": "internal error", "request_id": rid})
}
