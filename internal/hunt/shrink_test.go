package hunt

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/rosenhouse-bot-sandbox/reconciler-fuzzer/internal/config"
	"github.com/rosenhouse-bot-sandbox/reconciler-fuzzer/internal/generate"
	"github.com/rosenhouse-bot-sandbox/reconciler-fuzzer/internal/plan"
)

const toyConfig = "../../targets/toy-widget/config.yaml"

var samples = []map[string]any{{"apiVersion": "toy.reconciler-fuzzer/v1", "kind": "Widget", "metadata": map[string]any{"name": "widget"}, "spec": map[string]any{"count": 3, "mode": "fast"}}}

func parsePlan(t *testing.T, text string) *plan.Plan {
	t.Helper()
	var p plan.Plan
	if err := json.Unmarshal([]byte(text), &p); err != nil {
		t.Fatal(err)
	}
	if err := p.Validate(); err != nil {
		t.Fatal(err)
	}
	return &p
}

func encoded(t *testing.T, p *plan.Plan) string {
	t.Helper()
	data, err := json.Marshal(p)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

func encodedAll(t *testing.T, plans []*plan.Plan) []string {
	var all []string
	for _, p := range plans {
		all = append(all, encoded(t, p))
	}
	return all
}

const (
	widget = `"apiVersion":"toy.reconciler-fuzzer/v1","kind":"Widget"`
	create = `{"do":"create","slot":"a","object":{` + widget + `,"metadata":{"name":"widget"},"spec":{"count":5,"mode":"fast"}}}`
)

func TestCandidatesDropEachStepThatLeavesAValidPlan(t *testing.T) {
	p := parsePlan(t, `{"steps":[{"actions":[`+create+`]},{"actions":[{"do":"restart"}]},{"actions":[{"do":"delete","slot":"a"}]}]}`)
	got := encodedAll(t, Candidates(p, samples))
	for _, want := range []string{
		`{"steps":[{"actions":[` + create + `]},{"actions":[{"do":"delete","slot":"a"}]}]}`,
		`{"steps":[{"actions":[` + create + `]},{"actions":[{"do":"restart"}]}]}`,
	} {
		if !slices.Contains(got, want) {
			t.Errorf("Candidates lack %s.", want)
		}
	}
	for _, c := range got {
		if !strings.Contains(c, `"create"`) {
			t.Errorf("A candidate drops the create a later delete needs: %s.", c)
		}
	}
	if got[0] != `{"steps":[{"actions":[`+create+`]},{"actions":[{"do":"delete","slot":"a"}]}]}` {
		t.Errorf("The first candidate is %s, want the first droppable step dropped.", got[0])
	}
}

func TestCandidatesDropEachActionAndFaultOfAStepThatKeepsOne(t *testing.T) {
	fault := `{"match":{"verb":"get"},"drop":true}`
	p := parsePlan(t, `{"steps":[{"actions":[`+create+`]},{"period":"1s","actions":[{"do":"restart"},{"do":"kill"}],"faults":[`+fault+`]}]}`)
	got := encodedAll(t, Candidates(p, samples))
	head := `{"steps":[{"actions":[` + create + `]},{"period":"1s",`
	for _, want := range []string{
		head + `"actions":[{"do":"kill"}],"faults":[` + fault + `]}]}`,
		head + `"actions":[{"do":"restart"}],"faults":[` + fault + `]}]}`,
		head + `"actions":[{"do":"restart"},{"do":"kill"}]}]}`,
		`{"steps":[{"actions":[` + create + `]},{"period":"500ms","actions":[{"do":"restart"},{"do":"kill"}],"faults":[` + fault + `]}]}`,
	} {
		if !slices.Contains(got, want) {
			t.Errorf("Candidates lack %s.", want)
		}
	}
}

func TestCandidatesHalvePeriodsAndOffsetsDownToHalfASecond(t *testing.T) {
	for _, testCase := range []struct{ period, at, wantPeriod, wantAt string }{
		{"3s", "41ms", "1.5s", "20ms"},
		{"700ms", "1ms", "500ms", ""},
		{"500ms", "", "", ""},
	} {
		at := ""
		if testCase.at != "" {
			at = `"at":"` + testCase.at + `",`
		}
		p := parsePlan(t, `{"steps":[{"period":"`+testCase.period+`","actions":[{`+at+`"do":"restart"}],"faults":[{"match":{},"drop":true}]}]}`)
		var halved []string
		for _, c := range Candidates(p, samples) {
			if len(c.Steps) == 1 && len(c.Steps[0].Actions) == 1 && len(c.Steps[0].Faults) == 1 {
				halved = append(halved, time.Duration(c.Steps[0].Period).String()+" "+time.Duration(c.Steps[0].Actions[0].At).String())
			}
		}
		var want []string
		if testCase.wantPeriod != "" {
			wantAt := "0s"
			if testCase.wantAt != "" {
				wantAt = testCase.wantAt
			}
			want = []string{testCase.wantPeriod + " " + wantAt}
		}
		if !slices.Equal(halved, want) {
			t.Errorf("Period %s halves to %v, want %v.", testCase.period, halved, want)
		}
	}
}

func TestCandidatesResetEachSpecFieldOfACreateToTheSample(t *testing.T) {
	p := parsePlan(t, `{"steps":[{"actions":[{"do":"create","slot":"a","object":{`+widget+`,"metadata":{"name":"w"},"spec":{"count":5,"extra":{"x":1}}}}]}]}`)
	want := []string{
		`{` + widget + `,"metadata":{"name":"w"},"spec":{"count":3,"extra":{"x":1}}}`,
		`{` + widget + `,"metadata":{"name":"w"},"spec":{"count":5}}`,
		`{` + widget + `,"metadata":{"name":"w"},"spec":{"count":5,"extra":{"x":1},"mode":"fast"}}`,
	}
	if got := creates(t, p, samples); !slices.Equal(got, want) {
		t.Errorf("Resets are %v, want %v.", got, want)
	}
}

var fleetSamples = []map[string]any{
	{"apiVersion": "toy.reconciler-fuzzer/v1", "kind": "Cluster", "metadata": map[string]any{"name": "app"}, "spec": map[string]any{"volumes": 2}},
	{"apiVersion": "toy.reconciler-fuzzer/v1", "kind": "Database", "metadata": map[string]any{"name": "app"}, "spec": map[string]any{"engine": "postgres"}},
}

// creates are the objects of the candidates that keep p's one create.
func creates(t *testing.T, p *plan.Plan, samples []map[string]any) []string {
	t.Helper()
	var objects []string
	for _, c := range Candidates(p, samples) {
		if len(c.Steps) == 1 {
			objects = append(objects, string(c.Steps[0].Actions[0].Object))
		}
	}
	return objects
}

func TestCandidatesResetACreateTowardTheSampleOfItsKind(t *testing.T) {
	const database = `{"apiVersion":"toy.reconciler-fuzzer/v1","kind":"Database","metadata":{"name":"app"},"spec":{"engine":%q}}`
	p := parsePlan(t, `{"steps":[{"actions":[{"do":"create","slot":"a","object":`+fmt.Sprintf(database, "mysql")+`}]}]}`)
	if got, want := creates(t, p, fleetSamples), []string{fmt.Sprintf(database, "postgres")}; !slices.Equal(got, want) {
		t.Errorf("Resets are %v, want %v.", got, want)
	}
}

func TestCandidatesKeepTheFieldsOfACreateOfAKindWithoutASample(t *testing.T) {
	for _, header := range []string{
		`"apiVersion":"toy.reconciler-fuzzer/v1","kind":"User"`,
		`"apiVersion":"other.reconciler-fuzzer/v1","kind":"Database"`,
	} {
		p := parsePlan(t, `{"steps":[{"actions":[{"do":"create","slot":"a","object":{`+header+`,"metadata":{"name":"app"},"spec":{"engine":"mysql"}}}]}]}`)
		if got := creates(t, p, fleetSamples); len(got) != 0 {
			t.Errorf("Resets of {%s} are %v, want none.", header, got)
		}
	}
}

func TestCandidatesDropEachFieldOfAnUpdatePatchThatHasTwo(t *testing.T) {
	p := parsePlan(t, `{"steps":[{"actions":[`+create+`,{"do":"update","slot":"a","patch":{"spec":{"count":1,"mode":"safe"}}},{"do":"update","slot":"a","patch":{"spec":{"count":2}}}]}]}`)
	var patches []string
	for _, c := range Candidates(p, samples) {
		if len(c.Steps) == 1 && len(c.Steps[0].Actions) == 3 && string(c.Steps[0].Actions[0].Object) == string(p.Steps[0].Actions[0].Object) {
			actions := c.Steps[0].Actions
			patches = append(patches, string(actions[1].Patch)+" "+string(actions[2].Patch))
		}
	}
	want := []string{`{"spec":{"mode":"safe"}} {"spec":{"count":2}}`, `{"spec":{"count":1}} {"spec":{"count":2}}`}
	if !slices.Equal(patches, want) {
		t.Errorf("Patch drops are %v, want %v.", patches, want)
	}
}

func TestCandidatesMergeSlotsWhoseCRsDoNotOverlap(t *testing.T) {
	createB := strings.ReplaceAll(create, `"slot":"a"`, `"slot":"b"`)
	apart := parsePlan(t, `{"steps":[{"actions":[`+create+`]},{"actions":[{"do":"delete","slot":"a"}]},{"actions":[`+createB+`]},{"actions":[{"do":"damage","slot":"b","target":{"resource":"configmaps"}}]}]}`)
	got := Candidates(apart, samples)
	last := encoded(t, got[len(got)-1])
	want := `{"steps":[{"actions":[` + create + `]},{"actions":[{"do":"delete","slot":"a"}]},{"actions":[` + create + `]},{"actions":[{"do":"damage","slot":"a","target":{"resource":"configmaps"}}]}]}`
	if last != want {
		t.Errorf("The last candidate is %s, want b merged into a: %s.", last, want)
	}
	overlapping := parsePlan(t, `{"steps":[{"actions":[`+create+`]},{"actions":[`+strings.ReplaceAll(createB, `"widget"`, `"widget-b"`)+`]}]}`)
	for _, c := range Candidates(overlapping, samples) {
		if !strings.Contains(encoded(t, c), `"slot":"b"`) && len(c.Steps) == 2 {
			t.Errorf("Candidates merge overlapping slots: %s.", encoded(t, c))
		}
	}
}

func TestEveryCandidateOfGeneratedPlansIsValid(t *testing.T) {
	cfg, err := config.Load(toyConfig)
	if err != nil {
		t.Fatal(err)
	}
	g, err := generate.New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	for seed := uint64(1); seed <= 30; seed++ {
		p, err := g.Plan(seed)
		if err != nil {
			t.Fatal(err)
		}
		for _, c := range Candidates(p, samples) {
			if err := c.Validate(); err != nil {
				t.Fatalf("seed %d: %v", seed, err)
			}
		}
		if err := p.Validate(); err != nil {
			t.Fatalf("Candidates changed the plan of seed %d: %v", seed, err)
		}
	}
}

// deletesB fails while the plan deletes slot b.
func deletesB(_ context.Context, p *plan.Plan) (bool, error) {
	for _, step := range p.Steps {
		for _, a := range step.Actions {
			if a.Do == plan.Delete && a.Slot == "b" {
				return true, nil
			}
		}
	}
	return false, nil
}

func TestShrinkReachesAPlanNoCandidateOfWhichFails(t *testing.T) {
	cfg, err := config.Load(toyConfig)
	if err != nil {
		t.Fatal(err)
	}
	g, err := generate.New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	shrunk := 0
	for seed := uint64(1); seed <= 60 && shrunk < 3; seed++ {
		p, err := g.Plan(seed)
		if err != nil {
			t.Fatal(err)
		}
		if fails, _ := deletesB(context.Background(), p); !fails {
			continue
		}
		shrunk++
		got, err := Shrink(context.Background(), p, samples, deletesB)
		if err != nil {
			t.Fatal(err)
		}
		var dos []string
		for _, step := range got.Steps {
			if len(step.Faults) > 0 || step.Period != 0 {
				t.Errorf("seed %d shrinks to a step with faults: %s", seed, encoded(t, got))
			}
			for _, a := range step.Actions {
				dos = append(dos, string(a.Do)+" "+a.Slot)
			}
		}
		if !slices.Equal(dos, []string{"create b", "delete b"}) || got.Seed != seed {
			t.Errorf("seed %d shrinks to %s, want a create and a delete of b.", seed, encoded(t, got))
		}
		for _, c := range Candidates(got, samples) {
			if fails, _ := deletesB(context.Background(), c); fails {
				t.Errorf("seed %d: the shrunk plan has the failing candidate %s.", seed, encoded(t, c))
			}
		}
	}
	if shrunk == 0 {
		t.Fatal("No seed deletes slot b.")
	}
}

// hasCreate fails while the plan creates something.
func hasCreate(_ context.Context, p *plan.Plan) (bool, error) {
	return slices.ContainsFunc(p.Steps, func(s plan.Step) bool {
		return slices.ContainsFunc(s.Actions, func(a plan.Action) bool { return a.Do == plan.Create })
	}), nil
}

func TestShrinkResetsACreateTowardTheSampleOfItsKind(t *testing.T) {
	const database = `{"steps":[{"actions":[{"do":"create","slot":"a","object":{"apiVersion":"toy.reconciler-fuzzer/v1","kind":"Database","metadata":{"name":"app"},"spec":{"engine":%q}}}]}]}`
	got, err := Shrink(context.Background(), parsePlan(t, fmt.Sprintf(database, "mysql")), fleetSamples, hasCreate)
	if want := fmt.Sprintf(database, "postgres"); err != nil || encoded(t, got) != want {
		t.Errorf("Shrink = %s, %v; want %s.", encoded(t, got), err, want)
	}
}

func TestShrinkKeepsAPlanWhoseCandidatesPass(t *testing.T) {
	p := parsePlan(t, `{"steps":[{"actions":[`+create+`]},{"actions":[{"do":"restart"}]}]}`)
	got, err := Shrink(context.Background(), p, samples, func(context.Context, *plan.Plan) (bool, error) { return false, nil })
	if err != nil || encoded(t, got) != encoded(t, p) {
		t.Errorf("Shrink = %s, %v; want the plan unchanged.", encoded(t, got), err)
	}
}

func TestShrinkReturnsTheErrorOfFails(t *testing.T) {
	p := parsePlan(t, `{"steps":[{"actions":[`+create+`]},{"actions":[{"do":"restart"}]}]}`)
	boom := errors.New("boom")
	if _, err := Shrink(context.Background(), p, samples, func(context.Context, *plan.Plan) (bool, error) { return false, boom }); !errors.Is(err, boom) {
		t.Errorf("Shrink returned %v, want boom.", err)
	}
}

func TestCandidatesClearTheShortPeriodOfAStepWithoutFaults(t *testing.T) {
	p := parsePlan(t, `{"steps":[{"period":"500ms","actions":[{"do":"kill"}]}]}`)
	if got := encodedAll(t, Candidates(p, samples)); !slices.Contains(got, `{"steps":[{"actions":[{"do":"kill"}]}]}`) {
		t.Errorf("Candidates are %v, want the period cleared.", got)
	}
}
