package generate

import (
	"bytes"
	"maps"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"slices"
	"testing"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"pgregory.net/rapid"

	"github.com/rosenhouse-bot-sandbox/reconciler-fuzzer/internal/run"
	"github.com/rosenhouse-bot-sandbox/reconciler-fuzzer/internal/target"
)

func newGenerator(t *testing.T, loaded *target.Target, opts Options) *Generator {
	t.Helper()
	g, err := New(loaded, opts)
	if err != nil {
		t.Fatalf("New(%s) failed: %v.", loaded.Name, err)
	}
	return g
}

// mutablePaths are the paths each target lets the generator move, and the
// kinds it declares it manages (DESIGN.md §8.1).
var targets = []struct {
	path         string
	mutablePaths []string
	managed      []string
}{
	{toyTarget, []string{"spec.count"}, []string{"v1/ConfigMap"}},
	{certManagerTarget,
		[]string{"spec.dnsNames", "spec.duration", "spec.privateKey.algorithm", "spec.privateKey.rotationPolicy"},
		[]string{"v1/Secret", "cert-manager.io/v1/CertificateRequest"}},
	{externalSecretsTarget, []string{"spec.refreshInterval", "spec.target.name"}, []string{"v1/Secret"}},
	{fixturesTarget, []string{"spec.count"}, []string{"v1/ConfigMap"}},
}

const fixturesTarget = "testdata/fixtures/target.yaml"

// fixtureWord is what a Secret's data reads as base64 and a label takes.
var fixtureWord = regexp.MustCompile(`^([a-z0-9]{4}){1,3}$`)

func TestFixtureOpsActOnlyWhereTheTargetAllows(t *testing.T) {
	g := newGenerator(t, loadTarget(t, fixturesTarget), Options{})
	rapid.Check(t, func(rt *rapid.T) {
		for _, op := range g.sequence(rt).Ops {
			paths, declared := fixturePaths[op.Kind+" "+op.Name]
			switch op.Type {
			case run.OpUpdateFixture:
				changed := differences(nil, op.Patch, "")
				if len(changed) != 1 || !slices.Contains(paths, changed[0]) {
					rt.Fatalf("Op %d sets %v of the %s %s, and the target lets generation set %v.", op.Index, changed, op.Kind, op.Name, paths)
				}
				if value := leaf(op.Patch); !fixtureWord.MatchString(value) {
					rt.Fatalf("Op %d sets %q, which a Secret's data does not read as base64.", op.Index, value)
				}
			case run.OpDeleteFixture:
				if !declared {
					rt.Fatalf("Op %d deletes the %s %s, which generate.fixtures does not name.", op.Index, op.Kind, op.Name)
				}
			}
		}
	})
}

// fixturePaths are the fixtures the fixtures target lets generation delete,
// and the paths in each it lets generation set.
var fixturePaths = map[string][]string{
	"v1/ConfigMap widget-config": {"data.label", "data.app.properties"},
	"v1/Secret token":            {"data.token"},
	"v1/ConfigMap ca-bundle":     nil,
}

// A sequence's first deleteFixture finds every fixture there, so it may
// delete any.
func TestFixtureOpsReachEveryFixtureAndPathTheTargetAllows(t *testing.T) {
	const seeds = 100
	g := newGenerator(t, loadTarget(t, fixturesTarget), Options{})
	var wantUpdated, updated, wantDeleted, deletedFirst []string
	for fixture, paths := range fixturePaths {
		wantDeleted = append(wantDeleted, fixture)
		for _, path := range paths {
			wantUpdated = append(wantUpdated, fixture+" "+path)
		}
	}
	for seed := range int64(seeds) {
		sequence, err := g.Draw(seed)
		if err != nil {
			t.Fatalf("Draw(%d) failed: %v.", seed, err)
		}
		first := true
		for _, op := range sequence.Ops {
			fixture := op.Kind + " " + op.Name
			switch {
			case op.Type == run.OpUpdateFixture:
				updated = append(updated, fixture+" "+differences(nil, op.Patch, "")[0])
			case op.Type == run.OpDeleteFixture && first:
				deletedFirst, first = append(deletedFirst, fixture), false
			}
		}
	}
	if got := sortedSet(updated); !slices.Equal(got, sortedSet(wantUpdated)) {
		t.Errorf("Seeds 0 to %d set %v, want %v.", seeds-1, got, sortedSet(wantUpdated))
	}
	if got := sortedSet(deletedFirst); !slices.Equal(got, sortedSet(wantDeleted)) {
		t.Errorf("Seeds 0 to %d first delete %v, want %v.", seeds-1, got, sortedSet(wantDeleted))
	}
}

