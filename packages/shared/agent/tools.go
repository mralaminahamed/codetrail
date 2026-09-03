package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/mralaminahamed/codetrail/packages/shared/llm"
	"github.com/mralaminahamed/codetrail/packages/shared/models"
	"github.com/mralaminahamed/codetrail/packages/shared/rag"
	"github.com/mralaminahamed/codetrail/packages/shared/store"
)

// Corpus is the store surface the four tools need, as an interface for the same
// reason rag.Searcher and handler.Reader are ones: these tests run without a
// database, and every error path has to be producible on demand.
//
// Every method takes repoID first, and NewTools closes over the one value the
// handler took from the URL path. No tool schema has a repo field.
type Corpus interface {
	Search(ctx context.Context, repoID, q string, limit int) (rag.Result, error)
	GetSpan(ctx context.Context, repoID, spanID string) (models.Span, error)
	Definitions(ctx context.Context, repoID, name, pkg string, suffix bool, limit int) ([]models.Symbol, error)
	CallersOf(ctx context.Context, repoID, symbolID string, depth, limit int) ([]store.Caller, error)
	ApproximateCallersOf(ctx context.Context, repoID, name string, limit int) ([]store.Approximate, error)
}

// ToolLimits bounds what one loop may pull out of the store.
//
// MaxSpanChars is a CUMULATIVE budget across all read_span calls, and it bounds
// HOW MANY SPANS the loop may read rather than the bytes of any one of them: a
// span longer than the whole budget still comes back whole. Clipping a span
// under an unclipped digest is a citation that lies, which is the exact failure
// spec:222's digest exists to make impossible, and P3 made it structurally
// impossible for the extractive assembler for the same reason.
type ToolLimits struct {
	MaxHits        int
	MaxSpanChars   int
	MaxDefinitions int
	MaxCallers     int
	MaxDepth       int
}

// DefaultToolLimits. MaxSpanChars is derived rather than guessed: the loop's
// MaxInputTokens is 12,000 and the estimator is four characters to a token, so
// 48,000 characters is the hard ceiling above which a span cannot be used at
// all. 32,000 leaves the system prompt, the schemas and the transcript room
// inside it.
func DefaultToolLimits() ToolLimits {
	return ToolLimits{MaxHits: 5, MaxSpanChars: 32000, MaxDefinitions: 10, MaxCallers: 20, MaxDepth: 3}
}

// maxToolQuery bounds a model-written query the same way read.go bounds a
// caller-written one.
const maxToolQuery = 1000

const (
	toolSearch       = "search_code"
	toolReadSpan     = "read_span"
	toolDefinitionOf = "definition_of"
	toolCallersOf    = "callers_of"
)

// NewTools binds the four tools to one repository, before the first model call.
// The repo id comes from the URL path; the model has no say in it and no schema
// gives it one.
func NewTools(c Corpus, repoID string, lim ToolLimits) ToolSet {
	return &toolset{c: c, repoID: repoID, lim: lim}
}

type toolset struct {
	c      Corpus
	repoID string
	lim    ToolLimits
	// chars is what read_span has already spent. State per loop, which is why
	// NewTools is called per request rather than once at boot.
	chars int
}

func (t *toolset) Specs() []llm.ToolSpec {
	return []llm.ToolSpec{
		{
			Name:        toolSearch,
			Description: "Rank spans of this repository against a query. Returns locations, not code: open a span with read_span to see it.",
			Schema:      json.RawMessage(`{"type":"object","properties":{"q":{"type":"string"}},"required":["q"],"additionalProperties":false}`),
		},
		{
			Name:        toolReadSpan,
			Description: "Read one span whole, with its digest. A span you have read may be cited.",
			Schema:      json.RawMessage(`{"type":"object","properties":{"span_id":{"type":"string"}},"required":["span_id"],"additionalProperties":false}`),
		},
		{
			Name:        toolDefinitionOf,
			Description: "Find declarations of a name in this repository.",
			Schema:      json.RawMessage(`{"type":"object","properties":{"name":{"type":"string"},"pkg":{"type":"string"},"suffix":{"type":"boolean"}},"required":["name"],"additionalProperties":false}`),
		},
		{
			Name:        toolCallersOf,
			Description: "Find callers of a symbol. Resolved callers are facts; approximate ones matched on a name only.",
			Schema:      json.RawMessage(`{"type":"object","properties":{"symbol_id":{"type":"string"},"depth":{"type":"integer"}},"required":["symbol_id"],"additionalProperties":false}`),
		},
	}
}

