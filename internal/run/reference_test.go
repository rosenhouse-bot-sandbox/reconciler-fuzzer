package run

import (
	"fmt"
	"maps"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/rosenhouse-bot-sandbox/reconciler-fuzzer/internal/proxy"
	"github.com/rosenhouse-bot-sandbox/reconciler-fuzzer/internal/reference"
)

const (
	opsHeading        = "### Ops"
	referenceSequence = "../../docs/reference/sequence.json"
)

var (
	sequenceKeys = reference.Keys(reflect.TypeFor[Sequence](), reflect.TypeFor[Op]())
	opKeys       = reference.Keys(reflect.TypeFor[Op](), reflect.TypeFor[Fault]())
	faultKeys    = reference.Keys(reflect.TypeFor[Fault]())
)

func TestTheReferenceListsEveryField(t *testing.T) {
	for heading, fields := range map[string][]string{
		"### Sequence fields": sequenceKeys,
		"### Op fields":       opKeys,
		"### Fault fields":    faultKeys,
	} {
		t.Run(heading, func(t *testing.T) {
			documented := reference.Listed(t, heading)

			for _, field := range fields {
				if !slices.Contains(documented, field) {
					t.Errorf("%s has no row for %s.", reference.Path, field)
				}
			}
			for _, row := range documented {
				if !slices.Contains(fields, row) {
					t.Errorf("%s has a row for %s, which is no such field.", reference.Path, row)
				}
			}
		})
	}
}

func TestTheReferenceSaysASettleWaitCanRunPastTimeoutsSettle(t *testing.T) {
	const says = "After a fault, a restart or a CR's deletion, it can run longer, while the checks still give your controller time."

	if !reference.Says(t, opsHeading, says) {
		t.Errorf("%s does not say under %q: %s", reference.Path, opsHeading, says)
	}
}

func TestTheReferenceSaysAHeldRequestHoldsASettleWait(t *testing.T) {
	for _, says := range []string{
		"A request the proxy holds under `action.delay` counts as a change until the proxy forwards it.",
		"One held as the wait's time runs out keeps the wait open until `timeouts.settle` past its release.",
		"It keeps a `recreate`'s wait for its old CR open the same way.",
		"reconciler-fuzzer does not check your properties where a wait ends with a request held, or released within `timeouts.stable`, and the run notes it.",
	} {
		if !reference.Says(t, opsHeading, says) {
			t.Errorf("%s does not say under %q: %s", reference.Path, opsHeading, says)
		}
	}
}

func TestTheReferenceSaysAFaultExcusesPropertiesAndStopsARecreate(t *testing.T) {
	for _, says := range []string{
		"Nor does it where a fault is active, or your controller is still owed time to recover from one.",
		"A property evaluated `always` is still checked at every change.",
		"A `recreate` whose old CR a fault keeps past the wait cannot create its CR, so it stops the run, and the run notes it.",
		"End the fault before the `recreate` with `until.op` to test the ops after it.",
	} {
		if !reference.Says(t, opsHeading, says) {
			t.Errorf("%s does not say under %q: %s", reference.Path, opsHeading, says)
		}
	}
}

func TestTheReferenceListsEveryOpType(t *testing.T) {
	var want []string
	for _, opType := range opTypes {
		want = append(want, string(opType))
	}

	if documented := reference.Listed(t, opsHeading); !slices.Equal(documented, want) {
		t.Errorf("%s lists the ops %v, want %v.", reference.Path, documented, want)
	}
}

func TestTheReferenceSaysWhatEachOpTakesAndWhetherItSettles(t *testing.T) {
	for _, opType := range opTypes {
		row := reference.Row(t, opsHeading, string(opType))
		var needs, mayCarry []string
		for field := range maps.Keys(fieldsOf(opType)) {
			if field == "until" {
				field = "until.op"
			}
			needs = append(needs, field)
		}
		if slices.Contains(namingOps, opType) {
			mayCarry = append(mayCarry, "cr")
		}
		if opType.OnCR() {
			mayCarry = append(mayCarry, "noSettle")
		}
		settles := map[bool]string{true: "yes", false: "no"}[Op{Type: opType}.Settles()]

		if got := sorted(reference.Spans(row[0])); !slices.Equal(got, sorted(needs)) {
			t.Errorf("The reference says %s needs %v, want %v.", opType, got, sorted(needs))
		}
		if got := sorted(reference.Spans(row[1])); !slices.Equal(got, sorted(mayCarry)) {
			t.Errorf("The reference says %s may carry %v, want %v.", opType, got, sorted(mayCarry))
		}
		if row[2] != settles {
			t.Errorf("The reference says whether %s settles: %s, want %s.", opType, row[2], settles)
		}
	}
}

