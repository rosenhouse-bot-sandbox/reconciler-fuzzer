package generate

import (
	"bytes"
	"cmp"
	"encoding/json"
	"flag"
	"io/fs"
	"maps"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/rosenhouse-bot-sandbox/reconciler-fuzzer/internal/run"
	"github.com/rosenhouse-bot-sandbox/reconciler-fuzzer/internal/target"
	structuralschema "k8s.io/apiextensions-apiserver/pkg/apiserver/schema"
	"k8s.io/apiextensions-apiserver/pkg/apiserver/schema/objectmeta"
	"k8s.io/apiextensions-apiserver/pkg/apiserver/schema/pruning"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	kinds "k8s.io/apimachinery/pkg/runtime/schema"
)

var updateGolden = flag.Bool("update", false, "rewrite "+goldenDraws+" from what the seeds draw now")

const (
	externalSecretsTarget = "../../examples/external-secrets/target.yaml"
	goldenDraws           = "testdata/draws.golden.json"
)

type goldenTarget struct {
	name    string
	declare declaration
	seeds   []int64
}

// goldenSeeds are the seeds the Makefile and the envtest tier run by number.
func goldenSeeds(t *testing.T) []goldenTarget {
	example := makefileSeeds(t, "EXAMPLE")
	return []goldenTarget{
		{"toy-widget", file(toyTarget), append(seedRange(1, 10), makefileSeeds(t, "KIND")...)},
		{"cert-manager", file(certManagerTarget), append(seedRange(1, 10), example...)},
		{"external-secrets", file(externalSecretsTarget), example},
		{"toy-widget with a label fixture", toyWithALabelFixture, seedRange(1, 60)},
	}
}

// makefileSeeds are the seeds the Makefile's <tier>_SEED and <tier>_RUNS draw.
func makefileSeeds(t *testing.T, tier string) []int64 {
	t.Helper()
	makefile, err := os.ReadFile("../../Makefile")
	if err != nil {
		t.Fatal(err)
	}
	pin := func(name string) int64 {
		m := regexp.MustCompile(`(?m)^` + name + ` \?= (\d+)$`).FindSubmatch(makefile)
		if m == nil {
			t.Fatalf("The Makefile sets no %s.", name)
		}
		value, _ := strconv.ParseInt(string(m[1]), 10, 64)
		return value
	}
	first := pin(tier + "_SEED")
	return seedRange(first, first+pin(tier+"_RUNS")-1)
}

type declaration func(*testing.T) *target.Target

func file(path string) declaration {
	return func(t *testing.T) *target.Target { return loadTarget(t, path) }
}

// toyWithALabelFixture is the toy as pkg/run's TestGeneratedFixtureOps
// declares it. That test fails unless it draws what the golden file records.
func toyWithALabelFixture(t *testing.T) *target.Target {
	toy := loadTarget(t, toyTarget)
	toy.Generate.Fixtures = []target.MutableFixture{{
		GVK:    kinds.GroupVersionKind{Version: "v1", Kind: "ConfigMap"},
		Name:   "widget-config",
		Mutate: []target.Path{target.MustParsePath("data.label")},
	}}
	return toy
}

func seedRange(first, last int64) []int64 {
	var seeds []int64
	for seed := first; seed <= last; seed++ {
		seeds = append(seeds, seed)
	}
	return seeds
}

// A seed names a sequence only for one build of reconciler-fuzzer, so a change
// to what a seed draws has to be deliberate: rerun with -update and say why.
func TestSeedsDrawTheGoldenSequences(t *testing.T) {
	drawn := map[string]map[string]json.RawMessage{}
	for _, declared := range goldenSeeds(t) {
		g := newGenerator(t, declared.declare(t), Options{})
		drawn[declared.name] = map[string]json.RawMessage{}
		for _, seed := range declared.seeds {
			drawn[declared.name][strconv.FormatInt(seed, 10)] = draw(t, g, seed)
		}
	}
	encoded, err := json.MarshalIndent(drawn, "", "  ")
	if err != nil {
		t.Fatalf("Encoding the draws failed: %v.", err)
	}
	encoded = append(encoded, '\n')
	if *updateGolden {
		if err := os.WriteFile(goldenDraws, encoded, 0o644); err != nil {
			t.Fatalf("Writing %s failed: %v.", goldenDraws, err)
		}
		return
	}

	recorded, err := os.ReadFile(goldenDraws)
	if err != nil {
		t.Fatalf("Reading %s failed: %v.", goldenDraws, err)
	}
	if bytes.Equal(recorded, encoded) {
		return
	}
	var golden map[string]map[string]json.RawMessage
	if err := json.Unmarshal(recorded, &golden); err != nil {
		t.Fatalf("Reading %s failed: %v.", goldenDraws, err)
	}
	var differ []string
	for _, name := range keys(drawn, golden) {
		for _, seed := range keys(drawn[name], golden[name]) {
			if !equalJSON(golden[name][seed], drawn[name][seed]) {
				differ = append(differ, name+" seed "+seed)
			}
		}
	}
	if len(differ) == 0 {
		t.Fatalf("%s records every draw, but not as -update writes it. Rerun with -update.", goldenDraws)
	}
	t.Errorf("%s differs for %s. If the change is deliberate, rerun with -update, read its diff and say why in the commit.",
		goldenDraws, strings.Join(differ, ", "))
}

