package generate

import (
	"slices"
	"strconv"
	"testing"

	"github.com/rosenhouse-bot-sandbox/reconciler-fuzzer/internal/reference"
)

func TestTheReferenceNamesEveryOverlayKeyword(t *testing.T) {
	documented := reference.Spans(reference.Row(t, "## target.yaml", "generate.overlay")[1])

	if !slices.Equal(slices.Sorted(slices.Values(documented)), keywords) {
		t.Errorf("The row for generate.overlay names %v, want the keywords reconciler-fuzzer reads, %v.", documented, keywords)
	}
}

// The rules target's schema describes spec paths that generation cannot draw a
// value for.
func TestTheReferenceGivesTheDefaultMutate(t *testing.T) {
	documented := reference.Row(t, "## target.yaml", "generate.mutate")[0]

	if want := "each spec path generation can draw a value for"; documented != want {
		t.Errorf("The reference gives generate.mutate the default %q, want %q.", documented, want)
	}
	if leftAlone := newGenerator(t, loadTarget(t, rulesTarget), Options{}).LeftAlone(); len(leftAlone) == 0 {
		t.Errorf("Generation for %s changes every spec path its schema describes, want some left alone.", rulesTarget)
	}
}

func TestTheReferenceGivesTheDefaultMaxCRs(t *testing.T) {
	documented := reference.Row(t, "## target.yaml", "generate.maxCRs")[0]

	if want := "`" + strconv.Itoa(defaultMaxCRs) + "`"; documented != want {
		t.Errorf("The reference gives generate.maxCRs the default %s, want %s.", documented, want)
	}
}

func TestTheReferenceGivesTheDefaultFaults(t *testing.T) {
	documented := reference.Row(t, "## target.yaml", "generate.faults")[0]

	if want := "`true`"; documented != want {
		t.Errorf("The reference gives generate.faults the default %s, want %s.", documented, want)
	}
}
