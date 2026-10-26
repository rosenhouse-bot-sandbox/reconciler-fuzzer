// Package plan holds the steps of one run as plain data that plan.json stores.
package plan

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"time"

	"k8s.io/apimachinery/pkg/runtime/schema"
)

// Plan is the list of steps for one run.
type Plan struct {
	Seed  uint64 `json:"seed,omitempty"`
	Steps []Step `json:"steps"`
}

// Step is one unit of a run. Its fault period lasts Period, or until its
// last action ends if that is later. Its faults last as long, unless a
// create's or a stop's wait for a deleted CR ends them sooner.
type Step struct {
	Period  Duration `json:"period,omitempty"`
	Actions []Action `json:"actions,omitempty"`
	Faults  []Fault  `json:"faults,omitempty"`
}

// Do names what an action does.
type Do string

const (
	// Create creates Object, or the sample when Object is empty. A create of
	// a slot whose CR is still terminating waits until the name is free. Once
	// Period has passed, the wait ends the step's faults and restarts a killed
	// controller. It gives up as long after the faults end, or after the
	// slot's delete if that is later, as the step's deadline allows.
	Create Do = "create"
	// Update merge-patches the slot's CR with Patch.
	Update Do = "update"
	// Delete deletes the slot's CR.
	Delete Do = "delete"
	// Restart applies the Upgrade's CRDs, stops the controller with SIGTERM,
	// and starts it again with the Upgrade's binary and args.
	Restart Do = "restart"
	// Kill sends SIGKILL. The controller stays down until the fault period
	// ends, or until a create's or a stop's wait for a deleted CR restarts
	// it.
	Kill Do = "kill"
	// Damage deletes the object Target selects among those the controller
	// created, or merge-patches it with Patch when set. The harness never
	// undoes it.
	Damage Do = "damage"
	// ChangeFixture merge-patches the fixture Target names with Patch, or
	// deletes it when Patch is empty.
	ChangeFixture Do = "changeFixture"
	// Stop waits until the CRs that deletes removed are gone, as a create's
	// wait for its slot does, and then stops the controller with SIGTERM for
	// good. A CR is gone once its name is free or another CR holds it. After
	// a stop, a plan's only actions are deletes and fixture changes, and the
	// runner appends no steps.
	Stop Do = "stop"
)

// Action is something the harness does once in a step, At after the fault
// period starts.
type Action struct {
	At     Duration        `json:"at,omitempty"`
	Do     Do              `json:"do"`
	Slot   string          `json:"slot,omitempty"`
	Object json.RawMessage `json:"object,omitempty"`
	Patch  json.RawMessage `json:"patch,omitempty"`
	Target *Target         `json:"target,omitempty"`
	// Propagation is a delete's propagation policy. Empty is the server's
	// default.
	Propagation Propagation `json:"propagation,omitempty"`
	// Upgrade is a restart's.
	Upgrade
}

// Propagation is a delete's propagation policy.
type Propagation string

// Foreground deletes the CR's blocking dependents before the CR.
const Foreground Propagation = "Foreground"

// Target selects one object for damage or changeFixture. It is resolved
// when the action runs. An empty field matches any value. Resource is
// required on kube.
type Target struct {
	// System defaults to "kube".
	System   string `json:"system,omitempty"`
	Group    string `json:"group,omitempty"`
	Resource string `json:"resource"`
	// Namespace is empty for any namespace, cluster scope included.
	Namespace string `json:"namespace,omitempty"`
	Name      string `json:"name,omitempty"`
	Role      string `json:"role,omitempty"`
	// Nth picks the nth match in creation order, counting from 1. Zero
	// picks the first.
	Nth int `json:"nth,omitempty"`
}

// Fault starts with the fault period and ends with it, or when a create's
// or a stop's wait for a deleted CR ends it. A request fault acts on one
// system's controller listener. A model fault, Stuck or SlowDelete, acts on
// the stage model of the kind Match names, or on a fake service. Exactly one
// effect is set.
type Fault struct {
	// System defaults to "kube".
	System string `json:"system,omitempty"`
	Match  Match  `json:"match"`
	// Nth picks the nth matching request of the fault period, counting from
	// 1. Zero picks every matching request.
	Nth   int       `json:"nth,omitempty"`
	Error *Error    `json:"error,omitempty"`
	Delay *Duration `json:"delay,omitempty"`
	Drop  bool      `json:"drop,omitempty"`
	// DropResponse forwards the request, lets the server commit it, and
	// aborts the connection without a response.
	DropResponse bool `json:"dropResponse,omitempty"`
	// KillAfter forwards the request, kills the controller once the server
	// has answered, and then aborts the connection.
	KillAfter bool `json:"killAfter,omitempty"`
	// Stuck keeps each matching object not ready.
	Stuck bool `json:"stuck,omitempty"`
	// SlowDelete holds each matching object's deletion until the fault ends.
	SlowDelete bool `json:"slowDelete,omitempty"`
	// HoldWatch pauses the controller's watch events of the kind Match's
	// group and resource name, and delivers them in order when it ends.
	HoldWatch bool `json:"holdWatch,omitempty"`
}