func sortedSet(items []string) []string {
	return slices.Compact(slices.Sorted(slices.Values(items)))
}

// leaf is the one string a patch that sets one field sets.
func leaf(patch map[string]any) string {
	for _, value := range patch {
		if nested, isObject := value.(map[string]any); isObject {
			return leaf(nested)
		}
		text, _ := value.(string)
		return text
	}
	return ""
}

// The target may rightly not be ready while a fixture is gone, so no settle
// wait runs without it.
func TestADeletedFixtureComesBackBeforeTheNextOpThatSettles(t *testing.T) {
	g := newGenerator(t, loadTarget(t, fixturesTarget), Options{})
	rapid.Check(t, func(rt *rapid.T) {
		ops := g.sequence(rt).Ops
		for i, op := range ops {
			if op.Type != run.OpDeleteFixture {
				continue
			}
			if next := i + 1 + slices.IndexFunc(ops[i+1:], run.Op.Settles); op.Until.Op != next {
				rt.Fatalf("Op %d deletes a fixture until op %d, and op %d is the next that settles.", i, op.Until.Op, next)
			}
		}
	})
}

func TestADeletedFixtureIsBackForTheNextDrawOnceAnOpSettles(t *testing.T) {
	loaded := loadTarget(t, fixturesTarget)
	g := newGenerator(t, loaded, Options{})
	at := state{crs: []drawnCR{{object: loaded.Sample.Object, live: true}}}
	token := run.Op{Type: run.OpDeleteFixture, Kind: "v1/Secret", Name: "token"}

	at.advance(token, 0)
	if present := g.present(&at); len(present) != 2 || slices.ContainsFunc(present, func(fixture target.MutableFixture) bool {
		return fixture.Name == "token"
	}) {
		t.Errorf("With the token deleted, generation may delete %v, want every other fixture.", present)
	}
	at.advance(run.Op{Type: run.OpSettle}, 0)
	if present := g.present(&at); len(present) != 3 {
		t.Errorf("Once an op settled, generation may delete %v, want every fixture.", present)
	}
}

func TestATargetWithoutGenerateFixturesDrawsNoFixtureOp(t *testing.T) {
	// The toy declares a fixture, and no generate.fixtures.
	g := newGenerator(t, loadTarget(t, toyTarget), Options{})
	rapid.Check(t, func(rt *rapid.T) {
		for _, op := range g.sequence(rt).Ops {
			if op.Type == run.OpUpdateFixture || op.Type == run.OpDeleteFixture {
				rt.Fatalf("Op %d is a %s, and the target names no fixture generation may change.", op.Index, op.Type)
			}
		}
	})
}

func TestATargetThatMayOnlyDeleteItsFixturesDrawsNoUpdateFixture(t *testing.T) {
	deleteOnly := loadTarget(t, fixturesTarget)
	deleteOnly.Generate.Fixtures = slices.DeleteFunc(deleteOnly.Generate.Fixtures, func(fixture target.MutableFixture) bool {
		return len(fixture.Mutate) > 0
	})
	g := newGenerator(t, deleteOnly, Options{})
	rapid.Check(t, func(rt *rapid.T) {
		for _, op := range g.sequence(rt).Ops {
			if op.Type == run.OpUpdateFixture {
				rt.Fatalf("Op %d is an updateFixture, and the target names no path generation may set.", op.Index)
			}
		}
	})
}

