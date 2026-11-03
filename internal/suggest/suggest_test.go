package suggest

import "testing"

func TestNearest(t *testing.T) {
	names := []string{"slot", "steps", "status", "resource"}
	for key, want := range map[string]string{
		"slott":    "slot",
		"Slot":     "slot",
		"stpes":    "steps",
		"resourse": "resource",
		"sltuo":    "",
		"sl":       "",
		"":         "",
	} {
		if got := Nearest(key, names); got != want {
			t.Errorf("Nearest(%q) = %q, want %q", key, got, want)
		}
	}
	if got := Nearest("stat", []string{"state", "stats"}); got != "state" {
		t.Errorf("Nearest(stat) = %q, want the first of two at one edit", got)
	}
}

func TestList(t *testing.T) {
	for want, names := range map[string][]string{
		"":          nil,
		"a":         {"a"},
		"a or b":    {"a", "b"},
		"a, b or c": {"a", "b", "c"},
	} {
		if got := List(names, "or"); got != want {
			t.Errorf("List(%q) = %q, want %q", names, got, want)
		}
	}
}