// keys are the keys of either map, sorted.
func keys[V any](a, b map[string]V) []string {
	union := slices.AppendSeq(slices.Collect(maps.Keys(a)), maps.Keys(b))
	slices.Sort(union)
	return slices.Compact(union)
}

func TestCertManagersNegativeControlIsASingleCreate(t *testing.T) {
	// One op costs no replay to minimize.
	const control = "../../examples/cert-manager/sequences/create.json"
	sequence, err := run.ReadSequence(control)
	if err != nil {
		t.Fatal(err)
	}
	if ops := sequence.Ops; len(ops) != 1 || ops[0].Type != run.OpCreate {
		t.Errorf("%s holds %v, want a single create.", control, opTypes(ops))
	}
}

func TestCertManagersExampleSeedsDrawWhatTheMakefileSays(t *testing.T) {
	seeds := makefileSeeds(t, "EXAMPLE")
	drawn := map[string]bool{}
	for _, seed := range seeds {
		for _, op := range drawOps(t, file(certManagerTarget), seed) {
			drawn[string(op.Type)] = true
			if op.Type == run.OpCreate && op.Obj.GetName() != "example" {
				drawn["a second Certificate"] = true
			}
		}
	}
	for _, want := range []string{"a second Certificate", string(run.OpRestart), string(run.OpFault), string(run.OpDelete), string(run.OpDeleteManaged)} {
		if !drawn[want] {
			t.Errorf("Seeds %v draw no %s, and the Makefile says they do.", seeds, want)
		}
	}
}

func TestCertManagersPinnedSequencesRecreateTheCertificate(t *testing.T) {
	for _, path := range glob(t, certManagerSequences) {
		sequence, err := run.ReadSequence(path)
		if err != nil {
			t.Fatal(err)
		}
		if slices.ContainsFunc(sequence.Ops, func(op run.Op) bool { return op.Type == run.OpRecreate }) {
			return
		}
	}
	t.Errorf("No sequence in %s recreates the Certificate, and the Makefile says one does.", certManagerSequences)
}

const (
	certManagerSequences     = "../../examples/cert-manager/sequences/*.json"
	certManagerHuntSequences = "../../examples/cert-manager/sequences/hunt/*.json"
)

func TestCertManagersPinnedSequencesReissueUnderRotationPolicyNever(t *testing.T) {
	// Draws never set Never, so the example tier runs it only from the
	// sequences it pins.
	for _, change := range certificateChanges(t, glob(t, certManagerSequences)...) {
		if renamesUnderNever(change) {
			return
		}
	}
	t.Errorf("No sequence in %s updates the names of a Certificate that stays under rotationPolicy Never.", certManagerSequences)
}

func renamesUnderNever(change crWrite) bool {
	renames := change.op.Type == run.OpUpdate && !slices.Equal(namesOf(change.before), namesOf(change.after))
	return renames && privateKeyOf(change.before).rotationPolicy == "Never" && privateKeyOf(change.after).rotationPolicy == "Never"
}