func TestGeneratedCRsMatchTheirCRD(t *testing.T) {
	for _, testCase := range targets {
		t.Run(testCase.path, func(t *testing.T) {
			// The API server enforces the CRD; the overlay only tightens what
			// the generator draws, and the sample need not satisfy it.
			loaded := loadTarget(t, testCase.path)
			g := newGenerator(t, loaded, Options{})
			crd := crdSchemaOf(t, testCase.path)
			rapid.Check(t, func(rt *rapid.T) {
				follow(g.sequence(rt), loaded.Sample.GetName(), func(op run.Op, name string, crs map[string]*written) {
					if !op.Type.OnCR() || op.Type == run.OpDelete {
						return
					}
					if err := validate(crd, crs[name].object, ""); err != nil {
						rt.Fatalf("Op %d left the CR %s outside its schema: %v.", op.Index, name, err)
					}
				})
			})
		})
	}
}

// The gadget's CRD states rules its schema cannot. The test reads them here,
// apart from the API server's code that generation runs.
func gadgetRuleBroken(cr, old map[string]any) string {
	spec, _ := cr["spec"].(map[string]any)
	value := func(spec map[string]any, name string, absent int64) int64 {
		if set, isInteger := spec[name].(int64); isInteger {
			return set
		}
		return absent
	}
	_, left := spec["left"]
	_, right := spec["right"]
	switch count := value(spec, "count", 0); {
	case value(spec, "maxUnavailable", 0) > count:
		return "maxUnavailable exceeds count"
	case count < value(spec, "minCount", 1):
		return "count is below minCount's default"
	case left == right:
		return "left and right are both set or both unset"
	}
	previous, _ := old["spec"].(map[string]any)
	switch {
	case old == nil:
	case previous["mode"] != spec["mode"]:
		return "mode changed"
	case value(spec, "minCount", 1) > value(previous, "minCount", 1):
		return "minCount rose"
	}
	return ""
}

func TestGeneratedGadgetsKeepTheirCRDsRules(t *testing.T) {
	loaded := loadTarget(t, rulesTarget)
	g := newGenerator(t, loaded, Options{})
	crd := crdSchemaOf(t, rulesTarget)
	var changed, switchedMode, updated int
	rapid.Check(t, func(rt *rapid.T) {
		crs := map[string]map[string]any{}
		for _, op := range g.sequence(rt).Ops {
			var next, old map[string]any
			name := orSample(op.CR, loaded.Sample.GetName())
			switch op.Type {
			case run.OpCreate, run.OpRecreate:
				next, name = op.Obj.DeepCopy().Object, op.Obj.GetName()
				if !equalJSON(next["spec"], loaded.Sample.Object["spec"]) {
					changed++
				}
				if mode, _, _ := unstructured.NestedString(next, "spec", "mode"); mode != "fast" {
					switchedMode++
				}
			case run.OpUpdate:
				next, old = merge(crs[name], op.Patch), crs[name]
				updated++
			default:
				continue
			}
			if err := validate(crd, next, ""); err != nil {
				rt.Fatalf("Op %d (%s) left the gadget outside its schema: %v.", op.Index, op.Type, err)
			}
			if broken := gadgetRuleBroken(next, old); broken != "" {
				rt.Fatalf("Op %d (%s) left a gadget whose %s: %v.", op.Index, op.Type, broken, next["spec"])
			}
			crs[name] = next
		}
	})
	if changed == 0 || updated == 0 {
		t.Errorf("%d creates changed the sample and %d updates were drawn; a generator that changes nothing keeps every rule.",
			changed, updated)
	}
	if switchedMode == 0 {
		t.Error("No create switched the sample's mode, which only an update may not change.")
	}
}

// countField is spec.count, drawn always as the count given. It counts its
// draws.
func countField(count int64, draws *int) field {
	values := rapid.Custom(func(t *rapid.T) any {
		*draws++
		return rapid.Just(count).Draw(t, "count")
	})
	return field{path: []string{"spec", "count"}, dotted: "spec.count", values: values}
}

func TestARefusedUpdateIsDrawnEightTimes(t *testing.T) {
	loaded := loadTarget(t, rulesTarget)
	g := newGenerator(t, loaded, Options{})
	for _, testCase := range []struct {
		count    int64
		draws    int
		accepted bool
	}{
		{5, 1, true},
		// No count reaches minCount's default.
		{0, 8, false},
	} {
		draws := 0
		g.fields = []field{countField(testCase.count, &draws)}
		at := &state{crs: []drawnCR{{object: loaded.Sample.Object, live: true}}}
		patch := rapid.Custom(func(t *rapid.T) map[string]any { return g.patch(t, 0, at) }).Example(0)
		if (patch != nil) != testCase.accepted || draws != testCase.draws {
			t.Errorf("With every count %d, an update drew %d times and patched %v, want %d draws and accepted=%t.",
				testCase.count, draws, patch, testCase.draws, testCase.accepted)
		}
	}
}

