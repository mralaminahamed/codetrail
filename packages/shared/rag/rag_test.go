package rag

import "testing"

// RETRIEVAL_MODE is validated at boot, so ParseMode is what stands between a
// typo and a service that retrieves differently than it was asked to. The
// empty string is the case a deployment hits by not setting the variable, and
// it must not parse: the default belongs to the caller, which is the only
// place it can be logged.
func TestParseModeIsAClosedSet(t *testing.T) {
	for _, s := range []string{"vector", "lexical", "hybrid"} {
		m, err := ParseMode(s)
		if err != nil || string(m) != s {
			t.Fatalf("ParseMode(%q) = %q, %v", s, m, err)
		}
	}
	for _, s := range []string{"", "Hybrid", "both", "vector,lexical"} {
		if m, err := ParseMode(s); err == nil {
			t.Fatalf("ParseMode(%q) accepted, returning %q", s, m)
		}
	}
}