func TestRenamesUnderNeverNeedsAnUpdateOfTheNamesThatKeepsNever(t *testing.T) {
	with := func(certificate map[string]any, value any, path ...string) map[string]any {
		changed := runtime.DeepCopyJSON(certificate)
		if err := unstructured.SetNestedField(changed, value, path...); err != nil {
			t.Fatal(err)
		}
		return changed
	}
	never := map[string]any{"spec": map[string]any{
		"commonName": "a.test", "dnsNames": []any{"a.test", "b.test"}, "privateKey": map[string]any{"rotationPolicy": "Never"},
	}}
	always := with(never, "Always", "spec", "privateKey", "rotationPolicy")
	for _, c := range []struct {
		name          string
		op            run.OpType
		before, after map[string]any
		want          bool
	}{
		{"an update of dnsNames", run.OpUpdate, never, with(never, []any{"a.test"}, "spec", "dnsNames"), true},
		{"an update of commonName", run.OpUpdate, never, with(never, "b.test", "spec", "commonName"), true},
		{"an update that reorders dnsNames", run.OpUpdate, never, with(never, []any{"b.test", "a.test"}, "spec", "dnsNames"), false},
		{"a recreate with other dnsNames", run.OpRecreate, never, with(never, []any{"a.test"}, "spec", "dnsNames"), false},
		{"an update of dnsNames into Always", run.OpUpdate, never, with(always, []any{"a.test"}, "spec", "dnsNames"), false},
		{"an update of dnsNames out of Always", run.OpUpdate, always, with(never, []any{"a.test"}, "spec", "dnsNames"), false},
	} {
		if got := renamesUnderNever(crWrite{op: run.Op{Type: c.op}, before: c.before, after: c.after}); got != c.want {
			t.Errorf("renamesUnderNever(%s) = %t, want %t.", c.name, got, c.want)
		}
	}
}

// namesOf are the names a Certificate asks its certificate to carry.
// cert-manager reissues when they change, and ignores their order.
func namesOf(certificate map[string]any) []string {
	commonName, _, _ := unstructured.NestedString(certificate, "spec", "commonName")
	dnsNames, _, _ := unstructured.NestedStringSlice(certificate, "spec", "dnsNames")
	return append([]string{commonName}, slices.Sorted(slices.Values(dnsNames))...)
}

func TestCertManagersSequencesKeepTheAlgorithmUnderRotationPolicyNever(t *testing.T) {
	for _, change := range certificateChanges(t, glob(t, certManagerSequences, certManagerHuntSequences)...) {
		if changesAlgorithmUnderNever(change) {
			t.Errorf("%s op %d moves spec.privateKey.algorithm from %s to %s under rotationPolicy Never.",
				change.file, change.op.Index, privateKeyOf(change.before).algorithm, privateKeyOf(change.after).algorithm)
		}
	}
}

// Under Never, cert-manager keeps a stored key that another algorithm does not
// match, and waits for a user.
func changesAlgorithmUnderNever(change crWrite) bool {
	before, after := privateKeyOf(change.before), privateKeyOf(change.after)
	return after.rotationPolicy == "Never" && after.algorithm != before.algorithm
}

func TestChangesAlgorithmUnderNeverReadsAnUnsetAlgorithmAsRSA(t *testing.T) {
	key := func(privateKey map[string]any) map[string]any {
		return map[string]any{"spec": map[string]any{"privateKey": privateKey}}
	}
	for _, c := range []struct {
		name          string
		before, after map[string]any
		want          bool
	}{
		{"an unset algorithm, then RSA", key(map[string]any{"rotationPolicy": "Never"}), key(map[string]any{"rotationPolicy": "Never", "algorithm": "RSA"}), false},
		{"an unset algorithm, then ECDSA", key(map[string]any{"rotationPolicy": "Never"}), key(map[string]any{"rotationPolicy": "Never", "algorithm": "ECDSA"}), true},
		{"ECDSA, then RSA into Never", key(map[string]any{"algorithm": "ECDSA"}), key(map[string]any{"rotationPolicy": "Never"}), true},
		{"ECDSA, then RSA out of Never", key(map[string]any{"rotationPolicy": "Never", "algorithm": "ECDSA"}), key(map[string]any{}), false},
	} {
		if got := changesAlgorithmUnderNever(crWrite{before: c.before, after: c.after}); got != c.want {
			t.Errorf("changesAlgorithmUnderNever(%s) = %t, want %t.", c.name, got, c.want)
		}
	}
}

// privateKey is what a Certificate asks of its key, with cert-manager's
// defaults filled in.
type privateKey struct{ rotationPolicy, algorithm string }

func privateKeyOf(certificate map[string]any) privateKey {
	policy, _, _ := unstructured.NestedString(certificate, "spec", "privateKey", "rotationPolicy")
	algorithm, _, _ := unstructured.NestedString(certificate, "spec", "privateKey", "algorithm")
	return privateKey{cmp.Or(policy, "Always"), cmp.Or(algorithm, "RSA")}
}

