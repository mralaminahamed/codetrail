package agent

import (
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// The egress claim, as a check rather than a sentence.
//
// The loop takes an llm.Model and a ToolSet, both interfaces, and constructs no
// client of its own. This asserts the property that makes that readable: no
// file in this package imports an HTTP package or the os/exec surface.
//
// It is deliberately about THIS PACKAGE'S OWN imports and not the transitive
// closure, because the transitive claim is false and saying it would be the
// false-comment shape: rag pulls net/http through prometheus and store pulls it
// through pgx, so `go list -deps ./packages/shared/agent` names net/http and
// always will.
func TestThisPackageDialsNothing(t *testing.T) {
	banned := map[string]bool{
		"net/http": true, "net/url": true, "net": true,
		"os/exec": true, "crypto/tls": true,
	}
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	if len(files) < 5 {
		t.Fatalf("globbed %d files; this check would be vacuous", len(files))
	}
	fset := token.NewFileSet()
	checked := 0
	for _, f := range files {
		src, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		file, err := parser.ParseFile(fset, f, src, parser.ImportsOnly)
		if err != nil {
			t.Fatalf("%s: %v", f, err)
		}
		checked++
		for _, im := range file.Imports {
			p, err := strconv.Unquote(im.Path.Value)
			if err != nil {
				t.Fatal(err)
			}
			if banned[p] {
				t.Errorf("%s imports %q: the loop must construct no client of its own", f, p)
			}
		}
	}
	if checked == 0 {
		t.Fatal("no files parsed")
	}
}

// The other half of the same claim: the loop holds a Corpus, never a concrete
// store handle, so it cannot open a connection even by accident.
func TestTheLoopHoldsNoConcreteStore(t *testing.T) {
	files, _ := filepath.Glob("*.go")
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") {
			continue
		}
		b, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		for _, bad := range []string{"*store.Store", "pgxpool", "store.New("} {
			if strings.Contains(string(b), bad) {
				t.Errorf("%s names %s", f, bad)
			}
		}
	}
}
