package llm

import (
	"errors"
	"fmt"
	"strings"
	"testing"
)

// One Failure of each kind, asserted as a set: a fixture with one failure
// cannot tell a correct classifier from a constant.
func TestClassifySeparatesTheFiveKindsAndSaysNoToAnythingElse(t *testing.T) {
	for _, k := range Kinds {
		got, ok := Classify(&Failure{Kind: k, Status: 429})
		if !ok {
			t.Errorf("Classify(%s) said it was not a provider failure", k)
			continue
		}
		if got != k {
			t.Errorf("Classify(%s) = %s, want %s", k, got, k)
		}
	}
	// Wrapped, because the client returns fmt.Errorf("%w") in places.
	if got, ok := Classify(fmt.Errorf("dialing: %w", &Failure{Kind: KindUnavailable})); !ok || got != KindUnavailable {
		t.Errorf("Classify(wrapped unavailable) = (%s, %v), want (%s, true)", got, ok, KindUnavailable)
	}
	// The boolean is the whole point of the second return: a bug in our own
	// code must not land in the provider-failure counters.
	if got, ok := Classify(errors.New("some bug")); ok {
		t.Errorf("Classify(errors.New(…)) = (%s, %v), want (_, false)", got, ok)
	}
	if got, ok := Classify(nil); ok {
		t.Errorf("Classify(nil) = (%s, %v), want (_, false)", got, ok)
	}
}

// A Detail of 4 KB. A short detail cannot tell a bounded copy from an unbounded
// one.
func TestAFailureCarriesItsStatusAndDetailForALogAndNotForAPayload(t *testing.T) {
	long := strings.Repeat("x", 4096)
	f := &Failure{Kind: KindUnauthorized, Status: 401, Detail: long}
	got := f.Error()
	if len(got) > maxDetail+64 {
		t.Errorf("Error() is %d bytes, want at most %d plus the prefix", len(got), maxDetail)
	}
	if !strings.Contains(got, "unauthorized") || !strings.Contains(got, "401") {
		t.Errorf("Error() = %q, want it to name the kind and the status", got)
	}
	if strings.Contains(got, long) {
		t.Errorf("Error() carried the whole 4096-byte detail")
	}
	// A failure with no status reads as a failure, not as "http 0".
	if got := (&Failure{Kind: KindDeadline}).Error(); strings.Contains(got, "http 0") {
		t.Errorf("Error() = %q, want no http 0", got)
	}
}

// Iterating Kinds is vacuous under a mutant that drops one, so the length is
// asserted against a hand-written set as well.
func TestKindsIsClosedAndEveryKindHasAStringForm(t *testing.T) {
	want := map[Kind]bool{
		KindRateLimited: true, KindUnauthorized: true, KindUnavailable: true,
		KindDeadline: true, KindMalformed: true,
	}
	if len(Kinds) != len(want) {
		t.Errorf("Kinds has %d entries, want %d: %v", len(Kinds), len(want), Kinds)
	}
	seen := map[Kind]bool{}
	for _, k := range Kinds {
		if !want[k] {
			t.Errorf("Kinds carries %q, which is not in the hand-written set", k)
		}
		if seen[k] {
			t.Errorf("Kinds carries %q twice", k)
		}
		seen[k] = true
		if k.String() != string(k) || k.String() == "" {
			t.Errorf("%q has no string form", k)
		}
	}
	for k := range want {
		if !seen[k] {
			t.Errorf("Kinds is missing %q", k)
		}
	}
}
