package run

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"

	"github.com/rosenhouse-bot-sandbox/reconciler-fuzzer/internal/target"
)

const goldenSequence = "testdata/sequence.json"

// designExample is the sequence of DESIGN.md §7, as written there.
const designExample = `{
  "seed": 8675309,
  "target": "toy-widget",
  "ops": [
    {"i": 0, "t": "create", "obj": {"apiVersion": "toy.reconciler-fuzzer/v1", "kind": "Widget", "spec": {"count": 3}}},
    {"i": 1, "t": "fault", "spec": {"match": {"verb": "create", "resource": "configmaps", "fraction": 0.5}, "action": {"error": 500}, "until": {"op": 3}}},
    {"i": 2, "t": "update", "patch": {"spec": {"count": 5}}, "noSettle": true},
    {"i": 3, "t": "settle"},
    {"i": 4, "t": "restart"},
    {"i": 5, "t": "settle"},
    {"i": 6, "t": "deleteManaged", "kind": "v1/ConfigMap", "index": 0},
    {"i": 7, "t": "delete"}
  ]
}`

func TestGoldenSequenceIsByteIdenticalAfterARoundTrip(t *testing.T) {
	want, err := os.ReadFile(goldenSequence)
	if err != nil {
		t.Fatalf("Reading the golden sequence failed: %v", err)
	}

	decoded, err := UnmarshalSequence(want)
	if err != nil {
		t.Fatalf("Decoding the golden sequence failed: %v", err)
	}
	got, err := decoded.Marshal()
	if err != nil {
		t.Fatalf("Encoding the golden sequence failed: %v", err)
	}

	if string(got) != string(want) {
		t.Errorf("The golden sequence came back as\n%s\nwant\n%s", got, want)
	}
}

// The golden sequence holds every op type, so the round trip covers each one.
func TestGoldenSequenceHoldsEveryOpType(t *testing.T) {
	decoded := readGolden(t)

	held := map[OpType]bool{}
	for _, op := range decoded.Ops {
		held[op.Type] = true
	}
	for _, opType := range opTypes {
		if !held[opType] {
			t.Errorf("The golden sequence holds no %q op.", opType)
		}
	}
}

func TestSequenceReadsTheDesignExample(t *testing.T) {
	decoded, err := UnmarshalSequence([]byte(designExample))
	if err != nil {
		t.Fatalf("Decoding the example of DESIGN.md §7 failed: %v", err)
	}

	if decoded.Seed != 8675309 || decoded.Target != "toy-widget" {
		t.Errorf("The example decoded to seed %d and target %q.", decoded.Seed, decoded.Target)
	}
	create := decoded.Ops[0]
	if count, _, _ := unstructured.NestedInt64(create.Obj.Object, "spec", "count"); count != 3 {
		t.Errorf("The create op carries %v, want a Widget of count 3.", create.Obj)
	}
	fault := decoded.Ops[1].Fault
	if fault == nil || fault.Match.Fraction == nil || *fault.Match.Fraction != 0.5 || fault.Action.Error != 500 || fault.Until.Op == nil || *fault.Until.Op != 3 {
		t.Errorf("The fault op decoded to %+v.", fault)
	}
	update := decoded.Ops[2]
	if !update.NoSettle || update.Patch == nil {
		t.Errorf("The update op decoded to %+v, want a patch it does not settle after.", update)
	}
	deleteManaged := decoded.Ops[6]
	if deleteManaged.Kind != "v1/ConfigMap" || deleteManaged.Nth == nil || *deleteManaged.Nth != 0 {
		t.Errorf("The deleteManaged op decoded to %+v.", deleteManaged)
	}
	if types := opTypesOf(decoded); strings.Join(types, ",") != "create,fault,update,settle,restart,settle,deleteManaged,delete" {
		t.Errorf("The example decoded to the ops %v.", types)
	}
}

func slashRefused(resource string) string {
	return `match.resource "` + resource + `" holds a slash; name the plural alone, such as configmaps. ` +
		"A fault on a resource matches its subresources' requests too"
}

