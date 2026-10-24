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

	"github.com/rosenhouse-bot-sandbox/reconciler-fuzzer/internal/trace"
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
		{Actions: []Action{{Do: Delete, Slot: "a", Propagation: Foreground}}},
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
		"unknown do":              {[]Step{{Actions: []Action{{Do: "explode"}}}}, "steps[0].actions[0]: unknown do"},
		"create without slot":     {[]Step{{Actions: []Action{{Do: Create}}}}, "steps[0].actions[0]: create needs a slot"},
		"update before create":    {[]Step{{Actions: []Action{{Do: Update, Slot: "a", Patch: json.RawMessage(`{}`)}}}}, "steps[0].actions[0]: update names slot \"a\""},
		"update without patch":    {[]Step{{Actions: []Action{create}}, {Actions: []Action{{Do: Update, Slot: "a"}}}}, "steps[1].actions[0]: update needs a JSON object patch"},
		"update array patch":      {[]Step{{Actions: []Action{create, {Do: Update, Slot: "a", Patch: json.RawMessage(`[1]`)}}}}, "steps[0].actions[1]: update needs a JSON object patch"},
		"update null patch":       {[]Step{{Actions: []Action{create, {Do: Update, Slot: "a", Patch: json.RawMessage(`null`)}}}}, "steps[0].actions[1]: update needs a JSON object patch"},
		"update other slot":       {[]Step{{Actions: []Action{create, {Do: Update, Slot: "b", Patch: json.RawMessage(`{}`)}}}}, "steps[0].actions[1]: update names slot \"b\""},
		"delete before create":    {[]Step{{Actions: []Action{{Do: Delete, Slot: "a"}}}}, "steps[0].actions[0]: delete names slot \"a\""},
		"delete without slot":     {[]Step{{Actions: []Action{create, {Do: Delete}}}}, "steps[0].actions[1]: delete names slot \"\""},
		"update after delete":     {[]Step{{Actions: []Action{create}}, {Actions: []Action{{Do: Delete, Slot: "a"}, {Do: Update, Slot: "a", Patch: json.RawMessage(`{}`)}}}}, "steps[1].actions[1]: update names slot \"a\", which an earlier delete emptied"},
		"delete twice":            {[]Step{{Actions: []Action{create, {Do: Delete, Slot: "a"}, {Do: Delete, Slot: "a"}}}}, "steps[0].actions[2]: delete names slot \"a\", which an earlier delete emptied"},
		"restart with slot":       {[]Step{{Actions: []Action{{Do: Restart, Slot: "a"}}}}, "steps[0].actions[0]: restart takes no slot, object or patch"},
		"kill with object":        {[]Step{{Actions: []Action{{Do: Kill, Object: json.RawMessage(`{}`)}}}}, "steps[0].actions[0]: kill takes no slot, object or patch"},
		"restart with patch":      {[]Step{{Actions: []Action{{Do: Restart, Patch: json.RawMessage(`{}`)}}}}, "steps[0].actions[0]: restart takes no slot, object or patch"},
		"stop with slot":          {[]Step{{Actions: []Action{create, {Do: Stop, Slot: "a"}}}}, "steps[0].actions[1]: stop takes no slot, object or patch"},
		"stop with target":        {[]Step{{Actions: []Action{{Do: Stop, Target: cms}}}}, "steps[0].actions[0]: stop takes no target"},
		"restart after a stop":    {[]Step{{Actions: []Action{{Do: Stop}}}, {Actions: []Action{{Do: Restart}}}}, "steps[1].actions[0]: restart after a stop"},
		"kill after a stop":       {[]Step{{Actions: []Action{{Do: Stop}, {Do: Kill}}}}, "steps[0].actions[1]: kill after a stop"},
		"stop after a stop":       {[]Step{{Actions: []Action{{Do: Stop}}}, {Actions: []Action{{Do: Stop}}}}, "steps[1].actions[0]: stop after a stop"},
		"create after a stop":     {[]Step{{Actions: []Action{{Do: Stop}, create}}}, "steps[0].actions[1]: create after a stop"},
		"update after a stop":     {[]Step{{Actions: []Action{create, {Do: Stop}}}, {Actions: []Action{{Do: Update, Slot: "a", Patch: json.RawMessage(`{}`)}}}}, "steps[1].actions[0]: update after a stop"},
		"damage after a stop":     {[]Step{{Actions: []Action{{Do: Stop}, {Do: Damage, Target: cms}}}}, "steps[0].actions[1]: damage after a stop"},
		"negative at":             {[]Step{{Actions: []Action{{Do: Kill, At: -1}}}}, "steps[0].actions[0]: at is negative"},
		"negative period":         {[]Step{{}, {Period: -1}}, "steps[1]: period is negative"},
		"fault with no effect":    {[]Step{{Faults: []Fault{{}}}}, "steps[0].faults[0]: a fault sets 0 of error, delay, drop, dropResponse, killAfter, stuck, slowDelete and holdWatch"},
		"holdWatch and drop":      {[]Step{{Faults: []Fault{{HoldWatch: true, Drop: true, Match: gadgets}}}}, "a fault sets 2 of"},
		"holdWatch no resource":   {[]Step{{Faults: []Fault{{HoldWatch: true, Match: Match{Group: "example.com"}}}}}, "holdWatch needs a match with a resource"},
		"holdWatch on a fake":     {[]Step{{Faults: []Fault{{HoldWatch: true, System: "cloud", Match: Match{Resource: "buckets"}}}}}, "holdWatch acts only on kube"},
		"holdWatch with verb":     {[]Step{{Faults: []Fault{{HoldWatch: true, Match: Match{Verb: "watch", Resource: "gadgets"}}}}}, "holdWatch takes no verb, subresource, name or nth"},
		"holdWatch subresource":   {[]Step{{Faults: []Fault{{HoldWatch: true, Match: Match{Resource: "gadgets", Subresource: "status"}}}}}, "holdWatch takes no verb, subresource, name or nth"},
		"holdWatch with name":     {[]Step{{Faults: []Fault{{HoldWatch: true, Match: Match{Resource: "gadgets", Name: "g"}}}}}, "holdWatch takes no verb, subresource, name or nth"},
		"holdWatch with nth":      {[]Step{{Faults: []Fault{{HoldWatch: true, Match: gadgets, Nth: 1}}}}, "holdWatch takes no verb, subresource, name or nth"},
		"stuck and drop":          {[]Step{{Faults: []Fault{{Stuck: true, Drop: true, Match: gadgets}}}}, "a fault sets 2 of"},
		"stuck no resource":       {[]Step{{Faults: []Fault{{Stuck: true}}}}, "steps[0].faults[0]: stuck needs a match with a resource"},
		"slowDelete with verb":    {[]Step{{Faults: []Fault{{SlowDelete: true, Match: Match{Verb: "get", Resource: "gadgets"}}}}}, "slowDelete matches objects, so it takes no verb or subresource"},
		"stuck subresource":       {[]Step{{Faults: []Fault{{Stuck: true, Match: Match{Resource: "gadgets", Subresource: "status"}}}}}, "stuck matches objects, so it takes no verb or subresource"},
		"stuck with nth":          {[]Step{{Faults: []Fault{{Stuck: true, Match: gadgets, Nth: 1}}}}, "stuck takes no nth"},
		"fault with two":          {[]Step{{Faults: []Fault{{Drop: true}, {Drop: true, Delay: &delay}}}}, "steps[0].faults[1]: a fault sets 2 of"},
		"drop and dropResponse":   {[]Step{{Faults: []Fault{{Drop: true, DropResponse: true}}}}, "a fault sets 2 of"},
		"error and killAfter":     {[]Step{{Faults: []Fault{{Error: &Error{Code: 500}, KillAfter: true}}}}, "a fault sets 2 of"},
		"damage without target":   {[]Step{{Actions: []Action{{Do: Damage}}}}, "steps[0].actions[0]: damage needs a target with a resource"},
		"damage no resource":      {[]Step{{Actions: []Action{{Do: Damage, Target: &Target{Name: "x"}}}}}, "damage needs a target with a resource"},
		"damage empty slot":       {[]Step{{Actions: []Action{{Do: Damage, Slot: "a", Target: cms}}}}, "damage names slot \"a\", which no earlier create made"},
		"damage deleted slot":     {[]Step{{Actions: []Action{create, {Do: Delete, Slot: "a"}, {Do: Damage, Slot: "a", Target: cms}}}}, "damage names slot \"a\", which an earlier delete emptied"},
		"damage array patch":      {[]Step{{Actions: []Action{{Do: Damage, Target: cms, Patch: json.RawMessage(`[]`)}}}}, "damage needs a JSON object patch"},
		"damage with object":      {[]Step{{Actions: []Action{{Do: Damage, Target: cms, Object: json.RawMessage(`{}`)}}}}, "damage takes no object"},
		"fixture without name":    {[]Step{{Actions: []Action{{Do: ChangeFixture, Target: cms}}}}, "changeFixture needs a target name"},
		"fixture no target":       {[]Step{{Actions: []Action{{Do: ChangeFixture}}}}, "changeFixture needs a target with a resource"},
		"fixture null patch":      {[]Step{{Actions: []Action{{Do: ChangeFixture, Target: fixture, Patch: json.RawMessage(`null`)}}}}, "changeFixture needs a JSON object patch"},
		"fixture with slot":       {[]Step{{Actions: []Action{create, {Do: ChangeFixture, Slot: "a", Target: fixture}}}}, "changeFixture takes no slot or object"},
		"fixture with object":     {[]Step{{Actions: []Action{{Do: ChangeFixture, Object: json.RawMessage(`{}`), Target: fixture}}}}, "changeFixture takes no slot or object"},
		"restart with target":     {[]Step{{Actions: []Action{{Do: Restart, Target: cms}}}}, "restart takes no target"},
		"kill with target":        {[]Step{{Actions: []Action{{Do: Kill, Target: cms}}}}, "kill takes no target"},
		"create with target":      {[]Step{{Actions: []Action{{Do: Create, Slot: "a", Target: cms}}}}, "create takes no target"},
		"update with target":      {[]Step{{Actions: []Action{create, {Do: Update, Slot: "a", Patch: json.RawMessage(`{}`), Target: cms}}}}, "update takes no target"},
		"delete with target":      {[]Step{{Actions: []Action{create, {Do: Delete, Slot: "a", Target: cms}}}}, "delete takes no target"},
		"negative nth":            {[]Step{{Faults: []Fault{{Drop: true, Nth: -1}}}}, "steps[0].faults[0]: nth is negative"},
		"error code 0":            {[]Step{{Faults: []Fault{{Error: &Error{}}}}}, "steps[0].faults[0]: error code 0 is not from 400 to 599"},
		"error code 302":          {[]Step{{Faults: []Fault{{Error: &Error{Code: 302}}}}}, "error code 302"},
		"error code 600":          {[]Step{{Faults: []Fault{{Error: &Error{Code: 600}}}}}, "error code 600"},
		"malformed system":        {[]Step{{Faults: []Fault{{System: "No such", Drop: true}}}}, "steps[0].faults[0]: system \"No such\" is not a system name"},
		"kube stuck no resource":  {[]Step{{Faults: []Fault{{System: "kube", Stuck: true}}}}, "stuck needs a match with a resource"},
		"fake stuck with verb":    {[]Step{{Faults: []Fault{{System: "cloud", Stuck: true, Match: Match{Verb: "get"}}}}}, "stuck matches objects"},
		"kube target no resource": {[]Step{{Actions: []Action{{Do: Damage, Target: &Target{System: "kube"}}}}}, "damage needs a target with a resource"},
		"malformed target system": {[]Step{{Actions: []Action{{Do: Damage, Target: &Target{System: "Cloud"}}}}}, "steps[0].actions[0]: system \"Cloud\" is not a system name"},
		"fake fixture no name":    {[]Step{{Actions: []Action{{Do: ChangeFixture, Target: &Target{System: "cloud", Resource: "buckets"}}}}}, "changeFixture needs a target name"},
		"create a live slot":      {[]Step{{Actions: []Action{create}}, {Actions: []Action{create}}}, "steps[1].actions[0]: create names slot \"a\", which still holds a CR"},
		"two samples":             {[]Step{{Actions: []Action{create, {Do: Create, Slot: "b"}}}}, "steps[0].actions[1]: create of slot \"b\" gives its CR the name of slot \"a\"'s CR"},
		"two names":               {[]Step{{Actions: []Action{named("a", "w"), named("b", "w")}}}, "create of slot \"b\" gives its CR the name"},
		"malformed object":        {[]Step{{Actions: []Action{{Do: Create, Slot: "a", Object: json.RawMessage(`[`)}}}}, "decoding object"},
		"orphan propagation":      {[]Step{{Actions: []Action{create, {Do: Delete, Slot: "a", Propagation: "Orphan"}}}}, "steps[0].actions[1]: delete propagation \"Orphan\" is not Foreground"},
		"create propagation":      {[]Step{{Actions: []Action{{Do: Create, Slot: "a", Propagation: Foreground}}}}, "steps[0].actions[0]: create takes no propagation"},
		"damage propagation":      {[]Step{{Actions: []Action{{Do: Damage, Target: cms, Propagation: Foreground}}}}, "steps[0].actions[0]: damage takes no propagation"},
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