// certificateChanges are the writes to a Certificate that is there.
func certificateChanges(t *testing.T, sequences ...string) []crWrite {
	t.Helper()
	var changes []crWrite
	for _, write := range crWrites(t, sequences...) {
		if write.before != nil {
			changes = append(changes, write)
		}
	}
	return changes
}

// A recreate soon after a delete can keep the deleted Certificate's key. Once
// the Runner settles, the key is gone.
func TestCRWritesCountADeletedCRAsThereUntilTheRunnerSettles(t *testing.T) {
	certificate := func(name, commonName string) string {
		return `{"apiVersion": "cert-manager.io/v1", "kind": "Certificate", "metadata": {"name": "` + name + `"},
			"spec": {"secretName": "` + name + `-tls", "commonName": "` + commonName + `", "issuerRef": {"name": "selfsigned"}}}`
	}
	// crWrites reads the file by name, not as a pattern.
	file := filepath.Join(t.TempDir(), "sequence[1].json")
	if err := os.WriteFile(file, []byte(`{"seed": 1, "target": "cert-manager", "ops": [
		{"i": 0, "t": "create", "obj": `+certificate("example", "a.test")+`},
		{"i": 1, "t": "create", "obj": `+certificate("example-2", "b.test")+`},
		{"i": 2, "t": "update", "cr": "example-2", "patch": {"spec": {"commonName": "c.test"}}},
		{"i": 3, "t": "delete"},
		{"i": 4, "t": "recreate", "obj": `+certificate("example", "a.test")+`},
		{"i": 5, "t": "recreate", "obj": `+certificate("example", "d.test")+`},
		{"i": 6, "t": "delete", "noSettle": true},
		{"i": 7, "t": "recreate", "noSettle": true, "obj": `+certificate("example", "a.test")+`},
		{"i": 8, "t": "update", "patch": {"spec": {"commonName": "e.test"}}},
		{"i": 9, "t": "delete", "noSettle": true},
		{"i": 10, "t": "settle"},
		{"i": 11, "t": "recreate", "obj": `+certificate("example", "a.test")+`}]}`), 0o644); err != nil {
		t.Fatal(err)
	}
	commonName := func(certificate map[string]any) string {
		name, _, _ := unstructured.NestedString(certificate, "spec", "commonName")
		return cmp.Or(name, "none")
	}
	var writes []string
	for _, write := range crWrites(t, file) {
		writes = append(writes, strconv.Itoa(write.op.Index)+": "+commonName(write.before)+" to "+commonName(write.after))
	}
	want := []string{"0: none to a.test", "1: none to b.test", "2: b.test to c.test", "4: none to a.test",
		"5: a.test to d.test", "7: d.test to a.test", "8: a.test to e.test", "11: none to a.test"}
	if !slices.Equal(writes, want) {
		t.Errorf("crWrites finds\n%s\nwant\n%s", strings.Join(writes, "\n"), strings.Join(want, "\n"))
	}
	var changed []int
	for _, change := range certificateChanges(t, file) {
		changed = append(changed, change.op.Index)
	}
	if want := []int{2, 5, 7, 8}; !slices.Equal(changed, want) {
		t.Errorf("certificateChanges finds ops %v, want %v.", changed, want)
	}
}

// crWrite is a create, update or recreate of a CR, and the CR before and
// after it. A deleted CR counts as there until the Runner settles. Before is
// nil where the CR is not there.
type crWrite struct {
	file          string
	target        *target.Target
	op            run.Op
	before, after map[string]any
}

// glob returns the sequences that patterns match, each of which matches one
// or more.
func glob(t *testing.T, patterns ...string) []string {
	t.Helper()
	var sequences []string
	for _, pattern := range patterns {
		files, err := filepath.Glob(pattern)
		if err != nil || len(files) == 0 {
			t.Fatalf("%s matches no sequence: %v", pattern, err)
		}
		sequences = append(sequences, files...)
	}
	return sequences
}