func TestSequenceRejectsMalformedOps(t *testing.T) {
	for _, test := range []struct {
		name string
		ops  string
		want string
	}{
		{
			name: "an unknown op type",
			ops:  `{"i": 0, "t": "reboot"}`,
			want: `"reboot"`,
		},
		{
			name: "an index that is not the op's position",
			ops:  `{"i": 1, "t": "settle"}`,
			want: "position",
		},
		{
			name: "an unknown field",
			ops:  `{"i": 0, "t": "settle", "wait": "5s"}`,
			want: "wait",
		},
		{
			name: "a create without an object",
			ops:  `{"i": 0, "t": "create"}`,
			want: "obj",
		},
		{
			name: "a recreate without an object",
			ops:  `{"i": 0, "t": "recreate"}`,
			want: "obj",
		},
		{
			name: "an update without a patch",
			ops:  `{"i": 0, "t": "update"}`,
			want: "patch",
		},
		{
			name: "a delete carrying an object",
			ops:  `{"i": 0, "t": "delete", "obj": {"kind": "Widget"}}`,
			want: "obj",
		},
		{
			name: "a settle carrying a patch",
			ops:  `{"i": 0, "t": "settle", "patch": {"spec": {"count": 1}}}`,
			want: "patch",
		},
		{
			name: "a deleteManaged without a kind",
			ops:  `{"i": 0, "t": "deleteManaged", "index": 0}`,
			want: "kind",
		},
		{
			name: "a deleteManaged without an index",
			ops:  `{"i": 0, "t": "deleteManaged", "kind": "v1/ConfigMap"}`,
			want: "index",
		},
		{
			name: "a deleteManaged with a negative index",
			ops:  `{"i": 0, "t": "deleteManaged", "kind": "v1/ConfigMap", "index": -1}`,
			want: "index",
		},
		{
			name: "a restart carrying a kind",
			ops:  `{"i": 0, "t": "restart", "kind": "v1/ConfigMap"}`,
			want: "kind",
		},
		{
			name: "a fault without a spec",
			ops:  `{"i": 0, "t": "fault"}`,
			want: "spec",
		},
		{
			name: "a fault with no action",
			ops:  `{"i": 0, "t": "fault", "spec": {"match": {"verb": "create"}}}`,
			want: "action",
		},
		{
			name: "a fault with two actions",
			ops:  `{"i": 0, "t": "fault", "spec": {"action": {"error": 500, "drop": true}}}`,
			want: "action",
		},
		{
			name: "a fault on a verb the proxy never records",
			ops:  `{"i": 0, "t": "fault", "spec": {"match": {"verb": "post"}, "action": {"drop": true}}}`,
			want: `match.verb "post" is not one of get, list, watch, create, update, patch, delete and deletecollection`,
		},
		{
			name: "a fault on a verb in capitals",
			ops:  `{"i": 0, "t": "fault", "spec": {"match": {"verb": "Create"}, "action": {"drop": true}}}`,
			want: `"Create"`,
		},
		{
			name: "a fault on a subresource",
			ops:  `{"i": 0, "t": "fault", "spec": {"match": {"resource": "widgets/status"}, "action": {"drop": true}}}`,
			want: slashRefused("widgets/status"),
		},
		{
			name: "a fault on a resource with its version",
			ops:  `{"i": 0, "t": "fault", "spec": {"match": {"resource": "v1/configmaps"}, "action": {"drop": true}}}`,
			want: slashRefused("v1/configmaps"),
		},
		{
			name: "a fault on a resource with a leading slash",
			ops:  `{"i": 0, "t": "fault", "spec": {"match": {"resource": "/configmaps"}, "action": {"drop": true}}}`,
			want: slashRefused("/configmaps"),
		},
		{
			name: "a fault on a resource with a trailing slash",
			ops:  `{"i": 0, "t": "fault", "spec": {"match": {"resource": "configmaps/"}, "action": {"drop": true}}}`,
			want: slashRefused("configmaps/"),
		},
		{
			name: "a fault on a percentage of requests",
			ops:  `{"i": 0, "t": "fault", "spec": {"match": {"fraction": 50}, "action": {"drop": true}}}`,
			want: "match.fraction 50 is not a share of requests above 0 and up to 1",
		},
		{
			name: "a fault on a negative fraction of requests",
			ops:  `{"i": 0, "t": "fault", "spec": {"match": {"fraction": -0.5}, "action": {"drop": true}}}`,
			want: "match.fraction -0.5 is not a share of requests above 0 and up to 1",
		},
		{
			name: "a fault on no share of requests",
			ops:  `{"i": 0, "t": "fault", "spec": {"match": {"fraction": 0}, "action": {"drop": true}}}`,
			want: "match.fraction 0 is not a share of requests above 0 and up to 1",
		},
		{
			name: "a fault on a name that is not a glob",
			ops:  `{"i": 0, "t": "fault", "spec": {"match": {"name": "widget-["}, "action": {"drop": true}}}`,
			want: `match.name "widget-[" is not a glob`,
		},
		{
			name: "a fault answering with a success",
			ops:  `{"i": 0, "t": "fault", "spec": {"action": {"error": 200}}}`,
			want: "action.error 200 is not an HTTP error status from 400 to 599",
		},
		{
			name: "a fault answering with a status past 599",
			ops:  `{"i": 0, "t": "fault", "spec": {"action": {"error": 600}}}`,
			want: "action.error 600 is not an HTTP error status from 400 to 599",
		},
		{
			name: "a fault answering with a negative status",
			ops:  `{"i": 0, "t": "fault", "spec": {"action": {"error": -500}}}`,
			want: "action.error -500 is not an HTTP error status from 400 to 599",
		},
		{
			name: "a fault delaying by a negative duration",
			ops:  `{"i": 0, "t": "fault", "spec": {"action": {"delay": "-1s"}}}`,
			want: "action.delay -1s is negative",
		},
		{
			name: "a fault that runs out after a negative count",
			ops:  `{"i": 0, "t": "fault", "spec": {"action": {"drop": true}, "until": {"count": -3}}}`,
			want: "until.count -3 is negative",
		},
		{
			name: "a fault that runs out after a negative duration",
			ops:  `{"i": 0, "t": "fault", "spec": {"action": {"drop": true}, "until": {"for": "-2s"}}}`,
			want: "until.for -2s is negative",
		},
		{
			name: "a fault that runs out on an unknown trigger",
			ops:  `{"i": 0, "t": "fault", "spec": {"action": {"drop": true}, "until": {"after": "2s"}}}`,
			want: `"after"`,
		},
		{
			name: "a fault whose until is no object",
			ops:  `{"i": 0, "t": "fault", "spec": {"action": {"drop": true}, "until": [1]}}`,
			want: "of type run.Trigger",
		},
		{
			name: "a fault that runs out at its own op",
			ops:  `{"i": 0, "t": "fault", "spec": {"action": {"drop": true}, "until": {"op": 0}}}`,
			want: "until.op names op 0; want an op after the fault",
		},
		{
			name: "a fault that runs out at an op before it",
			ops:  `{"i": 0, "t": "fault", "spec": {"action": {"drop": true}, "until": {"op": -1}}}`,
			want: "until.op names op -1; want an op after the fault",
		},
		{
			name: "a later fault that runs out at its own op",
			ops:  `{"i": 0, "t": "settle"}, {"i": 1, "t": "fault", "spec": {"action": {"drop": true}, "until": {"op": 1}}}`,
			want: "op 1: until.op names op 1; want an op after the fault",
		},
		{
			name: "a restart that skips its settle",
			ops:  `{"i": 0, "t": "restart", "noSettle": true}`,
			want: "noSettle",
		},
		{
			name: "a settle that skips its settle",
			ops:  `{"i": 0, "t": "settle", "noSettle": true}`,
			want: "noSettle",
		},
		{
			name: "a deleteManaged that skips its settle",
			ops:  `{"i": 0, "t": "deleteManaged", "kind": "v1/ConfigMap", "index": 0, "noSettle": true}`,
			want: "noSettle",
		},
		{
			name: "an updateFixture without a kind",
			ops:  `{"i": 0, "t": "updateFixture", "name": "token", "patch": {"data": {"token": "abcd"}}}`,
			want: "kind",
		},
		{
			name: "an updateFixture without a name",
			ops:  `{"i": 0, "t": "updateFixture", "kind": "v1/Secret", "patch": {"data": {"token": "abcd"}}}`,
			want: "name",
		},
		{
			name: "an updateFixture without a patch",
			ops:  `{"i": 0, "t": "updateFixture", "kind": "v1/Secret", "name": "token"}`,
			want: "patch",
		},
		{
			name: "an updateFixture that skips its settle",
			ops:  `{"i": 0, "t": "updateFixture", "kind": "v1/Secret", "name": "token", "patch": {"data": {"token": "abcd"}}, "noSettle": true}`,
			want: "noSettle",
		},
		{
			name: "a deleteFixture without an until",
			ops:  `{"i": 0, "t": "deleteFixture", "kind": "v1/Secret", "name": "token"}`,
			want: "until",
		},
		{
			name: "a deleteFixture carrying a patch",
			ops:  `{"i": 0, "t": "deleteFixture", "kind": "v1/Secret", "name": "token", "patch": {"data": {}}, "until": {"op": 1}}`,
			want: "patch",
		},
		{
			name: "an until that counts requests",
			ops:  `{"i": 0, "t": "deleteFixture", "kind": "v1/Secret", "name": "token", "until": {"count": 1}}`,
			want: "count",
		},
		{
			name: "a settle carrying a name",
			ops:  `{"i": 0, "t": "settle", "name": "token"}`,
			want: "name",
		},
		{
			name: "an update carrying an until",
			ops:  `{"i": 0, "t": "update", "patch": {"spec": {"count": 1}}, "until": {"op": 1}}`,
			want: "until",
		},
		{
			name: "a create naming a CR apart from its object",
			ops:  `{"i": 0, "t": "create", "cr": "widget-2", "obj": {"kind": "Widget"}}`,
			want: "a create op takes no cr",
		},
		{
			name: "a deleteManaged naming a CR",
			ops:  `{"i": 0, "t": "deleteManaged", "cr": "widget-2", "kind": "v1/ConfigMap", "index": 0}`,
			want: "a deleteManaged op takes no cr",
		},
		{
			name: "a restart naming a CR",
			ops:  `{"i": 0, "t": "restart", "cr": "widget-2"}`,
			want: "a restart op takes no cr",
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			_, err := UnmarshalSequence([]byte(`{"seed": 1, "target": "toy-widget", "ops": [` + test.ops + `]}`))

			if err == nil {
				t.Fatalf("The op %s was accepted, want an error naming %q.", test.ops, test.want)
			}
			if !strings.Contains(err.Error(), test.want) {
				t.Errorf("The op %s was rejected with %q, want the error to name %q.", test.ops, err, test.want)
			}
		})
	}
}

