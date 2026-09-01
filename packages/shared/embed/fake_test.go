package embed

import (
	"context"
	"math"
	"reflect"
	"strings"
	"testing"
)

func dot(a, b []float32) float64 {
	var sum float64
	for i := range a {
		sum += float64(a[i]) * float64(b[i])
	}
	return sum
}

// Deterministic means across processes and runs, not merely within one call.
// A fake that drifted would make the CI eval's numbers unreproducible and the
// mechanics check would be measuring the fake.
func TestFakeIsDeterministic(t *testing.T) {
	a, b := NewFake(768), NewFake(768)
	x, err := a.Embed(context.Background(), []string{"func (s *Store) PutSpans"})
	if err != nil {
		t.Fatal(err)
	}
	y, err := b.Embed(context.Background(), []string{"func (s *Store) PutSpans"})
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(x, y) {
		t.Fatal("two Fakes gave different vectors for the same text")
	}
}

// The vector is pinned by value, not merely against another Fake in the same
// process: a change to the tokeniser or the hash would leave the two-Fakes
// comparison above passing while every committed eval number moved.
func TestFakeVectorIsStableAcrossBuilds(t *testing.T) {
	v, err := NewFake(768).Embed(context.Background(), []string{"func (s *Store) PutSpans"})
	if err != nil {
		t.Fatal(err)
	}
	// Measured, not predicted: four tokens (func, s, store, putspans), one
	// occurrence each, so every set bucket is 1/sqrt(4).
	want := map[int]float32{34: 0.5, 423: 0.5, 543: 0.5, 654: 0.5}
	for i, f := range v[0] {
		w := want[i]
		if math.Abs(float64(f-w)) > 1e-6 {
			t.Fatalf("dimension %d is %v, want %v", i, f, w)
		}
	}
}

func TestFakeVectorsAreUnitLengthAndTheRightWidth(t *testing.T) {
	got, err := NewFake(768).Embed(context.Background(), []string{"alpha beta", "gamma"})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("got %d vectors for 2 texts", len(got))
	}
	for i, v := range got {
		if len(v) != 768 {
			t.Fatalf("vector %d has width %d, want 768", i, len(v))
		}
		var sum float64
		for _, f := range v {
			sum += float64(f) * float64(f)
		}
		if math.Abs(sum-1) > 1e-5 {
			t.Fatalf("vector %d has squared norm %v, want 1", i, sum)
		}
	}
}

// The property Task 7's end-to-end assertion needs: text that shares words
// with a document must score higher against it than against an unrelated one.
// Without this the fake proves only that no error was returned.
func TestFakeVectorsCarryLexicalSimilarity(t *testing.T) {
	f := NewFake(768)
	v, err := f.Embed(context.Background(), []string{
		"PutSpans writes spans and their embeddings in one transaction",
		"func (s *Store) PutSpans(ctx context.Context, spans []EmbeddedSpan) error { tx := begin() }",
		"the admission policy refuses every scheme but https",
	})
	if err != nil {
		t.Fatal(err)
	}
	near, far := dot(v[0], v[1]), dot(v[0], v[2])
	if near <= far {
		t.Fatalf("similar texts scored %v, unrelated scored %v", near, far)
	}
}

// Every text in a batch gets its own vector. A loop that embedded texts[0] n
// times would satisfy determinism, width and norm, and only this notices.
func TestFakeDistinguishesDifferentText(t *testing.T) {
	f := NewFake(768)
	v, err := f.Embed(context.Background(), []string{"alpha", "beta"})
	if err != nil {
		t.Fatal(err)
	}
	if reflect.DeepEqual(v[0], v[1]) {
		t.Fatal("two different texts got the same vector")
	}
}

// Position in the batch must not change a text's vector: the indexer pairs
// vector i with span i, so a batching bug that shifted them would file every
// span's text under its neighbour's embedding and nothing would look wrong.
func TestFakeBatchOrderMatchesInputOrder(t *testing.T) {
	f := NewFake(768)
	batch, err := f.Embed(context.Background(), []string{"alpha", "beta", "gamma"})
	if err != nil {
		t.Fatal(err)
	}
	for i, text := range []string{"alpha", "beta", "gamma"} {
		one, err := f.Embed(context.Background(), []string{text})
		if err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(batch[i], one[0]) {
			t.Fatalf("%q at position %d differs from the same text embedded alone", text, i)
		}
	}
}

// A corpus built with the fake has to be identifiable in the database. The
// model name is written to every span row, so "these numbers came from the
// fake" is a fact a reader can check rather than a thing they must remember.
func TestFakeNamesItself(t *testing.T) {
	if m := NewFake(768).Model(); !strings.Contains(m, "fake") {
		t.Fatalf("model name %q does not say it is a fake", m)
	}
	if d := NewFake(768).Dim(); d != 768 {
		t.Fatalf("Dim() is %d, want 768", d)
	}
}

// A text with no tokens has no bag of words, and a zero vector's cosine
// distance to everything is undefined — pgvector returns NaN, which sorts
// unpredictably. Refuse it here where the message can say why.
func TestFakeRefusesTextWithNoTokens(t *testing.T) {
	if _, err := NewFake(768).Embed(context.Background(), []string{"   \n\t"}); err == nil {
		t.Fatal("a text with no tokens produced a vector")
	}
}

// A non-positive width would divide by zero in the bucket choice. An error
// beats the panic that a mis-wired dim would otherwise become.
func TestFakeRefusesANonPositiveWidth(t *testing.T) {
	if _, err := NewFake(0).Embed(context.Background(), []string{"alpha"}); err == nil {
		t.Fatal("a zero-width Fake produced a vector")
	}
}
