package invariant_test

import (
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/rosenhouse-bot-sandbox/reconciler-fuzzer/internal/invariant"
	"github.com/rosenhouse-bot-sandbox/reconciler-fuzzer/internal/observe"
)

// A violation quotes the evidence nearest it, because that is what explains it
// (DESIGN.md §5.7, D35).
func TestRecentQuotesTheEntriesNearestTheViolation(t *testing.T) {
	entries := make([]int, 25)
	for i := range entries {
		entries[i] = i
	}

	got := invariant.Recent(entries).Quoted

	if len(got) != 20 {
		t.Fatalf("Recent kept %d of %d entries, want the 20 a report carries.", len(got), len(entries))
	}
	if got[0] != 5 || got[19] != 24 {
		t.Errorf("Recent kept the entries %v, want the last 20.", got)
	}
}

// A check bounds what it quotes, so it has to say how much it chose from or a
// report cannot tell a complete excerpt from a bounded one (#22).
func TestRecentSaysHowMuchItChoseFrom(t *testing.T) {
	for _, entries := range []int{3, invariant.MaxEvidence, 25} {
		got := invariant.Recent(make([]int, entries))

		if got.Total != entries {
			t.Errorf("Recent over %d entries says it chose from %d.", entries, got.Total)
		}
		if want := min(entries, invariant.MaxEvidence); len(got.Quoted) != want {
			t.Errorf("Recent over %d entries quotes %d, want %d.", entries, len(got.Quoted), want)
		}
	}
}

func TestSampleQuotesOneFromEachKindInTurnNewestFirst(t *testing.T) {
	var managed []observe.Version
	for i := range 3 {
		managed = append(managed,
			version(configMapGVK, "w-"+strconv.Itoa(i), time.Duration(i)*time.Second),
			version(secretGVK, "s-"+strconv.Itoa(i), time.Duration(i)*time.Second))
	}

	got := invariant.Sample(managed).Quoted

	want := []string{"w-2", "s-2", "w-1", "s-1", "w-0", "s-0"}
	if names := names(got); strings.Join(names, ",") != strings.Join(want, ",") {
		t.Errorf("Sample quotes %v, want %v.", names, want)
	}
}

// KindName orders the kinds, so toy.reconciler-fuzzer/v1/Widget comes before
// v1/ConfigMap.
func TestSampleTakesTheKindsInTheOrderOfTheirNames(t *testing.T) {
	managed := []observe.Version{version(configMapGVK, "c", 0), version(widgetGVK, "w", 0)}

	got := names(invariant.Sample(managed).Quoted)

	if want := []string{"w", "c"}; !slices.Equal(got, want) {
		t.Errorf("Sample quotes %v, want %v.", got, want)
	}
}

func TestSampleBoundsTheStateAndSaysHowManyThereWere(t *testing.T) {
	for _, objects := range []int{3, invariant.MaxEvidence, 25} {
		var managed []observe.Version
		for i := range objects {
			managed = append(managed, version(configMapGVK, "w-"+strconv.Itoa(i), time.Duration(i)*time.Second))
		}

		got := invariant.Sample(managed)

		if got.Total != objects {
			t.Errorf("Sample over %d objects says the target managed %d.", objects, got.Total)
		}
		if want := min(objects, invariant.MaxEvidence); len(got.Quoted) != want {
			t.Errorf("Sample over %d objects quotes %d, want %d.", objects, len(got.Quoted), want)
		}
	}
}

// A timeline of one object's history names it, so that no site spells the
// subject out (#24).
func TestRecentHistoryNamesTheObjectItQuotes(t *testing.T) {
	history := make([]observe.Version, 25)
	for i := range history {
		history[i] = version(widgetGVK, widgetName, time.Duration(i)*time.Second)
	}

	got := invariant.RecentHistory(history[0].Key, history)

	if want := "toy.reconciler-fuzzer/v1/Widget w"; got.Of != want {
		t.Errorf("RecentHistory quotes the versions of %q, want %q.", got.Of, want)
	}
	if len(got.Quoted) != invariant.MaxEvidence || got.Total != len(history) {
		t.Errorf("RecentHistory quotes %d of %d, want %d of %d.", len(got.Quoted), got.Total, invariant.MaxEvidence, len(history))
	}
}
