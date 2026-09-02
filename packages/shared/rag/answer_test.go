package rag

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/mralaminahamed/codetrail/packages/shared/models"
)

var answerRepo = models.Repo{
	ID: "repo-1", Remote: "https://github.com/o/n", Ref: "main",
	Commit: "dfd11cca1234567890abcdef1234567890abcdef",
	// Fixed, so the staleness note is a value and not a clock reading.
	IndexedAt: time.Date(2026, 8, 1, 12, 0, 0, 0, time.UTC),
}

var answerNow = answerRepo.IndexedAt.Add(48 * time.Hour)

// span builds a span whose digest is the real hash of its text, because every
// assertion below is about whether the text shown is the text the digest
// covers. A made-up digest would make those tests agree with themselves.
func span(id, path string, start, size int) models.Span {
	text := strings.Repeat(id[:1], size)
	return models.Span{
		ID: id, RepoID: answerRepo.ID, Path: path, Kind: models.KindFunc,
		Symbol: strings.ToUpper(id[:1]), StartLine: start, EndLine: start + 9,
		Text: text, Digest: digestOf(text),
	}
}

func digestOf(text string) string {
	sum := sha256.Sum256([]byte(text))
	return hex.EncodeToString(sum[:])
}

// corpus turns spans into the pair Assemble takes: the ranked hits, in the
// order given, and the map of spans behind them.
func corpus(spans ...models.Span) ([]Fused, map[string]models.Span) {
	hits := make([]Fused, len(spans))
	byID := make(map[string]models.Span, len(spans))
	for i, s := range spans {
		hits[i] = Fused{SpanID: s.ID, Path: s.Path, StartLine: s.StartLine,
			Score: 1 / float64(62+i), VectorRank: i + 1}
		byID[s.ID] = s
	}
	return hits, byID
}

// Over budget, whole spans are dropped. Truncating one would put a digest on
// text that is not the text the digest was taken of — the one thing a code
// citation exists to make impossible.
//
// The budget is smaller than the corpus, or the branch is never reached; the
// spans are of three different sizes, because with equal ones a length
// assertion cannot tell a wrong drop order from the right one. (The plan
// prescribed three equal 4,000-character spans and then gave that as the reason
// not to; the sizes here follow the reason.)
func TestAssembleDropsWholeSpansAndNeverTruncatesOne(t *testing.T) {
	hits, spans := corpus(
		span("cee", "c.go", 1, 4000),
		span("aay", "a.go", 20, 3500),
		span("bee", "b.go", 40, 3000),
	)
	a := Assemble(answerRepo, hits, spans, Newer{}, answerNow, Budget{MaxSpans: 5, MaxChars: 9000})

	if len(a.Citations) != 2 || a.Dropped != 1 {
		t.Fatalf("kept %d citations and dropped %d, want 2 and 1: %d characters",
			len(a.Citations), a.Dropped, len(a.Text))
	}
	// Identity, not count: dropping the first span instead of the last leaves
	// two citations too.
	for i, want := range []string{"cee", "aay"} {
		if a.Citations[i].SpanID != want {
			t.Errorf("citation %d cites %q, want %q", i+1, a.Citations[i].SpanID, want)
		}
	}
	// Every cited span appears whole. A truncating assembler leaves the
	// citation and cuts the text, which no assertion on the citation can see.
	for _, c := range a.Citations {
		s := spans[c.SpanID]
		if !strings.Contains(a.Text, s.Text) {
			t.Errorf("citation %d names digest %s, whose text is not in the answer",
				c.Marker, s.Digest[:12])
		}
	}
	// And the dropped span is absent entirely, not present in part.
	if strings.Contains(a.Text, spans["bee"].Text[:100]) {
		t.Error("the dropped span's text is in the answer")
	}
}

// The top span is kept whole however long it is: a budget that dropped it would
// answer nothing while reporting hits, and cutting it is the citation that
// lies. So the no-truncation rule outranks the budget, and the count says so.
func TestASpanLargerThanTheWholeBudgetIsStillCitedWhole(t *testing.T) {
	hits, spans := corpus(span("cee", "c.go", 1, 5000), span("aay", "a.go", 20, 10))
	a := Assemble(answerRepo, hits, spans, Newer{}, answerNow, Budget{MaxSpans: 5, MaxChars: 100})

	if len(a.Citations) != 1 || a.Dropped != 1 {
		t.Fatalf("kept %d and dropped %d, want 1 and 1", len(a.Citations), a.Dropped)
	}
	if !strings.Contains(a.Text, spans["cee"].Text) {
		t.Errorf("the top span was cut to fit a %d-character budget", 100)
	}
}