func (t *toolset) Call(ctx context.Context, c llm.ToolCall) (Result, error) {
	switch c.Name {
	case toolSearch:
		return t.search(ctx, c.Args)
	case toolReadSpan:
		return t.readSpan(ctx, c.Args)
	case toolDefinitionOf:
		return t.definitionOf(ctx, c.Args)
	case toolCallersOf:
		return t.callersOf(ctx, c.Args)
	}
	// Unreachable through Run, which checks the name against Specs first. Here
	// so a second caller of the ToolSet cannot dispatch a name nobody declared.
	return Result{}, fmt.Errorf("%w: no tool named %q", ErrMalformedCall, c.Name)
}

// decode refuses an unknown key rather than dropping it. P3 shipped the
// dropping version once with `mode` — echo's binder discarded the key and the
// response's own mode read as an echo of what had been asked for — and had to
// refuse the field to fix it. A silently dropped `repo` key is the same bug
// where it matters more.
func decode(args json.RawMessage, into any) error {
	d := json.NewDecoder(bytes.NewReader(args))
	d.DisallowUnknownFields()
	if err := d.Decode(into); err != nil {
		return fmt.Errorf("%w: %v", ErrMalformedCall, err)
	}
	return nil
}

func (t *toolset) search(ctx context.Context, args json.RawMessage) (Result, error) {
	var a struct {
		Q string `json:"q"`
	}
	if err := decode(args, &a); err != nil {
		return Result{}, err
	}
	q := strings.TrimSpace(a.Q)
	// The same rules the HTTP edge uses (read.go's query), so "has a usable
	// term" means the same thing to a model as it does to a caller.
	switch {
	case q == "":
		return Result{}, fmt.Errorf("%w: q must not be empty", ErrMalformedCall)
	case len(q) > maxToolQuery:
		return Result{}, fmt.Errorf("%w: q must be at most %d characters", ErrMalformedCall, maxToolQuery)
	case len(rag.Terms(q, false)) == 0:
		return Result{}, fmt.Errorf("%w: q must contain at least one letter or digit", ErrMalformedCall)
	}
	out, err := t.c.Search(ctx, t.repoID, q, t.lim.MaxHits)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			// An empty corpus ranks nothing, which is information rather than a
			// broken store — the same reading read.go's noSpans gives it.
			return t.data(toolSearch, map[string]any{"hits": []any{}, "count": 0}, nil, true)
		}
		return Result{}, err
	}
	hits := make([]map[string]any, 0, len(out.Hits))
	for _, h := range out.Hits {
		sp, ok := out.Spans[h.SpanID]
		if !ok {
			continue
		}
		// Locations, never text. A span the model has not opened is a span it
		// may not cite, and giving it the text here would make "every citation
		// points at a span the loop opened" true only nominally.
		hits = append(hits, map[string]any{
			"span_id": h.SpanID, "path": sp.Path, "symbol": sp.Symbol, "kind": sp.Kind,
			"start_line": sp.StartLine, "end_line": sp.EndLine,
			"vector_rank": h.VectorRank, "lexical_rank": h.LexicalRank,
		})
	}
	return t.data(toolSearch, map[string]any{"hits": hits, "count": len(hits)}, nil, len(hits) == 0)
}

func (t *toolset) readSpan(ctx context.Context, args json.RawMessage) (Result, error) {
	var a struct {
		SpanID string `json:"span_id"`
	}
	if err := decode(args, &a); err != nil {
		return Result{}, err
	}
	if strings.TrimSpace(a.SpanID) == "" {
		return Result{}, fmt.Errorf("%w: span_id must not be empty", ErrMalformedCall)
	}
	sp, err := t.c.GetSpan(ctx, t.repoID, a.SpanID)
	if errors.Is(err, store.ErrNotFound) {
		return t.data(toolReadSpan, map[string]any{"error": "not_found", "span_id": a.SpanID}, nil, true)
	}
	if err != nil {
		return Result{}, err
	}
	// The budget refuses the next span; it never clips this one. The first read
	// always succeeds however long the span is, which is what makes the bound a
	// count of spans rather than a limit on bytes.
	if t.chars > 0 && t.chars+len(sp.Text) > t.lim.MaxSpanChars {
		return t.data(toolReadSpan, map[string]any{
			"error": "read_budget_exhausted", "span_id": a.SpanID,
			"chars_read": t.chars, "chars_allowed": t.lim.MaxSpanChars,
		}, nil, true)
	}
	t.chars += len(sp.Text)
	return t.data(toolReadSpan, map[string]any{
		"span_id": sp.ID, "path": sp.Path, "symbol": sp.Symbol, "kind": sp.Kind,
		"start_line": sp.StartLine, "end_line": sp.EndLine,
		"digest": sp.Digest, "text": sp.Text,
	}, []models.Span{sp}, false)
}