// crWrites replays the CRs of the sequences.
func crWrites(t *testing.T, sequences ...string) []crWrite {
	t.Helper()
	targets := map[string]*target.Target{}
	var writes []crWrite
	for _, file := range sequences {
		sequence, err := run.ReadSequence(file)
		if err != nil {
			t.Fatal(err)
		}
		declared := targets[sequence.Target]
		if declared == nil {
			declared = loadTarget(t, filepath.Join("../../examples", sequence.Target, "target.yaml"))
			targets[sequence.Target] = declared
		}
		// unsettled holds each CR a delete removed since the Runner last
		// settled.
		crs, unsettled := map[string]map[string]any{}, map[string]map[string]any{}
		for _, op := range sequence.Ops {
			name := cmp.Or(op.CR, declared.Sample.GetName())
			if op.Type == run.OpCreate {
				name = op.Obj.GetName()
			}
			before := crs[name]
			if before == nil {
				before = unsettled[name]
			}
			switch op.Type {
			case run.OpCreate, run.OpRecreate:
				crs[name] = op.Obj.Object
			case run.OpUpdate:
				crs[name] = run.MergePatch(runtime.DeepCopyJSON(before), op.Patch)
			case run.OpDelete:
				unsettled[name] = crs[name]
				delete(crs, name)
			}
			if op.Settles() {
				clear(unsettled)
			}
			if op.Type.OnCR() && op.Type != run.OpDelete {
				writes = append(writes, crWrite{file, declared, op, before, crs[name]})
			}
		}
	}
	return writes
}

// The Runner stops at an op whose CR the API server refuses, and the API
// server drops an unknown metadata field and a field the CRD does not declare.
// No unit test runs these sequences against an API server: the example tiers
// and make hunt-<example> run an example's, and nothing runs a finding's.
func TestSequencesOnDiskWriteCRsTheirCRDAccepts(t *testing.T) {
	rules := map[*target.Target]*crdRules{}
	for _, write := range crWrites(t, sequencesOnDisk(t)...) {
		if rules[write.target] == nil {
			schema, err := openAPISchema(write.target)
			if err != nil {
				t.Fatal(err)
			}
			if rules[write.target], err = newCRDRules(schema); err != nil {
				t.Fatal(err)
			}
		}
		if err := rules[write.target].refusal(write.after, nil); err != nil {
			t.Errorf("%s op %d writes a CR the CRD refuses: %v", write.file, write.op.Index, err)
		}
		dropped, err := droppedFields(rules[write.target].structural, write.after)
		if err != nil {
			t.Errorf("%s op %d writes metadata the API server refuses: %v", write.file, write.op.Index, err)
		}
		if len(dropped) > 0 {
			t.Errorf("%s op %d writes %v, which the API server drops.", write.file, write.op.Index, dropped)
		}
	}
}

// sequencesOnDisk are each example's sequences, in any directory under its
// sequences, and each finding's.
func sequencesOnDisk(t *testing.T) []string {
	t.Helper()
	examples, err := filepath.Glob("../../examples/*/sequences")
	if err != nil || len(examples) == 0 {
		t.Fatalf("found example sequences %v: %v", examples, err)
	}
	var files []string
	for _, dir := range examples {
		found := len(files)
		err := filepath.WalkDir(dir, func(file string, _ fs.DirEntry, err error) error {
			if err == nil && strings.HasSuffix(file, ".json") {
				files = append(files, file)
			}
			return err
		})
		if err != nil {
			t.Fatal(err)
		}
		if len(files) == found {
			t.Fatalf("%s holds no sequence", dir)
		}
	}
	findings, err := filepath.Glob("../../docs/findings/*/sequence.json")
	if err != nil || len(findings) == 0 {
		t.Fatalf("found finding sequences %v: %v; with the last findings draft, delete this glob and return files", findings, err)
	}
	return append(files, findings...)
}

// droppedFields are the unknown metadata fields of cr and the fields its CRD
// does not declare. The error says why the API server refuses its metadata.
func droppedFields(structural *structuralschema.Structural, cr map[string]any) ([]string, error) {
	_, _, dropped, err := objectmeta.GetObjectMetaWithOptions(cr, objectmeta.ObjectMetaOptions{ReturnUnknownFieldPaths: true})
	return append(dropped, pruning.PruneWithOptions(runtime.DeepCopyJSON(cr), structural, true,
		structuralschema.UnknownFieldPathOptions{TrackUnknownFieldPaths: true})...), err
}

