//go:build ollama

package embed

import (
	"context"
	"os"
	"reflect"
	"strings"
	"testing"
	"time"
)

// baseURL is the Ollama this smoke check talks to. The build tag keeps it out
// of CI on purpose: spec §9 requires the harness to be runnable with no model
// anywhere, and the fake is what makes that true.
func baseURL(t *testing.T) string {
	t.Helper()
	v := os.Getenv("OLLAMA_URL")
	if v == "" {
		// Building with -tags=ollama is already a statement that a server is
		// meant to be there. A skip then prints the same "ok" as a run, so a
		// dropped variable would look green with zero live coverage.
		if os.Getenv("CI") != "" {
			t.Fatal("OLLAMA_URL unset in CI — the ollama suite must never silently skip")
		}
		t.Skip("set OLLAMA_URL to run")
	}
	return v
}

// What the httptest tests cannot prove: that the real model returns the width
// the schema is built for. store.CheckDim refuses any other at startup, so a
// model whose width is not 768 is a stop, not something to adapt the schema to.
func TestOllamaLiveReturnsTheSchemaWidth(t *testing.T) {
	o := NewOllama(baseURL(t), "nomic-embed-text", 768, 60*time.Second)
	got, err := o.Embed(context.Background(), []string{"func Add(a, b int) int", "the admission policy"})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("got %d vectors for 2 inputs", len(got))
	}
	for i, v := range got {
		if len(v) != 768 {
			t.Fatalf("vector %d has width %d, want 768", i, len(v))
		}
	}
	if reflect.DeepEqual(got[0], got[1]) {
		t.Fatal("two different texts got the same vector")
	}
}

// The same text twice must give the same vector, or a re-index would churn
// every row and no eval number would reproduce.
func TestOllamaLiveIsRepeatable(t *testing.T) {
	o := NewOllama(baseURL(t), "nomic-embed-text", 768, 60*time.Second)
	first, err := o.Embed(context.Background(), []string{"func Add(a, b int) int"})
	if err != nil {
		t.Fatal(err)
	}
	second, err := o.Embed(context.Background(), []string{"func Add(a, b int) int"})
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(first, second) {
		t.Fatal("the same text twice gave different vectors")
	}
}

// A model that was never pulled is the common misconfiguration. Its 404 body
// is JSON and decodes cleanly, so a client without the status check reports
// "returned 0 embeddings for 1 inputs" and sends the reader after the wrong
// thing — asserting on the status is what makes this test worth running.
func TestOllamaLiveReportsAMissingModel(t *testing.T) {
	o := NewOllama(baseURL(t), "no-such-model", 768, 30*time.Second)
	_, err := o.Embed(context.Background(), []string{"x"})
	if err == nil {
		t.Fatal("a missing model produced vectors")
	}
	if !strings.Contains(err.Error(), "http 404") {
		t.Fatalf("want an error naming http 404, got %v", err)
	}
}