// A reader who guessed a field learns which ones the op needs.
func TestSequenceNamesTheFieldsAnOpNeedsBesideOneItTakesNot(t *testing.T) {
	for ops, want := range map[string]string{
		`{"i": 0, "t": "deleteManaged", "kind": "v1/ConfigMap", "name": "widget-0"}`:                           "op 0: a deleteManaged op takes no name; it needs kind and index",
		`{"i": 0, "t": "deleteFixture", "kind": "v1/Secret", "name": "token", "index": 0, "until": {"op": 1}}`: "op 0: a deleteFixture op takes no index; it needs kind, name and until",
		`{"i": 0, "t": "create", "obj": {"kind": "Widget"}, "name": "widget"}`:                                 "op 0: a create op takes no name; it needs obj",
		`{"i": 0, "t": "settle", "name": "token"}`:                                                             "op 0: a settle op takes no name",
		`{"i": 0, "t": "update", "obj": {"kind": "Widget"}}`:                                                   "op 0: an update op takes no obj; it needs patch",
		`{"i": 0, "t": "updateFixture", "kind": "v1/Secret", "name": "token"}`:                                 "op 0: an updateFixture op needs patch",
		`{"i": 0, "t": "updateFixture", "cr": "widget", "kind": "v1/Secret", "name": "token", "patch": {}}`:    "op 0: an updateFixture op takes no cr",
	} {
		_, err := UnmarshalSequence([]byte(`{"seed": 1, "target": "toy-widget", "ops": [` + ops + `]}`))

		if err == nil || err.Error() != want {
			t.Errorf("The ops %s were refused with %v, want %q.", ops, err, want)
		}
	}
}

