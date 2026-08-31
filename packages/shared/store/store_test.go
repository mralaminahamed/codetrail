package store

import (
	"errors"
	"regexp"
	"slices"
	"strings"
	"testing"
)

// Migrations run in filename order, so the names have to sort the way they are
// meant to run. A test rather than a convention: "0010 before 0002" is a
// mistake that only shows up on a fresh database, which is the one nobody has.
func TestMigrationNamesAreOrderedAndNumbered(t *testing.T) {
	names, err := MigrationNames()
	if err != nil {
		t.Fatal(err)
	}
	if len(names) == 0 {
		t.Fatal("no migrations are embedded — the //go:embed pattern is not matching")
	}
	if !slices.IsSorted(names) {
		t.Fatalf("migrations are not in sorted order: %v", names)
	}
	num := regexp.MustCompile(`^\d{4}_[a-z0-9_]+\.sql$`)
	seen := map[string]bool{}
	for i, n := range names {
		if !num.MatchString(n) {
			t.Errorf("%q does not match NNNN_name.sql — ordering depends on the prefix", n)
		}
		prefix := strings.SplitN(n, "_", 2)[0]
		if seen[prefix] {
			t.Errorf("duplicate migration number %s: two migrations would race", prefix)
		}
		seen[prefix] = true
		if i == 0 && prefix != "0001" {
			t.Errorf("first migration is %q, want it to start at 0001", n)
		}
	}
}

// The first migration has to create the extension before anything declares a
// vector column, or a fresh database fails on 0001 rather than on setup.
func TestFirstMigrationCreatesTheVectorExtension(t *testing.T) {
	body, err := migrationFS.ReadFile("migrations/0001_init.sql")
	if err != nil {
		t.Fatal(err)
	}
	sql := string(body)
	ext := strings.Index(sql, "CREATE EXTENSION")
	vec := strings.Index(sql, "vector(")
	if ext < 0 {
		t.Fatal("0001 does not create the vector extension")
	}
	if vec >= 0 && ext > vec {
		t.Fatal("the vector extension is created after a vector column is declared")
	}
}

// The schema is vector(768) and an ANN index needs that fixed. An embedder of
// a different width must fail at startup, not write vectors that share a table
// with a different vector space and rank nonsense confidently.
func TestCheckDim(t *testing.T) {
	if err := CheckDim(EmbeddingDim); err != nil {
		t.Fatalf("the schema's own dimension must be accepted: %v", err)
	}
	for _, dim := range []int{0, 1, 384, 767, 769, 1536, 3072} {
		err := CheckDim(dim)
		if err == nil {
			t.Fatalf("dim %d must be refused", dim)
		}
		if !errors.Is(err, ErrDimMismatch) {
			t.Fatalf("dim %d: want ErrDimMismatch, got %v", dim, err)
		}
		// The message has to name both numbers, or an operator cannot tell
		// which side to change.
		if !strings.Contains(err.Error(), "768") {
			t.Fatalf("dim %d: error does not name the schema width: %v", dim, err)
		}
	}
}

// The declared width and the schema must agree. Nothing else checks this: the
// constant is what CheckDim compares against, and the column is what Postgres
// enforces, so a change to one and not the other is silent until an insert.
func TestEmbeddingDimMatchesTheSchema(t *testing.T) {
	body, err := migrationFS.ReadFile("migrations/0001_init.sql")
	if err != nil {
		t.Fatal(err)
	}
	m := regexp.MustCompile(`vector\((\d+)\)`).FindStringSubmatch(string(body))
	if m == nil {
		t.Fatal("0001 declares no vector(N) column")
	}
	if want := m[1]; want != "768" || EmbeddingDim != 768 {
		t.Fatalf("schema declares vector(%s) but EmbeddingDim is %d", want, EmbeddingDim)
	}
}
