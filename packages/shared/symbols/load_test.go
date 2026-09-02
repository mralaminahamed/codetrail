package symbols

import (
	"context"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"golang.org/x/tools/go/packages"

	"github.com/mralaminahamed/codetrail/packages/shared/chunk"
)

// checkout copies testdata/mod/<name> into a fresh checkout. The sources are
// .gotxt and go.mod.txt so that neither the repository's own build nor
// gofmt -l walks into a module that is meant to be broken.
func checkout(t *testing.T, name string) Policy {
	t.Helper()
	p := testPolicy(t)
	src := filepath.Join("testdata", "mod", name)
	err := filepath.WalkDir(src, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(src, path)
		if err != nil {
			return err
		}
		out := filepath.Join(p.Root, rel)
		if d.IsDir() {
			return os.MkdirAll(out, 0o755)
		}
		switch {
		case strings.HasSuffix(out, ".gotxt"):
			out = strings.TrimSuffix(out, ".gotxt") + ".go"
		case strings.HasSuffix(out, ".txt"):
			out = strings.TrimSuffix(out, ".txt")
		}
		b, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		return os.WriteFile(out, b, 0o644)
	})
	if err != nil {
		t.Fatalf("copying fixture %s: %v", name, err)
	}
	return p
}

// keyOf is the call site Parse records for the nth call to name in path — the
// same key Resolve must produce, derived rather than transcribed so that the
// two passes are compared instead of both being compared to a literal.
func keyOf(t *testing.T, p Policy, path, name string, nth int) Key {
	t.Helper()
	src, err := os.ReadFile(filepath.Join(p.Root, path))
	if err != nil {
		t.Fatalf("reading %s: %v", path, err)
	}
	f, err := Parse(path, src)
	if err != nil {
		t.Fatalf("parsing %s: %v", path, err)
	}
	seen := 0
	for _, c := range f.Calls {
		if c.Name != name {
			continue
		}
		if seen == nth {
			return Key{Path: c.Path, Offset: c.Offset}
		}
		seen++
	}
	t.Fatalf("%s has no call number %d to %s", path, nth, name)
	return Key{}
}

func mustResolve(t *testing.T, key Key, got map[Key]Target, wantPath string, wantLine int) {
	t.Helper()
	tgt, ok := got[key]
	if !ok {
		t.Fatalf("the call at %s offset %d resolved to nothing, want %s:%d", key.Path, key.Offset, wantPath, wantLine)
	}
	if tgt.Path != wantPath || tgt.Line != wantLine {
		t.Errorf("the call at %s offset %d resolved to %s:%d, want %s:%d",
			key.Path, key.Offset, tgt.Path, tgt.Line, wantPath, wantLine)
	}
}

func mustNotResolve(t *testing.T, key Key, got map[Key]Target, why string) {
	t.Helper()
	if tgt, ok := got[key]; ok {
		t.Errorf("the call at %s offset %d resolved to %s:%d, want no target: %s",
			key.Path, key.Offset, tgt.Path, tgt.Line, why)
	}
}

type recorder struct {
	mu  sync.Mutex
	got []string
	Srv *httptest.Server
}

func newRecorder(t *testing.T) *recorder {
	t.Helper()
	r := &recorder{}
	r.Srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		r.mu.Lock()
		r.got = append(r.got, req.URL.Path)
		r.mu.Unlock()
		http.Error(w, "the module proxy should not have been reached", http.StatusNotFound)
	}))
	t.Cleanup(r.Srv.Close)
	return r
}

func (r *recorder) paths() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.got...)
}