// ModelFault reports whether a stage model, not a listener, takes f.
func (f Fault) ModelFault() bool { return f.Stuck || f.SlowDelete }

// Match selects requests. An empty field matches any value, but a
// holdWatch's empty Group names the core group. Subresource may also be
// NoSubresource.
type Match struct {
	Verb        string `json:"verb,omitempty"`
	Group       string `json:"group,omitempty"`
	Resource    string `json:"resource,omitempty"`
	Subresource string `json:"subresource,omitempty"`
	Name        string `json:"name,omitempty"`
}

// NoSubresource as a Match's Subresource matches only a request without a
// subresource.
const NoSubresource = "-"

// Error answers a request with a Status of Code.
type Error struct {
	Code       int      `json:"code"`
	RetryAfter Duration `json:"retryAfter,omitempty"`
}

// Duration marshals as a string such as "1.5s".
type Duration time.Duration

func (d Duration) MarshalJSON() ([]byte, error) { return json.Marshal(time.Duration(d).String()) }

func (d *Duration) UnmarshalJSON(data []byte) error {
	if string(data) == "null" {
		return nil
	}
	var s string
	if err := json.Unmarshal(data, &s); err != nil {
		return fmt.Errorf("a duration is a string such as \"1.5s\", not %s", data)
	}
	parsed, err := time.ParseDuration(s)
	if err != nil {
		return err
	}
	*d = Duration(parsed)
	return nil
}

// Load reads plan.json strictly and validates it. It resolves each
// restart's binary and crds against the file's directory.
func Load(path string) (*Plan, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	var p Plan
	if err := decoder.Decode(&p); err != nil {
		return nil, fmt.Errorf("loading plan %s: %w", path, err)
	}
	if decoder.More() {
		return nil, fmt.Errorf("loading plan %s: data after the plan", path)
	}
	if err := p.Validate(); err != nil {
		return nil, fmt.Errorf("loading plan %s: %w", path, err)
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return nil, fmt.Errorf("loading plan %s: %w", path, err)
	}
	p.resolve(filepath.Dir(abs))
	return &p, nil
}

// resolve joins each relative binary and crds path to dir.
func (p *Plan) resolve(dir string) {
	for i := range p.Steps {
		for j := range p.Steps[i].Actions {
			u := &p.Steps[i].Actions[j].Upgrade
			if u.Binary != "" {
				u.Binary = under(dir, u.Binary)
			}
			for k, path := range u.CRDs {
				u.CRDs[k] = under(dir, path)
			}
		}
	}
}

// under joins path to dir unless path is absolute.
func under(dir, path string) string {
	if filepath.IsAbs(path) {
		return path
	}
	return filepath.Join(dir, path)
}

// Save writes p as indented JSON.
func Save(path string, p *Plan) error {
	data, err := json.MarshalIndent(p, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, append(data, '\n'), 0o644)
}

// Validate reports the first malformed step, action or fault, naming its
// position.
func (p *Plan) Validate() error {
	// names maps each slot that holds a CR to that CR's group, kind,
	// namespace and name, which is sampleName for the sample and empty for a
	// generated name. A slot an earlier delete emptied maps to emptied.
	names := map[string]string{}
	stopped := false
	for i, step := range p.Steps {
		if step.Period < 0 {
			return fmt.Errorf("steps[%d]: period is negative", i)
		}
		for j, a := range step.Actions {
			if err := a.validate(names); err != nil {
				return fmt.Errorf("steps[%d].actions[%d]: %w", i, j, err)
			}
			if stopped && a.Do != Delete && a.Do != ChangeFixture {
				return fmt.Errorf("steps[%d].actions[%d]: %s after a stop", i, j, a.Do)
			}
			stopped = stopped || a.Do == Stop
		}
		for k, f := range step.Faults {
			if err := f.Validate(); err != nil {
				return fmt.Errorf("steps[%d].faults[%d]: %w", i, k, err)
			}
		}
	}
	return nil
}

