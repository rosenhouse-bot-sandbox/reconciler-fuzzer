package main

import (
	"strings"
	"testing"
)

func TestRunRejectsAnUnknownBug(t *testing.T) {
	if err := run([]string{"--bug=99"}, nil); err == nil || !strings.Contains(err.Error(), "--bug=99") {
		t.Errorf("run(--bug=99) = %v, want an error naming --bug=99", err)
	}
}

func TestCacheOptionsFollowTheWatchNamespace(t *testing.T) {
	if got := cacheOptions(""); got.DefaultNamespaces != nil {
		t.Errorf("no namespace gave %v", got.DefaultNamespaces)
	}
	got := cacheOptions("ns").DefaultNamespaces
	if _, ok := got["ns"]; !ok || len(got) != 1 {
		t.Errorf("ns gave %v", got)
	}
}
