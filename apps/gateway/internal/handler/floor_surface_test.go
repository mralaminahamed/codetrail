package handler

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"

	"github.com/mralaminahamed/codetrail/packages/shared/rag"
)

// The floor has two states and exactly one place knows which is current:
// rag.Floor.Calibrated. Everything a reader ever sees about it has to be
// derived from that field rather than written down as a fact.
//
// This is the guard P6's plan specified as
// TestNoShippedSurfaceStillSaysTheFloorIsMeasuredInP6 — "greps the tree for
// `measured in P6` and `until P6` outside docs/superpowers/ and fails on a
// hit" — and never wrote. It is generalised in the two directions that made
// that spelling too narrow to be worth having.
//
//  1. Not two literals. Any PHASE IDENTIFIER used as the floor's discriminator
//     is refused — "in P6", "until P6", "before P6", "while P6", "after P6"
//     and the rest of the preposition family, over P0 through P9. A phase is
//     not a state, no reader outside this repository can resolve one, and a
//     sentence keyed to a phase goes on saying the same thing after the phase
//     lands, which is exactly what happened. Past-tense narrative — "P6
//     measured a distribution and declined to calibrate from it" — is a fact
//     about a finished event and stays true, so it is allowed. The preposition
//     is what turns a phase into a claim about now.
//
//  2. Not raw file bytes. What is policed is the set of SHIPPED SURFACES: text
//     that reaches a reader. For Go that is string literals only, parsed out
//     of the AST — a Prometheus Help string is scraped onto a dashboard while
//     the comment above it is not, and both live in metrics.go. For the
//     console it is the source with comments stripped. For documentation it is
//     the whole file. For alerts.yml it is the annotations, which are what get
//     paged. Rationale comments citing spec:315 — which really does put the
//     number in a phase — are untouched, so nothing here asks anyone to lie
//     about where the requirement came from.
//
// docs/superpowers/ is excluded because the spec and the plans are records of
// what was believed when they were written, and correcting those into hindsight
// is the one thing this project's convention forbids.
//
// It would have failed before the fix wave, on five surfaces at once: README's
// "The number is measured in P6" and "nobody will see until P6 sets a number";
// docs/eval/README's "every surface that says the number is measured in P6
// still says so"; and metrics.go's two scraped Help strings, "the floor is
// calibrated in P6 …" and "It is 0 until P6 measures one". The alert
// annotation that told an operator the firing was "Expected while P6 is
// unlanded" is caught by the annotations rule below.
func TestNoShippedSurfaceAssertsTheFloorsCalibrationState(t *testing.T) {
	root := floorSurfaceRoot(t)
	scanned := 0
	for _, f := range floorSurfaceFiles(t, root) {
		text, ok := floorSurfaceText(t, root, f)
		if !ok {
			continue
		}
		scanned++
		for _, hit := range phaseKeyedFloorClaims(text) {
			t.Errorf("%s states the floor's calibration state against a phase instead of following Floor.Calibrated: %q", f, hit)
		}
	}
	// A classifier that skipped everything would pass in silence, which is the
	// failure mode this whole test exists to catch one directory over.
	if scanned < 20 {
		t.Fatalf("only %d shipped surfaces were scanned; the file classifier has stopped matching anything", scanned)
	}
}

// And the half a grep cannot see: the sentence a caller actually reads has to
// CHANGE when Calibrated changes. A surface producing the same words either way
// would pass the textual rule above and still be a literal.
//
// The gateway's boot log line is the other runtime surface and is asserted in
// apps/gateway/cmd by TestTheDefaultRetrieverIsVectorAtTheUncalibratedFloor,
// which matches on what the line says rather than on the phase it used to name.
func TestTheRefusalSentenceFollowsTheFloorRatherThanStatingOne(t *testing.T) {
	uncal := detail(rag.ReasonBelowFloor, rag.Floor{Value: 0.4, Calibrated: false})
	cal := detail(rag.ReasonBelowFloor, rag.Floor{Value: 0.4, Calibrated: true})
	if uncal == cal {
		t.Fatalf("the below-floor sentence is identical for a measured and an unmeasured floor: %q", cal)
	}
	if !strings.Contains(uncal, "not a measured threshold") {
		t.Errorf("an uncalibrated floor does not say it is unmeasured: %q", uncal)
	}
	for _, banned := range []string{"not a measured threshold", "no evaluation has chosen"} {
		if strings.Contains(cal, banned) {
			t.Errorf("a measured floor is still described as unmeasured (%q): %q", banned, cal)
		}
	}
	// Both spellings have to carry the number, or "the floor" is a word an
	// operator cannot act on.
	for _, s := range []string{uncal, cal} {
		if !strings.Contains(s, "0.4") {
			t.Errorf("a below-floor sentence does not name the floor it compared against: %q", s)
		}
	}
}

// prepositionPhase is a phase identifier as a preposition's object, which is
// what makes it a claim about the present rather than a note about history.
var prepositionPhase = regexp.MustCompile(`(?i)\b(in|into|until|till|before|after|while|since|by|at|from|during|through|pending|awaiting)\s+P[0-9]\b`)

// floorWord is what makes such a claim this test's business rather than an
// ordinary phase reference: "vector since P7" beside a retrieval default is
// fine and says something true.
var floorWord = regexp.MustCompile(`(?i)\b(floor|calibrat\w*|ANSWER_SCORE_FLOOR|score_floor)\b`)