var (
	cms     = &Target{Resource: "configmaps"}
	fixture = &Target{Resource: "configmaps", Name: "widget-config"}
)

var gadgets = Match{Group: "example.com", Resource: "gadgets"}

func named(slot, name string) Action {
	return Action{Do: Create, Slot: slot, Object: json.RawMessage(`{"metadata":{"name":"` + name + `"}}`)}
}

func TestValidateAccepts(t *testing.T) {
	delay := Duration(time.Millisecond)
	generated := json.RawMessage(`{"metadata":{"generateName":"w-"}}`)
	p := &Plan{Steps: []Step{
		{Actions: []Action{named("b", "w"), {Do: Delete, Slot: "b"}, named("c", "w"), named("d", "x"),
			{Do: Create, Slot: "e", Object: generated}, {Do: Create, Slot: "f", Object: generated},
			{Do: Create, Slot: "g", Object: json.RawMessage(`{"metadata":{"name":"x","namespace":"other"}}`)}}},
		{Actions: []Action{{Do: Create, Slot: "a"}, {Do: Update, Slot: "a", Patch: json.RawMessage(`{"spec":{}}`)}}},
		{Period: Duration(time.Second), Faults: []Fault{{Drop: true}, {Delay: &delay}, {Error: &Error{Code: 409}, Nth: 2}, {System: "kube", Error: &Error{Code: 599}}},
			Actions: []Action{{Do: Delete, Slot: "a", Propagation: Foreground}, {Do: Create, Slot: "a", At: Duration(time.Millisecond)}, {Do: Restart}, {Do: Kill}}},
		{Faults: []Fault{{Stuck: true, Match: gadgets}, {SlowDelete: true, Match: Match{Group: "example.com", Resource: "gadgets", Name: "g"}}}},
		{Faults: []Fault{{DropResponse: true}, {KillAfter: true, Nth: 1}}, Actions: []Action{
			{Do: Damage, Target: cms}, {Do: Damage, Slot: "a", Target: cms, Patch: json.RawMessage(`{"data":{}}`)},
			{Do: ChangeFixture, Target: fixture}, {Do: ChangeFixture, Target: fixture, Patch: json.RawMessage(`{"data":{"k":"v"}}`)},
		}},
		{Faults: []Fault{{System: "cloud", Stuck: true}, {System: "cloud-2", SlowDelete: true, Match: Match{Resource: "buckets"}}, {System: "cloud", Error: &Error{Code: 503}}},
			Actions: []Action{{Do: Damage, Target: &Target{System: "cloud"}}, {Do: Damage, Target: &Target{System: "cloud", Resource: "buckets", Role: "r"}}}},
		{Faults: []Fault{{HoldWatch: true, Match: gadgets}, {HoldWatch: true, System: "kube", Match: Match{Resource: "secrets"}}},
			Actions: []Action{{Do: ChangeFixture, Target: &Target{System: "vault", Name: "secret/app"}, Patch: json.RawMessage(`{"data":{"k":"v"}}`)},
				{Do: ChangeFixture, Target: &Target{System: "cloud", Resource: "buckets", Name: "b"}}}},
		{Actions: []Action{{Do: Stop}, {Do: Delete, Slot: "c"}}},
		{Faults: []Fault{{SlowDelete: true, Match: gadgets}}, Actions: []Action{{Do: Delete, Slot: "d"}, {Do: ChangeFixture, Target: fixture}}},
	}}
	if err := p.Validate(); err != nil {
		t.Fatal(err)
	}
}