// loadWithProxy runs the same load Resolve runs, over the same policy
// environment, with only GOPROXY replaced. It exists because GOPROXY=off is
// upstream of every other fetch: under it nothing is fetched at all, so a
// toolchain directive and a module requirement are indistinguishable from a
// repository that needs neither. Everything else in the environment is
// production's, so a control run here still reads GOTOOLCHAIN, GOVCS and the
// rest from Env.
func loadWithProxy(t *testing.T, p Policy, proxy string) ([]*packages.Package, error) {
	t.Helper()
	for _, d := range p.dirs() {
		if err := os.MkdirAll(d, 0o700); err != nil {
			t.Fatalf("creating %s: %v", d, err)
		}
	}
	env := p.Env()
	for i, e := range env {
		if strings.HasPrefix(e, "GOPROXY=") {
			env[i] = "GOPROXY=" + proxy
		}
	}
	return packages.Load(&packages.Config{
		Mode: loadMode, Dir: p.Root, Env: env, Context: context.Background(), Tests: false,
	}, "./...")
}

func TestCallsWithinTheModuleResolveToTheirDefinitions(t *testing.T) {
	p := checkout(t, "std")
	got, st := Resolve(context.Background(), p)

	if st.Reason != ReasonOK {
		t.Fatalf("reason %q, want %q; stats %+v", st.Reason, ReasonOK, st)
	}
	mustResolve(t, keyOf(t, p, "app.go", "Double", 0), got, "lib/lib.go", 3)
	mustResolve(t, keyOf(t, p, "app.go", "Add", 0), got, "lib/lib.go", 5)
	mustResolve(t, keyOf(t, p, "app.go", "One", 0), got, "lib/lib.go", 9)
	mustResolve(t, keyOf(t, p, "app.go", "One", 1), got, "lib/lib.go", 9)

	want := Stats{Packages: 2, Loaded: 2, Resolved: 4, External: 1, Unresolved: 1, Reason: ReasonOK}
	if st != want {
		t.Errorf("stats %+v, want %+v", st, want)
	}
}

// fmt.Println names a real object and has no symbols row to point at. §3 says
// a resolved edge points at one; spec:84 says a null target is what makes the
// label mean anything. The counter is where the fact survives — asserting only
// that the map has no entry passes under a mutant that drops the call on the
// floor.
func TestACallIntoTheStandardLibraryIsExternalNotResolved(t *testing.T) {
	p := checkout(t, "std")
	got, st := Resolve(context.Background(), p)

	mustNotResolve(t, keyOf(t, p, "app.go", "Println", 0), got, "fmt.Println has no symbols row")
	if st.External != 1 {
		t.Errorf("External is %d, want 1: the fact that a call left the corpus has to survive somewhere", st.External)
	}
}

func TestACallToALocalClosureIsNotResolved(t *testing.T) {
	p := checkout(t, "std")
	got, st := Resolve(context.Background(), p)

	mustNotResolve(t, keyOf(t, p, "app.go", "fn", 0), got,
		"Uses names the variable holding the closure, whose position is inside Run itself")
	if st.Unresolved != 1 {
		t.Errorf("Unresolved is %d, want 1", st.Unresolved)
	}
	if st.External != 1 || st.Resolved != 4 {
		t.Errorf("stats %+v: the closure must be unresolved, not external and not resolved", st)
	}
}

// A Def's StartLine begins at its doc comment; a target's line is the
// declaration's own. lib.One is documented so the two differ, and a caller
// matching a target to a definition has to match by containment.
func TestATargetsLineIsTheDeclarationNotItsDocComment(t *testing.T) {
	p := checkout(t, "std")
	got, _ := Resolve(context.Background(), p)

	src, err := os.ReadFile(filepath.Join(p.Root, "lib", "lib.go"))
	if err != nil {
		t.Fatal(err)
	}
	f, err := Parse("lib/lib.go", src)
	if err != nil {
		t.Fatal(err)
	}
	var def Def
	for _, d := range f.Defs {
		if d.Name == "One" {
			def = d
		}
	}
	if def.StartLine != 7 || def.EndLine != 9 {
		t.Fatalf("One is defined at %d..%d, want 7..9; the fixture's doc comment moved", def.StartLine, def.EndLine)
	}
	tgt := got[keyOf(t, p, "app.go", "One", 0)]
	if tgt.Line != 9 {
		t.Fatalf("the target of One is line %d, want 9", tgt.Line)
	}
	if tgt.Line == def.StartLine {
		t.Errorf("the target's line equals the definition's start line, so this fixture no longer separates them")
	}
	if tgt.Line < def.StartLine || tgt.Line > def.EndLine {
		t.Errorf("the target line %d is outside the definition's range %d..%d, so containment cannot link them",
			tgt.Line, def.StartLine, def.EndLine)
	}
}

