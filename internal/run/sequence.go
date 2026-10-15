package run

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path"
	"slices"
	"strings"
	"time"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"

	"github.com/rosenhouse-bot-sandbox/reconciler-fuzzer/internal/observe"
	"github.com/rosenhouse-bot-sandbox/reconciler-fuzzer/internal/proxy"
	"github.com/rosenhouse-bot-sandbox/reconciler-fuzzer/internal/target"
)

// Sequence is one test sequence, as a sequence file holds it. It is the unit
// of generation, replay and shrinking.
type Sequence struct {
	Seed   int64  `json:"seed"`
	Target string `json:"target"`
	Ops    []Op   `json:"ops"`
}

// Op is one step of a sequence. Marshalling follows the field order below, so
// a sequence read from disk and written back is byte-identical.
type Op struct {
	// Index is the op's position in the sequence.
	Index int    `json:"i"`
	Type  OpType `json:"t"`
	// CR names the CR update, delete and recreate act on. Empty names the
	// sample's.
	CR string `json:"cr,omitempty"`
	// Kind and Nth select deleteManaged's object: the Nth managed object of
	// Kind, ordered by creationTimestamp then name. Kind and Name select a
	// fixture op's fixture.
	Kind string `json:"kind,omitempty"`
	Nth  *int   `json:"index,omitempty"`
	Name string `json:"name,omitempty"`
	// Obj is what create and recreate create.
	Obj *unstructured.Unstructured `json:"obj,omitempty"`
	// Patch is the JSON merge patch (RFC 7386) update and updateFixture apply.
	Patch map[string]any `json:"patch,omitempty"`
	// Fault is the fault a fault op injects.
	Fault *Fault `json:"spec,omitempty"`
	// Until is where reconciler-fuzzer creates the fixture of a deleteFixture op
	// again.
	Until *Until `json:"until,omitempty"`
	// NoSettle skips the Runner's implicit settle wait.
	NoSettle bool `json:"noSettle,omitempty"`
}

// Until names the op before which reconciler-fuzzer creates a deleted fixture
// again, as it last wrote it.
type Until struct {
	Op int `json:"op"`
}

// OpType is an op's "t".
type OpType string

const (
	OpCreate        OpType = "create"
	OpUpdate        OpType = "update"
	OpDelete        OpType = "delete"
	OpRecreate      OpType = "recreate"
	OpSettle        OpType = "settle"
	OpRestart       OpType = "restart"
	OpFault         OpType = "fault"
	OpDeleteManaged OpType = "deleteManaged"
	OpUpdateFixture OpType = "updateFixture"
	OpDeleteFixture OpType = "deleteFixture"
)

// opTypes are every op type the format defines.
var opTypes = []OpType{OpCreate, OpUpdate, OpDelete, OpRecreate, OpSettle, OpRestart, OpFault, OpDeleteManaged,
	OpUpdateFixture, OpDeleteFixture}

// crOps act on the primary CR and may carry noSettle.
var crOps = []OpType{OpCreate, OpUpdate, OpDelete, OpRecreate}

// OnCR reports whether the op type acts on the primary CR.
func (t OpType) OnCR() bool { return slices.Contains(crOps, t) }

// Fault is what a fault op injects.
type Fault struct {
	Match  Match   `json:"match,omitzero"`
	Action Action  `json:"action"`
	Until  Trigger `json:"until,omitzero"`
}

// Match selects the requests a fault applies to. An unset field matches every
// request.
type Match struct {
	Verb     string   `json:"verb,omitempty"`
	Resource string   `json:"resource,omitempty"`
	Name     string   `json:"name,omitempty"`
	Fraction *float64 `json:"fraction,omitempty"`
}

// Action is what the proxy does to a matched request. Exactly one field is set.
type Action struct {
	Error int      `json:"error,omitempty"`
	Delay Duration `json:"delay,omitempty"`
	Drop  bool     `json:"drop,omitempty"`
}

// Trigger ends a fault at an op index, once it has applied to a count of
// requests, or after a duration.
type Trigger struct {
	Op    *int     `json:"op,omitempty"`
	Count int      `json:"count,omitempty"`
	For   Duration `json:"for,omitempty"`
}

// Duration marshals as a Go duration string, as the rest of the harness's
// configuration does.
type Duration time.Duration

func (d Duration) MarshalJSON() ([]byte, error) {
	return json.Marshal(time.Duration(d).String())
}