// An uninstall stops the controller for good after its CRs are gone.
func TestLoadAcceptsStop(t *testing.T) {
	p, err := Load(write(t, `{"steps": [{"actions": [{"do": "create", "slot": "a"}]}, {"actions": [{"do": "delete", "slot": "a"}]}, {"actions": [{"do": "stop"}]}]}`))
	if err != nil || p.Steps[2].Actions[0].Do != "stop" {
		t.Fatalf("Load = %+v, %v; want a stop action", p, err)
	}
}

func TestToyPlansLoad(t *testing.T) {
	paths, err := filepath.Glob("../../targets/toy-widget/plans/*.json")
	if err != nil || len(paths) == 0 {
		t.Fatalf("no plans: %v", err)
	}
	for _, path := range paths {
		if _, err := Load(path); err != nil {
			t.Errorf("%s: %v", path, err)
		}
	}
}

func TestUnknownOutcomeEffectsMatchTrace(t *testing.T) {
	if got := (Fault{DropResponse: true}).Effect(); got != trace.DropResponse {
		t.Errorf("dropResponse Effect() = %q, want %q", got, trace.DropResponse)
	}
	if got := (Fault{KillAfter: true}).Effect(); got != trace.KillAfter {
		t.Errorf("killAfter Effect() = %q, want %q", got, trace.KillAfter)
	}
}

