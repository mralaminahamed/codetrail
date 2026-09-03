package metrics_test

import (
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/promhttp"

	"github.com/mralaminahamed/codetrail/packages/shared/admit"
	"github.com/mralaminahamed/codetrail/packages/shared/metrics"
)

// scrape reads this process's own metrics the way Prometheus does, so what is
// asserted is what an operator sees. Keyed by the whole series line up to the
// value, so a label that moved is a different key rather than a silent match.
func scrape(t *testing.T) map[string]float64 {
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
		if line == "" || strings.HasPrefix(line, "#") || !strings.HasPrefix(line, "codetrail_") {
			continue
		}
		i := strings.LastIndex(line, " ")
		if i < 0 {
			t.Fatalf("unparseable metric line %q", line)
		}
		v, err := strconv.ParseFloat(line[i+1:], 64)
		if err != nil {
			t.Fatalf("metric %q: %v", line, err)
		}
		out[line[:i]] = v
	}
	return out
}

// moved returns only the series that changed, so an assertion names the whole
// movement rather than the one series it expected. A mutant that increments a
// neighbouring label passes an assertion on the right one.
func moved(t *testing.T, before map[string]float64) map[string]float64 {
	t.Helper()
	out := map[string]float64{}
	for series, now := range scrape(t) {
		if d := now - before[series]; d != 0 {
			out[series] = d
		}
	}
	return out
}

func TestTheAdmissionRuleLabelsAreExactlyAdmitsRules(t *testing.T) {
	want := []string{string(admit.RuleForm), string(admit.RuleScheme), string(admit.RuleHost)}
	if !slices.Equal(metrics.AdmissionRules, want) {
		t.Fatalf("AdmissionRules is %v, want admit's own vocabulary %v", metrics.AdmissionRules, want)
	}
}

// rate() over an absent series is nothing at all, so every alert in
// infra/prometheus/alerts.yml that divides one of these by another needs both
// to exist before the first request.
func TestEveryNewSeriesExistsBeforeTheFirstEvent(t *testing.T) {
	have := scrape(t)
	var want []string
	for _, outcome := range metrics.JobOutcomes {
		want = append(want,
			`codetrail_job_total{outcome="`+outcome+`"}`,
			`codetrail_job_seconds_count{outcome="`+outcome+`"}`)
	}
	for _, outcome := range []string{"accepted", "rejected"} {
		want = append(want, `codetrail_admission_total{outcome="`+outcome+`"}`)
	}
	for _, rule := range metrics.AdmissionRules {
		want = append(want, `codetrail_admission_rejected_total{rule="`+rule+`"}`)
	}
	want = append(want, "codetrail_evicted_total")
	for _, series := range want {
		if _, ok := have[series]; !ok {
			t.Errorf("%s is absent before the first event", series)
		}
	}
	// The one series deliberately not seeded: its labels are not known until a
	// connection exists.
	for series := range have {
		if strings.HasPrefix(series, "codetrail_datastore_info") {
			t.Errorf("%s exists before anything read a server version", series)
		}
	}
}

func TestAJobsDurationIsRecordedUnderItsOwnOutcome(t *testing.T) {
	before := scrape(t)
	metrics.CountJob(metrics.JobFailed, 90*time.Second)
	got := moved(t, before)

	want := map[string]float64{
		`codetrail_job_total{outcome="failed"}`:         1,
		`codetrail_job_seconds_count{outcome="failed"}`: 1,
		`codetrail_job_seconds_sum{outcome="failed"}`:   90,
	}
	for series, delta := range want {
		if got[series] != delta {
			t.Errorf("%s moved by %v, want %v", series, got[series], delta)
		}
		delete(got, series)
	}
	// 90s lands in the 128s bucket and every wider one; the buckets below it
	// must not have moved, which is what "under its own outcome" means for a
	// histogram.
	for series, delta := range got {
		if !strings.HasPrefix(series, `codetrail_job_seconds_bucket{`) {
			t.Errorf("%s moved by %v and nothing about this job should have touched it", series, delta)
			continue
		}
		if !strings.Contains(series, `outcome="failed"`) {
			t.Errorf("%s moved, but the job's outcome was failed", series)
		}
		_, after, _ := strings.Cut(series, `le="`)
		le, _, _ := strings.Cut(after, `"`)
		if le == "+Inf" {
			continue
		}
		if n, err := strconv.ParseFloat(le, 64); err != nil || n < 90 {
			t.Errorf("bucket le=%s moved on a 90s job", le)
		}
	}
}

func TestARefusedAdmissionMovesBothCountersAndAnAcceptedOneMovesNeither(t *testing.T) {
	before := scrape(t)
	metrics.CountAdmission(false, string(admit.RuleHost))
	if got, want := moved(t, before), map[string]float64{
		`codetrail_admission_total{outcome="rejected"}`:   1,
		`codetrail_admission_rejected_total{rule="host"}`: 1,
	}; !sameSeries(got, want) {
		t.Errorf("a host refusal moved %v, want %v", got, want)
	}

	before = scrape(t)
	metrics.CountAdmission(true, "")
	if got, want := moved(t, before), map[string]float64{
		`codetrail_admission_total{outcome="accepted"}`: 1,
	}; !sameSeries(got, want) {
		t.Errorf("an accepted submission moved %v, want %v", got, want)
	}
}

func TestAnEvictionSweepCountsRowsNotSweeps(t *testing.T) {
	before := scrape(t)
	metrics.CountEvicted(40)
	if got, want := moved(t, before), map[string]float64{"codetrail_evicted_total": 40}; !sameSeries(got, want) {
		t.Errorf("a sweep of forty rows moved %v, want %v", got, want)
	}
}

func TestTheDatastoreInfoGaugeCarriesItsVersionsAsLabels(t *testing.T) {
	metrics.SetDatastoreInfo("17.6", "0.8.6")
	series := `codetrail_datastore_info{pgvector="0.8.6",postgres="17.6"}`
	if v, ok := scrape(t)[series]; !ok || v != 1 {
		t.Fatalf("%s is %v (present=%v), want 1", series, v, ok)
	}
}

func sameSeries(got, want map[string]float64) bool {
	if len(got) != len(want) {
		return false
	}
	for k, v := range want {
		if got[k] != v {
			return false
		}
	}
	return true
}
