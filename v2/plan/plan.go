// Package plan holds the steps of one run as plain data that plan.json stores.
package plan

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"
)

// Plan is the list of steps for one run.
type Plan struct {
	Seed  uint64 `json:"seed,omitempty"`
	Steps []Step `json:"steps"`
}

// Step is one unit of a run. Its fault period lasts Period, or until its
// last action ends if that is later.
type Step struct {
	Period  Duration `json:"period,omitempty"`
	Actions []Action `json:"actions,omitempty"`
	Faults  []Fault  `json:"faults,omitempty"`
}

// Do names what an action does.
type Do string

const (
	// Create creates Object, or the sample when Object is empty. A create of
	// a slot whose CR is still terminating waits until the name is free.
	Create Do = "create"
	// Update merge-patches the slot's CR with Patch.
	Update Do = "update"
	// Delete deletes the slot's CR.
	Delete Do = "delete"
	// Restart stops the controller with SIGTERM and starts it again.
	Restart Do = "restart"
	// Kill sends SIGKILL. The controller stays down until the fault period
	// ends.
	Kill Do = "kill"
	// Damage deletes the object Target selects among those the controller
	// created, or merge-patches it with Patch when set. The harness never
	// undoes it.
	Damage Do = "damage"
	// ChangeFixture merge-patches the fixture Target names with Patch, or
	// deletes it when Patch is empty.
	ChangeFixture Do = "changeFixture"
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
}

// Target selects one object for damage or changeFixture. It is resolved
// when the action runs. An empty field matches any value.
type Target struct {
	// System defaults to "kube".
	System   string `json:"system,omitempty"`
	Group    string `json:"group,omitempty"`
	Resource string `json:"resource"`
	Name     string `json:"name,omitempty"`
	Role     string `json:"role,omitempty"`
	// Nth picks the nth match in creation order, counting from 1. Zero
	// picks the first.
	Nth int `json:"nth,omitempty"`
}

// Fault is a request fault on one system's controller listener. It starts
// with the fault period and ends with it. Exactly one of Error, Delay, Drop,
// DropResponse and KillAfter is set.
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
}

// Match selects requests. An empty field matches any value.
type Match struct {
	Verb        string `json:"verb,omitempty"`
	Group       string `json:"group,omitempty"`
	Resource    string `json:"resource,omitempty"`
	Subresource string `json:"subresource,omitempty"`
	Name        string `json:"name,omitempty"`
}

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

// Load reads plan.json strictly and validates it.
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
	return &p, nil
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
	// names maps each slot that holds a CR to that CR's name, which is
	// sampleName for the sample and empty for a generated name. A slot an
	// earlier delete emptied maps to emptied.
	names := map[string]string{}
	for i, step := range p.Steps {
		if step.Period < 0 {
			return fmt.Errorf("steps[%d]: period is negative", i)
		}
		for j, a := range step.Actions {
			if err := a.validate(names); err != nil {
				return fmt.Errorf("steps[%d].actions[%d]: %w", i, j, err)
			}
		}
		for k, f := range step.Faults {
			if err := f.Validate(); err != nil {
				return fmt.Errorf("steps[%d].faults[%d]: %w", i, k, err)
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
	case Restart, Kill:
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
	if a.Target == nil || a.Target.Resource == "" {
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

// createdName is the metadata.name of object, sampleName when object is
// empty, or empty for a generated name.
func createdName(object json.RawMessage) (string, error) {
	if object == nil {
		return sampleName, nil
	}
	var o struct {
		Metadata struct {
			Name string `json:"name"`
		} `json:"metadata"`
	}
	if err := json.Unmarshal(object, &o); err != nil {
		return "", fmt.Errorf("decoding object: %w", err)
	}
	return o.Metadata.Name, nil
}

func isObject(raw json.RawMessage) bool {
	var object map[string]any
	return json.Unmarshal(raw, &object) == nil && object != nil
}

// Validate requires exactly one effect, an error code from 400 to 599, a
// known system and a non-negative nth.
func (f Fault) Validate() error {
	effects := 0
	for _, set := range []bool{f.Error != nil, f.Delay != nil, f.Drop, f.DropResponse, f.KillAfter} {
		if set {
			effects++
		}
	}
	if effects != 1 {
		return fmt.Errorf("a fault sets %d of error, delay, drop, dropResponse and killAfter, want exactly one", effects)
	}
	if f.Error != nil && (f.Error.Code < 400 || f.Error.Code > 599) {
		return fmt.Errorf("error code %d is not from 400 to 599", f.Error.Code)
	}
	if f.System != "" && f.System != "kube" {
		return fmt.Errorf("unknown system %q", f.System)
	}
	if f.Nth < 0 {
		return errors.New("nth is negative")
	}
	return nil
}

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