// Markers follow the ranking, and the fixture's ranking disagrees with path
// order and with span-id order — asserted here rather than constructed and
// trusted, because a fixture that stops disagreeing turns a kill into a
// coincidence.
func TestAssembleNumbersCitationsInRankOrder(t *testing.T) {
	hits, spans := corpus(
		span("cee", "c.go", 1, 40),
		span("aay", "a.go", 20, 40),
		span("bee", "b.go", 40, 40),
	)
	if hits[0].Path < hits[1].Path {
		t.Fatal("fixture: the ranking agrees with path order, so a lost ORDER BY would pass")
	}
	if hits[0].SpanID < hits[1].SpanID {
		t.Fatal("fixture: the ranking agrees with span-id order")
	}

	a := Assemble(answerRepo, hits, spans, Newer{}, answerNow, DefaultBudget())
	if len(a.Citations) != 3 {
		t.Fatalf("kept %d citations, want 3", len(a.Citations))
	}
	for i, want := range []string{"cee", "aay", "bee"} {
		c := a.Citations[i]
		if c.Marker != i+1 || c.SpanID != want {
			t.Errorf("citation %d is marker %d of %q, want marker %d of %q",
				i+1, c.Marker, c.SpanID, i+1, want)
		}
		// The marker in the text is what a reader follows; a Cited.Marker that
		// disagrees with it points at the wrong span.
		head := fmt.Sprintf("[%d] %s:%d-%d", c.Marker, spans[want].Path,
			spans[want].StartLine, spans[want].EndLine)
		if !strings.Contains(a.Text, head) {
			t.Errorf("no marker %q in the answer: %.120s", head, a.Text)
		}
	}
	if a.Dropped != 0 {
		t.Errorf("dropped %d of a corpus that fits", a.Dropped)
	}
}

func TestAssembleReportsWhatItDropped(t *testing.T) {
	spans := []models.Span{
		span("cee", "c.go", 1, 40),
		span("aay", "a.go", 20, 40),
		span("bee", "b.go", 40, 40),
		span("dee", "d.go", 60, 40),
	}
	for _, tc := range []struct {
		name           string
		budget         Budget
		wantKept, drop int
	}{
		{"the span budget", Budget{MaxSpans: 2, MaxChars: 8000}, 2, 2},
		{"the character budget", Budget{MaxSpans: 9, MaxChars: 130}, 2, 2},
		{"neither", DefaultBudget(), 4, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			hits, byID := corpus(spans...)
			a := Assemble(answerRepo, hits, byID, Newer{}, answerNow, tc.budget)
			if len(a.Citations) != tc.wantKept || a.Dropped != tc.drop {
				t.Fatalf("kept %d dropped %d, want %d and %d (%d characters)",
					len(a.Citations), a.Dropped, tc.wantKept, tc.drop, len(a.Text))
			}
		})
	}
}

// A hit whose span did not travel with it is counted, not cited: a marker over
// no text is a citation pointing at nothing.
func TestAHitWithNoSpanIsDroppedRatherThanCited(t *testing.T) {
	hits, spans := corpus(span("cee", "c.go", 1, 40), span("aay", "a.go", 20, 40))
	delete(spans, "cee")
	a := Assemble(answerRepo, hits, spans, Newer{}, answerNow, DefaultBudget())
	if len(a.Citations) != 1 || a.Citations[0].SpanID != "aay" || a.Dropped != 1 {
		t.Fatalf("citations %+v, dropped %d", a.Citations, a.Dropped)
	}
	if a.Citations[0].Marker != 1 {
		t.Errorf("marker %d after a skipped hit, want 1: the numbering must follow what is shown",
			a.Citations[0].Marker)
	}
}

// The claim the whole phase rests on: the text a reader sees hashes to the
// digest the citation names. Checked against the answer, not against the
// fixture, so an assembler that alters what it renders is caught.
func TestEveryRenderedSpanStillMatchesItsDigest(t *testing.T) {
	hits, spans := corpus(
		span("cee", "c.go", 1, 400),
		span("aay", "a.go", 20, 350),
		span("bee", "b.go", 40, 300),
	)
	a := Assemble(answerRepo, hits, spans, Newer{}, answerNow, DefaultBudget())
	if len(a.Citations) != 3 {
		t.Fatalf("kept %d citations, want 3", len(a.Citations))
	}
	for _, c := range a.Citations {
		s := spans[c.SpanID]
		shown := fmt.Sprintf("[%d] %s:%d-%d (%s)\n%s",
			c.Marker, s.Path, s.StartLine, s.EndLine, s.Symbol, s.Text)
		if !strings.Contains(a.Text, shown) {
			t.Errorf("citation %d: the span is not in the answer as the citation describes it", c.Marker)
			continue
		}
		if got := digestOf(s.Text); got != c.Citation.Digest {
			t.Errorf("citation %d: digest %s, but the text shown hashes to %s",
				c.Marker, c.Citation.Digest[:12], got[:12])
		}
		if c.Citation.StartLine != s.StartLine || c.Citation.EndLine != s.EndLine {
			t.Errorf("citation %d: lines %d-%d, want %d-%d",
				c.Marker, c.Citation.StartLine, c.Citation.EndLine, s.StartLine, s.EndLine)
		}
	}
}

func TestABudgetThatCanAnswerNothingIsRefusedAtBoot(t *testing.T) {
	for _, b := range []Budget{{MaxSpans: 0, MaxChars: 8000}, {MaxSpans: 5, MaxChars: 0}} {
		if err := b.Validate(); err == nil {
			t.Errorf("%+v accepted: every answer would be an empty string with no error anywhere", b)
		}
	}
	if err := DefaultBudget().Validate(); err != nil {
		t.Errorf("the shipped default is invalid: %v", err)
	}
}
