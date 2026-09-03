package metric

import (
	"os"
	"reflect"
	"testing"

	"github.com/mralaminahamed/codetrail/apps/evalrunner/internal/golden"
	"github.com/mralaminahamed/codetrail/packages/shared/chunk"
)

// Every overlap shape in one fixture. Range intersection has four ways to be
// spelled wrong and three of them agree on the containment case, so a fixture
// where the declaration sits inside one window proves almost nothing.
var overlapShapes = []Span{
	{ID: "w1", Path: "a.go", Start: 1, End: 12},  // overlaps the declaration's start
	{ID: "w2", Path: "a.go", Start: 11, End: 19}, // wholly inside the declaration
	{ID: "w3", Path: "a.go", Start: 18, End: 30}, // overlaps the declaration's end
	{ID: "w4", Path: "a.go", Start: 31, End: 40}, // disjoint
	{ID: "b1", Path: "b.go", Start: 10, End: 20}, // the same range, another file
}

func TestGoldIsEverySpanOverlappingTheDeclaration(t *testing.T) {
	lenient, strict := Gold("a.go", 10, 20, overlapShapes)
	if want := []string{"w1", "w2", "w3"}; !reflect.DeepEqual(lenient, want) {
		t.Errorf("gold set is %v, want %v", lenient, want)
	}
	// w2 overlaps 9 lines, w1 and w3 three each.
	if strict != "w2" {
		t.Errorf("strict gold is %q, want w2", strict)
	}
}

func TestGoldIncludesAWindowWhollyInsideTheDeclaration(t *testing.T) {
	lenient, strict := Gold("a.go", 5, 40, []Span{{ID: "w", Path: "a.go", Start: 11, End: 19}})
	if !reflect.DeepEqual(lenient, []string{"w"}) || strict != "w" {
		t.Errorf("gold set for decl 5..40 against window 11..19 is %v/%q, want [w]/w", lenient, strict)
	}
}

func TestGoldIncludesADeclarationWhollyInsideAWindow(t *testing.T) {
	lenient, strict := Gold("a.go", 12, 14, []Span{{ID: "w1", Path: "a.go", Start: 1, End: 40}})
	if !reflect.DeepEqual(lenient, []string{"w1"}) || strict != "w1" {
		t.Errorf("gold set for decl 12..14 against window 1..40 is %v/%q, want [w1]/w1", lenient, strict)
	}
	// The reverse fixture alone passes under a containment spelling, which is
	// why both are here rather than one "overlap works" test.
	if l, _ := Gold("a.go", 1, 40, []Span{{ID: "w1", Path: "a.go", Start: 12, End: 14}}); !reflect.DeepEqual(l, []string{"w1"}) {
		t.Errorf("the reverse containment gave %v, want [w1]", l)
	}
}

func TestGoldExcludesASpanInAnotherFile(t *testing.T) {
	lenient, _ := Gold("a.go", 10, 20, []Span{
		{ID: "a1", Path: "a.go", Start: 10, End: 20},
		{ID: "b1", Path: "b.go", Start: 10, End: 20},
	})
	if want := []string{"a1"}; !reflect.DeepEqual(lenient, want) {
		t.Errorf("gold set is %v, want %v: two files have lines 10-20", lenient, want)
	}
}

func TestStrictGoldIsTheLargestOverlapAndNotTheFirst(t *testing.T) {
	lenient, strict := Gold("a.go", 10, 20, []Span{
		{ID: "first", Path: "a.go", Start: 1, End: 11}, // overlap 2, sorts first
		{ID: "big", Path: "a.go", Start: 5, End: 25},   // overlap 11
	})
	if want := []string{"first", "big"}; !reflect.DeepEqual(lenient, want) {
		t.Errorf("gold set is %v, want %v", lenient, want)
	}
	if strict != "big" {
		t.Errorf("strict gold is %q, want big: 11 overlapping lines against 2", strict)
	}

	// Equal overlap: the lower start line wins, then the span id. Never map
	// order — the strict gold set has to be the same in two runs over an
	// unchanged corpus.
	_, strict = Gold("a.go", 10, 20, []Span{
		{ID: "late", Path: "a.go", Start: 16, End: 22}, // overlap 5
		{ID: "early", Path: "a.go", Start: 8, End: 14}, // overlap 5
	})
	if strict != "early" {
		t.Errorf("strict gold on an overlap tie is %q, want early (start line 8 against 16)", strict)
	}
	_, strict = Gold("a.go", 10, 20, []Span{
		{ID: "zzz", Path: "a.go", Start: 8, End: 14},
		{ID: "aaa", Path: "a.go", Start: 8, End: 14},
	})
	if strict != "aaa" {
		t.Errorf("strict gold on an identical-range tie is %q, want aaa", strict)
	}
}

