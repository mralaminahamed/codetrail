// Command evalrunner is spec §9's harness: it runs the *same* retriever the
// gateway serves from over two corpora of one commit — AST spans and fixed
// windows — and reports hit@k, MRR and refusal behaviour in both directions.
//
// It builds rag.Retriever directly and calls Search. It does not go through
// the HTTP API: P3 refuses a mode field on /search precisely because this
// comparison runs here, against separate databases. Nothing here reimplements
// ranking, fusion, term-building or the refusal rule — a P6 that did would
// have found a P3 defect rather than a P6 requirement.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/mralaminahamed/codetrail/apps/evalrunner/internal/corpus"
	"github.com/mralaminahamed/codetrail/apps/evalrunner/internal/golden"
	"github.com/mralaminahamed/codetrail/apps/evalrunner/internal/metric"
	"github.com/mralaminahamed/codetrail/apps/evalrunner/internal/source"
	"github.com/mralaminahamed/codetrail/packages/shared/config"
	"github.com/mralaminahamed/codetrail/packages/shared/embed"
	"github.com/mralaminahamed/codetrail/packages/shared/rag"
	"github.com/mralaminahamed/codetrail/packages/shared/store"
)

// queryEmbedTimeout bounds one embed call, as the gateway's does.
const queryEmbedTimeout = 15 * time.Second

type flags struct {
	astDSN, windowDSN string
	repo, src, commit string
	out               string
	ks                string
	limit             int
	floors            string
	astCounters       string
	windowCounters    string
	// now is the run's clock, a parameter for the same reason rag.Assemble
	// takes one: §9 asks for a deterministic run, and two runs over one corpus
	// have to be comparable byte for byte rather than only in their averages.
	now time.Time
	// log is where the per-arm progress lines go. A seam, because "the runner
	// refused before it retrieved" is otherwise unobservable: a mutant that
	// probes after retrieving but before writing leaves no artefact either, so
	// an assertion on the missing file cannot tell the two apart. Measured —
	// that mutant survived until this existed.
	log io.Writer
}

func main() {
	var f flags
	flag.StringVar(&f.astDSN, "ast-dsn", "", "DSN of the database holding the AST arm")
	flag.StringVar(&f.windowDSN, "window-dsn", "", "DSN of the database holding the window arm")
	flag.StringVar(&f.repo, "repo", "", "repository id, the same in both arms")
	flag.StringVar(&f.src, "src", "", "checkout the questions are generated from")
	flag.StringVar(&f.commit, "commit", "", "commit both arms must be at")
	flag.StringVar(&f.out, "out", "docs/eval", "directory the artefact is written under")
	flag.StringVar(&f.ks, "k", "1,5,10", "comma-separated hit@k depths")
	flag.IntVar(&f.limit, "limit", 0, "retrieval depth; 0 means the deepest k")
	flag.StringVar(&f.floors, "floors", "", "comma-separated floors to sweep; empty means -1 to 1 by 0.01")
	// The indexer's per-job counters are a log line and no table, so the only
	// way into the artefact is an operator copying them across. Recorded as
	// zero when unset, which is a claim; the recipe in docs/eval says to paste
	// them.
	flag.StringVar(&f.astCounters, "ast-counters", "", "the AST arm's indexer counters, e.g. vanished=0,unstrippable=0,tokenless=3,unparsed=0")
	flag.StringVar(&f.windowCounters, "window-counters", "", "the window arm's indexer counters")
	flag.Parse()
	f.now, f.log = time.Now().UTC(), os.Stderr

	if err := run(context.Background(), f); err != nil {
		fmt.Fprintln(os.Stderr, "evalrunner: "+err.Error())
		os.Exit(1)
	}
}