// floorClaimWindow is how close the two have to be to count as one claim. Wide
// enough to cross a sentence boundary, because every real offender did.
const floorClaimWindow = 160

// phaseKeyedFloorClaims returns each phase-keyed claim about the floor as the
// text around it, so a failure names the sentence rather than an offset.
func phaseKeyedFloorClaims(text string) []string {
	var out []string
	for _, m := range prepositionPhase.FindAllStringIndex(text, -1) {
		lo, hi := max(m[0]-floorClaimWindow, 0), min(m[1]+floorClaimWindow, len(text))
		if around := text[lo:hi]; floorWord.MatchString(around) {
			out = append(out, strings.Join(strings.Fields(around), " "))
		}
	}
	return out
}

// floorSurfaceText returns the part of a file a reader ever sees, and false for
// a file that ships nothing. The classification is the substance of this test:
// every skip below is a claim that the file's text reaches nobody.
func floorSurfaceText(t *testing.T, root, rel string) (string, bool) {
	t.Helper()
	switch {
	case strings.HasPrefix(rel, "docs/superpowers/"):
		return "", false
	// A test may quote a forbidden phrase in order to forbid it — this file
	// does, and so does the console's Refusal suite.
	case strings.HasSuffix(rel, "_test.go"),
		strings.HasSuffix(rel, ".test.ts"), strings.HasSuffix(rel, ".test.tsx"):
		return "", false
	}
	abs := filepath.Join(root, rel)
	switch {
	case strings.HasSuffix(rel, ".go"):
		return goStringLiterals(t, abs), true
	case strings.HasSuffix(rel, ".ts"), strings.HasSuffix(rel, ".tsx"):
		return stripSlashComments(readWholeFile(t, abs)), true
	case strings.HasSuffix(rel, ".md"):
		return readWholeFile(t, abs), true
	// Annotations only: a summary and a description are paged to a human, and
	// the YAML comments around them are for whoever edits the rule.
	case rel == "infra/prometheus/alerts.yml":
		var b strings.Builder
		for _, line := range strings.Split(readWholeFile(t, abs), "\n") {
			if s := strings.TrimSpace(line); strings.HasPrefix(s, "summary:") || strings.HasPrefix(s, "description:") {
				b.WriteString(s + "\n")
			}
		}
		return b.String(), true
	}
	return "", false
}

// goStringLiterals is why this parses instead of grepping: metrics.go holds a
// scraped Help string and an unscraped comment about it in the same var block.
func goStringLiterals(t *testing.T, abs string) string {
	t.Helper()
	f, err := parser.ParseFile(token.NewFileSet(), abs, nil, parser.SkipObjectResolution)
	if err != nil {
		// Build tags do not stop a file parsing; a file that will not parse is
		// somebody else's failing build, not this test's finding.
		t.Logf("skipping unparseable %s: %v", abs, err)
		return ""
	}
	var b strings.Builder
	ast.Inspect(f, func(n ast.Node) bool {
		lit, ok := n.(*ast.BasicLit)
		if !ok || lit.Kind != token.STRING {
			return true
		}
		s, err := strconv.Unquote(lit.Value)
		if err != nil {
			s = lit.Value
		}
		b.WriteString(s + "\n")
		return true
	})
	return b.String()
}

// stripSlashComments removes // and /* */ so a comment explaining a banned
// phrase is not itself a violation. Crude — it does not know about string or
// regex literals — and deliberately so: over-removal can only make this test
// weaker on a file, never wrong about one, and every surface it guards here is
// JSX text rather than a string containing "//".
func stripSlashComments(src string) string {
	var b strings.Builder
	for i := 0; i < len(src); {
		switch {
		case strings.HasPrefix(src[i:], "//"):
			j := strings.IndexByte(src[i:], '\n')
			if j < 0 {
				return b.String()
			}
			i += j
		case strings.HasPrefix(src[i:], "/*"):
			j := strings.Index(src[i+2:], "*/")
			if j < 0 {
				return b.String()
			}
			i += j + 4
		default:
			b.WriteByte(src[i])
			i++
		}
	}
	return b.String()
}

func readWholeFile(t *testing.T, abs string) string {
	t.Helper()
	b, err := os.ReadFile(abs)
	if err != nil {
		t.Fatalf("read %s: %v", abs, err)
	}
	return string(b)
}

// floorSurfaceFiles asks git rather than walking, for the reason the Makefile
// does: node_modules is on disk and is nobody's shipped surface.
func floorSurfaceFiles(t *testing.T, root string) []string {
	t.Helper()
	out, err := exec.Command("git", "-C", root, "ls-files", "-z").Output()
	if err != nil {
		t.Fatalf("git ls-files: %v", err)
	}
	var files []string
	for _, f := range strings.Split(string(out), "\x00") {
		if f != "" {
			files = append(files, f)
		}
	}
	if len(files) == 0 {
		t.Fatal("git ls-files returned nothing; this test would pass vacuously")
	}
	return files
}

func floorSurfaceRoot(t *testing.T) string {
	t.Helper()
	out, err := exec.Command("git", "rev-parse", "--show-toplevel").Output()
	if err != nil {
		t.Skipf("not a git checkout: %v", err)
	}
	root := strings.TrimSpace(string(out))
	if _, err := os.Stat(filepath.Join(root, "go.mod")); err != nil {
		t.Fatalf("%s is not the module root", root)
	}
	return root
}