func TestAnUpdateTheCRDAlwaysRefusesBecomesASettle(t *testing.T) {
	loaded := loadTarget(t, rulesTarget)
	g := newGenerator(t, loaded, Options{})
	draws := 0
	g.fields = []field{countField(0, &draws)}
	refused := 0
	rapid.Check(t, func(rt *rapid.T) {
		before := draws
		op := g.op(rt, 1, &state{crs: []drawnCR{{object: loaded.Sample.Object, live: true}}})
		if draws-before != updateDraws {
			return
		}
		refused++
		if want := (run.Op{Index: 1, Type: run.OpSettle}); !reflect.DeepEqual(op, want) {
			rt.Fatalf("An update the CRD refused became %+v, want %+v.", op, want)
		}
	})
	if refused == 0 {
		t.Error("No update was drawn.")
	}
}

func TestTheStateFollowsTheCRReconcilerFuzzerLastWrote(t *testing.T) {
	spec := func(count int64) map[string]any {
		return map[string]any{"spec": map[string]any{"count": count, "mode": "fast"}}
	}
	at := state{crs: []drawnCR{{object: spec(3), live: true}}}
	holds := func(after string, want ...drawnCR) {
		t.Helper()
		same := len(at.crs) == len(want)
		for n := range min(len(at.crs), len(want)) {
			same = same && at.crs[n].live == want[n].live && equalJSON(at.crs[n].object, want[n].object)
		}
		if !same {
			t.Errorf("After %s the state holds %v, want %v.", after, at.crs, want)
		}
	}

	at.advance(run.Op{Type: run.OpCreate, Obj: &unstructured.Unstructured{Object: spec(1)}}, 1)
	at.advance(run.Op{Type: run.OpUpdate, Patch: map[string]any{"spec": map[string]any{"count": int64(5)}}}, 0)
	holds("an update of the first CR", drawnCR{spec(5), true}, drawnCR{spec(1), true})
	at.advance(run.Op{Type: run.OpUpdate, Patch: map[string]any{"spec": map[string]any{"count": int64(6)}}}, 1)
	holds("an update of the second CR", drawnCR{spec(5), true}, drawnCR{spec(6), true})
	at.advance(run.Op{Type: run.OpDelete}, 1)
	holds("a delete of the second CR", drawnCR{spec(5), true}, drawnCR{spec(6), false})
	if live := at.live(); !slices.Equal(live, []int{0}) {
		t.Errorf("After a delete of the second CR the CRs %v are live, want the first alone.", live)
	}
	at.advance(run.Op{Type: run.OpRecreate, Obj: &unstructured.Unstructured{Object: spec(7)}}, 1)
	holds("a recreate of the second CR", drawnCR{spec(5), true}, drawnCR{spec(7), true})
}

func TestSequencesAreLegalToReplay(t *testing.T) {
	const maxOps = 5
	for _, testCase := range targets {
		t.Run(testCase.path, func(t *testing.T) {
			loaded := loadTarget(t, testCase.path)
			g := newGenerator(t, loaded, Options{MaxOps: maxOps})
			rapid.Check(t, func(rt *rapid.T) {
				sequence := g.sequence(rt)
				if err := sequence.Validate(); err != nil {
					rt.Fatalf("The Runner rejects the sequence: %v.", err)
				}
				if sequence.Target != loaded.Name {
					rt.Fatalf("The sequence names the target %q, want %q.", sequence.Target, loaded.Name)
				}
				// checkpointed adds at most two settles per drawn op; a
				// fault adds a fault op and a settle.
				if len(sequence.Ops) > 3*maxOps+2 {
					rt.Fatalf("The sequence holds %d ops, over the %d drawn, their settles, and one fault.",
						len(sequence.Ops), maxOps)
				}
				if sequence.Ops[0].Type != run.OpCreate {
					rt.Fatalf("The sequence opens with a %s, want the create of the CR.", sequence.Ops[0].Type)
				}
				for i, op := range sequence.Ops[1:] {
					if op.Type == run.OpDeleteManaged {
						if !slices.Contains(testCase.managed, op.Kind) {
							rt.Fatalf("Op %d deletes a %s, which the target does not declare it manages.",
								op.Index, op.Kind)
						}
						if *op.Nth != 0 {
							rt.Fatalf("Op %d deletes the managed object %d, which generation cannot know exists.",
								op.Index, *op.Nth)
						}
						previous := sequence.Ops[i]
						if previous.Type == run.OpFault {
							previous = sequence.Ops[i-1]
						}
						if !previous.Settles() {
							rt.Fatalf("Op %d deletes a managed object after op %d, which waits for nothing.",
								op.Index, previous.Index)
						}
					}
				}
			})
		})
	}
}