// The JSON parses, but reconciler-fuzzer would read a count or for of 0 as no
// trigger.
func TestSequenceRefusesATriggerOf0AtItsOp(t *testing.T) {
	for until, want := range map[string]string{
		`{"count": 0}`:              "op 1: until.count is 0; give a count above 0, or leave it out",
		`{"count": 2, "for": "0s"}`: "op 1: until.for is 0s; give a duration above 0, or leave it out",
	} {
		_, err := UnmarshalSequence([]byte(`{"seed": 1, "target": "toy-widget", "ops": [{"i": 0, "t": "settle"}, ` +
			`{"i": 1, "t": "fault", "spec": {"action": {"drop": true}, "until": ` + until + `}}, {"i": 2, "t": "settle"}]}`))

		if err == nil || err.Error() != want {
			t.Errorf("The until %s was refused with %v, want %q.", until, err, want)
		}
	}
}

func TestSequenceRefusesATriggerOf0OnlyOnAFaultOpAfterTheOpsBeforeIt(t *testing.T) {
	for ops, want := range map[string]string{
		`{"i": 0, "t": "craete"}, {"i": 1, "t": "fault", "spec": {"action": {"drop": true}, "until": {"count": 0}}}`: `op 0: "craete" is not an op type`,
		`{"i": 0, "t": "settle", "spec": {"action": {"drop": true}, "until": {"for": "0s"}}}`:                        "op 0: a settle op takes no spec",
	} {
		_, err := UnmarshalSequence([]byte(`{"seed": 1, "target": "toy-widget", "ops": [` + ops + `]}`))

		if err == nil || !strings.HasPrefix(err.Error(), want) {
			t.Errorf("The ops %s were refused with %v, want %q.", ops, err, want)
		}
	}
}

