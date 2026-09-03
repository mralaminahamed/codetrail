package main

import (
	"context"
	"sort"
	"time"

	"github.com/mralaminahamed/codetrail/apps/evalrunner/internal/corpus"
	"github.com/mralaminahamed/codetrail/apps/evalrunner/internal/golden"
	"github.com/mralaminahamed/codetrail/apps/evalrunner/internal/metric"
	"github.com/mralaminahamed/codetrail/packages/shared/embed"
	"github.com/mralaminahamed/codetrail/packages/shared/models"
	"github.com/mralaminahamed/codetrail/packages/shared/rag"
)

// Arm is one side of the run: a verified corpus, the shipped retriever
// pointed at it, and the whole span set the gold rule needs.
//
// Retriever is *rag.Retriever and not a reimplementation of it. Spec:43 says
// the evalrunner runs the *same* retriever the gateway serves from, and P3
// refuses a mode field on /search precisely because this comparison runs here
// against separate databases rather than through the API.
type Arm struct {
	Name      string
	Corpus    corpus.Arm
	Retriever *rag.Retriever
	Repo      models.Repo
	Spans     []metric.Span
	Newer     rag.Newer
	Stats     ArmStats
}

// ArmStats is what the corpus holds, recorded beside every metric because a
// smaller haystack is easier and a more coherent document is better, and both
// directions are real.
type ArmStats struct {
	Spans          int `json:"spans"`
	Files          int `json:"files"`
	FilesWithSpans int `json:"files_with_spans"`
}

// Config is everything the run reads that is not the corpus.
//
// The retrieval knobs are the gateway's own, read from the same environment
// variables with the same defaults. Candidates in particular is left at
// whatever the environment says: raising it improves the numbers by asking the
// ANN index for more work, and the headline has to describe the product.
type Config struct {
	Mode       rag.Mode
	K          int
	Candidates int
	Split      bool
	Emb        embed.Embedder

	Ks     []int
	Limit  int
	Floor  rag.Floor
	Budget rag.Budget
	Floors []float64
	Now    time.Time
}

// ArmResult is one arm's whole output: every case, and the arithmetic over
// them.
type ArmResult struct {
	Name    string       `json:"name"`
	Mode    rag.Mode     `json:"mode"`
	Summary Summary      `json:"summary"`
	Cases   []CaseRecord `json:"cases"`
}

// Sort orders the cases the run will walk.
//
// By (path, start, symbol, docOf), before the run and once: Go randomises map
// iteration on purpose, and §9 asks for a deterministic run. This is the
// determinism bug that shows up as a diff nobody can explain three months
// later.
func Sort(cases []golden.Case) []golden.Case {
	out := append([]golden.Case(nil), cases...)
	sort.Slice(out, func(i, j int) bool {
		a, b := out[i], out[j]
		if a.Path != b.Path {
			return a.Path < b.Path
		}
		if a.Start != b.Start {
			return a.Start < b.Start
		}
		if a.Symbol != b.Symbol {
			return a.Symbol < b.Symbol
		}
		return a.DocOf < b.DocOf
	})
	return out
}

// RunArm retrieves once per case and records everything.
//
// One pass, not one per floor: retrieval does not depend on the floor, so the
// whole floor curve comes from this and metric.Sweep afterwards. That is not
// only cheaper — it makes the calibration reproducible from the committed
// artefact with no embedder in the loop.
func RunArm(ctx context.Context, a Arm, cases []golden.Case, cfg Config) (ArmResult, error) {
	res := ArmResult{Name: a.Name, Mode: a.Retriever.Mode}
	for _, c := range Sort(cases) {
		if err := ctx.Err(); err != nil {
			return res, err
		}
		rec, err := runCase(ctx, a, c, cfg)
		if err != nil {
			return res, err
		}
		res.Cases = append(res.Cases, rec)
	}
	res.Summary = summarise(res.Cases, cfg.Ks, cfg.Floor, cfg.Floors, metric.Applicable(a.Retriever.Mode))
	res.Summary.Spans = a.Stats.Spans
	res.Summary.FilesWithSpans = a.Stats.FilesWithSpans
	return res, nil
}

func runCase(ctx context.Context, a Arm, c golden.Case, cfg Config) (CaseRecord, error) {
	rec := CaseRecord{
		CaseID: c.ID, Path: c.Path, Symbol: c.Symbol, Kind: c.Kind, DocOf: c.DocOf,
		Grouped: c.Grouped, Question: c.Question,
		QuestionTerms: len(rag.Terms(c.Question, a.Retriever.Split)),
		QuestionLines: countLines(c.Question),
		Start:         c.Start, End: c.End, RawStart: c.RawStart, RawEnd: c.RawEnd,
		Moved: c.Moved(), Limit: cfg.Limit,
	}

	lenient, strict := metric.Gold(c.Path, c.Start, c.End, a.Spans)
	rec.GoldLenient, rec.GoldStrict = lenient, strict
	// A case with no gold span in this arm has no right answer here. It is
	// counted in its own cell and left out of every denominator, never scored
	// as a miss: that would say the retriever failed where the corpus is
	// empty.
	rec.Scoreable = len(lenient) > 0
	if rec.GoldLenient == nil {
		rec.GoldLenient = []string{}
	}

	out, err := a.Retriever.Search(ctx, a.Corpus.RepoID, c.Question, cfg.Limit)
	if err != nil && !noSpans(err) {
		return rec, err
	}
	rec.Hits, rec.VectorRan = len(out.Hits), out.VectorRan
	rec.TopScore = number(out.TopScore)
	if len(out.Hits) > 0 {
		rec.FusedTop = number(out.Hits[0].Score)
	}
	goldSet := metric.Set(lenient)
	for i, h := range out.Hits {
		rec.Ranked = append(rec.Ranked, h.SpanID)
		if rec.GoldRank == 0 && goldSet[h.SpanID] {
			rec.GoldRank = i + 1
			rec.GoldVectorRank, rec.GoldLexicalRank = h.VectorRank, h.LexicalRank
		}
		if rec.GoldRankStrict == 0 && h.SpanID == strict {
			rec.GoldRankStrict = i + 1
		}
	}
	if rec.Ranked == nil {
		rec.Ranked = []string{}
	}
	rec.RankedLen = len(rec.Ranked)

	// Assembled, not simulated: rag.Assemble drops spans past the budget, so a
	// gold span ranked seventh is retrieved and not cited, and "answered
	// without citing the right span" is what a user experiences.
	ans := rag.Assemble(a.Repo, out.Hits, out.Spans, a.Newer, cfg.Now, cfg.Budget)
	rec.AnswerCitations, rec.AnswerDropped = len(ans.Citations), ans.Dropped
	for _, cite := range ans.Citations {
		if goldSet[cite.SpanID] {
			rec.AnswerHasGold = true
			break
		}
	}

	rec.Outcome, rec.Reason = out.Decide(cfg.Floor)
	return rec, nil
}

func countLines(s string) int {
	n := 0
	for _, r := range s {
		if r == '\n' {
			n++
		}
	}
	if len(s) > 0 && s[len(s)-1] != '\n' {
		n++
	}
	return n
}
