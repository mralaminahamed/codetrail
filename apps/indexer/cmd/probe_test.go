package main

import (
	"context"
	"errors"
	"io"
	"maps"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	"github.com/prometheus/client_golang/prometheus/promhttp"

	"github.com/mralaminahamed/codetrail/apps/indexer/internal/clone"
	"github.com/mralaminahamed/codetrail/packages/shared/metrics"
	"github.com/mralaminahamed/codetrail/packages/shared/symbols"
)

func okPing(context.Context) error { return nil }

// serve runs one request against a worker's probe surface.
func serve(t *testing.T, ix *indexer, ping func(context.Context) error, path string) (int, string) {
	t.Helper()
	srv := httptest.NewServer(ix.probeServer(ping))
	defer srv.Close()
	resp, err := http.Get(srv.URL + path)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	return resp.StatusCode, string(body)
}

func TestTheProbeServerAnswersAllThreeEndpoints(t *testing.T) {
	ix, _ := testIndexer(t, &fakeQueue{})
	for _, path := range []string{"/health", "/ready", "/metrics"} {
		if code, _ := serve(t, ix, okPing, path); code != http.StatusOK {
			t.Errorf("%s answered %d, want 200", path, code)
		}
	}
	// The point of the endpoint, not the endpoint itself: P4's three instruments
	// have had nowhere to be scraped from since they were written.
	_, body := serve(t, ix, okPing, "/metrics")
	for _, series := range []string{
		"codetrail_graph_edges_total", "codetrail_typecheck_total", "codetrail_typecheck_seconds_count",
	} {
		if !strings.Contains(body, series) {
			t.Errorf("%s is not on the indexer's /metrics", series)
		}
	}
}

func TestTheIndexerIsReadyWhenPostgresIsAndNotWhenItIsNot(t *testing.T) {
	ix, _ := testIndexer(t, &fakeQueue{})
	if code, _ := serve(t, ix, okPing, "/ready"); code != http.StatusOK {
		t.Errorf("/ready answered %d with Postgres reachable, want 200", code)
	}
	down := func(context.Context) error { return errors.New("postgres is gone") }
	if code, _ := serve(t, ix, down, "/ready"); code != http.StatusServiceUnavailable {
		t.Errorf("/ready answered %d with Postgres gone, want 503", code)
	}
	// And /health does not follow it: an orchestrator that restarted the worker
	// on a shared-datastore outage would turn one outage into a crash loop.
	if code, _ := serve(t, ix, down, "/health"); code != http.StatusOK {
		t.Errorf("/health answered %d with Postgres gone, want 200", code)
	}
}

// graph.go:241-250 makes a missing toolchain a downgrade by design, and
// TypecheckHasNoToolchain is the alert for it. "The indexer cannot type-check,
// mark it unready" is the change a reasonable reviewer would ask for, and it
// would turn a documented degradation into an outage.
func TestAMissingGoToolchainDoesNotMakeTheIndexerUnready(t *testing.T) {
	ix, _ := testIndexer(t, &fakeQueue{})
	ix.lim.goBin, ix.lim.typecheck = "", true
	if code, _ := serve(t, ix, okPing, "/ready"); code != http.StatusOK {
		t.Fatalf("/ready answered %d with no go on PATH, want 200", code)
	}
}

// jobSeries is every series an alert over job outcomes or admission reads,
// scraped whole. Whole, because a mutant that increments a neighbouring label
// passes an assertion on the one series a test expected to move.
func jobSeries(t *testing.T) map[string]float64 {
	t.Helper()
	srv := httptest.NewServer(promhttp.Handler())
	defer srv.Close()
	resp, err := http.Get(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]float64{}
	for _, line := range strings.Split(string(raw), "\n") {
		switch {
		case strings.HasPrefix(line, "codetrail_job_total{"),
			strings.HasPrefix(line, "codetrail_job_seconds_count{"),
			strings.HasPrefix(line, "codetrail_admission_total{"),
			strings.HasPrefix(line, "codetrail_admission_rejected_total{"),
			strings.HasPrefix(line, "codetrail_evicted_total "):
		default:
			continue
		}
		i := strings.LastIndex(line, " ")
		v, err := strconv.ParseFloat(line[i+1:], 64)
		if err != nil {
			t.Fatalf("metric %q: %v", line, err)
		}
		out[line[:i]] = v
	}
	// 3 outcomes x 2 job instruments + 2 admission outcomes + 3 rules + evicted.
	if want := len(metrics.JobOutcomes)*2 + 2 + len(metrics.AdmissionRules) + 1; len(out) != want {
		t.Fatalf("read %d job series, want %d; every assertion below would be vacuous", len(out), want)
	}
	return out
}

