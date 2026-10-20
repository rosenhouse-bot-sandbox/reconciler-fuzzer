package plan

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestDurationJSON(t *testing.T) {
	out, err := json.Marshal(Duration(1500 * time.Millisecond))
	if err != nil || string(out) != `"1.5s"` {
		t.Fatalf("Marshal = %s, %v", out, err)
	}
	var d Duration
	if err := json.Unmarshal([]byte(`"250ms"`), &d); err != nil || d != Duration(250*time.Millisecond) {
		t.Fatalf("Unmarshal = %v, %v", d, err)
	}
	for _, bad := range []string{`1000`, `"soon"`, `true`, `"1"`} {
		if err := json.Unmarshal([]byte(bad), &d); err == nil {
			t.Errorf("Unmarshal(%s) succeeded", bad)
		}
	}
}

func write(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "plan.json")
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestLoadRejectsUnknownFields(t *testing.T) {
	_, err := Load(write(t, `{"steps": [{"actions": [{"do": "restart", "when": "now"}]}]}`))
	if err == nil || !strings.Contains(err.Error(), "when") {
		t.Fatalf("Load = %v, want an unknown field error", err)
	}
}

func TestLoadRejectsTrailingData(t *testing.T) {
	_, err := Load(write(t, `{"steps": []} {"steps": []}`))
	if err == nil || !strings.Contains(err.Error(), "after the plan") {
		t.Fatalf("Load = %v, want a trailing data error", err)
	}
}

func TestLoadValidates(t *testing.T) {
	_, err := Load(write(t, `{"steps": [{"actions": [{"do": "update", "slot": "a", "patch": {}}]}]}`))
	if err == nil || !strings.Contains(err.Error(), "steps[0].actions[0]") {
		t.Fatalf("Load = %v, want a validation error", err)
	}
}

func TestSaveRoundTrips(t *testing.T) {
	delay := Duration(10 * time.Millisecond)
	p := &Plan{Seed: 7, Steps: []Step{
		{Actions: []Action{{Do: Create, Slot: "a", Object: json.RawMessage(`{"kind":"Widget"}`)}}},
		{
			Period:  Duration(time.Second),
			Faults:  []Fault{{Match: Match{Verb: "patch", Resource: "widgets"}, Delay: &delay}, {Nth: 1, Error: &Error{Code: 500, RetryAfter: Duration(time.Second)}}},
			Actions: []Action{{At: Duration(20 * time.Millisecond), Do: Update, Slot: "a", Patch: json.RawMessage(`{"spec":{"count":2}}`)}},
		},
	}}
	path := filepath.Join(t.TempDir(), "plan.json")
	if err := Save(path, p); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasSuffix(string(data), "}\n") || !strings.Contains(string(data), "\n  \"steps\"") {
		t.Errorf("Save wrote unindented JSON or no trailing newline:\n%s", data)
	}
	loaded, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, raw := range []*json.RawMessage{&loaded.Steps[0].Actions[0].Object, &loaded.Steps[1].Actions[0].Patch} {
		var compact bytes.Buffer
		if err := json.Compact(&compact, *raw); err != nil {
			t.Fatal(err)
		}
		*raw = compact.Bytes()
	}
	if !reflect.DeepEqual(loaded, p) {
		t.Errorf("Load(Save(p)) = %+v, want %+v", loaded, p)
	}
}