func (d *Duration) UnmarshalJSON(data []byte) error {
	var text string
	if err := json.Unmarshal(data, &text); err != nil {
		return err
	}
	parsed, err := time.ParseDuration(text)
	if err != nil {
		return err
	}
	*d = Duration(parsed)
	return nil
}

// Settles reports whether the Runner waits for convergence after the op. A
// settle op is the wait itself.
func (o Op) Settles() bool {
	return o.Type == OpSettle || (!o.NoSettle && slices.Contains(mutatingOps, o.Type))
}

// mutatingOps change the CR or a managed object, or update a fixture, so the
// Runner settles after them.
var mutatingOps = append(slices.Clone(crOps), OpDeleteManaged, OpUpdateFixture)

// ReadSequence reads and validates a sequence file.
func ReadSequence(path string) (Sequence, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return Sequence{}, fmt.Errorf("reading the sequence: %w", err)
	}
	sequence, err := UnmarshalSequence(data)
	if err != nil {
		return Sequence{}, fmt.Errorf("reading the sequence %s: %w", path, err)
	}
	return sequence, nil
}

// WriteSequence writes the sequence in its canonical form.
func WriteSequence(path string, s Sequence) error {
	data, err := s.Marshal()
	if err != nil {
		return err
	}
	if err := os.WriteFile(path, data, 0o644); err != nil {
		return fmt.Errorf("writing the sequence: %w", err)
	}
	return nil
}

// UnmarshalSequence decodes a sequence and validates every op. An unknown op
// type, an unknown field and an op missing what its type needs are all
// configuration errors.
func UnmarshalSequence(data []byte) (Sequence, error) {
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	var sequence Sequence
	if err := decoder.Decode(&sequence); err != nil {
		return Sequence{}, fmt.Errorf("the sequence does not parse: %w", err)
	}
	if err := sequence.validate(zeroTriggers(data)); err != nil {
		return Sequence{}, err
	}
	return sequence, nil
}

// zeroTriggers refuses each op whose until.count or until.for is written as 0,
// which a Trigger reads as no trigger.
func zeroTriggers(data []byte) map[int]error {
	var written struct {
		Ops []struct {
			Spec struct {
				Until struct {
					Count *int      `json:"count"`
					For   *Duration `json:"for"`
				} `json:"until"`
			} `json:"spec"`
		} `json:"ops"`
	}
	// data decoded as a Sequence, so it decodes here too.
	_ = json.NewDecoder(bytes.NewReader(data)).Decode(&written)
	refused := map[int]error{}
	for i, op := range written.Ops {
		switch until := op.Spec.Until; {
		case until.Count != nil && *until.Count == 0:
			refused[i] = errors.New("until.count is 0; give a count above 0, or leave it out")
		case until.For != nil && *until.For == 0:
			refused[i] = errors.New("until.for is 0s; give a duration above 0, or leave it out")
		}
	}
	return refused
}

// Marshal returns the sequence's canonical form: JSON indented two spaces and
// newline-terminated.
func (s Sequence) Marshal() ([]byte, error) {
	data, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("encoding the sequence: %w", err)
	}
	return append(data, '\n'), nil
}

// Validate reports the first malformed op, or a sequence that ends while the
// target is still working.
func (s Sequence) Validate() error { return s.validate(nil) }

// validate is Validate that also refuses each well-formed op refused holds an
// error for.
func (s Sequence) validate(refused map[int]error) error {
	for i, op := range s.Ops {
		err := op.validate(i)
		if err == nil {
			err = refused[i]
		}
		if err != nil {
			return fmt.Errorf("op %d: %w", i, err)
		}
	}
	// The teardown's quiet window is the only one such a sequence would be
	// judged on, and the teardown waits for convergence before it opens that
	// window only after a fault.
	if len(s.Ops) == 0 {
		return fmt.Errorf("the sequence holds no ops; want at least the create it opens with")
	}
	if !s.Ops[len(s.Ops)-1].Settles() {
		return fmt.Errorf("the sequence does not end with an op that settles")
	}
	return s.validateFixtures()
}

// fixture names a fixture op's fixture by kind and name.
func (o Op) fixture() string { return o.Kind + " " + o.Name }