// ValidateKinds reports the first create whose object lacks apiVersion or
// kind, or has a group and kind that listed lacks. A create without an
// object makes the sample, of listed[0].
func (p *Plan) ValidateKinds(listed []schema.GroupKind) error {
	for i, step := range p.Steps {
		for j, a := range step.Actions {
			if a.Do != Create || a.Object == nil {
				continue
			}
			o, err := decodeHeader(a.Object)
			switch {
			case err != nil:
				return fmt.Errorf("steps[%d].actions[%d]: %w", i, j, err)
			case o.APIVersion == "" || o.Kind == "":
				return fmt.Errorf("steps[%d].actions[%d]: create of slot %q makes an object without apiVersion or kind", i, j, a.Slot)
			case !slices.Contains(listed, o.groupKind()):
				return fmt.Errorf("steps[%d].actions[%d]: create of slot %q makes a %s, which primaries does not list", i, j, a.Slot, o.groupKind())
			}
		}
	}
	return nil
}

const (
	sampleName = "\x00sample"
	emptied    = "\x00emptied"
)

func (a Action) validate(names map[string]string) error {
	if a.At < 0 {
		return errors.New("at is negative")
	}
	if a.Target != nil && a.Do != Damage && a.Do != ChangeFixture {
		return fmt.Errorf("%s takes no target", a.Do)
	}
	if (a.Binary != "" || a.Args != nil || a.CRDs != nil) && a.Do != Restart {
		return fmt.Errorf("%s takes no binary, args or crds", a.Do)
	}
	switch {
	case a.Propagation != "" && a.Do != Delete:
		return fmt.Errorf("%s takes no propagation", a.Do)
	case a.Propagation != "" && a.Propagation != Foreground:
		return fmt.Errorf("delete propagation %q is not %s", a.Propagation, Foreground)
	}
	switch a.Do {
	case Create:
		if a.Slot == "" {
			return errors.New("create needs a slot")
		}
		name, err := createdName(a.Object)
		if err != nil {
			return err
		}
		for slot, other := range names {
			if slot == a.Slot && other != emptied {
				return fmt.Errorf("create names slot %q, which still holds a CR", a.Slot)
			}
			if slot != a.Slot && name != "" && other == name {
				return fmt.Errorf("create of slot %q gives its CR the name of slot %q's CR", a.Slot, slot)
			}
		}
		names[a.Slot] = name
	case Update, Delete:
		name, made := names[a.Slot]
		if !made {
			return fmt.Errorf("%s names slot %q, which no earlier create made", a.Do, a.Slot)
		}
		if name == emptied {
			return fmt.Errorf("%s names slot %q, which an earlier delete emptied", a.Do, a.Slot)
		}
		if a.Do == Update && !isObject(a.Patch) {
			return errors.New("update needs a JSON object patch")
		}
		if a.Do == Delete {
			names[a.Slot] = emptied
		}
	case Restart, Kill, Stop:
		if a.Slot != "" || a.Object != nil || a.Patch != nil {
			return fmt.Errorf("%s takes no slot, object or patch", a.Do)
		}
	case Damage, ChangeFixture:
		return a.validateChange(names)
	default:
		return fmt.Errorf("unknown do %q", a.Do)
	}
	return nil
}

func (a Action) validateChange(names map[string]string) error {
	if a.Target == nil {
		return fmt.Errorf("%s needs a target with a resource", a.Do)
	}
	if err := validSystem(a.Target.System); err != nil {
		return err
	}
	if onKube(a.Target.System) && a.Target.Resource == "" {
		return fmt.Errorf("%s needs a target with a resource", a.Do)
	}
	if a.Patch != nil && !isObject(a.Patch) {
		return fmt.Errorf("%s needs a JSON object patch", a.Do)
	}
	if a.Do == ChangeFixture {
		if a.Slot != "" || a.Object != nil {
			return errors.New("changeFixture takes no slot or object")
		}
		if a.Target.Name == "" {
			return errors.New("changeFixture needs a target name")
		}
		return nil
	}
	if a.Object != nil {
		return errors.New("damage takes no object")
	}
	if a.Slot == "" {
		return nil
	}
	switch name, made := names[a.Slot]; {
	case !made:
		return fmt.Errorf("damage names slot %q, which no earlier create made", a.Slot)
	case name == emptied:
		return fmt.Errorf("damage names slot %q, which an earlier delete emptied", a.Slot)
	}
	return nil
}