func run(ctx context.Context, f flags) error {
	if f.log == nil {
		f.log = io.Discard
	}
	for _, req := range []struct{ name, v string }{
		{"-ast-dsn", f.astDSN}, {"-window-dsn", f.windowDSN},
		{"-repo", f.repo}, {"-src", f.src}, {"-commit", f.commit},
	} {
		if req.v == "" {
			return fmt.Errorf("%s is required", req.name)
		}
	}
	ks, err := parseInts(f.ks)
	if err != nil {
		return fmt.Errorf("-k: %w", err)
	}
	if len(ks) == 0 {
		return errors.New("-k: at least one depth is required")
	}
	// The gateway's ask defaults its limit to the answer budget. An eval
	// measuring hit@10 with a limit of 5 reports a hit rate capped at 5 and
	// nothing in the output says so, so the depth is max(k) unless an operator
	// says otherwise, and it is recorded either way.
	limit := f.limit
	if limit == 0 {
		limit = max(ks...)
	}
	floors, err := parseFloors(f.floors)
	if err != nil {
		return fmt.Errorf("-floors: %w", err)
	}

	cfg, err := readConfig(ctx)
	if err != nil {
		return err
	}
	cfg.Ks, cfg.Limit, cfg.Floors = ks, limit, floors
	cfg.Now = f.now

	// The questions, and the bytes they came from.
	files, err := source.Read(ctx, f.src, source.Limits())
	if err != nil {
		return fmt.Errorf("reading -src: %w", err)
	}
	cases, stats, err := generate(files)
	if err != nil {
		return err
	}
	if len(cases) == 0 {
		return fmt.Errorf("-src %s generated no cases; %d files, %d skipped", f.src, stats.Files, stats.FilesSkipped)
	}

	ast, err := openArm(ctx, "ast", f.astDSN, f.repo, cfg)
	if err != nil {
		return err
	}
	defer ast.Close()
	window, err := openArm(ctx, "window", f.windowDSN, f.repo, cfg)
	if err != nil {
		return err
	}
	defer window.Close()

	// Refuse before retrieving. A leaking or mismatched corpus otherwise
	// produces a complete artefact full of numbers that mean nothing, and an
	// artefact that exists is an artefact someone quotes.
	report, err := corpus.Verify(ctx, ast.Arm.Corpus, window.Arm.Corpus, source.Blobs(files))
	if err != nil {
		return err
	}
	if report.Commit != f.commit {
		return fmt.Errorf("both arms are at %s and -commit says %s", report.Commit, f.commit)
	}
	for _, a := range []*openedArm{ast, window} {
		if _, err := corpus.Probe(ctx, a.Arm.Corpus, cases); err != nil {
			return err
		}
	}

	chunkOpt, strip, err := readChunk()
	if err != nil {
		return err
	}
	pg, err := readPostgres(ctx, ast.store)
	if err != nil {
		return err
	}

	run := Run{
		Commit: report.Commit, Repo: f.repo, Remote: ast.Arm.Repo.Remote, Source: f.src,
		Corpus: report, Golden: stats,
		Retrieval:   retrievalConfig(cfg),
		Command:     strings.Join(os.Args, " "),
		StartedAt:   cfg.Now,
		CodetrailAt: codetrailCommit(),
		Postgres:    pg,
	}
	counters := map[string]string{"ast": f.astCounters, "window": f.windowCounters}
	for _, a := range []*openedArm{ast, window} {
		res, rerr := RunArm(ctx, a.Arm, cases, cfg)
		if rerr != nil {
			return fmt.Errorf("%s arm: %w", a.Arm.Name, rerr)
		}
		run.Arms = append(run.Arms, res)
		ac, cerr := armConfig(a, chunkOpt, strip, counters[a.Arm.Name])
		if cerr != nil {
			return cerr
		}
		run.ArmConfig = append(run.ArmConfig, ac)
		fmt.Fprintf(f.log, "%s: %d cases, %d unscoreable, MRR %v, spans %d\n",
			res.Name, res.Summary.Cases, res.Summary.Unscoreable, res.Summary.MRR, res.Summary.Spans)
	}

	path, err := Write(run, f.out)
	if err != nil {
		return err
	}
	fmt.Fprintln(f.log, "wrote "+path)
	return nil
}

