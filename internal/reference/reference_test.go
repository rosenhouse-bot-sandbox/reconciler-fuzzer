package reference_test

import (
	"reflect"
	"slices"
	"testing"
	"time"

	"github.com/rosenhouse-bot-sandbox/reconciler-fuzzer/internal/reference"
)

type (
	declared struct {
		Name   string          `json:"name"`
		Block  block           `json:"block"`
		Maybe  *block          `json:"maybe,omitempty"`
		List   []block         `json:"list"`
		Map    map[string]item `json:"map"`
		Leaf   *leaf           `json:"leaf"`
		Stamp  time.Time       `json:"stamp"`
		Counts map[string]int  `json:"counts"`
	}
	block struct {
		A int `json:"a"`
		B int `json:"b"`
	}
	item struct {
		C []string `json:"c"`
	}
	leaf struct {
		D int `json:"d"`
	}
)

func TestKeysNamesEachKeyAndNoBlock(t *testing.T) {
	want := []string{"name", "block.a", "block.b", "maybe.a", "maybe.b", "list", "list[*].a", "list[*].b",
		"map", "map[*].c", "leaf", "stamp", "counts"}

	if got := reference.Keys(reflect.TypeFor[declared](), reflect.TypeFor[leaf]()); !slices.Equal(got, want) {
		t.Errorf("Keys named %v, want %v.", got, want)
	}
}

func TestSetKeysNamesTheKeysAValueSets(t *testing.T) {
	value := declared{
		Block:  block{B: 1},
		Maybe:  &block{A: 1},
		List:   []block{{A: 1}, {B: 1}},
		Map:    map[string]item{"x": {}},
		Leaf:   &leaf{},
		Counts: map[string]int{},
	}
	want := []string{"block.b", "leaf", "list", "list[*].a", "list[*].b", "map", "maybe.a"}

	if got := reference.SetKeys(reflect.ValueOf(value), reflect.TypeFor[leaf]()); !slices.Equal(got, want) {
		t.Errorf("SetKeys named %v, want %v.", got, want)
	}
}

func TestSpansListsTheCodeSpansInACell(t *testing.T) {
	if got, want := reference.Spans("One of `get` and `list`, or none."), []string{"get", "list"}; !slices.Equal(got, want) {
		t.Errorf("Spans found %v, want %v.", got, want)
	}
}

func TestSaysFindsASentenceAcrossLinesInItsSectionAlone(t *testing.T) {
	const wrapped = "After a fault, a restart or a CR's deletion, it can run longer, while the checks still give your controller time."
	const underFaultFields = "A fault sets exactly one action."

	if !reference.Says(t, "### Ops", wrapped) {
		t.Errorf("Says did not find under ### Ops: %s", wrapped)
	}
	if reference.Says(t, "### Ops", underFaultFields) {
		t.Errorf("Says found under ### Ops a sentence of ### Fault fields: %s", underFaultFields)
	}
}