func TestDroppedFieldsAreWhatTheAPIServerDrops(t *testing.T) {
	schema, err := openAPISchema(loadTarget(t, certManagerTarget))
	if err != nil {
		t.Fatal(err)
	}
	rules, err := newCRDRules(schema)
	if err != nil {
		t.Fatal(err)
	}
	certificate := map[string]any{"apiVersion": "cert-manager.io/v1", "kind": "Certificate",
		"metadata": map[string]any{"name": "example", "labelz": map[string]any{}},
		"spec":     map[string]any{"secretName": "example-tls", "colour": "blue"}}
	if got, err := droppedFields(rules.structural, certificate); err != nil || !slices.Equal(got, []string{"metadata.labelz", "spec.colour"}) {
		t.Errorf("droppedFields = %v, %v; want [metadata.labelz spec.colour]", got, err)
	}
	certificate["metadata"] = map[string]any{"name": "example", "labels": []any{"a"}}
	if _, err := droppedFields(rules.structural, certificate); err == nil {
		t.Error("droppedFields took labels that are a list")
	}
}

func TestTheToysSeed2DrawsMoreThanTheShrunkB2Reproducer(t *testing.T) {
	// The envtest tier shrinks what seed 2 draws to three ops or fewer.
	if ops := drawOps(t, file(toyTarget), 2); len(ops) <= 3 {
		t.Errorf("Seed 2 draws %v, which leaves the shrink pass nothing to do.", opTypes(ops))
	}
}

func TestTheToyWithALabelFixtureDrawsB14sReproducerAtSeed41(t *testing.T) {
	// TestGeneratedFixtureOps finds B14 with it.
	if ops := drawOps(t, toyWithALabelFixture, 41); !revealsB14(ops) {
		t.Errorf("Seed 41 draws %v, want an updateFixture under a settled Widget, then only settles, then a restart.", opTypes(ops))
	}
}

func TestOnlyARestartNextAfterTheLabelChangesUnderASettledWidgetRevealsB14(t *testing.T) {
	create, unsettledCreate := run.Op{Type: run.OpCreate}, run.Op{Type: run.OpCreate, NoSettle: true}
	label, settle, restart := run.Op{Type: run.OpUpdateFixture}, run.Op{Type: run.OpSettle}, run.Op{Type: run.OpRestart}
	for _, test := range []struct {
		name string
		ops  []run.Op
		want bool
	}{
		{"a settled create", []run.Op{create, label, settle, restart}, true},
		{"a create a settle follows", []run.Op{unsettledCreate, settle, label, restart}, true},
		{"a recreate", []run.Op{create, {Type: run.OpRecreate}, label, restart}, true},
		{"an update after the label", []run.Op{create, label, {Type: run.OpUpdate}, restart}, false},
		{"a deleteFixture after the label", []run.Op{create, label, {Type: run.OpDeleteFixture}, restart}, false},
		{"a delete", []run.Op{create, {Type: run.OpDelete}, label, restart}, false},
		{"no Widget", []run.Op{label, settle, restart}, false},
		{"a create that has not settled", []run.Op{unsettledCreate, label, settle, restart}, false},
		{"an update that has not settled", []run.Op{create, {Type: run.OpUpdate, NoSettle: true}, label, settle, restart}, false},
	} {
		if got := revealsB14(test.ops); got != test.want {
			t.Errorf("With %s, revealsB14(%v) = %t, want %t.", test.name, opTypes(test.ops), got, test.want)
		}
	}
}

// revealsB14 says whether the label changes once the Widget's last CR op has
// settled, and a restart is the next op to reconcile it.
func revealsB14(ops []run.Op) bool {
	var exists, unsettled bool
	for i, op := range ops {
		switch {
		case op.Type.OnCR():
			exists, unsettled = op.Type != run.OpDelete, op.NoSettle
		case op.Type == run.OpSettle:
			unsettled = false
		}
		next := slices.IndexFunc(ops[i+1:], func(op run.Op) bool { return op.Type != run.OpSettle })
		if op.Type == run.OpUpdateFixture && exists && !unsettled && next >= 0 && ops[i+1+next].Type == run.OpRestart {
			return true
		}
	}
	return false
}

func drawOps(t *testing.T, declare declaration, seed int64) []run.Op {
	t.Helper()
	sequence, err := newGenerator(t, declare(t), Options{}).Draw(seed)
	if err != nil {
		t.Fatalf("Draw(%d) failed: %v.", seed, err)
	}
	return sequence.Ops
}

func opTypes(ops []run.Op) []run.OpType {
	var types []run.OpType
	for _, op := range ops {
		types = append(types, op.Type)
	}
	return types
}
