package main

import (
	"bytes"
	"crypto/sha1"
	"encoding/hex"
	"fmt"
	"unicode/utf8"

	"github.com/mralaminahamed/codetrail/apps/indexer/internal/walk"
)

// blobHash is git's own object name for a file's content: sha1 over
// "blob <len>\x00" and the bytes. Spec §3 calls files.blob git's content hash,
// so it has to be the hash git would produce and not a hash of our own — a
// test checks it against git hash-object.
//
// Filled here because the chunk pass already holds the bytes; nothing reads
// the column until P7's incremental re-index.
func blobHash(body []byte) string {
	h := sha1.New()
	fmt.Fprintf(h, "blob %d\x00", len(body))
	h.Write(body)
	return hex.EncodeToString(h.Sum(nil))
}

// indexable reports whether a file's bytes can become spans.
//
// The UTF-8 and NUL halves are not taste. Measured against pg17: a span text
// carrying a NUL is refused with `invalid byte sequence for encoding "UTF8":
// 0x00` and one carrying an invalid byte with `... 0xff`, and PutSpans writes
// a repo's spans in one transaction — so a single PNG would fail the whole
// job rather than cost one file.
//
// The Lang half is the cost call (Open Question 5): a lockfile or a minified
// bundle has nothing retrievable in it and costs an embedding call per window.
func indexable(f walk.File, body []byte) bool {
	return f.Lang != "" && utf8.Valid(body) && !bytes.ContainsRune(body, 0)
}
