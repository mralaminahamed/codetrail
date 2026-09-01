package embed

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"
)

// writeVectors answers the way the real server does: n vectors of width dim,
// each distinct, so a test can tell one from another.
func writeVectors(w http.ResponseWriter, dim, n int) {
	out := struct {
		Model      string      `json:"model"`
		Embeddings [][]float32 `json:"embeddings"`
	}{Model: "nomic-embed-text"}
	for i := 0; i < n; i++ {
		v := make([]float32, dim)
		v[i%dim] = 1
		out.Embeddings = append(out.Embeddings, v)
	}
	json.NewEncoder(w).Encode(out)
}

// The width is fixed at 768 and enforced (spec §3). A model swapped under a
// running deployment is exactly how two vector spaces end up in one table,
// where they rank nonsense confidently and nothing about the query looks wrong.
func TestOllamaRejectsAVectorOfTheWrongWidth(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, `{"embeddings":[[0.1,0.2,0.3]]}`)
	}))
	defer srv.Close()
	_, err := NewOllama(srv.URL, "nomic-embed-text", 768, time.Second).
		Embed(context.Background(), []string{"x"})
	if err == nil || !strings.Contains(err.Error(), "768") {
		t.Fatalf("want a width error naming 768, got %v", err)
	}
}

// One vector per input, in order. A server that returns fewer would otherwise
// pair span i's text with span j's vector, and every row would look fine.
func TestOllamaRejectsAShortResponse(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writeVectors(w, 768, 1)
	}))
	defer srv.Close()
	_, err := NewOllama(srv.URL, "nomic-embed-text", 768, time.Second).
		Embed(context.Background(), []string{"a", "b"})
	if err == nil || !strings.Contains(err.Error(), "1") || !strings.Contains(err.Error(), "2") {
		t.Fatalf("want a count error naming 1 and 2, got %v", err)
	}
}

func TestOllamaSendsModelAndInputs(t *testing.T) {
	var got struct {
		Model string   `json:"model"`
		Input []string `json:"input"`
	}
	var path, method string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path, method = r.URL.Path, r.Method
		json.NewDecoder(r.Body).Decode(&got)
		writeVectors(w, 768, len(got.Input))
	}))
	defer srv.Close()
	out, err := NewOllama(srv.URL, "nomic-embed-text", 768, time.Second).
		Embed(context.Background(), []string{"a", "b"})
	if err != nil {
		t.Fatal(err)
	}
	if path != "/api/embed" || method != http.MethodPost {
		t.Fatalf("sent %s %s, want POST /api/embed", method, path)
	}
	if got.Model != "nomic-embed-text" || !reflect.DeepEqual(got.Input, []string{"a", "b"}) {
		t.Fatalf("sent %+v", got)
	}
	// The server's two vectors are distinguishable on purpose: this is what
	// catches a client that returns the first vector for every input.
	if out[0][0] != 1 || out[1][1] != 1 {
		t.Fatalf("vectors came back out of order or duplicated: %v %v", out[0][:3], out[1][:3])
	}
}

// A 404 from an older server that only has /api/embeddings must be an error
// naming the status, not an empty result the indexer writes as null vectors.
func TestOllamaReportsANon200(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		io.WriteString(w, `404 page not found`)
	}))
	defer srv.Close()
	_, err := NewOllama(srv.URL, "nomic-embed-text", 768, time.Second).
		Embed(context.Background(), []string{"x"})
	// "http 404", not "404": with the status check removed the body decodes as
	// a bare JSON number and the error names that instead, but httptest's port
	// is random and can itself contain 404. The prefix cannot.
	if err == nil || !strings.Contains(err.Error(), "http 404") {
		t.Fatalf("want an error naming http 404, got %v", err)
	}
}

// The job's deadline has to reach the HTTP call, or a hung model outlives the
// job that owns it.
func TestOllamaHonoursContextCancellation(t *testing.T) {
	// The handler drains the request body before blocking, and the test
	// releases it before closing the server. Measured on Go 1.27: a handler
	// that blocks on r.Context().Done() without reading the body never wakes,
	// because net/http only starts the background read that notices a
	// disconnected client once the body hits EOF. srv.Close() then waits on
	// that handler and the whole package times out.
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.Copy(io.Discard, r.Body)
		select {
		case <-r.Context().Done():
		case <-release:
		}
	}))
	defer srv.Close()
	defer close(release)
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	start := time.Now()
	// The client timeout is a minute so that only the context can end this in
	// under five seconds: two timeouts of the same size would be
	// indistinguishable and the test would prove nothing.
	_, err := NewOllama(srv.URL, "m", 768, time.Minute).Embed(ctx, []string{"x"})
	if err == nil {
		t.Fatal("want a deadline error")
	}
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Fatalf("returned after %v; the context did not reach the request", elapsed)
	}
}

func TestOllamaNamesItsModelAndWidth(t *testing.T) {
	o := NewOllama("http://x", "nomic-embed-text", 768, time.Second)
	if o.Model() != "nomic-embed-text" || o.Dim() != 768 {
		t.Fatalf("Model()=%q Dim()=%d", o.Model(), o.Dim())
	}
}

// An empty batch must not become a request. Ollama answers a null embeddings
// list for one, which the count check would then report as an error on a
// caller that simply had nothing to embed.
func TestOllamaSkipsTheCallForAnEmptyBatch(t *testing.T) {
	called := false
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
		// A well-formed empty answer, so dropping the short circuit fails on
		// "a request was made" rather than on a decode error that would kill
		// the mutant for the wrong reason.
		io.WriteString(w, `{"embeddings":[]}`)
	}))
	defer srv.Close()
	got, err := NewOllama(srv.URL, "m", 768, time.Second).Embed(context.Background(), nil)
	if err != nil || len(got) != 0 {
		t.Fatalf("got %v, %v", got, err)
	}
	if called {
		t.Fatal("an empty batch was posted to the server")
	}
}