// generate derives the golden set from the checkout, over the files the
// indexer would have chunked and no others.
func generate(files []source.File) ([]golden.Case, golden.Stats, error) {
	var cases []golden.Case
	var stats golden.Stats
	for _, file := range files {
		if !file.Indexable() {
			continue
		}
		cs, st, err := golden.Generate(file.Path, file.Body)
		if err != nil {
			return nil, stats, fmt.Errorf("generating from %s: %w", file.Path, err)
		}
		cases = append(cases, cs...)
		stats.Add(st)
	}
	// Per-file counts cannot be summed: two files can carry the same prose.
	stats.DuplicateQuestions = golden.Duplicates(cases)
	stats.Cases = len(cases)
	return cases, stats, nil
}

type openedArm struct {
	Arm   Arm
	store *store.Store
}

func (a *openedArm) Close() {
	if a.store != nil {
		a.store.Close()
	}
}

func openArm(ctx context.Context, name, dsn, repoID string, cfg Config) (*openedArm, error) {
	s, err := store.New(ctx, dsn)
	if err != nil {
		return nil, fmt.Errorf("%s arm: %w", name, err)
	}
	repo, err := s.GetRepo(ctx, repoID)
	if err != nil {
		s.Close()
		return nil, fmt.Errorf("%s arm: repo %s: %w", name, repoID, err)
	}
	ranges, err := s.SpanRanges(ctx, repoID)
	if err != nil {
		s.Close()
		return nil, fmt.Errorf("%s arm: %w", name, err)
	}
	spans := make([]metric.Span, 0, len(ranges))
	for _, r := range ranges {
		spans = append(spans, metric.Span{ID: r.ID, Path: r.Path, Start: r.StartLine, End: r.EndLine})
	}
	stats, err := s.RepoStats(ctx, repoID)
	if err != nil {
		s.Close()
		return nil, fmt.Errorf("%s arm: %w", name, err)
	}
	newerCommit, at, err := s.NewerCommit(ctx, repoID)
	if err != nil {
		s.Close()
		return nil, fmt.Errorf("%s arm: %w", name, err)
	}
	r, err := newRetriever(ctx, s, cfg)
	if err != nil {
		s.Close()
		return nil, fmt.Errorf("%s arm: %w", name, err)
	}
	return &openedArm{store: s, Arm: Arm{
		Name:      name,
		Corpus:    corpus.Arm{Name: name, DSN: dsn, RepoID: repoID, Read: corpus.FromStore(s)},
		Retriever: r,
		Repo:      repo,
		Spans:     spans,
		Newer:     rag.Newer{Commit: newerCommit, IndexedAt: at},
		Stats: ArmStats{
			Spans: stats.Spans, Files: stats.Files, FilesWithSpans: stats.FilesWithSpans,
		},
	}}, nil
}

