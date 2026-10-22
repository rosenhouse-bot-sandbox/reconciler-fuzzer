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

func TestRunRejectsAnUnknownVersion(t *testing.T) {
	if err := run([]string{"--version=3"}, nil); err == nil || !strings.Contains(err.Error(), "--version=3") {
		t.Errorf("run(--version=3) = %v, want an error naming --version=3", err)
	}
}

func TestCacheOptionsFollowTheWatchNamespace(t *testing.T) {
	if got := cacheOptions(""); got.DefaultNamespaces != nil {
		t.Errorf("no namespace gave %v", got.DefaultNamespaces)
	}
	if got := cacheOptions("ns"); len(got.DefaultNamespaces) != 1 {
		t.Errorf("ns gave %v", got.DefaultNamespaces)
	}
}
