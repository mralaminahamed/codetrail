package embed

import (
	"context"
	"fmt"
	"hash/fnv"
	"math"
	"strings"
	"unicode"
)

// Fake is a hashed bag of words: it embeds without a model, without a network
// and without a clock, so CI can run the whole harness (spec §9).
//
// Hashed *tokens*, not a hash of the text. A hash of the whole text gives any
// two distinct inputs near-orthogonal vectors, which proves "no error" and
// nothing else — hit@k would be noise and no end-to-end retrieval assertion
// could be written against it. Feature-hashing the tokens gives real lexical
// similarity instead, so Task 7 can assert that a query drawn from a span
// retrieves that span.
//
// The price is that its scores are lexical overlap dressed as similarity, which
// is exactly what spec §9's banner about CI figures not being quality describes.
type Fake struct{ dim int }

func NewFake(dim int) *Fake { return &Fake{dim: dim} }

// Model names itself so a corpus built with the fake is identifiable in the
// database rather than something a reader has to remember.
func (f *Fake) Model() string { return "fake-hashed-bow" }

func (f *Fake) Dim() int { return f.dim }

func (f *Fake) Embed(_ context.Context, texts []string) ([][]float32, error) {
	if f.dim <= 0 {
		return nil, fmt.Errorf("embed: fake width must be positive, got %d", f.dim)
	}
	out := make([][]float32, len(texts))
	for i, text := range texts {
		v := make([]float32, f.dim)
		var n int
		for _, tok := range tokenize(text) {
			h := fnv.New64a()
			h.Write([]byte(tok))
			v[h.Sum64()%uint64(f.dim)]++
			n++
		}
		if n == 0 {
			// A zero vector's cosine distance is undefined — pgvector answers
			// NaN, which sorts unpredictably — so refuse it where the message
			// can still say which text was empty.
			return nil, fmt.Errorf("embed: text %d has no tokens to hash", i)
		}
		var sum float64
		for _, f := range v {
			sum += float64(f) * float64(f)
		}
		norm := float32(math.Sqrt(sum))
		for j := range v {
			v[j] /= norm
		}
		out[i] = v
	}
	return out, nil
}

// tokenize cuts runs of letters and digits, lowercased. Deterministic by
// construction: source order, no map iteration, no clock, no math/rand.
func tokenize(text string) []string {
	return strings.FieldsFunc(strings.ToLower(text), func(r rune) bool {
		return !unicode.IsLetter(r) && !unicode.IsDigit(r)
	})
}