// readConfig reads the retrieval knobs the gateway reads, from the same
// environment variables with the same defaults, so a sweep is a shell loop and
// not a code change.
func readConfig(ctx context.Context) (Config, error) {
	var cfg Config
	// The weights before anything else, because rag.Params's zero value fuses
	// every span to score 0 — a ranking that is really no ranking, presenting
	// as a plausible result set rather than as an error. Retriever.validate
	// refuses it; this is what stops the harness ever building one.
	//
	// They are set here and not read from the environment on purpose: P7 ships
	// no RETRIEVAL_W_* knob. A sweep over weights is a struct copy —
	// `c := cfg; c.Fusion.WLexical = 0.5` — which is what rag.Retriever's
	// exported fields are for.
	cfg.Fusion = rag.DefaultParams()
	// rag.ModeVector, which is what the gateway defaults to. It was ModeHybrid
	// — written when the gateway's default was hybrid too, and left behind when
	// P7's experiment moved that one. The divergence is not a design choice:
	// this function's own contract, three lines up, is "the same environment
	// variables with the same defaults", and an eval whose unset default
	// measures a configuration nothing serves is the failure mode the contract
	// exists to prevent. Every published run sets RETRIEVAL_MODE explicitly, so
	// no recorded artefact is affected.
	mode, err := rag.ParseMode(config.Get("RETRIEVAL_MODE", string(rag.ModeVector)))
	if err != nil {
		return cfg, err
	}
	cfg.Mode = mode
	if cfg.Fusion.K, err = config.GetInt("RETRIEVAL_RRF_K", 60); err != nil {
		return cfg, err
	}
	if cfg.Fusion.K < 0 {
		return cfg, fmt.Errorf("RETRIEVAL_RRF_K must not be negative, got %d", cfg.Fusion.K)
	}
	if cfg.Candidates, err = config.GetInt("RETRIEVAL_CANDIDATES", 40); err != nil {
		return cfg, err
	}
	if cfg.Candidates < 1 {
		return cfg, fmt.Errorf("RETRIEVAL_CANDIDATES must be positive, got %d", cfg.Candidates)
	}
	v := config.Get("LEXICAL_SPLIT_IDENTIFIERS", "true")
	if cfg.Split, err = strconv.ParseBool(v); err != nil {
		return cfg, fmt.Errorf("LEXICAL_SPLIT_IDENTIFIERS must be a boolean, got %q", v)
	}
	cfg.Floor = rag.DefaultFloor()
	if v := config.Get("ANSWER_SCORE_FLOOR", ""); v != "" {
		n, perr := strconv.ParseFloat(v, 64)
		if perr != nil {
			return cfg, fmt.Errorf("ANSWER_SCORE_FLOOR must be a number, got %q", v)
		}
		// Calibrated stays false whatever the value: the flag says *codetrail*
		// measured this number, and an operator's own is not one this project
		// has evidence for.
		cfg.Floor = rag.Floor{Value: n, Calibrated: false}
	}
	if err := cfg.Floor.Validate(); err != nil {
		return cfg, err
	}
	cfg.Budget = rag.DefaultBudget()
	if cfg.Budget.MaxSpans, err = config.GetInt("ANSWER_MAX_SPANS", cfg.Budget.MaxSpans); err != nil {
		return cfg, err
	}
	if cfg.Budget.MaxChars, err = config.GetInt("ANSWER_MAX_CHARS", cfg.Budget.MaxChars); err != nil {
		return cfg, err
	}
	if err := cfg.Budget.Validate(); err != nil {
		return cfg, err
	}
	emb, err := embed.FromEnv(ctx, store.EmbeddingDim, store.CheckDim, queryEmbedTimeout)
	if err != nil {
		return cfg, err
	}
	cfg.Emb = emb
	return cfg, nil
}

func newRetriever(_ context.Context, s *store.Store, cfg Config) (*rag.Retriever, error) {
	return &rag.Retriever{
		Store: s, Emb: cfg.Emb, Mode: cfg.Mode,
		Fusion: cfg.Fusion, Candidates: cfg.Candidates, Split: cfg.Split, Floor: cfg.Floor,
	}, nil
}

// noSpans is the retriever's report that a repository holds nothing to rank,
// read the same way the gateway reads it: an outcome, not a failure.
func noSpans(err error) bool { return errors.Is(err, store.ErrNotFound) }

func parseInts(s string) ([]int, error) {
	var out []int
	for _, part := range strings.Split(s, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		n, err := strconv.Atoi(part)
		if err != nil {
			return nil, err
		}
		if n < 1 {
			return nil, fmt.Errorf("depth must be positive, got %d", n)
		}
		out = append(out, n)
	}
	return out, nil
}

// parseFloors defaults to the whole cosine range at a hundredth, which is the
// resolution the calibration is reported at. The list is generated rather than
// centred on a guess: a step that never lands on the recommended value would
// make the sweep unable to recommend it.
func parseFloors(s string) ([]float64, error) {
	if strings.TrimSpace(s) == "" {
		var out []float64
		for i := -100; i <= 100; i++ {
			out = append(out, float64(i)/100)
		}
		return out, nil
	}
	var out []float64
	for _, part := range strings.Split(s, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		v, err := strconv.ParseFloat(part, 64)
		if err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	return out, nil
}

func max(xs ...int) int {
	m := xs[0]
	for _, x := range xs[1:] {
		if x > m {
			m = x
		}
	}
	return m
}
