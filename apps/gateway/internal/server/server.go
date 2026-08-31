// Package server builds the gateway HTTP router.
package server

import (
	"github.com/labstack/echo/v4"
	"github.com/labstack/echo/v4/middleware"

	"github.com/mralaminahamed/codetrail/packages/shared/health"
)

// ServiceName identifies the gateway in logs and traces.
const ServiceName = "codetrail-gateway"

func New(ready func() bool) *echo.Echo {
	e := echo.New()
	e.HideBanner = true
	e.Use(middleware.Recover())
	e.Use(middleware.RequestID())
	e.Use(middleware.BodyLimit("2M"))
	health.Register(e, ready)
	return e
}
