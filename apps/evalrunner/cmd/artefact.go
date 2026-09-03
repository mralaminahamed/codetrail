package main

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/mralaminahamed/codetrail/apps/evalrunner/internal/corpus"
	"github.com/mralaminahamed/codetrail/apps/evalrunner/internal/golden"
	"github.com/mralaminahamed/codetrail/packages/shared/rag"
)

// FakeModel is what embed.Fake calls itself. A run under it is mechanics and
// not a measurement, and this string is one of the four surfaces that says so.
const FakeModel = "fake-hashed-bow"

// Banner is the sentence a mechanics run carries. Empty on a real run: a
// banner that is always present is a banner nobody reads.
const Banner = "MECHANICS ONLY — spec §9: the figures this run prints are not quality."

// Directories under -out. A mechanics run never lands in runs/, which is
// gitignored the other way round: mechanics/ is ignored so a developer's local
// run cannot be committed by accident, and runs/ is tracked because a real run
// is the evidence the calibration rests on.
const (
	DirRuns      = "runs"
	DirMechanics = "mechanics"
)

// Run is the artefact.
//
// Quality is the first key and Banner the second, so a reader who opens the
// file, greps it, or sees it in a diff hits the label before any number. Both
// are set by seal from the embedder the run actually used, never by a caller.
type Run struct {
	Quality bool   `json:"quality"`
	Banner  string `json:"banner"`

	Command     string    `json:"command"`
	StartedAt   time.Time `json:"started_at"`
	CodetrailAt string    `json:"codetrail_commit"`

	Repo   string        `json:"repo_id"`
	Remote string        `json:"remote"`
	Commit string        `json:"commit"`
	Source string        `json:"source"`
	Corpus corpus.Report `json:"corpus"`
	Golden golden.Stats  `json:"golden"`

	Retrieval RetrievalConfig `json:"retrieval"`
	Arms      []ArmResult     `json:"arms"`
	ArmConfig []ArmConfig     `json:"arm_config"`

	Postgres PostgresInfo `json:"postgres"`
}

// RetrievalConfig is everything a rerun needs that is not the corpus. A number
// whose configuration is not in the same file is a number nobody can
// reproduce.
type RetrievalConfig struct {
	Mode            rag.Mode `json:"mode"`
	K               int      `json:"rrf_k"`
	Candidates      int      `json:"candidates"`
	Split           bool     `json:"split_identifiers"`
	Limit           int      `json:"limit"`
	Ks              []int    `json:"k"`
	FloorValue      float64  `json:"score_floor"`
	FloorCalibrated bool     `json:"floor_calibrated"`
	AnswerMaxSpans  int      `json:"answer_max_spans"`
	AnswerMaxChars  int      `json:"answer_max_chars"`
	EmbedProvider   string   `json:"embed_provider"`
	EmbedModel      string   `json:"embed_model"`
	EmbedDim        int      `json:"embed_dim"`
}

// ArmConfig is what built one arm's corpus, including the two facts the schema
// does not record: which chunking strategy, and whether it was stripped.
type ArmConfig struct {
	Name           string `json:"name"`
	Database       string `json:"database"`
	Strategy       string `json:"strategy"`
	Strip          bool   `json:"strip"`
	WindowLines    int    `json:"window_lines"`
	WindowOverlap  int    `json:"window_overlap"`
	MaxDeclLines   int    `json:"max_decl_lines"`
	Files          int    `json:"files"`
	FilesWithSpans int    `json:"files_with_spans"`
	Spans          int    `json:"spans"`
	// The indexer's per-job counters, copied from its log line because they
	// are in no table and nowhere else. tokenless in particular is how a
	// corpus shrinks without anyone noticing.
	Vanished     int `json:"vanished"`
	Unstrippable int `json:"unstrippable"`
	Tokenless    int `json:"tokenless"`
	Unparsed     int `json:"unparsed"`
}

// PostgresInfo is read at run time rather than quoted, because the pgvector
// image is a floating tag: infra/docker-compose.yml pins ollama by digest and
// postgres by tag alone, so "pgvector 0.8.6 (pinned image)" is a claim about a
// container that may already have moved.
type PostgresInfo struct {
	Version           string `json:"version"`
	PgvectorVersion   string `json:"pgvector_version"`
	HNSWEfSearch      string `json:"hnsw_ef_search"`
	HNSWIterativeScan string `json:"hnsw_iterative_scan"`
}

// seal fills in the four surfaces that say what a run is, from the embedder it
// actually used. It is the only writer of Quality and Banner.
func (r *Run) seal() {
	r.Quality = r.Retrieval.EmbedModel != FakeModel
	r.Banner = ""
	if !r.Quality {
		r.Banner = Banner
	}
}

// Dir is the third surface: a mechanics run never lands beside a measurement.
func (r Run) Dir() string {
	if r.Quality {
		return DirRuns
	}
	return DirMechanics
}

// Name is the fourth surface: the embed model is in the filename, because it
// is the single fact that decides whether the file is a measurement.
//
// The mode is in it too, and that is not decoration — spec:316 makes the three
// modes an experiment, so one repository produces three runs on one day, and a
// name without the mode would have them overwrite each other. Found by running
// the measurement, not by reading the code.
func (r Run) Name() string {
	return fmt.Sprintf("%s-%s-%s-%s.json",
		r.StartedAt.UTC().Format("2006-01-02"), slug(r.Remote), r.Retrieval.Mode, r.Retrieval.EmbedModel)
}

// Write seals the run and writes it under out.
func Write(r Run, out string) (string, error) {
	r.seal()
	dir := filepath.Join(out, r.Dir())
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return "", err
	}
	b, err := json.MarshalIndent(r, "", "  ")
	if err != nil {
		return "", err
	}
	path := filepath.Join(dir, r.Name())
	if err := os.WriteFile(path, append(b, '\n'), 0o600); err != nil {
		return "", err
	}
	return path, nil
}

// slug turns a remote into a filename part: owner-repo, lower case.
func slug(remote string) string {
	s := strings.TrimSuffix(strings.TrimPrefix(remote, "https://"), ".git")
	parts := strings.Split(s, "/")
	if len(parts) >= 3 {
		parts = parts[len(parts)-2:]
	}
	joined := strings.ToLower(strings.Join(parts, "-"))
	var b strings.Builder
	for _, c := range joined {
		switch {
		case c >= 'a' && c <= 'z', c >= '0' && c <= '9', c == '-', c == '.', c == '_':
			b.WriteRune(c)
		default:
			b.WriteByte('-')
		}
	}
	if b.Len() == 0 {
		return "unknown"
	}
	return b.String()
}

// codetrailCommit is what the harness itself was at, so a run can be
// reproduced against the code that produced it. Empty when git is not
// available, never a guess.
func codetrailCommit() string {
	out, err := exec.Command("git", "rev-parse", "HEAD").Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}