// Two calls to One on one line. Task 1 keys them apart by offset and this pass
// has to key them the same way, or the two lookups collapse to one edge.
func TestResolutionIsKeyedByOffsetNotByLine(t *testing.T) {
	p := checkout(t, "std")
	got, _ := Resolve(context.Background(), p)

	first, second := keyOf(t, p, "app.go", "One", 0), keyOf(t, p, "app.go", "One", 1)
	if first.Offset != 374 || second.Offset != 385 {
		t.Fatalf("the two calls to One are at offsets %d and %d, want 374 and 385; the fixture moved", first.Offset, second.Offset)
	}
	if first == second {
		t.Fatalf("both calls to One share the key %+v, so they are one edge", first)
	}
	for _, k := range []Key{first, second} {
		if _, ok := got[k]; !ok {
			t.Errorf("the call at offset %d did not resolve; a line key would have kept only one of the two", k.Offset)
		}
	}
}

// The two passes read the same bytes on disk, which is the only reason their
// offsets agree — not because stripping preserves them. Task 1 measured that
// chunk.StripDocs keeps line numbers and moves offsets, and this pins the
// consequence: hand Parse stripped source and every lookup here misses
// silently.
func TestResolutionKeysAreOffsetsIntoTheBytesOnDisk(t *testing.T) {
	p := checkout(t, "std")
	got, _ := Resolve(context.Background(), p)

	raw, err := os.ReadFile(filepath.Join(p.Root, "app.go"))
	if err != nil {
		t.Fatal(err)
	}
	stripped, err := chunk.StripDocs("app.go", raw)
	if err != nil {
		t.Fatal(err)
	}
	fromDisk, err := Parse("app.go", raw)
	if err != nil {
		t.Fatal(err)
	}
	fromStripped, err := Parse("app.go", stripped)
	if err != nil {
		t.Fatal(err)
	}

	resolved := 0
	for _, c := range fromDisk.Calls {
		if _, ok := got[Key{Path: c.Path, Offset: c.Offset}]; ok {
			resolved++
		}
	}
	if resolved != 4 {
		t.Errorf("%d of Parse's keys resolved, want 4: the loader is not keying the bytes on disk", resolved)
	}

	same, moved := 0, 0
	for i, c := range fromStripped.Calls {
		if c.Offset == fromDisk.Calls[i].Offset {
			same++
		} else {
			moved++
		}
	}
	if moved == 0 {
		t.Fatalf("every offset survived stripping (%d unchanged), so this test no longer says why the caller must parse the bytes on disk", same)
	}
	strippedHits := 0
	for _, c := range fromStripped.Calls {
		if _, ok := got[Key{Path: c.Path, Offset: c.Offset}]; ok {
			strippedHits++
		}
	}
	if strippedHits == resolved {
		t.Errorf("stripped offsets hit as often as raw ones (%d), so a caller handing Parse stripped source would look fine", strippedHits)
	}
}