func TestValidate(t *testing.T) {
	create := Action{Do: Create, Slot: "a"}
	delay := Duration(time.Millisecond)
	cases := map[string]struct {
		steps []Step
		want  string
	}{
		"unknown do":           {[]Step{{Actions: []Action{{Do: "explode"}}}}, "steps[0].actions[0]: unknown do"},
		"create without slot":  {[]Step{{Actions: []Action{{Do: Create}}}}, "steps[0].actions[0]: create needs a slot"},
		"update before create": {[]Step{{Actions: []Action{{Do: Update, Slot: "a", Patch: json.RawMessage(`{}`)}}}}, "steps[0].actions[0]: update names slot \"a\""},
		"update without patch": {[]Step{{Actions: []Action{create}}, {Actions: []Action{{Do: Update, Slot: "a"}}}}, "steps[1].actions[0]: update needs a JSON object patch"},
		"update array patch":   {[]Step{{Actions: []Action{create, {Do: Update, Slot: "a", Patch: json.RawMessage(`[1]`)}}}}, "steps[0].actions[1]: update needs a JSON object patch"},
		"update null patch":    {[]Step{{Actions: []Action{create, {Do: Update, Slot: "a", Patch: json.RawMessage(`null`)}}}}, "steps[0].actions[1]: update needs a JSON object patch"},
		"update other slot":    {[]Step{{Actions: []Action{create, {Do: Update, Slot: "b", Patch: json.RawMessage(`{}`)}}}}, "steps[0].actions[1]: update names slot \"b\""},
		"delete before create": {[]Step{{Actions: []Action{{Do: Delete, Slot: "a"}}}}, "steps[0].actions[0]: delete names slot \"a\""},
		"delete without slot":  {[]Step{{Actions: []Action{create, {Do: Delete}}}}, "steps[0].actions[1]: delete names slot \"\""},
		"update after delete":  {[]Step{{Actions: []Action{create}}, {Actions: []Action{{Do: Delete, Slot: "a"}, {Do: Update, Slot: "a", Patch: json.RawMessage(`{}`)}}}}, "steps[1].actions[1]: update names slot \"a\", which an earlier delete emptied"},
		"delete twice":         {[]Step{{Actions: []Action{create, {Do: Delete, Slot: "a"}, {Do: Delete, Slot: "a"}}}}, "steps[0].actions[2]: delete names slot \"a\", which an earlier delete emptied"},
		"restart with slot":    {[]Step{{Actions: []Action{{Do: Restart, Slot: "a"}}}}, "steps[0].actions[0]: restart takes no slot, object or patch"},
		"kill with object":     {[]Step{{Actions: []Action{{Do: Kill, Object: json.RawMessage(`{}`)}}}}, "steps[0].actions[0]: kill takes no slot, object or patch"},
		"restart with patch":   {[]Step{{Actions: []Action{{Do: Restart, Patch: json.RawMessage(`{}`)}}}}, "steps[0].actions[0]: restart takes no slot, object or patch"},
		"negative at":          {[]Step{{Actions: []Action{{Do: Kill, At: -1}}}}, "steps[0].actions[0]: at is negative"},
		"negative period":      {[]Step{{}, {Period: -1}}, "steps[1]: period is negative"},
		"fault with no effect": {[]Step{{Faults: []Fault{{}}}}, "steps[0].faults[0]: a fault sets 0 of error, delay and drop"},
		"fault with two":       {[]Step{{Faults: []Fault{{Drop: true}, {Drop: true, Delay: &delay}}}}, "steps[0].faults[1]: a fault sets 2 of error, delay and drop"},
		"negative nth":         {[]Step{{Faults: []Fault{{Drop: true, Nth: -1}}}}, "steps[0].faults[0]: nth is negative"},
		"error code 0":         {[]Step{{Faults: []Fault{{Error: &Error{}}}}}, "steps[0].faults[0]: error code 0 is not from 400 to 599"},
		"error code 302":       {[]Step{{Faults: []Fault{{Error: &Error{Code: 302}}}}}, "error code 302"},
		"error code 600":       {[]Step{{Faults: []Fault{{Error: &Error{Code: 600}}}}}, "error code 600"},
		"unknown system":       {[]Step{{Faults: []Fault{{System: "nosuch", Drop: true}}}}, "steps[0].faults[0]: unknown system \"nosuch\""},
		"create a live slot":   {[]Step{{Actions: []Action{create}}, {Actions: []Action{create}}}, "steps[1].actions[0]: create names slot \"a\", which still holds a CR"},
		"two samples":          {[]Step{{Actions: []Action{create, {Do: Create, Slot: "b"}}}}, "steps[0].actions[1]: create of slot \"b\" gives its CR the name of slot \"a\"'s CR"},
		"two names":            {[]Step{{Actions: []Action{named("a", "w"), named("b", "w")}}}, "create of slot \"b\" gives its CR the name"},
		"malformed object":     {[]Step{{Actions: []Action{{Do: Create, Slot: "a", Object: json.RawMessage(`[`)}}}}, "decoding object"},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			err := (&Plan{Steps: c.steps}).Validate()
			if err == nil || !strings.Contains(err.Error(), c.want) {
				t.Fatalf("Validate = %v, want %q", err, c.want)
			}
		})
	}
}

func named(slot, name string) Action {
	return Action{Do: Create, Slot: slot, Object: json.RawMessage(`{"metadata":{"name":"` + name + `"}}`)}
}

func TestValidateAccepts(t *testing.T) {
	delay := Duration(time.Millisecond)
	generated := json.RawMessage(`{"metadata":{"generateName":"w-"}}`)
	p := &Plan{Steps: []Step{
		{Actions: []Action{named("b", "w"), {Do: Delete, Slot: "b"}, named("c", "w"), named("d", "x"),
			{Do: Create, Slot: "e", Object: generated}, {Do: Create, Slot: "f", Object: generated}}},
		{Actions: []Action{{Do: Create, Slot: "a"}, {Do: Update, Slot: "a", Patch: json.RawMessage(`{"spec":{}}`)}}},
		{Period: Duration(time.Second), Faults: []Fault{{Drop: true}, {Delay: &delay}, {Error: &Error{Code: 409}, Nth: 2}, {System: "kube", Error: &Error{Code: 599}}},
			Actions: []Action{{Do: Delete, Slot: "a"}, {Do: Create, Slot: "a", At: Duration(time.Millisecond)}, {Do: Restart}, {Do: Kill}}},
	}}
	if err := p.Validate(); err != nil {
		t.Fatal(err)
	}
}

func TestToyPlansLoad(t *testing.T) {
	paths, err := filepath.Glob("../targets/toy-widget/plans/*.json")
	if err != nil || len(paths) == 0 {
		t.Fatalf("no plans: %v", err)
	}
	for _, path := range paths {
		if _, err := Load(path); err != nil {
			t.Errorf("%s: %v", path, err)
		}
	}
}

func TestFaultString(t *testing.T) {
	delay := Duration(1500 * time.Millisecond)
	for f, want := range map[*Fault]string{
		{Error: &Error{Code: 500}}: "error(500)",
		{Delay: &delay, Match: Match{Verb: "patch", Resource: "widgets", Subresource: "status"}}: "delay(1.5s) on verb=patch resource=widgets subresource=status",
		{Drop: true, System: "kube", Match: Match{Group: "g", Name: "n"}, Nth: 2}:                "drop on system=kube group=g name=n nth=2",
	} {
		if got := f.String(); got != want {
			t.Errorf("String() = %q, want %q", got, want)
		}
	}
}
