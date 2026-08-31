package handler

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/labstack/echo/v4"
	"github.com/labstack/echo/v4/middleware"
	"github.com/rs/zerolog"

	"github.com/mralaminahamed/codetrail/packages/shared/admit"
	"github.com/mralaminahamed/codetrail/packages/shared/jobs"
)

type fakeQueue struct {
	enqueued []string
	job      jobs.Job
	err      error
}

func (f *fakeQueue) Enqueue(_ context.Context, remote, ref string) (jobs.Job, error) {
	if f.err != nil {
		return jobs.Job{}, f.err
	}
	f.enqueued = append(f.enqueued, remote+"@"+ref)
	return jobs.Job{ID: "job-1", Remote: remote, Ref: ref, Status: jobs.StatusPending}, nil
}

func (f *fakeQueue) Get(_ context.Context, id string) (jobs.Job, error) {
	if f.err != nil {
		return jobs.Job{}, f.err
	}
	if id != f.job.ID {
		return jobs.Job{}, jobs.ErrNotFound
	}
	return f.job, nil
}

func router(q *fakeQueue) *echo.Echo {
	e := echo.New()
	Mount(e, &Handler{Policy: admit.NewPolicy(admit.DefaultHosts), Jobs: q})
	return e
}

func do(e *echo.Echo, method, path, body string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	req.Header.Set("content-type", "application/json")
	rec := httptest.NewRecorder()
	e.ServeHTTP(rec, req)
	return rec
}

func TestSubmitEnqueuesTheNormalisedRemote(t *testing.T) {
	q := &fakeQueue{}
	rec := do(router(q), http.MethodPost, "/api/repos", `{"remote":"https://GitHub.com/Owner/Repo.git"}`)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("want 202, got %d: %s", rec.Code, rec.Body)
	}
	// The queue must see the normalised form, or the dedupe index sees three
	// spellings of one repository as three repositories.
	if len(q.enqueued) != 1 || q.enqueued[0] != "https://github.com/Owner/Repo@HEAD" {
		t.Fatalf("enqueued %v", q.enqueued)
	}
	var got jobs.Job
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if got.ID != "job-1" || got.Status != jobs.StatusPending {
		t.Fatalf("body %+v", got)
	}
}

// A rejection has to say which rule fired. "Bad request" tells an operator
// nothing about what to change.
func TestSubmitRejectionNamesTheRule(t *testing.T) {
	for body, wantRule := range map[string]string{
		`{"remote":"file:///etc/passwd"}`:           "scheme",
		`{"remote":"https://169.254.169.254/a/b"}`:  "host",
		`{"remote":"https://github.com/onlyowner"}`: "form",
	} {
		q := &fakeQueue{}
		rec := do(router(q), http.MethodPost, "/api/repos", body)
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("%s: want 400, got %d", body, rec.Code)
		}
		var out struct {
			Error string `json:"error"`
			Rule  string `json:"rule"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
			t.Fatal(err)
		}
		if out.Rule != wantRule {
			t.Fatalf("%s: want rule %q, got %q", body, wantRule, out.Rule)
		}
		if len(q.enqueued) != 0 {
			t.Fatalf("%s: a rejected URL must not be enqueued", body)
		}
	}
}

func TestSubmitRequiresARemote(t *testing.T) {
	rec := do(router(&fakeQueue{}), http.MethodPost, "/api/repos", `{}`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("want 400, got %d", rec.Code)
	}
}

// A body that is not JSON is the caller's mistake, not the queue's, and the
// rule field has to be populated on this path too or a client that reads it
// unconditionally sees an empty string it cannot act on.
func TestSubmitMalformedBodyIs400(t *testing.T) {
	q := &fakeQueue{}
	rec := do(router(q), http.MethodPost, "/api/repos", `{"remote":`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("want 400, got %d: %s", rec.Code, rec.Body)
	}
	var out struct {
		Rule string `json:"rule"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	if out.Rule != string(admit.RuleForm) {
		t.Fatalf("want rule %q, got %q", admit.RuleForm, out.Rule)
	}
	if len(q.enqueued) != 0 {
		t.Fatalf("enqueued %v", q.enqueued)
	}
}

// The ref the caller asks for has to reach the queue, and a blank one has to
// become HEAD: enqueueing "  " would clone a ref no forge has.
func TestSubmitHonoursTheRefAndDefaultsABlankOne(t *testing.T) {
	for body, want := range map[string]string{
		`{"remote":"https://github.com/a/b","ref":"v1.2.3"}`: "https://github.com/a/b@v1.2.3",
		`{"remote":"https://github.com/a/b","ref":"  "}`:     "https://github.com/a/b@HEAD",
	} {
		q := &fakeQueue{}
		rec := do(router(q), http.MethodPost, "/api/repos", body)
		if rec.Code != http.StatusAccepted {
			t.Fatalf("%s: want 202, got %d: %s", body, rec.Code, rec.Body)
		}
		if len(q.enqueued) != 1 || q.enqueued[0] != want {
			t.Fatalf("%s: want %q, enqueued %v", body, want, q.enqueued)
		}
	}
}

func TestGetJob(t *testing.T) {
	q := &fakeQueue{job: jobs.Job{ID: "job-1", Status: jobs.StatusDone}}
	rec := do(router(q), http.MethodGet, "/api/jobs/job-1", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("want 200, got %d", rec.Code)
	}
	// The poller reads the status out of the body, so a 200 carrying anything
	// else is a 200 that answers nothing.
	var got jobs.Job
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if got.ID != "job-1" || got.Status != jobs.StatusDone {
		t.Fatalf("body %+v", got)
	}
}

func TestGetUnknownJobIs404(t *testing.T) {
	q := &fakeQueue{job: jobs.Job{ID: "other"}}
	rec := do(router(q), http.MethodGet, "/api/jobs/job-1", "")
	if rec.Code != http.StatusNotFound {
		t.Fatalf("want 404, got %d", rec.Code)
	}
}

func TestQueueFailureIs500NotABadRequest(t *testing.T) {
	q := &fakeQueue{err: errors.New("postgres is down")}
	rec := do(router(q), http.MethodPost, "/api/repos", `{"remote":"https://github.com/a/b"}`)
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("want 500 — the caller's URL was fine — got %d", rec.Code)
	}
}

// A read failure that is not ErrNotFound is the database's, not the caller's:
// answering 404 would tell a poller its job vanished.
func TestGetJobQueueFailureIs500(t *testing.T) {
	q := &fakeQueue{err: errors.New("postgres is down")}
	rec := do(router(q), http.MethodGet, "/api/jobs/job-1", "")
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("want 500, got %d", rec.Code)
	}
}

