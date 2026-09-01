package store

import (
	"strconv"
	"strings"
	"testing"
)

// Spec §3: ids are deterministic, so a retried job converges instead of
// duplicating. The line numbers are part of the identity and must not be
// concatenable into each other.
//
// One field moves per case. A fixture that varied several at once would prove
// only that the id changed, not which field it keys on.
func TestSpanIDIsDeterministicAndSeparatesItsFields(t *testing.T) {
	a := SpanID("r", "a/b.go", 1, 23, "d")
	if a != SpanID("r", "a/b.go", 1, 23, "d") {
		t.Fatal("not deterministic")
	}
	for name, other := range map[string]string{
		// (1,23) and (12,3) concatenate to the same digits, so this is the
		// case that fails if the separator between them is gone.
		"start and end regrouped": SpanID("r", "a/b.go", 12, 3, "d"),
		"end merged into digest":  SpanID("r", "a/b.go", 1, 2, "3d"),
		"a different end line":    SpanID("r", "a/b.go", 1, 24, "d"),
		"a different start line":  SpanID("r", "a/b.go", 2, 23, "d"),
		"a different path":        SpanID("r", "a/b.gox", 1, 23, "d"),
		"a different repo":        SpanID("r2", "a/b.go", 1, 23, "d"),
		"a different digest":      SpanID("r", "a/b.go", 1, 23, "e"),
	} {
		if a == other {
			t.Errorf("%s: distinct spans share the id %s", name, a)
		}
	}
}

// The digest is the whole hash where the ids take the first 16 bytes. An id
// only has to be unique inside one repo; the digest is what spec §8 leans on
// to say retrieved text is the text that was indexed, and half a hash is a
// weaker claim for nothing gained.
func TestDigestIsTheFullSha256(t *testing.T) {
	// The empty string's sha256, so this pins the algorithm and not just the
	// width — a truncated sha512 would also be 64 hex characters.
	const wantEmpty = "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"
	if got := Digest(""); got != wantEmpty {
		t.Errorf("Digest(%q) = %s, want %s", "", got, wantEmpty)
	}
	if got := Digest("package main"); len(got) != 64 {
		t.Errorf("digest %s is %d hex characters, want 64", got, len(got))
	}
	if Digest("a") == Digest("b") {
		t.Error("different texts share a digest")
	}
	if n := len(SpanID("r", "p", 1, 2, "d")); n >= len(Digest("x")) {
		t.Errorf("the id is %d characters and the digest %d: the digest is meant to be the longer one", n, len(Digest("x")))
	}
}

func TestVectorLiteralRoundTripsFloat32(t *testing.T) {
	in := []float32{0, 1, -0.5, 3.4028235e38, 1e-7, 0.016927836}
	got := vecLiteral(in)
	if got[0] != '[' || got[len(got)-1] != ']' {
		t.Fatalf("not a pgvector literal: %s", got)
	}
	fields := strings.Split(got[1:len(got)-1], ",")
	if len(fields) != len(in) {
		t.Fatalf("literal has %d components, want %d: %s", len(fields), len(in), got)
	}
	for i, f := range fields {
		v, err := strconv.ParseFloat(f, 32)
		if err != nil || float32(v) != in[i] {
			t.Fatalf("component %d: %q parsed to %v, want %v", i, f, v, in[i])
		}
	}
}

// An empty vector still has to be a literal pgvector can parse, or a caller
// that hands over no components gets a syntax error from Postgres instead of
// the width error PutSpans is meant to raise.
func TestVectorLiteralOfNothingIsStillWellFormed(t *testing.T) {
	if got := vecLiteral(nil); got != "[]" {
		t.Fatalf("vecLiteral(nil) = %q, want []", got)
	}
}
