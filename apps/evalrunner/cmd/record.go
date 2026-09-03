package main

import (
	"math"
	"sort"

	"github.com/mralaminahamed/codetrail/apps/evalrunner/internal/metric"
	"github.com/mralaminahamed/codetrail/packages/shared/models"
	"github.com/mralaminahamed/codetrail/packages/shared/rag"
)

// CaseRecord is everything one case produced in one arm.
//
// Everything, because the floor sweep, the metrics and the artefact all have
// to be derivable from what was written without running an embedder again —
// which is what spec:249's "recorded with the numbers that produced it" means
// if it means anything. A summary and its evidence in two files is how they
// drift apart.
type CaseRecord struct {
	CaseID   string          `json:"case_id"`
	Path     string          `json:"path"`
	Symbol   string          `json:"symbol"`
	Kind     models.SpanKind `json:"kind"`
	DocOf    string          `json:"doc_of"`
	Grouped  bool            `json:"grouped"`
	Question string          `json:"question"`
	// QuestionTerms is what the lexical arm actually searched on, so the
	// split by question length in the artefact is a fact rather than a guess.
	QuestionTerms int  `json:"question_terms"`
	QuestionLines int  `json:"question_lines"`
	Start         int  `json:"start"`
	End           int  `json:"end"`
	RawStart      int  `json:"raw_start"`
	RawEnd        int  `json:"raw_end"`
	Moved         bool `json:"moved"`

	GoldLenient []string `json:"gold_lenient"`
	GoldStrict  string   `json:"gold_strict"`
	Scoreable   bool     `json:"scoreable"`

	Ranked    []string `json:"ranked"`
	RankedLen int      `json:"ranked_len"`
	Limit     int      `json:"limit"`
	Hits      int      `json:"hits"`
	VectorRan bool     `json:"vector_ran"`
	// TopScore is the vector arm's best cosine similarity — the quantity
	// rag.Decide compares and the one the floor is calibrated on. null when
	// that arm did not run or returned nothing; json.Marshal fails outright
	// on NaN.
	TopScore *float64 `json:"top_score"`
	// FusedTop is the top hit's RRF score, recorded beside it so a reader can
	// see the two orders of magnitude between them rather than take it on
	// trust. It carries no quality signal — it is a function of ranks alone.
	FusedTop *float64 `json:"fused_top"`

	GoldRank       int `json:"gold_rank"`
	GoldRankStrict int `json:"gold_rank_strict"`
	// The gold span's rank in each arm separately, which is what makes
	// spec:316's fusion question answerable from the record instead of from a
	// rerun. 0 means that arm did not return it.
	GoldVectorRank  int `json:"gold_vector_rank"`
	GoldLexicalRank int `json:"gold_lexical_rank"`

	AnswerHasGold   bool `json:"answer_has_gold"`
	AnswerCitations int  `json:"answer_citations"`
	AnswerDropped   int  `json:"answer_dropped"`

	Outcome rag.Outcome `json:"outcome"`
	Reason  rag.Reason  `json:"reason"`
}

// Outcome projects the record onto what the floor sweep reads.
func (c CaseRecord) Outcomes() metric.Outcome {
	return metric.Outcome{
		CaseID: c.CaseID, TopScore: value(c.TopScore), Hits: c.Hits,
		VectorRan: c.VectorRan, GoldRank: c.GoldRank,
		AnswerHasGold: c.AnswerHasGold, Scoreable: c.Scoreable,
	}
}

// HitRate is one k, under both gold rules. A slice rather than a map because
// nothing in this harness's output ranges a map.
type HitRate struct {
	K       int     `json:"k"`
	Lenient float64 `json:"lenient"`
	Strict  float64 `json:"strict"`
}

// Quantiles describes the top-score distribution the floor is read off.
type Quantiles struct {
	N      int      `json:"n"`
	Min    *float64 `json:"min"`
	P10    *float64 `json:"p10"`
	Median *float64 `json:"median"`
	P90    *float64 `json:"p90"`
	Max    *float64 `json:"max"`
}

// Summary is one arm's headline, and every figure in it is beside the thing
// that qualifies it: the gold-set sizes next to the hit rates, because the
// lenient rule hands a tiling arm more chances; the span and file counts,
// because a smaller haystack is easier; the unscoreable count, because those
// cases are in no denominator.
type Summary struct {
	Cases           int     `json:"cases"`
	Unscoreable     int     `json:"unscoreable"`
	Spans           int     `json:"spans"`
	FilesWithSpans  int     `json:"files_with_spans"`
	MeanGoldLenient float64 `json:"mean_gold_lenient"`
	MeanGoldStrict  float64 `json:"mean_gold_strict"`
	MovedCases      int     `json:"moved_cases"`
	GroupedCases    int     `json:"grouped_cases"`

	HitAt     []HitRate `json:"hit_at"`
	MRR       float64   `json:"mrr"`
	MRRStrict float64   `json:"mrr_strict"`
	TopScore  Quantiles `json:"top_score"`

	// FloorApplicable is false in lexical mode, where Decide short-circuits
	// before it reads the floor. "The floor refused nothing" and "the floor
	// does not apply" are different sentences and the artefact says which.
	FloorApplicable bool               `json:"floor_applicable"`
	Confusion       metric.Confusion   `json:"confusion_at_configured_floor"`
	Sweep           []metric.Confusion `json:"sweep"`

	// GoldFoundBy counts, over the scoreable cases whose gold span was
	// ranked, which arms returned it. spec:316's evidence.
	GoldFoundByVectorOnly  int `json:"gold_found_by_vector_only"`
	GoldFoundByLexicalOnly int `json:"gold_found_by_lexical_only"`
	GoldFoundByBoth        int `json:"gold_found_by_both"`
	GoldNotFound           int `json:"gold_not_found"`
}