// The spec's per-edge claim (spec:190): the label is a property of a call
// site. bad/ loads with an error and still resolves its own Helper, while the
// call through the import that is not there does not — both labels inside one
// package. blocked/ never loads at all, so its call has no target either.
func TestOneCallResolvesWhileAnotherInTheSamePackageDoesNot(t *testing.T) {
	p := checkout(t, "absent")
	got, st := Resolve(context.Background(), p)

	if st.Reason != ReasonLoadError {
		t.Fatalf("reason %q, want %q; stats %+v", st.Reason, ReasonLoadError, st)
	}
	mustResolve(t, keyOf(t, p, "good/good.go", "Helper", 0), got, "good/good.go", 3)
	mustResolve(t, keyOf(t, p, "bad/bad.go", "Helper", 0), got, "bad/bad.go", 5)
	mustNotResolve(t, keyOf(t, p, "bad/bad.go", "Missing", 0), got,
		"the module it comes from is not there, so Uses has no object for it")

	want := Stats{Packages: 2, Loaded: 1, Failed: 1, Resolved: 2, Unresolved: 1, Reason: ReasonLoadError}
	if st != want {
		t.Errorf("stats %+v, want %+v", st, want)
	}
}

func TestAPackageThatCannotLoadLeavesItsCallsUnresolved(t *testing.T) {
	p := checkout(t, "absent")
	got, st := Resolve(context.Background(), p)

	mustNotResolve(t, keyOf(t, p, "blocked/blocked.go", "Helper", 0), got,
		"a build constraint excludes the whole package, so the loader never saw it")
	if st.Packages != 2 {
		t.Errorf("Packages is %d, want 2: blocked/ is not one of them", st.Packages)
	}
}

// The test this task exists for. GOPROXY is the recorder in the *parent*, and
// the child never sees it. The control sub-test is what makes the zero mean
// something: without it, "no requests" cannot be told apart from a fixture
// that never wanted a module.
func TestTheLoaderNeverReachesTheModuleProxy(t *testing.T) {
	rec := newRecorder(t)
	t.Setenv("GOPROXY", rec.Srv.URL)
	t.Setenv("GOFLAGS", "-mod=mod")
	p := checkout(t, "absent")

	got, st := Resolve(context.Background(), p)
	if paths := rec.paths(); len(paths) != 0 {
		t.Errorf("the module proxy received %d requests %v, want 0", len(paths), paths)
	}
	mustResolve(t, keyOf(t, p, "good/good.go", "Helper", 0), got, "good/good.go", 3)
	if st.Resolved != 2 {
		t.Fatalf("stats %+v: the load has to have happened for zero requests to mean anything", st)
	}

	t.Run("control: the same fixture fetches when the child is allowed to", func(t *testing.T) {
		ctl := newRecorder(t)
		if _, err := loadWithProxy(t, p, ctl.Srv.URL); err != nil {
			t.Fatalf("control load: %v", err)
		}
		paths := ctl.paths()
		if len(paths) == 0 {
			t.Fatalf("the control run reached the proxy 0 times, so the fixture proves nothing about GOPROXY=off")
		}
		for _, path := range paths {
			if !strings.HasPrefix(path, "/example.com/nope/") {
				t.Errorf("the control run fetched %s, want only /example.com/nope/…", path)
			}
		}
	})
}

// The other half of the same claim, and the one that does not need a recorder:
// under this policy a repository that requires a real third-party module does
// not type-check, and its calls stay unresolved.
func TestARequiredThirdPartyModuleDoesNotTypeCheck(t *testing.T) {
	p := checkout(t, "thirdparty")
	got, st := Resolve(context.Background(), p)

	mustNotResolve(t, keyOf(t, p, "thirdparty.go", "Compare", 0), got,
		"golang.org/x/mod is not in the job's module cache and fetching is off")
	if st.Failed != 1 || st.Loaded != 0 {
		t.Errorf("stats %+v, want one failed package and none loaded", st)
	}
	if st.External != 0 {
		t.Errorf("External is %d, want 0: a package that never loaded has no external calls", st.External)
	}
	if st.Reason != ReasonLoadError {
		t.Errorf("reason %q, want %q", st.Reason, ReasonLoadError)
	}
}

