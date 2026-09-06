package rag

import (
	"fmt"
	"strings"
	"time"

	"github.com/mralaminahamed/codetrail/packages/shared/models"
)

// Budget bounds what one answer may contain. Both are knobs so P6 can vary
// them, and both are stated in the units a reader thinks in: how many
// citations, and how much text.
type Budget struct {
	MaxSpans int
	MaxChars int
}

// DefaultBudget: five citations is what a person reads, and 8,000 characters is
// roughly two screens — also the right order of magnitude for P7's tool loop,
// so the two paths will not need different numbers.
func DefaultBudget() Budget { return Budget{MaxSpans: 5, MaxChars: 8000} }

func (b Budget) Validate() error {
	if b.MaxSpans < 1 {
		return fmt.Errorf("rag: answer span budget must be positive, got %d", b.MaxSpans)
	}
	if b.MaxChars < 1 {
		return fmt.Errorf("rag: answer character budget must be positive, got %d", b.MaxChars)
	}
	return nil
}

// Cited is one span in an answer, numbered by the marker that points at it.
// Kind and Symbol travel next to the citation rather than inside it because a
// citation is spec §8's tuple and neither is part of it.
type Cited struct {
	Marker   int             `json:"marker"`
	SpanID   string          `json:"span_id"`
	Kind     models.SpanKind `json:"kind"`
	Symbol   string          `json:"symbol"`
	Citation Citation        `json:"citation"`
}

// Answer is the extractive answer: the spans themselves, marked, plus how many
// ranked spans did not fit.
//
// Dropped is on the wire because an answer assembled from 2 of 7 retrieved
// spans is a different claim from one assembled from all of them, and a caller
// cannot tell without being told.
type Answer struct {
	Text      string
	Citations []Cited
	Dropped   int
}

// Assemble renders ranked spans as an extractive answer with citation markers.
// No model call: what a span says is what the answer says, which is what makes
// every line of it attributable.
//
// A span is never truncated. A digest is a claim about the whole text, so
// showing part of a span under a digest of all of it is a citation that lies —
// the exact failure the digest exists to make impossible. Over budget, whole
// spans are dropped and the count says how many.
//
// The consequence, stated rather than discovered: the top-ranked span is kept
// whole however long it is. A budget that dropped it would answer nothing while
// reporting hits, and the honest ordering here is that the no-truncation rule
// outranks the budget rather than the other way round.
//
// Assembly stops at the first span that does not fit rather than skipping it
// for a shorter one further down: the ranked list is an ordered claim, and an
// answer that quietly prefers a worse-ranked span because it was smaller reads
// as a ranking it is not.
//
// now is a parameter for the same reason NewCitation takes one — this package
// stays free of the clock.
func Assemble(r models.Repo, hits []Fused, spans map[string]models.Span, newer Newer, now time.Time, b Budget) Answer {
	// Citations is allocated rather than grown from nil so an answer that cites
	// nothing serialises as [] and not as null, which is what every other list
	// this API serves does — including the LLM path's own copy one file over.
	a := Answer{Citations: make([]Cited, 0, len(hits))}
	var sb strings.Builder
	for i, h := range hits {
		sp, ok := spans[h.SpanID]
		if !ok {
			// A hit whose span did not travel with it has no text to cite and
			// no digest to check; citing it would be a marker pointing at
			// nothing.
			a.Dropped++
			continue
		}
		block := render(len(a.Citations)+1, sp)
		if sb.Len() > 0 {
			block = "\n\n" + block
		}
		if len(a.Citations) > 0 && (len(a.Citations) >= b.MaxSpans || sb.Len()+len(block) > b.MaxChars) {
			a.Dropped += len(hits) - i
			break
		}
		sb.WriteString(block)
		a.Citations = append(a.Citations, Cited{
			Marker:   len(a.Citations) + 1,
			SpanID:   sp.ID,
			Kind:     sp.Kind,
			Symbol:   sp.Symbol,
			Citation: NewCitation(r, sp, newer, now),
		})
	}
	a.Text = sb.String()
	return a
}

// render is the span, whole, under a marker naming where it came from. The text
// is written last and nothing follows it inside the block, so a span ending in
// a newline is still byte-for-byte what its digest covers.
func render(marker int, sp models.Span) string {
	head := fmt.Sprintf("[%d] %s:%d-%d", marker, sp.Path, sp.StartLine, sp.EndLine)
	if sp.Symbol != "" {
		head += " (" + sp.Symbol + ")"
	}
	return head + "\n" + sp.Text
}
