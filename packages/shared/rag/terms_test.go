package rag

import (
	"fmt"
	"strings"
	"testing"
	"unicode"
)

func eqTerms(t *testing.T, got, want []string) {
	t.Helper()
	// %q, not %v: one term holding a whole sentence prints the same as the
	// several terms it should have been split into, which is a failure message
	// that reads "got X, want X".
	if len(got) != len(want) {
		t.Fatalf("got %d terms %q, want %d %q", len(got), got, len(want), want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("got %q, want %q", got, want)
		}
	}
}

// Postgres holds parseConfig as one token — it does not split camel case — so
// the whole run is the only term that can reach a symbol. The parts come after
// it and widen the arm towards prose; a reader can see which is which from the
// order.
func TestTermsKeepsTheWholeIdentifierAndItsParts(t *testing.T) {
	eqTerms(t, Terms("parseConfig", true), []string{"parseconfig", "parse", "config"})
	// An acronym run breaks before its last capital, not after its first:
	// HTTPServer is HTTP and Server, not H and TTPServer.
	eqTerms(t, Terms("HTTPServer", true), []string{"httpserver", "http", "server"})
	// A digit starts no part. The index holds v2 as one token, so splitting it
	// would invent two terms it has never seen.
	eqTerms(t, Terms("v2", true), []string{"v2"})
}

// Postgres already splits on the underscore and already binds the dot, so the
// terms have to agree with the index rather than with Go's idea of a word.
// Measured on pg17's simple configuration: parse_config is the two tokens
// 'parse' and 'config'; x.y is the single token 'x.y'.
func TestTermsSplitsWhereTheIndexSplits(t *testing.T) {
	eqTerms(t, Terms("parse_config", false), []string{"parse", "config"})
	// The dot is the asymmetry: the index holds x.y whole and Terms cannot
	// spell it, so a dotted name is reachable only through its parts. That is
	// why 0008 takes the dots out of symbol on the index side.
	eqTerms(t, Terms("x.y", false), []string{"x", "y"})
}

// &, |, !, : and ( are tsquery operators and a question about code contains
// them. This is where they stop being operators — the alternative is a 42601
// for most of the questions this product exists to answer.
func TestTermsDropsPunctuationSoAQuestionIsNotASyntaxError(t *testing.T) {
	got := Terms("where is parseConfig() defined? (a & b)", false)
	eqTerms(t, got, []string{"where", "is", "parseconfig", "defined", "a", "b"})
	for _, term := range got {
		for _, r := range term {
			if !unicode.IsLetter(r) && !unicode.IsDigit(r) {
				t.Fatalf("term %q carries %q, which to_tsquery reads as syntax", term, r)
			}
		}
	}
	// The pure-punctuation case is what embed.Fake errors on, so it has to be
	// distinguishable here rather than three layers down.
	if got := Terms("??? &|!", true); len(got) != 0 {
		t.Fatalf("got %v, want no terms at all", got)
	}
}

func TestTermsWithSplittingOffKeepsOnlyWholeIdentifiers(t *testing.T) {
	eqTerms(t, Terms("parseConfig HTTPServer", false), []string{"parseconfig", "httpserver"})
}

func TestTermsIsCappedAndDeduped(t *testing.T) {
	// One identifier spelled three ways is one term: the index is lowercased,
	// so the three are the same token and OR-ing it with itself is noise.
	eqTerms(t, Terms("parseConfig parseconfig PARSECONFIG", false), []string{"parseconfig"})

	var b strings.Builder
	for i := range 40 {
		fmt.Fprintf(&b, "wordOne%d ", i)
	}
	got := Terms(b.String(), true)
	if len(got) != maxTerms {
		t.Fatalf("%d terms from 40 split identifiers, want the cap of %d", len(got), maxTerms)
	}
	// The cap must not be reached by repeating one term: dedup runs first.
	if got[0] != "wordone0" || got[1] != "word" {
		t.Fatalf("terms start %v, want the whole run then its parts", got[:2])
	}
}
