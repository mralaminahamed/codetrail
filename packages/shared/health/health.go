// Package health mounts liveness, readiness, and metrics endpoints.
package health

import (
	"net/http"

	"github.com/labstack/echo/v4"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

func Register(e *echo.Echo, ready func() bool) {
	e.GET("/health", func(c echo.Context) error { return c.NoContent(http.StatusOK) })
	e.GET("/ready", func(c echo.Context) error {
		if ready == nil || ready() {
			return c.NoContent(http.StatusOK)
		}
		return c.NoContent(http.StatusServiceUnavailable)
	})
	e.GET("/metrics", echo.WrapHandler(promhttp.Handler()))
}