func TestSequenceAcceptsTheOpsTheRunnerExecutes(t *testing.T) {
	for _, ops := range []string{
		`{"i": 0, "t": "create", "obj": {"kind": "Widget"}, "noSettle": true}`,
		`{"i": 0, "t": "update", "patch": {"spec": {"count": null}}}`,
		`{"i": 0, "t": "delete", "noSettle": true}`,
		`{"i": 0, "t": "recreate", "obj": {"kind": "Widget"}}`,
		`{"i": 0, "t": "update", "cr": "widget-2", "patch": {"spec": {"count": 1}}}`,
		`{"i": 0, "t": "delete", "cr": "widget-2"}`,
		`{"i": 0, "t": "recreate", "cr": "widget-2", "obj": {"kind": "Widget"}}`,
		`{"i": 0, "t": "fault", "spec": {"action": {"delay": "250ms"}, "until": {"for": "5s"}}}`,
		`{"i": 0, "t": "fault", "spec": {"action": {"drop": true}, "until": {"count": 3}}}`,
		`{"i": 0, "t": "fault", "spec": {"match": {"verb": "get"}, "action": {"drop": true}}}`,
		`{"i": 0, "t": "fault", "spec": {"match": {"verb": "list"}, "action": {"drop": true}}}`,
		`{"i": 0, "t": "fault", "spec": {"match": {"verb": "watch"}, "action": {"drop": true}}}`,
		`{"i": 0, "t": "fault", "spec": {"match": {"verb": "create"}, "action": {"drop": true}}}`,
		`{"i": 0, "t": "fault", "spec": {"match": {"verb": "update"}, "action": {"drop": true}}}`,
		`{"i": 0, "t": "fault", "spec": {"match": {"verb": "patch"}, "action": {"drop": true}}}`,
		`{"i": 0, "t": "fault", "spec": {"match": {"verb": "delete"}, "action": {"drop": true}}}`,
		`{"i": 0, "t": "fault", "spec": {"match": {"verb": "deletecollection"}, "action": {"drop": true}}}`,
		`{"i": 0, "t": "fault", "spec": {"match": {"name": "widget-*", "fraction": 1}, "action": {"error": 400}}}`,
		`{"i": 0, "t": "fault", "spec": {"match": {"fraction": 0.25}, "action": {"error": 599}}}`,
		`{"i": 0, "t": "fault", "spec": {"action": {"error": 500, "delay": "0s"}}}`,
		`{"i": 0, "t": "fault", "spec": {"action": {"drop": true}, "until": {"op": 1}}}`,
		`{"i": 0, "t": "fault", "spec": {"action": {"drop": true}, "until": {"op": 2}}}`,
		`{"i": 0, "t": "updateFixture", "kind": "v1/Secret", "name": "token", "patch": {"data": {"token": "abcd"}}}`,
		`{"i": 0, "t": "deleteFixture", "kind": "v1/Secret", "name": "token", "until": {"op": 1}}`,
	} {
		t.Run(ops, func(t *testing.T) {
			// The trailing settle is what the sequence needs, not the op under test.
			whole := `{"seed": 1, "target": "t", "ops": [` + ops + `, {"i": 1, "t": "settle"}]}`
			if _, err := UnmarshalSequence([]byte(whole)); err != nil {
				t.Errorf("The op was rejected: %v", err)
			}
		})
	}
}