// validateFixtures reports a deleted fixture that does not come back by the
// last op, and an op on a fixture while it is deleted.
func (s Sequence) validateFixtures() error {
	deletedBy := map[string]Op{}
	for i, op := range s.Ops {
		if op.Type != OpUpdateFixture && op.Type != OpDeleteFixture {
			continue
		}
		fixture := op.fixture()
		if deleted, gone := deletedBy[fixture]; gone && i < deleted.Until.Op {
			return fmt.Errorf("op %d acts on the fixture %s, which op %d deleted until op %d", i, fixture, deleted.Index, deleted.Until.Op)
		}
		if op.Type != OpDeleteFixture {
			continue
		}
		if op.Until.Op <= i || op.Until.Op >= len(s.Ops) {
			return fmt.Errorf("op %d: until names op %d; want an op after it, up to the last", i, op.Until.Op)
		}
		deletedBy[fixture] = op
	}
	return nil
}

func (o Op) validate(position int) error {
	if !slices.Contains(opTypes, o.Type) {
		return fmt.Errorf("%q is not an op type; want one of %v", o.Type, opTypes)
	}
	if o.Index != position {
		return fmt.Errorf("i is %d, want the op's position %d", o.Index, position)
	}
	if o.NoSettle && !slices.Contains(crOps, o.Type) {
		return fmt.Errorf("only a CR op carries noSettle; %q is not one", o.Type)
	}
	if err := o.validateFields(); err != nil {
		return err
	}
	if o.Type == OpFault {
		return o.Fault.validate(position)
	}
	return nil
}

// opFields are the fields an op may carry, in the order errors report them.
var opFields = []string{"obj", "patch", "spec", "kind", "name", "index", "until"}

// validateFields reports a field the op's type does not take, or one it needs
// and does not carry.
func (o Op) validateFields() error {
	carried := map[string]bool{
		"obj":   o.Obj != nil,
		"patch": o.Patch != nil,
		"spec":  o.Fault != nil,
		"kind":  o.Kind != "",
		"name":  o.Name != "",
		"index": o.Nth != nil,
		"until": o.Until != nil,
	}
	wanted := fieldsOf(o.Type)
	var needed []string
	for _, field := range opFields {
		if wanted[field] {
			needed = append(needed, field)
		}
	}
	for _, field := range opFields {
		if carried[field] == wanted[field] {
			continue
		}
		if wanted[field] {
			return fmt.Errorf("%s needs %s", o.Type.withArticle(), field)
		}
		if len(needed) == 0 {
			return fmt.Errorf("%s takes no %s", o.Type.withArticle(), field)
		}
		return fmt.Errorf("%s takes no %s; it needs %s", o.Type.withArticle(), field, inWords(needed))
	}
	if o.Type == OpDeleteManaged && *o.Nth < 0 {
		return fmt.Errorf("index is %d, want the position of a managed object", *o.Nth)
	}
	if o.CR != "" && !slices.Contains(namingOps, o.Type) {
		return fmt.Errorf("%s takes no cr", o.Type.withArticle())
	}
	return nil
}

// withArticle names the op type as a sentence does: "a create op", "an update op".
func (t OpType) withArticle() string {
	if strings.IndexAny(string(t), "aeiou") == 0 {
		return "an " + string(t) + " op"
	}
	return "a " + string(t) + " op"
}

// inWords joins words as a sentence lists them: "a", "a and b", "a, b and c".
func inWords(words []string) string {
	last := len(words) - 1
	if last == 0 {
		return words[0]
	}
	return strings.Join(words[:last], ", ") + " and " + words[last]
}

// namingOps act on a CR an earlier op created, which cr names.
var namingOps = []OpType{OpUpdate, OpDelete, OpRecreate}

