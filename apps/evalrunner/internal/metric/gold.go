package metric

import "sort"

// Span is one indexed region, as much of it as scoring needs.
type Span struct {
	ID    string `json:"id"`
	Path  string `json:"path"`
	Start int    `json:"start"`
	End   int    `json:"end"`
}

// Gold is the set of spans that answer a declaration at path:start-end.
//
// Range overlap in one file, never spans.symbol. Three verified reasons: the
// window arm's spans all carry an empty symbol; a declaration longer than
// MaxDeclLines has no span carrying its symbol under the AST arm either; and
// P2's stated rationale for leaving that symbol empty ("so a golden question
// has exactly one correct span rather than N") is false in both directions —
// under symbol keying the declaration has zero spans and under range keying it
// has four.
//
// Two rules, both returned, because they disagree in a way that is not neutral
// between the arms. lenient is every overlapping span: a 100-line declaration
// is one AST span and three windows, so it hands the window arm three chances.
// strict is the single largest overlap, which penalises a chunker for tiling.
// Publishing one without the other is choosing a winner with a definition.
//
// Ties in strict break on the lower start line, then the span id — never on
// map order, which would make the strict gold set differ between two runs over
// an unchanged corpus.
func Gold(path string, start, end int, spans []Span) ([]string, string) {
	type hit struct {
		s       Span
		overlap int
	}
	var hits []hit
	for _, s := range spans {
		if s.Path != path {
			continue
		}
		// Intersection, not containment: a window is not contained in a
		// declaration and a declaration is not contained in a window, and both
		// directions occur in this corpus.
		if start > s.End || s.Start > end {
			continue
		}
		hits = append(hits, hit{s, min(end, s.End) - max(start, s.Start) + 1})
	}
	sort.Slice(hits, func(i, j int) bool {
		a, b := hits[i].s, hits[j].s
		if a.Start != b.Start {
			return a.Start < b.Start
		}
		if a.End != b.End {
			return a.End < b.End
		}
		return a.ID < b.ID
	})

	lenient := make([]string, 0, len(hits))
	best, bestOverlap := "", -1
	for _, h := range hits {
		lenient = append(lenient, h.s.ID)
		// Strictly greater, so the sort above — start line, then id — is what
		// breaks a tie.
		if h.overlap > bestOverlap {
			best, bestOverlap = h.s.ID, h.overlap
		}
	}
	return lenient, best
}

// Set turns a gold list into the membership map the rank functions read.
func Set(ids []string) map[string]bool {
	out := make(map[string]bool, len(ids))
	for _, id := range ids {
		out[id] = true
	}
	return out
}
