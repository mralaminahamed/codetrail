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
	// A duplicate Enqueue returns the existing active job, which may already
	// carry attempts and the text of a failed one.
	return jobs.Job{ID: "job-1", Remote: remote, Ref: ref, Status: jobs.StatusPending,
		Attempts: f.job.Attempts, Error: f.job.Error}, nil
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
		Error string `json:"error"`
		Rule  string `json:"rule"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	if out.Rule != string(admit.RuleForm) {
		t.Fatalf("want rule %q, got %q", admit.RuleForm, out.Rule)
	}
	// echo's HTTPError stringifies with the bound Go type appended, which names
	// this package rather than anything the caller sent. A fixed message is the
	// only body that cannot be made to carry server state.
	if out.Error != "malformed request body" {
		t.Fatalf("want a fixed message, got %q", out.Error)
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
			// logger.New pins production to InfoLevel; a test logger that
			// accepts Debug would pass on a line production never writes.
			Log: zerolog.New(&logged).Level(zerolog.InfoLevel),
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
		var line struct {
			Error     string `json:"error"`
			RequestID string `json:"request_id"`
		}
		if err := json.Unmarshal([]byte(logged.String()), &line); err != nil {
			t.Fatalf("%s: no log line (%v): %s", tc.name, err, logged.String())
		}
		if line.Error != marker || line.RequestID != out.RequestID {
			t.Errorf("%s: log carries error %q, request_id %q; want %q and %q",
				tc.name, line.Error, line.RequestID, marker, out.RequestID)
		}
	}
}

// Important 2: jobs.Job is the internal row. attempts is scheduling detail,
// and error is written from the indexer's git stderr — a filesystem path and a
// credential-bearing URL have both been seen in it. Serving it to an anonymous
// poller would reopen on a 200 what the 500 path closes.
func TestNeitherResponseServesInternalJobFields(t *testing.T) {
	const leak = "clone failed: /srv/codetrail/work/tmp42: https://x-token:ghp_secret@github.com/a/b"
	q := &fakeQueue{job: jobs.Job{
		ID: "job-1", Remote: "https://github.com/a/b", Ref: "HEAD",
		Status: jobs.StatusPending, Attempts: 3, Error: leak,
	}}
	for _, tc := range []struct {
		name, method, path, body string
		want                     int
	}{
		{"submit", http.MethodPost, "/api/repos", `{"remote":"https://github.com/a/b"}`, http.StatusAccepted},
		{"poll", http.MethodGet, "/api/jobs/job-1", "", http.StatusOK},
	} {
		rec := do(router(q), tc.method, tc.path, tc.body)
		if rec.Code != tc.want {
			t.Fatalf("%s: want %d, got %d: %s", tc.name, tc.want, rec.Code, rec.Body)
		}
		var out map[string]any
		if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
			t.Fatal(err)
		}
		for _, internal := range []string{"error", "attempts"} {
			if _, ok := out[internal]; ok {
				t.Errorf("%s: body serves %q: %s", tc.name, internal, rec.Body)
			}
		}
		for _, leaked := range []string{"/srv/", "ghp_secret", "@github.com"} {
			if strings.Contains(rec.Body.String(), leaked) {
				t.Errorf("%s: body leaks %q: %s", tc.name, leaked, rec.Body)
			}
		}
		// What a poller does need is still there.
		for _, want := range []string{"id", "remote", "ref", "status"} {
			if v, ok := out[want].(string); !ok || v == "" {
				t.Errorf("%s: body is missing %q: %s", tc.name, want, rec.Body)
			}
		}
	}
}

// Important 3: ref is handed to git by the indexer. The front door is where an
// argument stops being possible, not the subprocess call site.
func TestSubmitRefusesARefGitShouldNeverSee(t *testing.T) {
	for _, ref := range []string{
		"--upload-pack=touch /tmp/pwn",
		"-x",
		"a..b",
		"refs/heads/x;rm -rf /",
		"a b",
		"a\nb",
		"tag^{}",
		strings.Repeat("a", 256),
	} {
		body, err := json.Marshal(map[string]string{"remote": "https://github.com/a/b", "ref": ref})
		if err != nil {
			t.Fatal(err)
		}
		q := &fakeQueue{}
		rec := do(router(q), http.MethodPost, "/api/repos", string(body))
		if rec.Code != http.StatusBadRequest {
			t.Errorf("ref %q: want 400, got %d: %s", ref, rec.Code, rec.Body)
		}
		var out struct {
			Rule string `json:"rule"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
			t.Fatal(err)
		}
		if out.Rule != string(admit.RuleForm) {
			t.Errorf("ref %q: want rule %q, got %q", ref, admit.RuleForm, out.Rule)
		}
		if len(q.enqueued) != 0 {
			t.Errorf("ref %q: refused but enqueued %v", ref, q.enqueued)
		}
	}
}

func TestSubmitAcceptsAnOrdinaryRef(t *testing.T) {
	for _, ref := range []string{"HEAD", "main", "v1.2.3", "refs/heads/feature/x", "release_1", "a.b-c"} {
		body, err := json.Marshal(map[string]string{"remote": "https://github.com/a/b", "ref": ref})
		if err != nil {
			t.Fatal(err)
		}
		q := &fakeQueue{}
		rec := do(router(q), http.MethodPost, "/api/repos", string(body))
		if rec.Code != http.StatusAccepted {
			t.Errorf("ref %q: want 202, got %d: %s", ref, rec.Code, rec.Body)
		}
		if len(q.enqueued) != 1 || q.enqueued[0] != "https://github.com/a/b@"+ref {
			t.Errorf("ref %q: enqueued %v", ref, q.enqueued)
		}
	}
}
