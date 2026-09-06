package agent

import (
	"context"
	"reflect"
	"strings"
	"testing"

	"github.com/mralaminahamed/codetrail/packages/shared/llm"
	"github.com/mralaminahamed/codetrail/packages/shared/models"
)

func spans(ids ...string) []models.Span {
	out := make([]models.Span, 0, len(ids))
	for _, id := range ids {
		out = append(out, models.Span{ID: id})
	}
	return out
}

func TestAMarkerResolvesToTheSpanReadAtThatPosition(t *testing.T) {
	read := spans("read-a", "read-b")
	text, order, dropped := Resolve("first [1] and second [2]", read)
	if text != "first [1] and second [2]" {
		t.Errorf("text was rewritten: %q", text)
	}
	if !reflect.DeepEqual(order, []int{0, 1}) {
		t.Errorf("order = %v, want [0 1]", order)
	}
	if dropped != 0 {
		t.Errorf("dropped %d, want 0", dropped)
	}
	if got := read[order[0]].ID; got != "read-a" {
		t.Errorf("marker 1 resolved span %q, want %q", got, "read-a")
	}
	// Order of first appearance, not sorted and not insertion order of the read
	// set: a fixture whose markers appear in ascending order could not tell them
	// apart.
	_, order2, _ := Resolve("second [2] then first [1] then [2] again", read)
	if !reflect.DeepEqual(order2, []int{1, 0}) {
		t.Errorf("order = %v, want [1 0]", order2)
	}
}

// [1] and [9] where the loop read two spans. A fixture with only valid markers
// cannot fire.
func TestAMarkerForASpanTheLoopNeverReadIsStrippedAndCounted(t *testing.T) {
	text, order, dropped := Resolve("real [1] and invented [9]", spans("read-a", "read-b"))
	if strings.Contains(text, "[9]") {
		t.Errorf("answer still contains %q; citations_dropped %d", "[9]", dropped)
	}
	if dropped != 1 {
		t.Errorf("citations_dropped %d, want 1", dropped)
	}
	if !strings.Contains(text, "[1]") {
		t.Errorf("the valid marker was stripped too: %q", text)
	}
	if !reflect.DeepEqual(order, []int{0}) {
		t.Errorf("order = %v, want [0]", order)
	}
	// Both returns, so a mutant that counts without rewriting also fails.
	if text == "real [1] and invented [9]" {
		t.Errorf("answer still contains %q although citations_dropped is %d", "[9]", dropped)
	}

	// NOTHING IS RENUMBERED. Stripping [9] leaves [2] meaning read[1], because
	// markers are positional and read is what they index.
	//
	// The surviving marker here is deliberately NOT the first one: with a text
	// whose only survivor is [1], renumbering produces [1] as well and the
	// mutation is invisible — measured, it survived the whole branch.
	text2, order2, dropped2 := Resolve("invented [9] then real [2]", spans("read-a", "read-b"))
	if !strings.Contains(text2, "[2]") {
		t.Errorf("Resolve renumbered the surviving marker: %q", text2)
	}
	if strings.Contains(text2, "[1]") {
		t.Errorf("Resolve introduced a marker the model did not write: %q", text2)
	}
	if !reflect.DeepEqual(order2, []int{1}) || dropped2 != 1 {
		t.Errorf("Resolve = (%q, %v, %d), want the second span cited once", text2, order2, dropped2)
	}
}

func TestAMarkerOfZeroIsNotASpan(t *testing.T) {
	text, order, dropped := Resolve("nothing [0] here [1]", spans("read-a"))
	if strings.Contains(text, "[0]") || dropped != 1 || !reflect.DeepEqual(order, []int{0}) {
		t.Errorf("Resolve(%q) = (%q, %v, %d)", "nothing [0] here [1]", text, order, dropped)
	}
}

func TestResolveWithNothingReadDropsEveryMarker(t *testing.T) {
	text, order, dropped := Resolve("claims [1] and [2]", nil)
	if len(order) != 0 || dropped != 2 || strings.Contains(text, "[") {
		t.Errorf("Resolve = (%q, %v, %d)", text, order, dropped)
	}
}

// Both shapes: prose with no markers at all (the common case) and prose whose
// only marker is unresolvable (the injection case). A mutant might handle one.
func TestAnAnswerThatResolvesNoCitationIsUncited(t *testing.T) {
	for name, final := range map[string]string{
		"no markers at all":         "I read the code and it looks fine.",
		"only unresolvable markers": "The comment says it is safe [7].",
	} {
		t.Run(name, func(t *testing.T) {
			f := llm.NewFake(readTurn("1", "read-a"), llm.Turn{Text: final})
			a := Run(context.Background(), f, readTools(), testBounds(), "q")
			if a.Trace.Stop != StopUncited {
				t.Errorf("stop %q with %d citations, want %q", a.Trace.Stop, len(a.Cited), StopUncited)
			}
			if a.Text != "" {
				t.Errorf("an uncited answer was served: %q", a.Text)
			}
		})
	}
}

// A search whose hits the loop then does NOT open in rank order: it searches
// (ranking one-a first, one-b second) and opens only one-b. The two lists
// therefore disagree at position 1, which is what separates resolving against
// Read from resolving against the search results. Two lists that agree cannot.
//
// Driven through the REAL tool set rather than a stub, so a production change
// that made a hit count as a read is visible here and not only in the tool
// test.
func TestAMarkerResolvesAgainstTheReadSetAndNotTheSearchResults(t *testing.T) {
	c := twoRepos()
	f := llm.NewFake(
		llm.Turn{Calls: []llm.ToolCall{{ID: "s", Name: "search_code", Args: []byte(`{"q":"Get"}`)}}},
		llm.Turn{Calls: []llm.ToolCall{{ID: "r", Name: "read_span", Args: []byte(`{"span_id":"one-b"}`)}}},
		llm.Turn{Text: "the only thing I opened [1]"},
	)
	a := Run(context.Background(), f, NewTools(c, "repo-1", DefaultToolLimits()), testBounds(), "q")
	if a.Trace.Stop != StopFinal {
		t.Fatalf("stop %q, want final", a.Trace.Stop)
	}
	if want := []string{"one-b"}; !reflect.DeepEqual(a.ReadIDs(), want) {
		t.Fatalf("Read = %v, want %v: a search result is not a read", a.ReadIDs(), want)
	}
	if len(a.Cited) != 1 {
		t.Fatalf("cited %v, want one", a.Cited)
	}
	if got := a.Read[a.Cited[0]].ID; got != "one-b" {
		t.Errorf("marker 1 resolved span %q, want %q", got, "one-b")
	}
}

func TestTheDroppedCountReachesTheTrace(t *testing.T) {
	f := llm.NewFake(readTurn("1", "read-a"), llm.Turn{Text: "real [1], invented [4] and [5]"})
	a := Run(context.Background(), f, readTools(), testBounds(), "q")
	if a.Trace.Stop != StopFinal {
		t.Fatalf("stop %q, want final", a.Trace.Stop)
	}
	if a.Trace.CitationsDropped != 2 {
		t.Errorf("trace.citations_dropped %d, want 2", a.Trace.CitationsDropped)
	}
	if strings.Contains(a.Text, "[4]") || strings.Contains(a.Text, "[5]") {
		t.Errorf("answer still carries an invented marker: %q", a.Text)
	}
}
