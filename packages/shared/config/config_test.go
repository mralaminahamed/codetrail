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