// checkCRs reports a CR with no name, an op on a CR that is not there, a
// recreate that creates another CR, and a create of a CR still there. An op
// that names no CR acts on the sample's.
func (s Sequence) checkCRs(sample string) error {
	// live holds the op that created each CR no op has deleted since, and
	// deleted the op that last deleted each other CR.
	live, deleted := map[string]int{}, map[string]int{}
	for _, op := range s.Ops {
		if !op.Type.OnCR() {
			continue
		}
		name := op.crName(sample)
		creator, isLive := live[name]
		deleter, wasDeleted := deleted[name]
		switch {
		case op.Obj != nil && op.Obj.GetName() == "":
			return fmt.Errorf("op %d (%s) writes a CR with no metadata.name; give it one, since ops name the CR they act on", op.Index, op.Type)
		case op.Type == OpCreate && isLive:
			return fmt.Errorf("op %d (create) creates the CR %s, which op %d created and no op since deleted", op.Index, name, creator)
		case op.Type == OpCreate && wasDeleted && !slices.ContainsFunc(s.Ops[deleter:op.Index], Op.Settles):
			return fmt.Errorf("op %d (create) creates the CR %s, which may still be there: op %d deleted it with noSettle and no op since settles; "+
				"use a recreate, which waits for it to go, or put a settle op before the create", op.Index, name, deleter)
		case op.Type == OpCreate:
		case !isLive && !wasDeleted && op.CR == "":
			return fmt.Errorf("op %d (%s) names no cr, so it acts on the sample's %s, which no op before it creates", op.Index, op.Type, name)
		case !isLive && !wasDeleted:
			return fmt.Errorf("op %d (%s) acts on the CR %s, which no op before it creates", op.Index, op.Type, name)
		case op.Type == OpRecreate && op.Obj.GetName() != name:
			return fmt.Errorf("op %d (recreate) acts on the CR %s and creates %s; a recreate creates the CR it deletes", op.Index, name, op.Obj.GetName())
		case op.Type != OpRecreate && !isLive:
			return fmt.Errorf("op %d (%s) acts on the CR %s, which op %d deleted", op.Index, op.Type, name, deleter)
		}
		switch op.Type {
		case OpCreate, OpRecreate:
			live[name] = op.Index
		case OpDelete:
			delete(live, name)
			deleted[name] = op.Index
		}
	}
	return nil
}

// crName is the name of the CR a CR op acts on, where sample is the sample's.
func (o Op) crName(sample string) string {
	switch {
	case o.Type == OpCreate:
		return o.Obj.GetName()
	case o.CR == "":
		return sample
	}
	return o.CR
}

// fieldsOf says which fields an op type carries.
func fieldsOf(opType OpType) map[string]bool {
	fields := map[string]bool{}
	switch opType {
	case OpCreate, OpRecreate:
		fields["obj"] = true
	case OpUpdate:
		fields["patch"] = true
	case OpFault:
		fields["spec"] = true
	case OpDeleteManaged:
		fields["kind"], fields["index"] = true, true
	case OpUpdateFixture:
		fields["kind"], fields["name"], fields["patch"] = true, true, true
	case OpDeleteFixture:
		fields["kind"], fields["name"], fields["until"] = true, true, true
	}
	return fields
}

func (f *Fault) validate(position int) error {
	actions := 0
	for _, set := range []bool{f.Action.Error != 0, f.Action.Delay != 0, f.Action.Drop} {
		if set {
			actions++
		}
	}
	if actions != 1 {
		return fmt.Errorf("the fault carries %d actions, want exactly one of error, delay or drop", actions)
	}
	if verb := f.Match.Verb; verb != "" && !slices.Contains(proxy.Verbs, verb) {
		return fmt.Errorf("match.verb %q is not one of %s", verb, inWords(proxy.Verbs))
	}
	if strings.Contains(f.Match.Resource, "/") {
		return fmt.Errorf("match.resource %q holds a slash; name the plural alone, such as configmaps. "+
			"A fault on a resource matches its subresources' requests too", f.Match.Resource)
	}
	if _, err := path.Match(f.Match.Name, ""); err != nil {
		return fmt.Errorf("match.name %q is not a glob: %w", f.Match.Name, err)
	}
	if fraction := f.Match.Fraction; fraction != nil && (*fraction <= 0 || *fraction > 1) {
		return fmt.Errorf("match.fraction %v is not a share of requests above 0 and up to 1", *fraction)
	}
	if code := f.Action.Error; code != 0 && (code < 400 || code > 599) {
		return fmt.Errorf("action.error %d is not an HTTP error status from 400 to 599", code)
	}
	switch {
	case f.Until.Op != nil && *f.Until.Op <= position:
		return fmt.Errorf("until.op names op %d; want an op after the fault", *f.Until.Op)
	case f.Action.Delay < 0:
		return fmt.Errorf("action.delay %s is negative", time.Duration(f.Action.Delay))
	case f.Until.Count < 0:
		return fmt.Errorf("until.count %d is negative", f.Until.Count)
	case f.Until.For < 0:
		return fmt.Errorf("until.for %s is negative", time.Duration(f.Until.For))
	}
	return nil
}

// managedKind resolves a deleteManaged op's kind against what the target
// declares it manages.
func managedKind(t *target.Target, declared string) (schema.GroupVersionKind, error) {
	for _, gvk := range t.Manages {
		if observe.KindName(gvk) == declared {
			return gvk, nil
		}
	}
	return schema.GroupVersionKind{}, fmt.Errorf("the target manages no kind %q", declared)
}