// The parent may carry anything. Each variable here changes the load when it
// reaches the child, and every one of them is closed by omission rather than
// by an entry setting it to something safe.
func TestAHostileParentEnvironmentCannotBreakTheLoad(t *testing.T) {
	for _, tc := range []struct{ k, v string }{
		{"GOFLAGS", "-mod=vendor"},
		{"GOROOT", "/nonexistent-goroot"},
		{"GOEXPERIMENT", "codetrail_bogus"},
		{"GOPRIVATE", "*"},
		{"GODEBUG", "gotypesalias=0"},
		{"GOTOOLCHAIN", "auto"},
		{"CGO_ENABLED", "1"},
		{"GOWORK", "/nonexistent.work"},
		{"GOPACKAGESDRIVER", "/bin/false"},
	} {
		t.Run(tc.k, func(t *testing.T) {
			t.Setenv(tc.k, tc.v)
			p := checkout(t, "absent")
			got, st := Resolve(context.Background(), p)
			mustResolve(t, keyOf(t, p, "good/good.go", "Helper", 0), got, "good/good.go", 3)
			mustResolve(t, keyOf(t, p, "bad/bad.go", "Helper", 0), got, "bad/bad.go", 5)
			if st.Resolved != 2 || st.Packages != 2 {
				t.Errorf("with %s=%s in the parent, stats are %+v, want 2 packages and 2 resolutions", tc.k, tc.v, st)
			}
		})
	}
}

// go/packages falls back to exec.LookPath("gopackagesdriver") on the parent's
// PATH and runs whatever it finds in place of the go command, handing it the
// config. GOPACKAGESDRIVER=off is the only thing that closes it.
func TestAnExternalPackagesDriverOnThePathIsNotRun(t *testing.T) {
	bin := t.TempDir()
	marker := filepath.Join(bin, "ran")
	script := "#!/bin/sh\necho ran > " + marker + "\necho '{}'\n"
	if err := os.WriteFile(filepath.Join(bin, "gopackagesdriver"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))

	p := checkout(t, "std")
	got, st := Resolve(context.Background(), p)

	if _, err := os.Stat(marker); err == nil {
		t.Errorf("a gopackagesdriver on the operator's PATH was executed against the checkout")
	}
	mustResolve(t, keyOf(t, p, "app.go", "Double", 0), got, "lib/lib.go", 3)
	if st.Resolved != 4 {
		t.Errorf("stats %+v, want 4 resolutions; a driver that answered instead of the go command returns none", st)
	}
}

// A go.work inside the checkout can name directories outside it.
func TestAGoWorkFileInTheCheckoutIsIgnored(t *testing.T) {
	p := checkout(t, "work")
	got, st := Resolve(context.Background(), p)

	mustResolve(t, keyOf(t, p, "app.go", "Double", 0), got, "lib/lib.go", 3)
	if st.Reason != ReasonOK || st.Resolved != 4 {
		t.Errorf("stats %+v, want 4 resolutions and %q: the go.work names /nonexistent-outside-the-checkout", st, ReasonOK)
	}
}

// ~/.config/go/env is a second copy of every setting the allowlist closes, and
// it is read even by a child with an otherwise empty environment.
func TestAGoEnvFileUnderTheScratchHomeIsIgnored(t *testing.T) {
	p := checkout(t, "absent")
	dir := filepath.Join(p.Home, ".config", "go")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "env"), []byte("GOFLAGS=-mod=vendor\nGOTOOLCHAIN=auto\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	got, st := Resolve(context.Background(), p)

	mustResolve(t, keyOf(t, p, "good/good.go", "Helper", 0), got, "good/good.go", 3)
	if st.Resolved != 2 {
		t.Errorf("stats %+v, want 2 resolutions; -mod=vendor from the env file makes the load refuse the module", st)
	}
}