// An invariant is evaluated where a settle wait ends (DESIGN.md §6), so an op
// that nothing waits on is a draw spent on a run nothing judges.
func TestEveryDrawnOpIsFollowedByTheSettleThatJudgesIt(t *testing.T) {
	for _, testCase := range targets {
		t.Run(testCase.path, func(t *testing.T) {
			g := newGenerator(t, loadTarget(t, testCase.path), Options{MaxOps: 5})
			rapid.Check(t, func(rt *rapid.T) {
				ops := g.sequence(rt).Ops
				if last := ops[len(ops)-1]; !last.Settles() {
					rt.Fatalf("The sequence ends with a %s, so only the teardown judges it (§6).", last.Type)
				}
				for i, op := range ops {
					if op.Type != run.OpRestart {
						continue
					}
					// G5 judges a restart only if no other op changed the run
					// between the converged states either side of it.
					if before := ops[i-1]; !before.Settles() {
						rt.Fatalf("Op %d restarts the target after a %s that waits for nothing, so G5 has nothing to judge.",
							i, before.Type)
					}
					if after := ops[i+1]; after.Type != run.OpSettle {
						rt.Fatalf("Op %d restarts the target and op %d is a %s, so G5 has nothing to judge.",
							i, i+1, after.Type)
					}
				}
			})
		})
	}
}

func TestSequencesRoundTripThroughTheirFile(t *testing.T) {
	for _, testCase := range targets {
		t.Run(testCase.path, func(t *testing.T) {
			g := newGenerator(t, loadTarget(t, testCase.path), Options{})
			file := filepath.Join(t.TempDir(), "sequence.json")
			rapid.Check(t, func(rt *rapid.T) {
				written := write(rt, file, g.sequence(rt))
				read, err := run.ReadSequence(file)
				if err != nil {
					rt.Fatalf("ReadSequence failed on %s: %v.", written, err)
				}
				if rewritten := write(rt, file, read); !bytes.Equal(written, rewritten) {
					rt.Fatalf("The sequence changed as it went through its file:\n%s\n%s", written, rewritten)
				}
			})
		})
	}
}

func write(rt *rapid.T, file string, sequence run.Sequence) []byte {
	if err := run.WriteSequence(file, sequence); err != nil {
		rt.Fatalf("WriteSequence failed: %v.", err)
	}
	written, err := os.ReadFile(file)
	if err != nil {
		rt.Fatalf("Reading back the sequence file failed: %v.", err)
	}
	return written
}

func TestOneSeedDrawsOneSequence(t *testing.T) {
	const seeds = 20
	g := newGenerator(t, loadTarget(t, certManagerTarget), Options{})
	distinct := map[string]bool{}
	for seed := int64(1); seed <= seeds; seed++ {
		first, second := draw(t, g, seed), draw(t, g, seed)
		if !bytes.Equal(first, second) {
			t.Fatalf("Seed %d drew two different sequences:\n%s\n%s", seed, first, second)
		}
		distinct[string(first)] = true
	}
	if len(distinct) < seeds/2 {
		t.Errorf("%d seeds drew %d distinct sequences; the seed chooses the sequence.", seeds, len(distinct))
	}
}

