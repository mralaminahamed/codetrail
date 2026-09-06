package config

import (
	"os"
	"strings"
	"testing"
)

func TestGetFallsBackToDefault(t *testing.T) {
	os.Unsetenv("TP_X")
	if got := Get("TP_X", "def"); got != "def" {
		t.Fatalf("want def, got %q", got)
	}
	os.Setenv("TP_X", "set")
	t.Cleanup(func() { os.Unsetenv("TP_X") })
	if got := Get("TP_X", "def"); got != "set" {
		t.Fatalf("want set, got %q", got)
	}
}

// A value that is not an integer is a refusal, not the default. The default is
// for a knob nobody set; a knob somebody set to "4O" is one they believe is in
// force, and answering 40 to it is the wrong-but-quiet failure this refuses.
func TestGetIntParsesOrRefuses(t *testing.T) {
	os.Unsetenv("TP_N")
	if got, err := GetInt("TP_N", 7); got != 7 || err != nil {
		t.Fatalf("unset: %d, %v", got, err)
	}
	os.Setenv("TP_N", "42")
	t.Cleanup(func() { os.Unsetenv("TP_N") })
	if got, err := GetInt("TP_N", 7); got != 42 || err != nil {
		t.Fatalf("want 42, got %d, %v", got, err)
	}
	// Empty is unset: a knob cleared in a compose file is not one set wrong.
	os.Setenv("TP_N", "")
	if got, err := GetInt("TP_N", 7); got != 7 || err != nil {
		t.Fatalf("empty: %d, %v", got, err)
	}
	// The message has to carry both, because "invalid syntax" alone leaves an
	// operator grepping eleven knobs for which one they mistyped.
	for _, v := range []string{"nope", "4O", "40.0", " 40", "40 "} {
		os.Setenv("TP_N", v)
		got, err := GetInt("TP_N", 7)
		if err == nil {
			t.Fatalf("%q was read as %d", v, got)
		}
		if !strings.Contains(err.Error(), "TP_N") || !strings.Contains(err.Error(), v) {
			t.Errorf("%q: the error names neither the setting nor the value: %v", v, err)
		}
	}
}

// A list-valued setting is where "empty is unset" fails OPEN, which is why
// GetList does not have that rule. Measured on ALLOWED_HOSTS, the only SSRF
// control codetrail has: read through Get, ALLOWED_HOSTS="" handed back
// ["github.com" "codeberg.org"] and admitted github.com, while ALLOWED_HOSTS=" "
// produced [" "] and refused it — one space between fail-open and fail-closed.
//
// The Get half is asserted alongside, because the contrast IS the property: a
// GetList that drifted back to Get's rule would otherwise only fail the
// indexer's test, one package away.
func TestGetListReadsAClearedSettingAsAnEmptyList(t *testing.T) {
	def := []string{"github.com", "codeberg.org"}
	os.Unsetenv("TP_L")
	if got := GetList("TP_L", def); len(got) != 2 || got[0] != "github.com" {
		t.Fatalf("unset: %q, want the default", got)
	}
	t.Cleanup(func() { os.Unsetenv("TP_L") })
	// Both spellings of "cleared" answer alike here rather than alike only
	// because a caller happens to trim.
	for _, v := range []string{"", " ", "  \t "} {
		os.Setenv("TP_L", v)
		if got := GetList("TP_L", def); len(got) != 0 {
			t.Errorf("TP_L=%q read as %q, want an empty list: clearing a permission grants nothing", v, got)
		}
	}
	// The contrast, on the spelling both readers see: Get hands the default
	// back, which for a permission means handing the permission back.
	os.Setenv("TP_L", "")
	if got := Get("TP_L", "github.com,codeberg.org"); got != "github.com,codeberg.org" {
		t.Errorf(`Get read TP_L="" as %q; the two readers are supposed to differ here`, got)
	}
	os.Setenv("TP_L", "a,b")
	if got := GetList("TP_L", def); len(got) != 2 || got[0] != "a" || got[1] != "b" {
		t.Errorf("TP_L=%q read as %q", "a,b", got)
	}
	// Unnormalised otherwise: the caller knows whether its entries are hosts.
	os.Setenv("TP_L", "a, b")
	if got := GetList("TP_L", def); len(got) != 2 || got[1] != " b" {
		t.Errorf("TP_L=%q read as %q, want the split left alone", "a, b", got)
	}
}

// The house rule this package now owns: a boolean knob is PARSED, so
// TYPECHECK=no is a refusal rather than a switch that reads as on. It was
// written out five times across two binaries — one helper per package plus
// three call sites, two bodies byte-identical — which is the drift GetInt was
// extracted to stop.
func TestGetBoolParsesOrRefuses(t *testing.T) {
	os.Unsetenv("TP_B")
	for _, def := range []bool{true, false} {
		if got, err := GetBool("TP_B", def); got != def || err != nil {
			t.Fatalf("unset with def %v: %v, %v", def, got, err)
		}
	}
	t.Cleanup(func() { os.Unsetenv("TP_B") })
	// Empty is unset, as it is for Get and GetInt: a knob cleared in a compose
	// file is not one set wrong. GetList is the one reader that differs, and
	// its doc says why.
	os.Setenv("TP_B", "")
	if got, err := GetBool("TP_B", true); !got || err != nil {
		t.Errorf("empty: %v, %v", got, err)
	}
	for v, want := range map[string]bool{
		"true": true, "TRUE": true, "True": true, "1": true, "t": true,
		"false": false, "FALSE": false, "0": false, "f": false,
	} {
		os.Setenv("TP_B", v)
		if got, err := GetBool("TP_B", !want); got != want || err != nil {
			t.Errorf("%q read as %v (%v), want %v", v, got, err, want)
		}
	}
	// The values that would silently invert a knob if this compared against
	// "true" instead of parsing.
	for _, v := range []string{"no", "yes", "on", "off", "nope", " true"} {
		os.Setenv("TP_B", v)
		got, err := GetBool("TP_B", true)
		if err == nil {
			t.Errorf("%q was read as %v", v, got)
		} else if !strings.Contains(err.Error(), "TP_B") || !strings.Contains(err.Error(), v) {
			t.Errorf("%q: the error names neither the setting nor the value: %v", v, err)
		}
	}
}