// DESIGN.md §6: the teardown waits for convergence before it opens its quiet
// window only after a fault, so a sequence that ends while the target is
// still working is judged on that work. Every sequence ends with an op that
// settles.
func TestSequenceRequiresALastOpThatSettles(t *testing.T) {
	create := `{"i": 0, "t": "create", "obj": {"kind": "Widget"}}`
	for _, test := range []struct {
		name string
		ops  string
		want string
	}{
		{name: "no ops at all", ops: ``, want: "no ops"},
		{name: "a last op that skips its settle", want: "settle",
			ops: `{"i": 0, "t": "create", "obj": {"kind": "Widget"}, "noSettle": true}`},
		{name: "a trailing restart", ops: create + `, {"i": 1, "t": "restart"}`, want: "settle"},
		{name: "a trailing fault", want: "settle",
			ops: create + `, {"i": 1, "t": "fault", "spec": {"action": {"drop": true}, "until": {"count": 3}}}`},
	} {
		t.Run(test.name, func(t *testing.T) {
			_, err := UnmarshalSequence([]byte(`{"seed": 1, "target": "t", "ops": [` + test.ops + `]}`))

			if err == nil {
				t.Fatalf("The ops %s were accepted, want an error naming %q.", test.ops, test.want)
			}
			if !strings.Contains(err.Error(), test.want) {
				t.Errorf("The ops %s were rejected with %q, want the error to name %q.", test.ops, err, test.want)
			}
		})
	}
}

func TestADeletedFixtureComesBackByTheLastOp(t *testing.T) {
	const deleted = `{"i": 1, "t": "deleteFixture", "kind": "v1/Secret", "name": "token", "until": {"op": %d}}`
	create := `{"i": 0, "t": "create", "obj": {"kind": "Widget"}}, `
	settle := func(i int) string { return fmt.Sprintf(`, {"i": %d, "t": "settle"}`, i) }
	for _, test := range []struct {
		name string
		ops  string
		want string
	}{
		{name: "an until that names its own op", want: "until names op 1",
			ops: create + fmt.Sprintf(deleted, 1) + settle(2)},
		{name: "an until that names an earlier op", want: "until names op 0",
			ops: create + fmt.Sprintf(deleted, 0) + settle(2)},
		{name: "an until past the last op", want: "until names op 3",
			ops: create + fmt.Sprintf(deleted, 3) + settle(2)},
		{name: "an update of the fixture while it is deleted", want: "op 2 acts on the fixture v1/Secret token, which op 1 deleted until op 4",
			ops: create + fmt.Sprintf(deleted, 4) +
				`, {"i": 2, "t": "updateFixture", "kind": "v1/Secret", "name": "token", "patch": {"data": {"token": "abcd"}}}` +
				settle(3) + settle(4) + settle(5)},
		{name: "a second delete of the fixture while it is deleted", want: "op 2 acts on the fixture v1/Secret token",
			ops: create + fmt.Sprintf(deleted, 4) +
				`, {"i": 2, "t": "deleteFixture", "kind": "v1/Secret", "name": "token", "until": {"op": 4}}` +
				settle(3) + settle(4) + settle(5)},
	} {
		t.Run(test.name, func(t *testing.T) {
			_, err := UnmarshalSequence([]byte(`{"seed": 1, "target": "t", "ops": [` + test.ops + `]}`))

			if err == nil {
				t.Fatalf("The ops %s were accepted, want an error naming %q.", test.ops, test.want)
			}
			if !strings.Contains(err.Error(), test.want) {
				t.Errorf("The ops %s were rejected with %q, want the error to name %q.", test.ops, err, test.want)
			}
		})
	}
}

// reconciler-fuzzer restores the fixture before op until, so that op and the
// ops after it may act on the fixture again, and ops before it may act on
// another.
func TestAFixtureOpActsOnAFixtureThatIsThere(t *testing.T) {
	ops := `{"i": 0, "t": "deleteFixture", "kind": "v1/Secret", "name": "token", "until": {"op": 2}},
		{"i": 1, "t": "updateFixture", "kind": "v1/Secret", "name": "other", "patch": {"data": {"token": "abcd"}}},
		{"i": 2, "t": "updateFixture", "kind": "v1/Secret", "name": "token", "patch": {"data": {"token": "abcd"}}},
		{"i": 3, "t": "deleteFixture", "kind": "v1/Secret", "name": "token", "until": {"op": 4}},
		{"i": 4, "t": "settle"}`

	if _, err := UnmarshalSequence([]byte(`{"seed": 1, "target": "t", "ops": [` + ops + `]}`)); err != nil {
		t.Errorf("The ops were rejected: %v", err)
	}
}

