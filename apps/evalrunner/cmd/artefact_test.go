package main

import (
	"encoding/json"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/mralaminahamed/codetrail/apps/evalrunner/internal/corpus"
	"github.com/mralaminahamed/codetrail/apps/evalrunner/internal/golden"
	"github.com/mralaminahamed/codetrail/apps/evalrunner/internal/metric"
	"github.com/mralaminahamed/codetrail/packages/shared/rag"
)

// requiredKeys is the whole shape of the artefact, in one place.
//
// Closed in *both* directions: every key here must be in the file, and every
// key in the file must be here. One direction alone is not a test — adding a
// knob to the runner without adding it to the artefact would pass the first,
// and deleting an entry from this list would pass the second. A number whose
// configuration is not in the same file is a number nobody can reproduce.
var requiredKeys = []string{
	"quality", "banner",
	"command", "started_at", "codetrail_commit",
	"repo_id", "remote", "commit", "source",
	"corpus.commit", "corpus.files", "corpus.spans", "corpus.shared_span_ids",
	"golden",
	"retrieval.mode", "retrieval.rrf_k", "retrieval.candidates", "retrieval.split_identifiers",
	"retrieval.limit", "retrieval.k", "retrieval.score_floor", "retrieval.floor_calibrated",
	"retrieval.answer_max_spans", "retrieval.answer_max_chars",
	"retrieval.embed_provider", "retrieval.embed_model", "retrieval.embed_dim",
	"arms[].name", "arms[].mode", "arms[].summary", "arms[].cases",
	"arm_config[].name", "arm_config[].database", "arm_config[].strategy", "arm_config[].strip",
	"arm_config[].window_lines", "arm_config[].window_overlap", "arm_config[].max_decl_lines",
	"arm_config[].files", "arm_config[].files_with_spans", "arm_config[].spans",
	"arm_config[].vanished", "arm_config[].unstrippable", "arm_config[].tokenless", "arm_config[].unparsed",
	"postgres.version", "postgres.pgvector_version",
	"postgres.hnsw_ef_search", "postgres.hnsw_iterative_scan",
}

// stopAt are the subtrees the key walk records as leaves. Descending into
// every case record would make the list a copy of the type rather than a
// statement about what a run has to carry.
var stopAt = map[string]bool{
	"golden": true, "arms[].summary": true, "arms[].cases": true,
	"corpus.files": true, "corpus.spans": true,
}

func sampleRun() Run {
	top := 0.72
	fused := 0.032
	return Run{
		Command:     "./bin/evalrunner -ast-dsn … -window-dsn …",
		StartedAt:   time.Date(2026, 9, 3, 12, 0, 0, 0, time.UTC),
		CodetrailAt: "38372ef",
		Repo:        "repo-1", Remote: "https://github.com/rs/zerolog", Commit: "dfd11cca", Source: "/tmp/checkout",
		Corpus: corpus.Report{
			Commit: "dfd11cca",
			Files:  map[string]int{"ast": 99, "window": 99},
			Spans:  map[string]int{"ast": 1303, "window": 768},
		},
		Golden: golden.Stats{Files: 99, Cases: 41},
		Retrieval: RetrievalConfig{
			Mode: rag.ModeHybrid, K: 60, Candidates: 40, Split: true,
			Limit: 10, Ks: []int{1, 5, 10}, FloorValue: -1, FloorCalibrated: false,
			AnswerMaxSpans: 5, AnswerMaxChars: 8000,
			EmbedProvider: "ollama", EmbedModel: "nomic-embed-text", EmbedDim: 768,
		},
		Postgres: PostgresInfo{
			Version: "PostgreSQL 17.7", PgvectorVersion: "0.8.6",
			HNSWEfSearch: "40", HNSWIterativeScan: "off",
		},
		ArmConfig: []ArmConfig{
			{Name: "ast", Database: "codetrail_eval_ast", Strategy: "ast", Strip: true,
				WindowLines: 40, WindowOverlap: 10, MaxDeclLines: 200, Spans: 1303, Tokenless: 0},
			{Name: "window", Database: "codetrail_eval_window", Strategy: "window", Strip: true,
				WindowLines: 40, WindowOverlap: 10, MaxDeclLines: 200, Spans: 768, Tokenless: 3},
		},
		Arms: []ArmResult{{
			Name: "ast", Mode: rag.ModeHybrid,
			Cases: []CaseRecord{
				{CaseID: "a", Hits: 5, VectorRan: true, TopScore: &top, FusedTop: &fused, GoldRank: 1, AnswerHasGold: true, Scoreable: true},
				{CaseID: "b", Hits: 0, VectorRan: true, GoldRank: 0, Scoreable: true},
				{CaseID: "c", Hits: 5, VectorRan: true, TopScore: &top, FusedTop: &fused, GoldRank: 3, Scoreable: false},
			},
		}},
	}
}