// M1.2's behavioural half, and it lives here because it needs both packages:
// golden's ranges and the chunker's spans. The fixture is golden's own
// long.gotxt, read across the package boundary rather than copied, so the
// seven-line doc comment and the geometry measured for it cannot drift apart.
//
// Under the window arm the raw range reaches one tile further back than the
// stripped one does, so a harness that harvested gold from unstripped source
// would score against a gold set one span too large. Under the AST arm it does
// not: the sub-windows start at the declaration's own start line, so they move
// with the range and the count does not change at any doc-comment length.
func TestTheGoldSetIsTheSpansThatOverlapTheDeclaration(t *testing.T) {
	src, err := os.ReadFile("../golden/testdata/long.gotxt")
	if err != nil {
		t.Fatalf("reading golden's fixture: %v", err)
	}
	cases, _, err := golden.Generate("long.go", src)
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	if len(cases) != 1 {
		t.Fatalf("the fixture produced %d cases, want 1", len(cases))
	}
	c := cases[0]
	// Errorf, not Fatalf: under a harness that harvested gold from unstripped
	// source this is the first thing to go, and stopping here would leave the
	// gold-set assertion below — the behavioural half — unrun.
	if c.Start != 10 || c.End != 33 || c.RawStart != 3 || c.RawEnd != 33 {
		t.Errorf("the fixture's geometry moved: %d..%d stripped, %d..%d raw; want 10..33 and 3..33", c.Start, c.End, c.RawStart, c.RawEnd)
	}
	strippedSrc, err := chunk.StripDocs("long.go", src)
	if err != nil {
		t.Fatalf("StripDocs: %v", err)
	}

	opt := chunk.Options{Strategy: chunk.StrategyWindow, WindowLines: 8, WindowOverlap: 2, MaxDeclLines: 10}
	for _, arm := range []struct {
		name             string
		strategy         chunk.Strategy
		stripped, raw    int
		discriminates    bool
		strippedRangeIDs []string
	}{
		{name: "window", strategy: chunk.StrategyWindow, stripped: 5, raw: 6, discriminates: true},
		{name: "ast", strategy: chunk.StrategyAST, stripped: 4, raw: 4, discriminates: false},
	} {
		o := opt
		o.Strategy = arm.strategy
		chunks, _, err := chunk.Chunks("long.go", strippedSrc, o)
		if err != nil {
			t.Fatalf("Chunks: %v", err)
		}
		spans := make([]Span, 0, len(chunks))
		for i, ch := range chunks {
			spans = append(spans, Span{ID: string(rune('a' + i)), Path: "long.go", Start: ch.StartLine, End: ch.EndLine})
		}
		got, _ := Gold("long.go", c.Start, c.End, spans)
		rawGot, _ := Gold("long.go", c.RawStart, c.RawEnd, spans)
		if len(got) != arm.stripped {
			t.Errorf("%s gold set for the stripped range %d..%d has %d spans %v over %v, want %d",
				arm.name, c.Start, c.End, len(got), got, ranges(spans), arm.stripped)
		}
		if len(rawGot) != arm.raw {
			t.Errorf("%s gold set for the raw range %d..%d has %d spans %v, want %d",
				arm.name, c.RawStart, c.RawEnd, len(rawGot), rawGot, arm.raw)
		}
		if arm.discriminates == reflect.DeepEqual(got, rawGot) {
			t.Errorf("%s arm: gold sets under the raw and stripped ranges are %v and %v; discriminates=%v",
				arm.name, rawGot, got, arm.discriminates)
		}
	}
}

func ranges(spans []Span) []string {
	out := make([]string, 0, len(spans))
	for _, s := range spans {
		out = append(out, s.ID+":"+itoa(s.Start)+".."+itoa(s.End))
	}
	return out
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b []byte
	for n > 0 {
		b = append([]byte{byte('0' + n%10)}, b...)
		n /= 10
	}
	return string(b)
}
