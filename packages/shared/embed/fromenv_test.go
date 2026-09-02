package embed

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// probeWidth is the width these tests build embedders at. Not the schema's
// 768: nothing here is written to a column, and a narrow vector keeps the
// fixtures readable.
const probeWidth = 8

var errWidth = errors.New("test: wrong width")

func checkWidth(dim int) error {
	if dim != probeWidth {
		return errWidth
	}
	return nil
}

// answersOneVector is an Ollama that has the model and returns the width it
// was asked for, which is what a healthy boot probe finds.
func answersOneVector(t *testing.T) string {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		v := make([]float32, probeWidth)
		v[0] = 1
		json.NewEncoder(w).Encode(struct {
			Embeddings [][]float32 `json:"embeddings"`
		}{[][]float32{v}})
	}))
	t.Cleanup(srv.Close)
	return srv.URL
}

// The width is checked at boot, not discovered at the first insert of the first
// job — by which time a clone, a walk and a whole embed pass have been spent.
//
// The check is the caller's, because the column it is about belongs to store.
// This asserts FromEnv runs it and says which knob was wrong.
func TestFromEnvRefusesTheWrongWidth(t *testing.T) {
	t.Setenv("EMBED_PROVIDER", "fake")
	t.Setenv("EMBED_DIM", "7")
	_, err := FromEnv(context.Background(), probeWidth, checkWidth, time.Minute)
	if !errors.Is(err, errWidth) {
		t.Fatalf("want the caller's width error, got %v", err)
	}
	if !strings.Contains(err.Error(), "EMBED_DIM=7") {
		t.Fatalf("the error does not name the knob that is wrong: %v", err)
	}
}

// Before the round trip, not after: a wrong width otherwise spends a probe and
// blames EMBED_MODEL for an EMBED_DIM fault. The server fails the test if it is
// reached at all.
func TestTheWidthIsCheckedBeforeTheProbe(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		t.Error("the embedder was probed before its width was checked")
	}))
	defer srv.Close()
	t.Setenv("EMBED_PROVIDER", "ollama")
	t.Setenv("OLLAMA_URL", srv.URL)
	t.Setenv("EMBED_DIM", "7")
	if _, err := FromEnv(context.Background(), probeWidth, checkWidth, time.Minute); !errors.Is(err, errWidth) {
		t.Fatalf("want the caller's width error, got %v", err)
	}
}

// Both providers are constructible, and nothing else is: a typo in
// EMBED_PROVIDER must not fall back to one of them silently.
func TestFromEnvProviders(t *testing.T) {
	// The ollama arm needs a server, because constructing it proves it
	// answers; the fake arm ignores the URL entirely.
	t.Setenv("OLLAMA_URL", answersOneVector(t))
	for _, tc := range []struct{ provider, model string }{
		{"fake", "fake-hashed-bow"},
		{"ollama", "nomic-embed-text"},
	} {
		t.Setenv("EMBED_PROVIDER", tc.provider)
		e, err := FromEnv(context.Background(), probeWidth, checkWidth, time.Minute)
		if err != nil {
			t.Fatalf("%s: %v", tc.provider, err)
		}
		if e.Model() != tc.model || e.Dim() != probeWidth {
			t.Errorf("%s: model %q dim %d, want %q and %d", tc.provider, e.Model(), e.Dim(), tc.model, probeWidth)
		}
	}
	t.Setenv("EMBED_PROVIDER", "olama")
	if _, err := FromEnv(context.Background(), probeWidth, checkWidth, time.Minute); err == nil {
		t.Fatal("a misspelt provider was accepted")
	}
}

// The README calls every knob boot-validated, and these two were not.
// Measured before: `OLLAMA_URL='not a url at all' EMBED_MODEL='no-such-model-xyz'`
// logged "indexer up" and the failure surfaced on the first leased job's embed
// call, spending an attempt against the cap for a setting no retry can fix.
//
// Each case names the knob that is wrong, because "connection refused" on a
// job is what this is replacing.
func TestTheEmbedderAddressAndModelAreValidatedAtBoot(t *testing.T) {
	missingModel := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// The real server's answer for a model that was never pulled: a 404
		// whose body is JSON and decodes cleanly into the success shape.
		w.WriteHeader(http.StatusNotFound)
		io.WriteString(w, `{"error":"model \"no-such-model-xyz\" not found"}`)
	}))
	defer missingModel.Close()
	// A listener that is closed: the address is well formed and nothing is
	// there, which is the misconfiguration a typo'd port makes.
	gone := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	goneURL := gone.URL
	gone.Close()

	const badAddress = "is not an http:// or https:// address"
	// Each want is the part of the message that could only have come from the
	// check being tested: "an error happened" would pass against a client that
	// merely failed differently later.
	for _, tc := range []struct{ name, url, model, want string }{
		{"not an address", "not a url at all", "nomic-embed-text", badAddress},
		{"no scheme", "localhost:11435", "nomic-embed-text", badAddress},
		{"no host", "http://", "nomic-embed-text", badAddress},
		{"model never pulled", missingModel.URL, "no-such-model-xyz", `EMBED_MODEL="no-such-model-xyz"`},
		{"nothing listening", goneURL, "nomic-embed-text", `EMBED_MODEL="nomic-embed-text" at OLLAMA_URL=`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("EMBED_PROVIDER", "ollama")
			t.Setenv("OLLAMA_URL", tc.url)
			t.Setenv("EMBED_MODEL", tc.model)
			_, err := FromEnv(context.Background(), probeWidth, checkWidth, time.Minute)
			if err == nil {
				t.Fatalf("%s booted", tc.name)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("the error does not name %s: %v", tc.want, err)
			}
		})
	}
}

// A server that answers the wrong width is the failure the probe exists for:
// the model is there, the address is right, and every vector it writes would
// be unrankable against the column.
//
// The complaint comes from the client rather than from probe's own width
// check — measured: Ollama.Embed checks each vector before returning — so what
// probe adds here is the EMBED_MODEL/OLLAMA_URL prefix that says which knob to
// look at. Its own check stays for an Embedder that does not check itself.
func TestAProbeThatAnswersTheWrongWidthRefusesToBoot(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(struct {
			Embeddings [][]float32 `json:"embeddings"`
		}{[][]float32{make([]float32, probeWidth+1)}})
	}))
	defer srv.Close()
	t.Setenv("EMBED_PROVIDER", "ollama")
	t.Setenv("OLLAMA_URL", srv.URL)
	_, err := FromEnv(context.Background(), probeWidth, checkWidth, time.Minute)
	if err == nil || !strings.Contains(err.Error(), `EMBED_MODEL="nomic-embed-text"`) ||
		!strings.Contains(err.Error(), "9-wide vector") {
		t.Fatalf("want a width complaint naming the knob, got %v", err)
	}
}