func write(t *testing.T, r Run) (string, []byte, Run) {
	t.Helper()
	out := t.TempDir()
	path, err := Write(r, out)
	if err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var back Run
	if err := json.Unmarshal(b, &back); err != nil {
		t.Fatal(err)
	}
	return path, b, back
}

// Four surfaces, asserted together. P3's and P4's sweeps both found that a
// single side effect nothing reads back is the most common surviving mutant in
// this project, so a mutant that moves one field must not leave three
// assertions passing in three separate tests.
func TestTheArtefactSaysWhatItIsOnEverySurface(t *testing.T) {
	for _, tc := range []struct {
		model   string
		quality bool
		banner  bool
		dir     string
	}{
		{"nomic-embed-text", true, false, DirRuns},
		{FakeModel, false, true, DirMechanics},
	} {
		r := sampleRun()
		r.Retrieval.EmbedModel = tc.model
		path, _, back := write(t, r)
		got := struct {
			quality bool
			banner  string
			dir     string
			name    string
		}{back.Quality, back.Banner, filepath.Base(filepath.Dir(path)), filepath.Base(path)}
		want := struct {
			quality bool
			banner  string
			dir     string
			name    string
		}{tc.quality, "", tc.dir, "2026-09-03-rs-zerolog-" + tc.model + ".json"}
		if tc.banner {
			want.banner = Banner
		}
		if got != want {
			t.Errorf("%s: got %+v, want %+v", tc.model, got, want)
		}
		if tc.banner && !strings.Contains(back.Banner, "§9") {
			t.Errorf("%s: the banner does not name spec §9: %q", tc.model, back.Banner)
		}
	}
}

// Quality is the first key of the file, so a reader who opens it, greps it, or
// sees it in a diff hits the label before any number.
func TestQualityIsTheFirstKeyOfTheFile(t *testing.T) {
	r := sampleRun()
	r.Retrieval.EmbedModel = FakeModel
	_, b, _ := write(t, r)
	lines := strings.Split(string(b), "\n")
	if len(lines) < 3 {
		t.Fatalf("the artefact is %d lines", len(lines))
	}
	if !strings.Contains(lines[1], `"quality"`) {
		t.Errorf("the first key is %q, want quality", strings.TrimSpace(lines[1]))
	}
	if !strings.Contains(lines[2], `"banner"`) {
		t.Errorf("the second key is %q, want banner", strings.TrimSpace(lines[2]))
	}
}

func TestTheArtefactCarriesEnoughToReproduceTheRun(t *testing.T) {
	_, b, back := write(t, sampleRun())

	var raw map[string]any
	if err := json.Unmarshal(b, &raw); err != nil {
		t.Fatal(err)
	}
	got := keyPaths(raw, "")
	want := append([]string(nil), requiredKeys...)
	sort.Strings(got)
	sort.Strings(want)
	if !reflect.DeepEqual(got, want) {
		for _, k := range missing(want, got) {
			t.Errorf("the artefact is missing required key %q", k)
		}
		for _, k := range missing(got, want) {
			t.Errorf("the artefact carries %q, which is not in requiredKeys; a knob the list does not name is a knob nobody reproduces", k)
		}
	}

	// The sweep has to be recomputable from the per-case records with no
	// embedder in the loop. A test that only checked the summary is present
	// cannot discriminate.
	if len(back.Arms) == 0 || len(back.Arms[0].Cases) == 0 {
		t.Fatalf("the artefact carries %d per-case records, want 3", caseCount(back))
	}
	outcomes := make([]metric.Outcome, 0, len(back.Arms[0].Cases))
	for _, c := range back.Arms[0].Cases {
		outcomes = append(outcomes, c.Outcomes())
	}
	got2 := metric.Sweep(outcomes, []float64{0.5})[0]
	want2 := metric.Confusion{
		Floor: 0.5, AnsweredWithGold: 1, RefusedWithoutGold: 1, Unscoreable: 1,
		ByReason: map[rag.Reason]int{rag.ReasonNoSpans: 1},
	}
	if !reflect.DeepEqual(got2, want2) {
		t.Errorf("the sweep recomputed from the file is %+v, want %+v", got2, want2)
	}
}

