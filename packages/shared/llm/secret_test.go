package llm

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/rs/zerolog"
)

// Five renderings, not one. A test asserting only String() passes under a
// mutant that fixes String and leaves GoString.
func TestTheKeyIsNeverInAnError_ALog_AString_OrJson(t *testing.T) {
	s := NewSecret(canary)
	for _, got := range []string{
		fmt.Sprintf("%s", s),
		fmt.Sprintf("%v", s),
		fmt.Sprintf("%+v", s),
		fmt.Sprintf("%#v", s),
		fmt.Sprint(s),
		s.String(),
		s.GoString(),
	} {
		if strings.Contains(got, canary) {
			t.Errorf("rendered %q", got)
		}
		if got != redacted && !strings.Contains(got, redacted) {
			t.Errorf("rendered %q, want %q", got, redacted)
		}
	}
	b, err := json.Marshal(s)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(b), canary) {
		t.Errorf("json.Marshal rendered %s", b)
	}
	b, err = json.Marshal(struct {
		Key Secret `json:"key"`
	}{s})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(b), canary) {
		t.Errorf("json.Marshal of an enclosing struct rendered %s", b)
	}
	// zerolog, which is what this project actually logs with.
	var buf bytes.Buffer
	lg := zerolog.New(&buf)
	lg.Info().Str("key", s.String()).Interface("secret", s).Msg("boot")
	if strings.Contains(buf.String(), canary) {
		t.Errorf("a zerolog line rendered %s", buf.String())
	}
	// And the fixture is proved able to see the key at all, so a passing run is
	// evidence rather than an absence.
	if !strings.Contains(fmt.Sprintf("%v", struct{ V string }{canary}), canary) {
		t.Fatal("the canary is not detectable by this assertion at all")
	}
}

// M6's fixture — a bare Secret — CANNOT see this, which is why both mutations
// exist: the three Secret methods are pinned separately from the fact that they
// are not sufficient.
func TestTheKeyIsNotPrintedWhenTheClientItselfIsFormatted(t *testing.T) {
	a, err := NewAnthropic(defaultBaseURL, "claude-opus-5", NewSecret(canary), time.Second)
	if err != nil {
		t.Fatal(err)
	}
	for _, got := range []string{
		fmt.Sprintf("%v", a),
		fmt.Sprintf("%+v", a),
		fmt.Sprintf("%#v", a),
		// Deliberately no %s here. go vet refuses %s on a non-Stringer, so
		// including it would make "delete Anthropic.String" a BUILD failure
		// rather than a behaviour change, and the mutation that matters — the
		// key appearing under %+v — would never be reached. %v covers the same
		// path behaviourally.
		// The value, not only the pointer: the methods are on a value receiver
		// so a dereferenced client is covered too.
		fmt.Sprintf("%+v", *a),
		fmt.Sprintf("%#v", *a),
	} {
		if strings.Contains(got, canary) {
			t.Errorf("rendered %q", got)
		}
	}
	var buf bytes.Buffer
	lg := zerolog.New(&buf)
	lg.Info().Str("client", fmt.Sprintf("%+v", a)).Interface("client_i", a).Msg("boot")
	if strings.Contains(buf.String(), canary) {
		t.Errorf("a zerolog line rendered %s", buf.String())
	}
	// Nested one level deeper, which is how a handler would hold it.
	type holder struct {
		Model *Anthropic
		Note  string
	}
	if got := fmt.Sprintf("%+v", holder{Model: a, Note: "n"}); strings.Contains(got, canary) {
		t.Errorf("a nested struct rendered %q", got)
	}
	// Every error the client can return, driven through the real paths, is
	// covered by the assertions in anthropic_test.go; this one covers the
	// constructor's own.
	if _, err := NewAnthropic("https://x", "", NewSecret(canary), time.Second); err != nil &&
		strings.Contains(err.Error(), canary) {
		t.Errorf("a constructor error carried the key: %v", err)
	}
}

func TestTheKeyComesFromAFileFirstAndAnEnvVarSecond(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "key")
	// With a trailing newline, which is what `echo > file` writes and which
	// would otherwise be a 401 nobody can see in a log.
	if err := os.WriteFile(path, []byte(canary+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	t.Setenv("LLM_API_KEY_FILE", path)
	t.Setenv("LLM_API_KEY", "sk-THE-WRONG-ONE")
	got, err := LoadSecret()
	if err != nil {
		t.Fatal(err)
	}
	if got.reveal() != canary {
		t.Errorf("LoadSecret read %q", got.reveal())
	}

	t.Setenv("LLM_API_KEY_FILE", "")
	got, err = LoadSecret()
	if err != nil {
		t.Fatal(err)
	}
	if got.reveal() != "sk-THE-WRONG-ONE" {
		t.Errorf("with no file, LoadSecret read %q", got.reveal())
	}

	t.Setenv("LLM_API_KEY", "")
	if _, err := LoadSecret(); err == nil {
		t.Errorf("LoadSecret accepted no key at all")
	} else if !strings.Contains(err.Error(), "LLM_API_KEY") {
		t.Errorf("the error does not name the setting: %v", err)
	}

	t.Setenv("LLM_API_KEY_FILE", filepath.Join(dir, "missing"))
	err = nil
	if _, err = LoadSecret(); err == nil {
		t.Errorf("a missing key file was accepted")
	}
	if err != nil && strings.Contains(err.Error(), canary) {
		t.Errorf("the error carried the key: %v", err)
	}
}