// createdName is the group, kind, namespace and name of object, sampleName
// when object is empty, or empty for a generated name.
func createdName(object json.RawMessage) (string, error) {
	if object == nil {
		return sampleName, nil
	}
	o, err := decodeHeader(object)
	if err != nil {
		return "", err
	}
	if o.Metadata.Name == "" {
		return "", nil
	}
	return o.groupKind().String() + " " + o.Metadata.Namespace + "/" + o.Metadata.Name, nil
}

// header holds the fields that name a create's object.
type header struct {
	APIVersion string `json:"apiVersion"`
	Kind       string `json:"kind"`
	Metadata   struct {
		Namespace string `json:"namespace"`
		Name      string `json:"name"`
	} `json:"metadata"`
}

func decodeHeader(object json.RawMessage) (header, error) {
	var h header
	if err := json.Unmarshal(object, &h); err != nil {
		return header{}, fmt.Errorf("decoding object: %w", err)
	}
	return h, nil
}

func (h header) groupKind() schema.GroupKind {
	return schema.FromAPIVersionAndKind(h.APIVersion, h.Kind).GroupKind()
}

func isObject(raw json.RawMessage) bool {
	var object map[string]any
	return json.Unmarshal(raw, &object) == nil && object != nil
}

// Validate requires exactly one effect, an error code from 400 to 599, a
// system name and a non-negative nth. A model fault takes no verb,
// subresource or nth, and on kube needs a resource. A holdWatch acts on
// kube, needs a resource and takes only a group besides.
func (f Fault) Validate() error {
	effects := 0
	for _, set := range []bool{f.Error != nil, f.Delay != nil, f.Drop, f.DropResponse, f.KillAfter, f.Stuck, f.SlowDelete, f.HoldWatch} {
		if set {
			effects++
		}
	}
	if effects != 1 {
		return fmt.Errorf("a fault sets %d of error, delay, drop, dropResponse, killAfter, stuck, slowDelete and holdWatch, want exactly one", effects)
	}
	if f.HoldWatch {
		switch {
		case !onKube(f.System):
			return errors.New("holdWatch acts only on kube")
		case f.Match.Resource == "":
			return errors.New("holdWatch needs a match with a resource")
		case f.Match.Verb != "" || f.Match.Subresource != "" || f.Match.Name != "" || f.Nth != 0:
			return errors.New("holdWatch takes no verb, subresource, name or nth")
		}
	}
	if f.ModelFault() {
		switch effect := f.Effect(); {
		case onKube(f.System) && f.Match.Resource == "":
			return fmt.Errorf("%s needs a match with a resource", effect)
		case f.Match.Verb != "" || f.Match.Subresource != "":
			return fmt.Errorf("%s matches objects, so it takes no verb or subresource", effect)
		case f.Nth != 0:
			return fmt.Errorf("%s takes no nth", effect)
		}
	}
	if f.Error != nil && (f.Error.Code < 400 || f.Error.Code > 599) {
		return fmt.Errorf("error code %d is not from 400 to 599", f.Error.Code)
	}
	if err := validSystem(f.System); err != nil {
		return err
	}
	if f.Nth < 0 {
		return errors.New("nth is negative")
	}
	return nil
}

var systemName = regexp.MustCompile(`^[a-z][a-z0-9-]*$`)

func validSystem(system string) error {
	if system != "" && !systemName.MatchString(system) {
		return fmt.Errorf("system %q is not a system name", system)
	}
	return nil
}

func onKube(system string) bool { return system == "" || system == "kube" }

// Effect names what the fault does, as a call's injected field does.
func (f Fault) Effect() string {
	switch {
	case f.Error != nil:
		return fmt.Sprintf("error(%d)", f.Error.Code)
	case f.Delay != nil:
		return fmt.Sprintf("delay(%s)", time.Duration(*f.Delay))
	case f.DropResponse:
		return "dropResponse"
	case f.KillAfter:
		return "killAfter"
	case f.Stuck:
		return "stuck"
	case f.SlowDelete:
		return "slowDelete"
	case f.HoldWatch:
		return "holdWatch"
	}
	return "drop"
}

// String is the effect and the match, such as "drop on verb=create".
func (f Fault) String() string {
	var match []string
	for _, field := range []struct{ name, value string }{
		{"system", f.System}, {"verb", f.Match.Verb}, {"group", f.Match.Group}, {"resource", f.Match.Resource},
		{"subresource", f.Match.Subresource}, {"name", f.Match.Name},
	} {
		if field.value != "" {
			match = append(match, field.name+"="+field.value)
		}
	}
	if f.Nth > 0 {
		match = append(match, fmt.Sprintf("nth=%d", f.Nth))
	}
	if len(match) == 0 {
		return f.Effect()
	}
	return f.Effect() + " on " + strings.Join(match, " ")
}