func sorted(fields []string) []string {
	return slices.Sorted(slices.Values(fields))
}

func TestTheReferenceGivesTheBoundsOfFaultValues(t *testing.T) {
	for _, bounded := range []struct {
		field, says, spec string
		in, out           []string
	}{
		{"match.fraction", "above 0 and up to 1", `{"match": {"fraction": %s}, "action": {"drop": true}}`, []string{"0.001", "1"}, []string{"0", "1.001"}},
		{"action.error", "from 400 to 599", `{"action": {"error": %s}}`, []string{"400", "599"}, []string{"399", "600"}},
		{"action.delay", "It is not negative, and 0 leaves it unset.", `{"action": {"delay": "%s"}}`, []string{"1ns"}, []string{"-1ns"}},
		{"until.op", "It is above the fault's own `i`. Past the last op, the fault lasts to the end.", `{"action": {"drop": true}, "until": {"op": %s}}`, []string{"1", "2"}, []string{"0", "-1"}},
		{"until.count", "It is above 0.", `{"action": {"drop": true}, "until": {"count": %s}}`, []string{"1"}, []string{"0", "-1"}},
		{"until.for", "It is above 0.", `{"action": {"drop": true}, "until": {"for": "%s"}}`, []string{"1ns"}, []string{"0s", "-1ns"}},
	} {
		t.Run(bounded.field, func(t *testing.T) {
			if row := reference.Row(t, "### Fault fields", bounded.field)[1]; !strings.Contains(row, bounded.says) {
				t.Errorf("The row for %s does not say %q: %s", bounded.field, bounded.says, row)
			}
			for _, value := range bounded.in {
				if err := readFault(fmt.Sprintf(bounded.spec, value)); err != nil {
					t.Errorf("A fault of %s %s was refused: %v", bounded.field, value, err)
				}
			}
			for _, value := range bounded.out {
				if err := readFault(fmt.Sprintf(bounded.spec, value)); err == nil {
					t.Errorf("A fault of %s %s was accepted.", bounded.field, value)
				}
			}
		})
	}
}

func readFault(spec string) error {
	_, err := UnmarshalSequence([]byte(`{"seed": 1, "target": "t", "ops": [{"i": 0, "t": "fault", "spec": ` + spec +
		`}, {"i": 1, "t": "settle"}]}`))
	return err
}

func TestTheReferenceListsEveryVerb(t *testing.T) {
	documented := reference.Spans(reference.Row(t, "### Fault fields", "match.verb")[1])

	if !slices.Equal(documented, proxy.Verbs) {
		t.Errorf("The row for match.verb names %v, want %v.", documented, proxy.Verbs)
	}
}

func TestTheReferenceSequenceHoldsEveryOpAndSetsEveryField(t *testing.T) {
	sequence, err := ReadSequence(referenceSequence)
	if err != nil {
		t.Fatal(err)
	}
	var held []OpType
	var opsSet, faultsSet []string
	for _, op := range sequence.Ops {
		held = append(held, op.Type)
		opsSet = append(opsSet, reference.SetKeys(reflect.ValueOf(op), reflect.TypeFor[Fault]())...)
		if op.Fault != nil {
			faultsSet = append(faultsSet, reference.SetKeys(reflect.ValueOf(*op.Fault))...)
		}
	}

	for _, opType := range opTypes {
		if !slices.Contains(held, opType) {
			t.Errorf("%s holds no %s op.", referenceSequence, opType)
		}
	}
	for _, field := range opKeys {
		if !slices.Contains(opsSet, field) {
			t.Errorf("No op in %s sets %s.", referenceSequence, field)
		}
	}
	for _, field := range faultKeys {
		if !slices.Contains(faultsSet, field) {
			t.Errorf("No fault in %s sets %s.", referenceSequence, field)
		}
	}
}