// OnCR says which ops carry noSettle (DESIGN.md §4), which is what lets
// generation draw one.
func TestOnlyTheOpsOnThePrimaryCRActOnIt(t *testing.T) {
	for _, test := range []struct {
		opType OpType
		want   bool
	}{
		{opType: OpCreate, want: true},
		{opType: OpUpdate, want: true},
		{opType: OpDelete, want: true},
		{opType: OpRecreate, want: true},
		{opType: OpSettle},
		{opType: OpRestart},
		{opType: OpFault},
		{opType: OpDeleteManaged},
		{opType: OpUpdateFixture},
		{opType: OpDeleteFixture},
	} {
		t.Run(string(test.opType), func(t *testing.T) {
			if got := test.opType.OnCR(); got != test.want {
				t.Errorf("A %s op acts on the CR: %t, want %t.", test.opType, got, test.want)
			}
		})
	}
}

func TestOpSettlesUnlessItSaysOtherwise(t *testing.T) {
	for _, test := range []struct {
		opType OpType
		want   bool
	}{
		{opType: OpCreate, want: true},
		{opType: OpUpdate, want: true},
		{opType: OpDelete, want: true},
		{opType: OpRecreate, want: true},
		{opType: OpDeleteManaged, want: true},
		{opType: OpSettle, want: true},
		{opType: OpUpdateFixture, want: true},
		{opType: OpRestart},
		{opType: OpFault},
		{opType: OpDeleteFixture},
	} {
		t.Run(string(test.opType), func(t *testing.T) {
			if got := (Op{Type: test.opType}).Settles(); got != test.want {
				t.Errorf("A %s op settles: %t, want %t.", test.opType, got, test.want)
			}
			if got := (Op{Type: test.opType, NoSettle: true}).Settles(); got && test.opType != OpSettle {
				t.Errorf("A %s op with noSettle still settles.", test.opType)
			}
		})
	}
}

func TestManagedKindResolvesAgainstWhatTheTargetManages(t *testing.T) {
	toy := &target.Target{Manages: []schema.GroupVersionKind{
		configMapKind,
		{Group: "apps", Version: "v1", Kind: "Deployment"},
	}}

	for _, test := range []struct {
		declared string
		want     schema.GroupVersionKind
	}{
		{declared: "v1/ConfigMap", want: configMapKind},
		{declared: "apps/v1/Deployment", want: schema.GroupVersionKind{Group: "apps", Version: "v1", Kind: "Deployment"}},
	} {
		t.Run(test.declared, func(t *testing.T) {
			got, err := managedKind(toy, test.declared)
			if err != nil || got != test.want {
				t.Errorf("managedKind returned (%v, %v), want %v.", got, err, test.want)
			}
		})
	}

	if _, err := managedKind(toy, "v1/Secret"); err == nil || !strings.Contains(err.Error(), "v1/Secret") {
		t.Errorf("managedKind returned %v for a kind the target does not manage, want an error naming it.", err)
	}
}

func TestReadSequenceReportsAMissingFile(t *testing.T) {
	_, err := ReadSequence(filepath.Join(t.TempDir(), "absent.json"))

	if err == nil || !strings.Contains(err.Error(), "absent.json") {
		t.Errorf("ReadSequence returned %v, want an error naming the file.", err)
	}
}

func TestWriteSequenceWritesTheCanonicalForm(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sequence.json")
	golden := readGolden(t)

	if err := WriteSequence(path, golden); err != nil {
		t.Fatalf("WriteSequence failed: %v", err)
	}

	written, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("Reading back the sequence failed: %v", err)
	}
	want, err := os.ReadFile(goldenSequence)
	if err != nil {
		t.Fatalf("Reading the golden sequence failed: %v", err)
	}
	if string(written) != string(want) {
		t.Errorf("WriteSequence wrote\n%s\nwant\n%s", written, want)
	}
}

func readGolden(t *testing.T) Sequence {
	t.Helper()
	sequence, err := ReadSequence(goldenSequence)
	if err != nil {
		t.Fatalf("Reading the golden sequence failed: %v", err)
	}
	return sequence
}

func opTypesOf(s Sequence) []string {
	var types []string
	for _, op := range s.Ops {
		types = append(types, string(op.Type))
	}
	return types
}