func movedJobSeries(t *testing.T, before map[string]float64) map[string]float64 {
	t.Helper()
	out := map[string]float64{}
	for series, now := range jobSeries(t) {
		if d := now - before[series]; d != 0 {
			out[series] = d
		}
	}
	return out
}

func TestEveryJobOutcomeMovesExactlyOneCounter(t *testing.T) {
	// tries is 4 in testIndexer. jobs.Fail decides terminality in SQL with
	// `attempts >= maxAttempts`, so the boundary is 3 against 4 and the two
	// cases below sit on either side of it — a pair one apart, because a
	// mutant that moves the comparison by one survives any wider gap.
	cases := []struct {
		name string
		run  func(t *testing.T)
		want map[string]float64
	}{
		{
			name: "a job that completes",
			run: func(t *testing.T) {
				ix, _ := fakeIndexer(t, withResolver(nil, symbols.Stats{Reason: symbols.ReasonOK}))
				ix.runJob(context.Background(), aJob())
			},
			want: map[string]float64{
				`codetrail_job_total{outcome="done"}`:         1,
				`codetrail_job_seconds_count{outcome="done"}`: 1,
			},
		},
		{
			name: "a job on the last attempt that still retries",
			run: func(t *testing.T) {
				ix, _ := fakeIndexer(t)
				ix.clone = failingClone
				job := aJob()
				job.Attempts = 3
				ix.runJob(context.Background(), job)
			},
			want: map[string]float64{
				`codetrail_job_total{outcome="retried"}`:         1,
				`codetrail_job_seconds_count{outcome="retried"}`: 1,
			},
		},
		{
			name: "a job one attempt further, which jobs.Fail marks terminal",
			run: func(t *testing.T) {
				ix, _ := fakeIndexer(t)
				ix.clone = failingClone
				job := aJob()
				job.Attempts = 4
				ix.runJob(context.Background(), job)
			},
			want: map[string]float64{
				`codetrail_job_total{outcome="failed"}`:         1,
				`codetrail_job_seconds_count{outcome="failed"}`: 1,
			},
		},
		{
			// The indexer re-checks the same allowlist the gateway admitted
			// against. Counting it here as well would double every submission,
			// so the whole-set assertion is what says it did not.
			name: "a remote the allowlist refuses",
			run: func(t *testing.T) {
				ix, _ := fakeIndexer(t)
				job := aJob()
				job.Remote = "https://gitlab.com/a/b"
				job.Attempts = 1
				ix.runJob(context.Background(), job)
			},
			want: map[string]float64{
				`codetrail_job_total{outcome="retried"}`:         1,
				`codetrail_job_seconds_count{outcome="retried"}`: 1,
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			before := jobSeries(t)
			tc.run(t)
			if got := movedJobSeries(t, before); !maps.Equal(got, tc.want) {
				t.Fatalf("moved %v, want exactly %v", got, tc.want)
			}
		})
	}
}

// §11 says "evictions", and a sweep that removed forty repositories and one
// that removed none are the same event and very different facts.
func TestAnEvictionSweepCountsRowsAndNotSweeps(t *testing.T) {
	ix, _ := fakeIndexer(t, withResolver(nil, symbols.Stats{Reason: symbols.ReasonOK}))
	ix.evict = func(context.Context, int, int) (int, error) { return 40, nil }

	before := jobSeries(t)
	ix.runJob(context.Background(), aJob())
	if got, want := movedJobSeries(t, before), map[string]float64{
		`codetrail_job_total{outcome="done"}`:         1,
		`codetrail_job_seconds_count{outcome="done"}`: 1,
		"codetrail_evicted_total":                     40,
	}; !maps.Equal(got, want) {
		t.Fatalf("moved %v, want exactly %v", got, want)
	}
}

func failingClone(context.Context, string, string, string, clone.Limits) (clone.Result, error) {
	return clone.Result{}, errors.New("clone refused")
}
