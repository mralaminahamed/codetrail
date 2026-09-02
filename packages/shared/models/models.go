// Package models holds the core domain types persisted to Postgres.
//
// The unit of retrieval is a Span: a byte- and line-delimited region of one
// file at one commit. Everything else exists to produce spans or to explain
// them. A citation is a span, and that is what makes a citation here
// checkable in a way a prose citation never is — the range either still holds
// what was claimed or it does not.
package models

import "time"

// Repo is one indexed repository at one commit. Re-indexing a new commit does
// not replace the row: spans are keyed by commit so a stale answer can say so
// rather than quietly citing a line that has moved.
type Repo struct {
	ID     string `json:"id"`
	Remote string `json:"remote"`
	Ref    string `json:"ref"`
	Commit string `json:"commit"`
	// SizeBytes is what the checkout weighed on disk, counting regular files
	// only. Written at index time because that is the one moment it is known:
	// the checkout is deleted immediately afterwards.
	SizeBytes int64     `json:"size_bytes"`
	IndexedAt time.Time `json:"indexed_at"`
}

// File is one blob in a repo at a commit. Blob is git's own content hash, so
// an unchanged file across commits is recognised without reading it.
type File struct {
	ID     string `json:"id"`
	RepoID string `json:"repo_id"`
	Path   string `json:"path"`
	Blob   string `json:"blob"`
	Lang   string `json:"lang"`
	Lines  int    `json:"lines"`
}

// SpanKind is what the chunker decided this region is. It is a closed set:
// anything the AST walker does not recognise becomes KindFile rather than a
// new kind invented at parse time.
type SpanKind string

const (
	KindFunc  SpanKind = "func"
	KindType  SpanKind = "type"
	KindConst SpanKind = "const"
	KindVar   SpanKind = "var"
	// KindFile is a fixed window: over a file the AST could not be read from,
	// over a declaration too long to be one span, or over anything at all
	// under the window strategy. It does not say which arm produced a row —
	// the AST strategy emits it too — so nothing may infer the strategy from
	// it.
	KindFile SpanKind = "file"
)

// SpanKinds is the whole range, exported so a test can pin the set.
var SpanKinds = []SpanKind{KindFunc, KindType, KindConst, KindVar, KindFile}

// Span is a retrievable region of code and the thing a citation points at.
//
// StartLine and EndLine are 1-based and inclusive, matching how every editor
// and every "file:line" convention counts. Off-by-one here is not cosmetic: it
// is a citation pointing at the wrong code.
type Span struct {
	ID        string   `json:"id"`
	RepoID    string   `json:"repo_id"`
	FileID    string   `json:"file_id"`
	Path      string   `json:"path"`
	Kind      SpanKind `json:"kind"`
	Symbol    string   `json:"symbol"`
	StartLine int      `json:"start_line"`
	EndLine   int      `json:"end_line"`
	Text      string   `json:"text"`
	// Digest is a content hash of Text, so a citation can be checked against
	// what is on disk now without re-reading the whole span.
	Digest string `json:"digest"`
}

// Cite is one retrieved span with its score, as returned to a caller.
type Cite struct {
	Span
	Score float32 `json:"score"`
}

// Provenance is how much an edge knows about its own target. It is a per-row
// property, not a per-repo one (spec:190): the edge set comes from the AST and
// type information only ever upgrades individual rows, so one repository whose
// packages type-check unevenly carries both labels.
type Provenance string

const (
	// ProvenanceResolved: the type-checker named an object and that object has
	// a symbol row in this repository, so ToSymbolID points at it.
	ProvenanceResolved Provenance = "resolved"
	// ProvenanceSyntactic: the target is null. The edge knows it calls
	// something named Close and cannot say which one (spec:84). A call that
	// resolves outside the corpus — fmt.Println — is this, not resolved,
	// because there is no symbol row to point at.
	ProvenanceSyntactic Provenance = "syntactic"
)

// Provenances is the whole range, exported so a test can pin the set.
var Provenances = []Provenance{ProvenanceResolved, ProvenanceSyntactic}

// EdgeKind is what one symbol does to another. Spec §3's closed set; P4 writes
// only EdgeCalls. EdgeImports is unwritable under this schema — an import
// belongs to a file, not to a definition, and FromSymbolID is NOT NULL — and
// EdgeReferences is a volume decision to make with a measured row count.
type EdgeKind string

const (
	EdgeCalls      EdgeKind = "calls"
	EdgeImports    EdgeKind = "imports"
	EdgeReferences EdgeKind = "references"
)

// EdgeKinds is the whole range, exported so a test can pin the set.
var EdgeKinds = []EdgeKind{EdgeCalls, EdgeImports, EdgeReferences}

// Symbol is one top-level definition, from the AST.
//
// StartLine and EndLine are the definition's own, not its span's, and they are
// what makes it citable: the chunker sub-windows a declaration longer than
// MaxDeclLines, so the largest declarations have no span with their range and
// a symbol locatable only through SpanID would be uncitable for exactly the
// definitions most worth asking about.
//
// SpanID is the span containing the definition's first line, or empty when
// there is none. Empty means SQL NULL; a span id is never the empty string.
type Symbol struct {
	ID        string   `json:"id"`
	RepoID    string   `json:"repo_id"`
	FileID    string   `json:"file_id"`
	Path      string   `json:"path"`
	Name      string   `json:"name"`
	Pkg       string   `json:"pkg"`
	Kind      SpanKind `json:"kind"`
	StartLine int      `json:"start_line"`
	EndLine   int      `json:"end_line"`
	SpanID    string   `json:"span_id,omitempty"`
}

// Edge is one call site: the definition it is in, the name it calls, where it
// is, and how much is known about what it means.
//
// ToSymbolID is empty for a syntactic edge, which is SQL NULL. The pair with
// Provenance is a CHECK constraint in the schema as well as a check in
// PutGraph, because an edge that claims to be resolved while pointing nowhere
// claims a precision no column records.
type Edge struct {
	ID           string     `json:"id"`
	RepoID       string     `json:"repo_id"`
	FromSymbolID string     `json:"from_symbol_id"`
	ToSymbolID   string     `json:"to_symbol_id,omitempty"`
	ToName       string     `json:"to_name"`
	Kind         EdgeKind   `json:"kind"`
	Provenance   Provenance `json:"provenance"`
	// Path and Line are the call site's, so a caller row can cite file:line.
	Path string `json:"path"`
	Line int    `json:"line"`
}