// summarise is the arithmetic over one arm's records. Every rate here is over
// the scoreable cases only: a case with no gold span in this arm cannot be
// right or wrong here, and putting it in a denominator says the retriever
// failed where the corpus is empty.
func summarise(records []CaseRecord, ks []int, floor rag.Floor, floors []float64, applicable bool) Summary {
	s := Summary{Cases: len(records), FloorApplicable: applicable}
	var rr, rrStrict []float64
	var goldLen, goldStrict []float64
	var tops []float64
	hits := make([]struct{ lenient, strict int }, len(ks))

	for _, r := range records {
		if r.Moved {
			s.MovedCases++
		}
		if r.Grouped {
			s.GroupedCases++
		}
		if !r.Scoreable {
			s.Unscoreable++
			continue
		}
		goldLen = append(goldLen, float64(len(r.GoldLenient)))
		if r.GoldStrict != "" {
			goldStrict = append(goldStrict, 1)
		}
		rr = append(rr, reciprocal(r.GoldRank))
		rrStrict = append(rrStrict, reciprocal(r.GoldRankStrict))
		if v := value(r.TopScore); !math.IsNaN(v) {
			tops = append(tops, v)
		}
		for i, k := range ks {
			if r.GoldRank > 0 && r.GoldRank <= k {
				hits[i].lenient++
			}
			if r.GoldRankStrict > 0 && r.GoldRankStrict <= k {
				hits[i].strict++
			}
		}
		switch {
		case r.GoldRank == 0:
			s.GoldNotFound++
		case r.GoldVectorRank > 0 && r.GoldLexicalRank > 0:
			s.GoldFoundByBoth++
		case r.GoldVectorRank > 0:
			s.GoldFoundByVectorOnly++
		case r.GoldLexicalRank > 0:
			s.GoldFoundByLexicalOnly++
		}
	}

	scored := len(records) - s.Unscoreable
	for i, k := range ks {
		s.HitAt = append(s.HitAt, HitRate{
			K: k, Lenient: rate(hits[i].lenient, scored), Strict: rate(hits[i].strict, scored),
		})
	}
	s.MRR = metric.MeanOverAll(rr)
	s.MRRStrict = metric.MeanOverAll(rrStrict)
	s.MeanGoldLenient = metric.MeanOverAll(goldLen)
	s.MeanGoldStrict = metric.MeanOverAll(goldStrict)
	s.TopScore = quantiles(tops)

	outcomes := make([]metric.Outcome, 0, len(records))
	for _, r := range records {
		outcomes = append(outcomes, r.Outcomes())
	}
	s.Confusion = metric.Sweep(outcomes, []float64{floor.Value})[0]
	s.Sweep = metric.Sweep(outcomes, floors)
	return s
}

// reciprocal is 1/rank, and 0 for a miss. Zero, not excluded: MRR is defined
// over every scoreable case, and averaging only the ones that hit reports a
// different and always-larger number.
func reciprocal(rank int) float64 {
	if rank <= 0 {
		return 0
	}
	return 1 / float64(rank)
}

func rate(n, of int) float64 {
	if of == 0 {
		return 0
	}
	return float64(n) / float64(of)
}

func quantiles(xs []float64) Quantiles {
	q := Quantiles{N: len(xs)}
	if len(xs) == 0 {
		return q
	}
	sorted := append([]float64(nil), xs...)
	sort.Float64s(sorted)
	at := func(p float64) *float64 {
		i := int(p * float64(len(sorted)-1))
		v := sorted[i]
		return &v
	}
	q.Min, q.P10, q.Median, q.P90, q.Max = at(0), at(0.10), at(0.50), at(0.90), at(1)
	return q
}

// number is the honest encoding of a score that may not be one: json.Marshal
// fails outright on NaN, and a lexical-only or empty result genuinely has no
// cosine similarity. The gateway encodes it the same way.
func number(v float64) *float64 {
	if math.IsNaN(v) {
		return nil
	}
	return &v
}

func value(p *float64) float64 {
	if p == nil {
		return math.NaN()
	}
	return *p
}