// Mount takes middleware so a caller can put auth or rate limiting in front of
// the API. Middleware that is accepted and dropped is worse than none.
func TestMountAppliesTheMiddlewareItIsGiven(t *testing.T) {
	e := echo.New()
	Mount(e, &Handler{Policy: admit.NewPolicy(admit.DefaultHosts), Jobs: &fakeQueue{}},
		func(echo.HandlerFunc) echo.HandlerFunc {
			return func(c echo.Context) error { return c.NoContent(http.StatusTeapot) }
		})
	for _, tc := range []struct{ method, path, body string }{
		{http.MethodPost, "/api/repos", `{"remote":"https://github.com/a/b"}`},
		{http.MethodGet, "/api/jobs/job-1", ""},
	} {
		if rec := do(e, tc.method, tc.path, tc.body); rec.Code != http.StatusTeapot {
			t.Errorf("%s %s: middleware did not run, got %d", tc.method, tc.path, rec.Code)
		}
	}
}

// A 500 must not hand the caller the database's own words. A pgx error carries
// the connection host, the database name, and constraint names, and this
// endpoint takes URLs from strangers. The operator gets the real error from the
// log, correlated by the request id the caller is given.
func TestServerErrorDoesNotLeakTheUnderlyingError(t *testing.T) {
	const marker = "pgx: host=secret.internal dbname=codetrail user=codetrail"
	for _, tc := range []struct{ name, method, path, body string }{
		{"submit", http.MethodPost, "/api/repos", `{"remote":"https://github.com/a/b"}`},
		{"poll", http.MethodGet, "/api/jobs/job-1", ""},
	} {
		var logged bytes.Buffer
		h := &Handler{
			Policy: admit.NewPolicy(admit.DefaultHosts),
			Jobs:   &fakeQueue{err: errors.New(marker)},
			Log:    zerolog.New(&logged),
		}
		e := echo.New()
		Mount(e, h, middleware.RequestID())
		rec := do(e, tc.method, tc.path, tc.body)

		if rec.Code != http.StatusInternalServerError {
			t.Fatalf("%s: want 500, got %d", tc.name, rec.Code)
		}
		body := rec.Body.String()
		for _, leak := range []string{marker, "secret.internal", "dbname=", "host="} {
			if strings.Contains(body, leak) {
				t.Errorf("%s: 500 body leaks %q: %s", tc.name, leak, body)
			}
		}
		var out struct {
			Error     string `json:"error"`
			RequestID string `json:"request_id"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
			t.Fatalf("%s: %v", tc.name, err)
		}
		if out.RequestID == "" || out.RequestID != rec.Header().Get(echo.HeaderXRequestID) {
			t.Errorf("%s: request_id %q, header %q", tc.name, out.RequestID, rec.Header().Get(echo.HeaderXRequestID))
		}
		// Withheld from the caller, not from the operator, and correlatable.
		if !strings.Contains(logged.String(), marker) || !strings.Contains(logged.String(), out.RequestID) {
			t.Errorf("%s: log line does not carry the error and the request id: %s", tc.name, logged.String())
		}
	}
}