func draw(t *testing.T, g *Generator, seed int64) []byte {
	t.Helper()
	sequence, err := g.Draw(seed)
	if err != nil {
		t.Fatalf("Draw(%d) failed: %v.", seed, err)
	}
	if sequence.Seed != seed {
		t.Fatalf("The sequence drawn at seed %d carries the seed %d.", seed, sequence.Seed)
	}
	if err := sequence.Validate(); err != nil {
		t.Fatalf("The Runner rejects the sequence drawn at seed %d: %v.", seed, err)
	}
	marshalled, err := sequence.Marshal()
	if err != nil {
		t.Fatalf("Marshalling the sequence drawn at seed %d failed: %v.", seed, err)
	}
	return marshalled
}

func TestGeneratedCountsStayInsideTheCRDsBounds(t *testing.T) {
	loaded := loadTarget(t, toyTarget)
	g := newGenerator(t, loaded, Options{})
	rapid.Check(t, func(rt *rapid.T) {
		for _, count := range generatedAt(g.sequence(rt), loaded.Sample, "spec", "count") {
			whole, isInteger := count.(int64)
			if !isInteger {
				rt.Fatalf("spec.count is %#v, want the int64 the API server's decoder writes.", count)
			}
			if whole < 0 || whole > 10 {
				rt.Fatalf("spec.count is %d, outside the CRD's 0 to 10.", whole)
			}
		}
	})
}

func TestGeneratedValuesObeyTheOverlay(t *testing.T) {
	// The CRD takes any string as a duration and any list of DNS names; only
	// the overlay says which (DESIGN.md §8.3).
	loaded := loadTarget(t, certManagerTarget)
	g := newGenerator(t, loaded, Options{})
	allowed := []any{"1h", "24h", "2160h"}
	rapid.Check(t, func(rt *rapid.T) {
		sequence := g.sequence(rt)
		for _, duration := range generatedAt(sequence, loaded.Sample, "spec", "duration") {
			if !slices.Contains(allowed, duration) {
				rt.Fatalf("spec.duration is %#v, which the overlay's enum %v does not allow.", duration, allowed)
			}
		}
		for _, names := range generatedAt(sequence, loaded.Sample, "spec", "dnsNames") {
			if length := len(names.([]any)); length < 1 || length > 3 {
				rt.Fatalf("spec.dnsNames holds %d names, outside the overlay's 1 to 3.", length)
			}
		}
		for _, policy := range carriedAt(sequence, "spec", "privateKey", "rotationPolicy") {
			if policy != "Always" {
				rt.Fatalf("spec.privateKey.rotationPolicy is %#v. Under Never, cert-manager waits for a user once a later op changes the algorithm.", policy)
			}
		}
	})
}

func TestTheOverlayBoundsTheListLength(t *testing.T) {
	const exactly = 2
	loaded := loadTarget(t, certManagerTarget)
	loaded.Generate.Overlay["spec.dnsNames"] = map[string]any{"minItems": exactly, "maxItems": exactly}
	g := newGenerator(t, loaded, Options{})
	rapid.Check(t, func(rt *rapid.T) {
		for _, names := range generatedAt(g.sequence(rt), loaded.Sample, "spec", "dnsNames") {
			if length := len(names.([]any)); length != exactly {
				rt.Fatalf("spec.dnsNames holds %d names, and the overlay allows %d.", length, exactly)
			}
		}
	})
}

func TestOnlyTheMutablePathsMove(t *testing.T) {
	for _, testCase := range targets {
		t.Run(testCase.path, func(t *testing.T) {
			loaded := loadTarget(t, testCase.path)
			g := newGenerator(t, loaded, Options{})
			rapid.Check(t, func(rt *rapid.T) {
				// Each CR after the first also takes a name and distinct values of
				// its own.
				moves := slices.Concat(testCase.mutablePaths, []string{"metadata.name"}, loaded.Generate.Distinct)
				for _, op := range g.sequence(rt).Ops {
					if op.Obj == nil {
						continue
					}
					for _, path := range differences(loaded.Sample.Object, op.Obj.Object, "") {
						if !slices.Contains(moves, path) {
							rt.Fatalf("Op %d moved %s, and the target lets the generator move %v.",
								op.Index, path, testCase.mutablePaths)
						}
					}
				}
			})
		})
	}
}

