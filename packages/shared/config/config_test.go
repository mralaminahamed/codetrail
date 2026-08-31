package config

import (
	"os"
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

func TestGetIntParsesOrFallsBack(t *testing.T) {
	os.Setenv("TP_N", "42")
	t.Cleanup(func() { os.Unsetenv("TP_N") })
	if got := GetInt("TP_N", 7); got != 42 {
		t.Fatalf("want 42, got %d", got)
	}
	os.Setenv("TP_N", "nope")
	if got := GetInt("TP_N", 7); got != 7 {
		t.Fatalf("want 7 on bad parse, got %d", got)
	}
}
