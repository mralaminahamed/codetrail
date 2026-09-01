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
	// KindFile is the fallback: a fixed window over a file the AST could not
	// be read from. It is deliberately distinguishable, because the eval
	// harness compares AST spans against exactly this.
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