// A go directive newer than the toolchain otherwise downloads one, before any
// policy about modules applies. The assertion has to run in proxy mode: under
// GOPROXY=off the download is refused by a different control and the two are
// indistinguishable.
func TestAToolchainDirectiveDoesNotFetchAToolchain(t *testing.T) {
	rec := newRecorder(t)
	t.Setenv("GOPROXY", rec.Srv.URL)
	p := checkout(t, "newgo")

	got, st := Resolve(context.Background(), p)
	if len(got) != 0 || st.Reason != ReasonLoadError {
		t.Errorf("stats %+v with %d resolutions, want none and %q", st, len(got), ReasonLoadError)
	}
	if paths := rec.paths(); len(paths) != 0 {
		t.Errorf("the module proxy received %d requests %v, want 0", len(paths), paths)
	}

	t.Run("in proxy mode, where the difference is observable", func(t *testing.T) {
		ctl := newRecorder(t)
		if _, err := loadWithProxy(t, p, ctl.Srv.URL); err == nil {
			t.Fatal("the load succeeded, want it to refuse go 1.99.0")
		}
		for _, path := range ctl.paths() {
			if strings.Contains(path, "golang.org/toolchain") {
				t.Errorf("the proxy received %s: a toolchain was fetched for a stranger's go directive", path)
			}
		}
	})
}

// Type-checking import "C" runs cgo, and #cgo LDFLAGS is arbitrary execution.
// Whether this kills its mutation depends on the machine: with no C toolchain
// both versions fail.
func TestACgoPackageDoesNotLoad(t *testing.T) {
	p := checkout(t, "cgo")
	got, st := Resolve(context.Background(), p)

	mustNotResolve(t, keyOf(t, p, "cgo.go", "Helper", 0), got, "the package's files are all excluded with cgo off")
	if st.Packages != 0 {
		t.Errorf("Packages is %d, want 0: a cgo package must not load at all", st.Packages)
	}
	if st.Reason != ReasonLoadError {
		t.Errorf("reason %q, want %q", st.Reason, ReasonLoadError)
	}
}

func TestARepositoryWithNoGoModRecordsItsReason(t *testing.T) {
	p := checkout(t, "nomod")
	got, st := Resolve(context.Background(), p)

	if st.Reason != ReasonNoModule {
		t.Errorf("reason %q, want %q: it is what tells this apart from a load that failed", st.Reason, ReasonNoModule)
	}
	if len(got) != 0 || st.Packages != 0 {
		t.Errorf("stats %+v with %d resolutions, want none", st, len(got))
	}
}

// Spec:194 — one deadline for the whole job. A stage that derived its own
// budget would resolve this fixture.
func TestAnExpiredContextResolvesNothingAndSaysWhy(t *testing.T) {
	p := checkout(t, "std")
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	got, st := Resolve(ctx, p)
	if st.Reason != ReasonDeadline {
		t.Errorf("reason %q, want %q", st.Reason, ReasonDeadline)
	}
	if len(got) != 0 || st.Resolved != 0 {
		t.Errorf("resolved %d calls with reason %q, want 0 and %q", st.Resolved, st.Reason, ReasonDeadline)
	}

	t.Run("control: the same fixture resolves with the budget intact", func(t *testing.T) {
		got, st := Resolve(context.Background(), p)
		if st.Resolved != 4 || len(got) != 4 {
			t.Fatalf("stats %+v: the expired-context assertion means nothing if this fixture never resolves", st)
		}
	})
}

// Open question 9: a missing toolchain downgrades a repository's edges. It
// does not fail a job and it must not fail a boot.
func TestAMissingGoBinaryDegradesRatherThanFailing(t *testing.T) {
	for _, tc := range []struct{ name, gobin, want string }{
		{"unset", "", ReasonNoToolchain},
		{"not the go on PATH", "/usr/bin/definitely-not-the-go-on-path", ReasonNoToolchain},
		{"bad proxy", "", ReasonPolicy},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := checkout(t, "std")
			if tc.want == ReasonPolicy {
				p.Proxy = "https://proxy.example,direct"
			} else {
				p.GoBin = tc.gobin
			}
			got, st := Resolve(context.Background(), p)
			if st.Reason != tc.want {
				t.Errorf("reason %q, want %q", st.Reason, tc.want)
			}
			if len(got) != 0 {
				t.Errorf("%d resolutions, want none", len(got))
			}
		})
	}
}
