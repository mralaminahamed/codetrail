package embed

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// Ollama embeds against a local Ollama server.
//
// Endpoint and shapes are measured, not assumed: against ollama 0.33.0 with
// nomic-embed-text, POST /api/embed with {"model","input":[…]} answers
// {"model","embeddings":[[…]],…} — one 768-wide vector per input, already
// unit-length. The older /api/embeddings is still served by that image and
// takes one "prompt" for one "embedding"; this client does not speak it,
// because /api/embed batches and the indexer embeds a file's spans together.
type Ollama struct {
	baseURL string
	model   string
	dim     int
	client  *http.Client
}

// NewOllama takes the width as configuration so a caller can hand the same
// number to store.CheckDim at startup, rather than each side deciding for
// itself and disagreeing only in the data.
func NewOllama(baseURL, model string, dim int, timeout time.Duration) *Ollama {
	return &Ollama{
		baseURL: strings.TrimRight(baseURL, "/"),
		model:   model,
		dim:     dim,
		client:  &http.Client{Timeout: timeout},
	}
}

func (o *Ollama) Model() string { return o.model }

func (o *Ollama) Dim() int { return o.dim }

func (o *Ollama) Embed(ctx context.Context, texts []string) ([][]float32, error) {
	if len(texts) == 0 {
		return nil, nil
	}
	body, err := json.Marshal(struct {
		Model string   `json:"model"`
		Input []string `json:"input"`
	}{Model: o.model, Input: texts})
	if err != nil {
		return nil, fmt.Errorf("embed: ollama request: %w", err)
	}
	url := o.baseURL + "/api/embed"
	// WithContext, so the job's one deadline reaches the model. Without it a
	// hung generation outlives the job that owns it and only the client's own
	// timeout ever ends the call.
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("embed: ollama request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := o.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("embed: ollama %s: %w", url, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		// Before the decode, not after. Measured against the real server with
		// this check removed: a 404 for a model that was never pulled — the
		// common misconfiguration — has a JSON body that decodes cleanly into
		// the struct below, and the failure then surfaces as "returned 0
		// embeddings for 1 inputs", which names the wrong cause entirely.
		snippet, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return nil, fmt.Errorf("embed: ollama %s: http %d: %s", url, resp.StatusCode, strings.TrimSpace(string(snippet)))
	}

	var out struct {
		Embeddings [][]float32 `json:"embeddings"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, fmt.Errorf("embed: ollama %s: decode: %w", url, err)
	}
	if len(out.Embeddings) != len(texts) {
		return nil, fmt.Errorf("embed: ollama returned %d embeddings for %d inputs", len(out.Embeddings), len(texts))
	}
	for i, v := range out.Embeddings {
		if len(v) != o.dim {
			return nil, fmt.Errorf("embed: ollama %s returned a %d-wide vector for input %d, want %d", o.model, len(v), i, o.dim)
		}
	}
	return out.Embeddings, nil
}