// The obvious implementation records the DSN, and a DSN carries a password —
// which is exactly what DATABASE_URL is in infra/docker-compose.yml and in CI.
func TestTheArtefactNeverCarriesADsnPassword(t *testing.T) {
	r := sampleRun()
	// The Arm the runner builds holds the DSN; only Database() reaches here.
	a := corpus.Arm{Name: "ast", DSN: "postgres://codetrail:codetrail@localhost:55432/codetrail_eval_ast?sslmode=disable"}
	r.ArmConfig[0].Database = a.Database()
	_, b, _ := write(t, r)
	for _, secret := range []string{"codetrail:codetrail@", "postgres://", "sslmode"} {
		if strings.Contains(string(b), secret) {
			t.Errorf("the artefact contains %q; it records the database name and nothing else from a DSN", secret)
		}
	}
	if !strings.Contains(string(b), "codetrail_eval_ast") {
		t.Error("the artefact does not name the database at all")
	}
}

// A test about the repository, not about a function, and the only thing that
// catches a fake-embedder run committed as a measurement.
func TestNoMechanicsRunIsCommittedAsAMeasurement(t *testing.T) {
	dir := filepath.Join("..", "..", "..", "docs", "eval", DirRuns)
	if _, err := os.Stat(dir); os.IsNotExist(err) {
		t.Fatalf("%s does not exist; every real run lives there and the test that guards it needs the directory", dir)
	}
	n := 0
	err := filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() || !strings.HasSuffix(p, ".json") {
			return err
		}
		n++
		b, rerr := os.ReadFile(p)
		if rerr != nil {
			return rerr
		}
		var r Run
		if uerr := json.Unmarshal(b, &r); uerr != nil {
			t.Errorf("%s: %v", p, uerr)
			return nil
		}
		if !r.Quality {
			t.Errorf("%s has quality=false; a mechanics run is not a measurement", p)
		}
		if r.Retrieval.EmbedModel == FakeModel {
			t.Errorf("%s was run on %s; the figures a fake-embedder run prints are not quality (spec §9)", p, FakeModel)
		}
		if strings.Contains(filepath.Base(p), FakeModel) {
			t.Errorf("%s is named for the fake embedder", p)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("%s holds %d committed runs", dir, n)
}

// keyPaths enumerates a decoded artefact's key paths, using "[]" for an array
// of objects and stopping at the subtrees stopAt names.
func keyPaths(v map[string]any, prefix string) []string {
	var out []string
	for k, val := range v {
		path := k
		if prefix != "" {
			path = prefix + "." + k
		}
		switch t := val.(type) {
		case map[string]any:
			if stopAt[path] {
				out = append(out, path)
				continue
			}
			out = append(out, keyPaths(t, path)...)
		case []any:
			if stopAt[path] || len(t) == 0 {
				out = append(out, path)
				continue
			}
			obj, ok := t[0].(map[string]any)
			if !ok {
				out = append(out, path)
				continue
			}
			out = append(out, keyPaths(obj, path+"[]")...)
		default:
			out = append(out, path)
		}
	}
	return out
}

func missing(want, got []string) []string {
	have := map[string]bool{}
	for _, g := range got {
		have[g] = true
	}
	var out []string
	for _, w := range want {
		if !have[w] {
			out = append(out, w)
		}
	}
	return out
}

func caseCount(r Run) int {
	n := 0
	for _, a := range r.Arms {
		n += len(a.Cases)
	}
	return n
}
