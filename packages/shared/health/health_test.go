package health

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/labstack/echo/v4"
)

func TestHealthAlwaysOK(t *testing.T) {
	e := echo.New()
	Register(e, func() bool { return false })
	req := httptest.NewRequest(http.MethodGet, "/health", nil)
	rec := httptest.NewRecorder()
	e.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("health want 200, got %d", rec.Code)
	}
}

func TestReadyReflectsProbe(t *testing.T) {
	e := echo.New()
	ready := false
	Register(e, func() bool { return ready })
	do := func() int {
		req := httptest.NewRequest(http.MethodGet, "/ready", nil)
		rec := httptest.NewRecorder()
		e.ServeHTTP(rec, req)
		return rec.Code
	}
	if do() != http.StatusServiceUnavailable {
		t.Fatal("ready should be 503 when not ready")
	}
	ready = true
	if do() != http.StatusOK {
		t.Fatal("ready should be 200 when ready")
	}
}