// generatedAt collects what a sequence generated at a path, in the CRs it
// creates and the patches it applies. What a CR kept from the sample is not
// the generator's doing.
func generatedAt(sequence run.Sequence, sample *unstructured.Unstructured, path ...string) []any {
	kept, _, _ := unstructured.NestedFieldNoCopy(sample.Object, path...)
	var values []any
	for _, op := range sequence.Ops {
		var object map[string]any
		switch op.Type {
		case run.OpCreate, run.OpRecreate:
			object = op.Obj.Object
		case run.OpUpdate:
			object = op.Patch
		default:
			continue
		}
		value, found, err := unstructured.NestedFieldNoCopy(object, path...)
		if found && err == nil && value != nil && !equalJSON(value, kept) {
			values = append(values, value)
		}
	}
	return values
}

// carriedAt collects what a sequence's CRs and patches hold at a path, the
// sample's values included.
func carriedAt(sequence run.Sequence, path ...string) []any {
	return generatedAt(sequence, &unstructured.Unstructured{}, path...)
}

// differences are the dotted paths where two objects disagree.
func differences(a, b map[string]any, prefix string) []string {
	var paths []string
	for _, name := range union(a, b) {
		left, inA := a[name]
		right, inB := b[name]
		leftObject, aIsObject := left.(map[string]any)
		rightObject, bIsObject := right.(map[string]any)
		switch {
		case (aIsObject || bIsObject) && (aIsObject || !inA) && (bIsObject || !inB):
			paths = append(paths, differences(leftObject, rightObject, join(prefix, name))...)
		case inA != inB || !equalJSON(left, right):
			paths = append(paths, join(prefix, name))
		}
	}
	return paths
}

func union(a, b map[string]any) []string {
	names := slices.Collect(maps.Keys(a))
	for name := range b {
		if _, inA := a[name]; !inA {
			names = append(names, name)
		}
	}
	slices.Sort(names)
	return names
}

// merge applies a JSON merge patch (RFC 7386), as the Runner does.
func merge(object, patch map[string]any) map[string]any {
	merged := map[string]any{}
	maps.Copy(merged, object)
	for name, value := range patch {
		nested, isObject := value.(map[string]any)
		switch {
		case value == nil:
			delete(merged, name)
		case isObject:
			into, _ := merged[name].(map[string]any)
			merged[name] = merge(into, nested)
		default:
			merged[name] = value
		}
	}
	return merged
}

func TestATargetWithNothingToMutateStillDrawsSequences(t *testing.T) {
	// A target may declare no manages, and a CRD may describe no path the
	// generator can write. Neither leaves an op with nothing to choose from.
	g := newGenerator(t, loadTarget(t, toyTarget), Options{})
	g.fields, g.managed = nil, nil
	rapid.Check(t, func(rt *rapid.T) {
		for _, op := range g.sequence(rt).Ops {
			if op.Type == run.OpUpdate || op.Type == run.OpDeleteManaged {
				rt.Fatalf("Op %d is a %s, and the target offers it nothing to name.", op.Index, op.Type)
			}
		}
	})
}

// A defaulting webhook fills a field that the CRD leaves optional. No webhook
// runs, so an overlay that requires the field keeps every draw from dropping it.
func TestAnOverlayThatRequiresAFieldKeepsItInEveryDraw(t *testing.T) {
	for _, overlaid := range []bool{false, true} {
		loaded := loadTarget(t, certManagerTarget)
		if overlaid {
			loaded.Generate.Overlay["spec"] = map[string]any{"required": []any{"secretName", "issuerRef", "dnsNames"}}
		}
		g := newGenerator(t, loaded, Options{})
		dropped := false
		rapid.Check(t, func(rt *rapid.T) {
			for _, op := range g.sequence(rt).Ops {
				switch op.Type {
				case run.OpCreate, run.OpRecreate:
					_, found, _ := unstructured.NestedFieldNoCopy(op.Obj.Object, "spec", "dnsNames")
					dropped = dropped || !found
				case run.OpUpdate:
					value, found, _ := unstructured.NestedFieldNoCopy(op.Patch, "spec", "dnsNames")
					dropped = dropped || (found && value == nil)
				}
			}
		})
		if dropped == overlaid {
			t.Errorf("With the overlay %v, a draw dropped spec.dnsNames: %v.", overlaid, dropped)
		}
	}
}