func (t *toolset) definitionOf(ctx context.Context, args json.RawMessage) (Result, error) {
	var a struct {
		Name   string `json:"name"`
		Pkg    string `json:"pkg"`
		Suffix bool   `json:"suffix"`
	}
	if err := decode(args, &a); err != nil {
		return Result{}, err
	}
	if strings.TrimSpace(a.Name) == "" {
		return Result{}, fmt.Errorf("%w: name must not be empty", ErrMalformedCall)
	}
	syms, err := t.c.Definitions(ctx, t.repoID, a.Name, a.Pkg, a.Suffix, t.lim.MaxDefinitions)
	if err != nil {
		return Result{}, err
	}
	return t.data(toolDefinitionOf, map[string]any{
		"definitions": symbolViews(syms), "count": len(syms),
	}, nil, len(syms) == 0)
}

func (t *toolset) callersOf(ctx context.Context, args json.RawMessage) (Result, error) {
	var a struct {
		SymbolID string `json:"symbol_id"`
		Depth    int    `json:"depth"`
	}
	if err := decode(args, &a); err != nil {
		return Result{}, err
	}
	if strings.TrimSpace(a.SymbolID) == "" {
		return Result{}, fmt.Errorf("%w: symbol_id must not be empty", ErrMalformedCall)
	}
	depth := a.Depth
	if depth == 0 {
		depth = 1
	}
	// Refused, not clamped: a model that asked for depth 40 has misunderstood
	// something, and quietly serving 3 hides it — the rule read.go applies to a
	// caller's limit, applied to a model's.
	if depth < 1 || depth > t.lim.MaxDepth {
		return Result{}, fmt.Errorf("%w: depth must be between 1 and %d, got %d", ErrMalformedCall, t.lim.MaxDepth, depth)
	}
	callers, err := t.c.CallersOf(ctx, t.repoID, a.SymbolID, depth, t.lim.MaxCallers)
	if err != nil {
		return Result{}, err
	}
	// Separate and labelled, never merged (spec:84). A syntactic edge knows it
	// calls something named Close and cannot say which one; a model handed one
	// flat list attributes a caller it cannot have.
	approx, err := t.c.ApproximateCallersOf(ctx, t.repoID, a.SymbolID, t.lim.MaxCallers)
	if err != nil {
		return Result{}, err
	}
	cs := make([]map[string]any, 0, len(callers))
	for _, c := range callers {
		cs = append(cs, map[string]any{
			"symbol": symbolView(c.Symbol), "depth": c.Depth,
			"call_path": c.CallPath, "call_line": c.CallLine, "matched_on": "target",
		})
	}
	as := make([]map[string]any, 0, len(approx))
	for _, c := range approx {
		as = append(as, map[string]any{
			"symbol": symbolView(c.Symbol), "to_name": c.ToName,
			"call_path": c.CallPath, "call_line": c.CallLine, "matched_on": "name",
		})
	}
	return t.data(toolCallersOf, map[string]any{
		"callers": cs, "count": len(cs),
		"approximate": as, "approximate_count": len(as),
	}, nil, len(cs) == 0 && len(as) == 0)
}

// data marshals one tool result and frames it. Every path out of a tool goes
// through here, so nothing can reach the model unframed.
func (t *toolset) data(name string, v map[string]any, read []models.Span, empty bool) (Result, error) {
	b, err := json.Marshal(v)
	if err != nil {
		return Result{}, err
	}
	return Result{Content: Frame(name, string(b)), NotFound: empty, Read: read}, nil
}

func symbolView(sy models.Symbol) map[string]any {
	return map[string]any{
		"symbol_id": sy.ID, "name": sy.Name, "pkg": sy.Pkg, "kind": sy.Kind,
		"path": sy.Path, "start_line": sy.StartLine, "end_line": sy.EndLine,
		"span_id": sy.SpanID,
	}
}

func symbolViews(syms []models.Symbol) []map[string]any {
	out := make([]map[string]any, 0, len(syms))
	for _, sy := range syms {
		out = append(out, symbolView(sy))
	}
	return out
}
