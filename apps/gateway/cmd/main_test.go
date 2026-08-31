package main

import (
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"

	"github.com/mralaminahamed/codetrail/apps/gateway/internal/handler"
	"github.com/mralaminahamed/codetrail/packages/shared/admit"
)

// The router has to carry both the probes and the API. Mounting one without
// the other boots a binary that passes its health check and serves nothing
// anyone asked for, which no handler test can catch.
func TestNewRouterMountsHealthAndTheAPI(t *testing.T) {
	e := newRouter(func() bool { return true }, &handler.Handler{Policy: admit.NewPolicy(admit.DefaultHosts)})

	// A refusal is answered before the queue is touched, so the API proves
	// reachable here without a database behind it.
	for _, tc := range []struct {
		method, path, body string
		want               int
	}{
		{http.MethodGet, "/health", "", http.StatusOK},
		{http.MethodGet, "/ready", "", http.StatusOK},
		{http.MethodPost, "/api/repos", `{"remote":"file:///etc/passwd"}`, http.StatusBadRequest},
	} {
		req := httptest.NewRequest(tc.method, tc.path, strings.NewReader(tc.body))
		req.Header.Set("content-type", "application/json")
		rec := httptest.NewRecorder()
		e.ServeHTTP(rec, req)
		if rec.Code != tc.want {
			t.Errorf("%s %s: want %d, got %d", tc.method, tc.path, tc.want, rec.Code)
		}
	}

	var mounted bool
	for _, r := range e.Routes() {
		mounted = mounted || (r.Method == http.MethodGet && r.Path == "/api/jobs/:id")
	}
	if !mounted {
		t.Error("GET /api/jobs/:id is not mounted")
	}
}

// ALLOWED_HOSTS is where an operator's list becomes a policy, and the natural
// way to write a list — with a space after the comma — must not silently
// produce a host nothing matches.
func TestAllowedHostsDefaultsAndSurvivesSpaces(t *testing.T) {
	if got := allowedHosts(); !slices.Equal(got, admit.DefaultHosts) {
		t.Errorf("unset: want %v, got %v", admit.DefaultHosts, got)
	}
	t.Setenv("ALLOWED_HOSTS", "github.com, example.test")
	p := admit.NewPolicy(allowedHosts())
	for _, raw := range []string{"https://github.com/a/b", "https://example.test/a/b"} {
		if _, err := p.Check(raw); err != nil {
			t.Errorf("%s: %v", raw, err)
		}
	}
	if _, err := p.Check("https://codeberg.org/a/b"); err == nil {
		t.Error("a configured list must replace the default, not extend it")
	}
}