func TestEachLegalOpIsDrawnWithEqualChance(t *testing.T) {
	g := newGenerator(t, toyWithALabelFixture(t), Options{})
	settled := func() *state {
		at := &state{}
		at.advance(run.Op{Type: run.OpCreate, Obj: g.base(0)}, 0)
		return at
	}
	legal := g.legal(settled())
	const each = 300
	drawn := map[run.OpType]int{}
	ops := rapid.Custom(func(t *rapid.T) run.OpType { return g.op(t, 1, settled()).Type })
	for seed := range each * len(legal) {
		drawn[ops.Example(seed)]++
	}
	for _, op := range legal {
		if drawn[op] < each*4/5 || drawn[op] > each*6/5 {
			t.Errorf("Of %d draws, %d are a %s, want about %d.", each*len(legal), drawn[op], op, each)
		}
	}
}

func TestEachLengthIsDrawnWithEqualChance(t *testing.T) {
	g := newGenerator(t, loadTarget(t, toyTarget), Options{MaxOps: 6})
	const each = 2000
	drawn := map[int]int{}
	lengths := rapid.Custom(g.opsAfterCreate)
	for seed := range each * 5 {
		drawn[lengths.Example(seed)]++
	}
	for length := 1; length <= 5; length++ {
		if drawn[length] < each*9/10 || drawn[length] > each*11/10 {
			t.Errorf("Of %d draws, %d draw %d ops after the create, want about %d.", each*5, drawn[length], length, each)
		}
	}
	if drawn[0] > 0 {
		t.Errorf("%d draws hold only the create.", drawn[0])
	}
}

func TestOneOpDrawsOnlyTheCreate(t *testing.T) {
	g := newGenerator(t, loadTarget(t, toyTarget), Options{MaxOps: 1})
	rapid.Check(t, func(rt *rapid.T) {
		for _, op := range g.sequence(rt).Ops[1:] {
			if op.Type != run.OpSettle && op.Type != run.OpFault {
				rt.Fatalf("With MaxOps 1, op %d is a %s.", op.Index, op.Type)
			}
		}
	})
}

func TestDeleteManagedIsLegalWhileACRIsLive(t *testing.T) {
	g := newGenerator(t, loadTarget(t, toyTarget), Options{})
	at := &state{}
	if slices.Contains(g.legal(at), run.OpDeleteManaged) {
		t.Errorf("With no CR, generation may draw %v.", g.legal(at))
	}
	at.advance(run.Op{Type: run.OpCreate, Obj: g.base(0), NoSettle: true}, 0)
	if !slices.Contains(g.legal(at), run.OpDeleteManaged) {
		t.Errorf("Right after a noSettle create, generation may draw only %v.", g.legal(at))
	}
}

func TestASettleWaitPrecedesADeleteManagedThatFollowsAnOpThatDoesNotSettle(t *testing.T) {
	deleteManaged := run.Op{Type: run.OpDeleteManaged, Kind: "v1/ConfigMap", Nth: new(int)}
	for _, testCase := range []struct {
		name  string
		drawn []run.Op
		want  []run.OpType
	}{
		{"a settled create", []run.Op{{Type: run.OpCreate}, deleteManaged},
			[]run.OpType{run.OpCreate, run.OpDeleteManaged}},
		{"a noSettle create", []run.Op{{Type: run.OpCreate, NoSettle: true}, deleteManaged},
			[]run.OpType{run.OpCreate, run.OpSettle, run.OpDeleteManaged}},
		{"a deleteFixture", []run.Op{{Type: run.OpCreate}, {Type: run.OpDeleteFixture}, deleteManaged},
			[]run.OpType{run.OpCreate, run.OpDeleteFixture, run.OpSettle, run.OpDeleteManaged}},
		{"a restart", []run.Op{{Type: run.OpCreate}, {Type: run.OpRestart}, deleteManaged},
			[]run.OpType{run.OpCreate, run.OpRestart, run.OpSettle, run.OpDeleteManaged}},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			if got := opTypes(checkpointed(testCase.drawn)); !slices.Equal(got, testCase.want) {
				t.Errorf("checkpointed gives %v, want %v.", got, testCase.want)
			}
		})
	}
}