func TestModelFaults(t *testing.T) {
	for f, want := range map[Fault]bool{{Stuck: true}: true, {SlowDelete: true}: true, {Drop: true}: false, {DropResponse: true}: false, {HoldWatch: true}: false} {
		if got := f.ModelFault(); got != want {
			t.Errorf("%s: ModelFault() = %v, want %v", f, got, want)
		}
	}
}

func TestFaultString(t *testing.T) {
	delay := Duration(1500 * time.Millisecond)
	for f, want := range map[*Fault]string{
		{Error: &Error{Code: 500}}: "error(500)",
		{Delay: &delay, Match: Match{Verb: "patch", Resource: "widgets", Subresource: "status"}}: "delay(1.5s) on verb=patch resource=widgets subresource=status",
		{Drop: true, System: "kube", Match: Match{Group: "g", Name: "n"}, Nth: 2}:                "drop on system=kube group=g name=n nth=2",
		{Stuck: true, Match: gadgets}:      "stuck on group=example.com resource=gadgets",
		{SlowDelete: true, Match: gadgets}: "slowDelete on group=example.com resource=gadgets",
		{HoldWatch: true, Match: gadgets}:  "holdWatch on group=example.com resource=gadgets",
	} {
		if got := f.String(); got != want {
			t.Errorf("String() = %q, want %q", got, want)
		}
	}
}
