// Package config loads the target config: one YAML file that describes the
// controller to the harness.
package config

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"io"
	"maps"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"time"

	"k8s.io/apimachinery/pkg/runtime/schema"
	utilyaml "k8s.io/apimachinery/pkg/util/yaml"
	"sigs.k8s.io/yaml"
)

// Config is a loaded target config. Every path is absolute, resolved against
// the config file's directory.
type Config struct {
	Launch Launch
	CRDs   []string
	// Primaries are the CR kinds the controller serves, in config order. A
	// primary key loads as the one entry.
	Primaries []Primary
	Fixtures  []string
	// FixtureValues maps a fixture file, as an absolute path, to the string
	// paths changeFixture may set. The file is a fixtures entry or a
	// system's.
	FixtureValues map[string][]string
	Models        []Model
	Systems       []System
	Budgets       Budgets
	Timeouts      Timeouts
	Properties    []Property
	References    []Reference
}

// Values say how the generator draws CRs of one kind. Paths are dotted,
// such as spec.count.
type Values struct {
	// Overlay maps a path to schema keywords that narrow the CRD's.
	Overlay map[string]map[string]any
	// Distinct paths hold a different value in each CR of a plan.
	Distinct []string
	// Mutate paths are the ones updates change.
	Mutate []string
}

// Launch says how to run the controller. Args and Env may hold
// ${systems.<name>.<key>}.
type Launch struct {
	Binary string
	Args   []string
	// Env overrides the environment the controller inherits. An unset or
	// empty KUBECONFIG is ${systems.kube.kubeconfig}.
	Env map[string]string
	// RBAC holds Role and ClusterRole manifests that the controller's
	// identity is bound to.
	RBAC []string
}

// Primary describes one CR kind the controller serves, which the harness
// creates, changes and deletes.
type Primary struct {
	// Kind is written group/version/Kind, or version/Kind for the core group.
	Kind   schema.GroupVersionKind
	Sample string
	// Ready is CEL over self, the CR.
	Ready string
	// Retain is CEL over self, a deleted CR. Where it holds, Clean allows
	// the CR's resources to stay. It is empty when unset.
	Retain string
	// Owns are owning-CR rule 3: fields of the CR that name its resources.
	Owns []Owns
	// Halted is CEL over self, the CR. Where it holds after an
	// unknown-outcome call for the CR, Converge counts the CR as done. It
	// is empty when unset.
	Halted string
	// Periodic is nil when unset.
	Periodic *Periodic
	// Values say how the generator draws CRs of Kind.
	Values Values

	ready, retain, halted func(self map[string]any) (bool, error)
}

// Periodic declares the CR writes a controller makes on a timer, such as a
// refresh time. Clause 7 does not count a controller write of the CR that
// changes only Paths, at most once per Every.
type Periodic struct {
	Every time.Duration
	// Paths are dotted, such as status.refreshTime. [*] names every item
	// of a list, as in status.conditions[*].lastTransitionTime.
	Paths []string
}

// Owns is one owns entry of a Primary. Name and Namespace are CEL over self,
// the CR, and yield strings.
type Owns struct {
	// System and Collection name a fake's resources, written
	// <system>:<collection>; Kind is then empty.
	System, Collection string
	Kind               schema.GroupVersionKind
	Name               string
	// Namespace is empty when unset: the resource is in the CR's namespace,
	// or cluster-scoped.
	Namespace string
}

// System is one systems entry: kube, which takes only Owner, or a fake
// service, which has a Process.
type System struct {
	Name string
	// Owner is owning-CR rule 2: CEL over self, one of the system's
	// resources, that yields its CR's UID, namespace/name or name. It is
	// empty when unset.
	Owner string
	// Process is nil for kube.
	Process *Process
	// API names the fake's API map: crud, vault or s3.
	API string
	// Token is the harness's credential for the fake, and its token var.
	// It is empty when unset.
	Token string
	// Fixtures are files of SystemFixture lists, created before the
	// controller starts.
	Fixtures []string
	// Role, Ready and Gone are CEL over one of the fake's resources, or
	// empty. Ready is required with Dependency.
	Role, Ready, Gone string
	Dependency        *Dependency
	// Poll defaults to Timeouts.Stable.
	Poll time.Duration
	// ReadOnly, from access: read, says the controller only reads the
	// fake, so generation draws no damage or write faults on it.
	ReadOnly bool
}

// APIMap holds what the harness knows of an API map beyond its code, in
// the internal package of its name.
type APIMap struct {
	// Ignore lists the top-level body fields Restore does not compare.
	Ignore []string
	// ModelFaults says the fake takes stuck and slowDelete: only a crud
	// fake serves the /_fake/ paths.
	ModelFaults bool
	// Token says the map needs the system's token.
	Token bool
}

// APIMaps holds every API map a fake may use, by name.
var APIMaps = map[string]APIMap{
	"crud":  {Ignore: []string{"version"}, ModelFaults: true},
	"vault": {Ignore: []string{"current_version", "versions", "created_time", "updated_time"}, Token: true},
	"s3":    {Ignore: []string{"creationDate", "lastModified", "versionId"}},
}

// Map is the fake's APIMap.
func (s System) Map() APIMap { return APIMaps[s.API] }

// Process runs a fake service in the config file's directory.
type Process struct {
	// Command's first word, when it holds a slash, is an absolute path.
	Command []string
	Dir     string
}

// Poll is the poll interval of the fake named system, or else
// Timeouts.Stable.
func (c *Config) Poll(system string) time.Duration {
	for _, s := range c.Fakes() {
		if s.Name == system {
			return s.Poll
		}
	}
	return c.Timeouts.Stable
}

// Fakes returns the systems entries with a process.
func (c *Config) Fakes() []System {
	var fakes []System
	for _, s := range c.Systems {
		if s.Process != nil {
			fakes = append(fakes, s)
		}
	}
	return fakes
}

// ReadyCondition is the only stage: the model sets the Ready condition True
// for the object's generation, After the object or its generation appears.
const ReadyCondition = "readyCondition"

// ReadyConditionCEL says whether a ReadyCondition model's object is ready.
const ReadyConditionCEL = `has(self.status) && has(self.status.conditions) && self.status.conditions.exists(c, c.type == 'Ready' && c.status == 'True' && has(c.observedGeneration) && c.observedGeneration == self.metadata.generation)`

// Model is one models entry: the harness plays the controller of Kind.
type Model struct {
	Kind  schema.GroupVersionKind
	Stage string
	// Status holds literal fields the stage sets in each status write, and
	// StuckStatus those it sets in their place while a stuck fault lasts.
	// A nil StuckStatus means Status, and an empty one means no fields.
	Status, StuckStatus map[string]any
	After               time.Duration
	// Dependency is nil unless the kind is a readiness dependency.
	Dependency *Dependency
}

// Dependency marks a model kind or a fake as a readiness dependency of the
// CRs that own its objects.
type Dependency struct {
	// Required fails a CR of the first Primaries entry that becomes Ready
	// while it owns none of the dependency's objects. It defaults to true.
	Required bool
	// Track, when set, fails a CR that stays Ready longer than Track after
	// the controller read a not-ready dependency.
	Track time.Duration
}

// Budgets cap the controller's requests to each system per live CR per
// poll period of that system. Kube's poll period is Timeouts.Stable.
// Reads count gets and lists. Writes count mutating requests, no-op writes
// included.
type Budgets struct {
	Reads  int
	Writes int
}

// Timeouts default to settle 30s, stable 3s and backoffCap 60s.
type Timeouts struct {
	Settle     time.Duration
	Stable     time.Duration
	BackoffCap time.Duration
}

// DefaultBudgets apply when the config sets no budgets key.
var DefaultBudgets = Budgets{Reads: 2, Writes: 4}

var defaultTimeouts = Timeouts{Settle: 30 * time.Second, Stable: 3 * time.Second, BackoffCap: 60 * time.Second}

// declaration mirrors the YAML file.
type declaration struct {
	Launch struct {
		Binary string            `json:"binary"`
		Args   []string          `json:"args"`
		Env    map[string]string `json:"env"`
		RBAC   []string          `json:"rbac"`
	} `json:"launch"`
	CRDs       []string            `json:"crds"`
	Primary    *declaredPrimary    `json:"primary"`
	Primaries  []declaredEntry     `json:"primaries"`
	References []declaredReference `json:"references"`
	Fixtures   []string            `json:"fixtures"`
	Models     []struct {
		Kind        string              `json:"kind"`
		Stages      string              `json:"stages"`
		Status      map[string]any      `json:"status"`
		StuckStatus map[string]any      `json:"stuckStatus"`
		After       string              `json:"after"`
		Dependency  *declaredDependency `json:"dependency"`
	} `json:"models"`
	// Values holds primary's overlay, distinct and mutate beside fixtures.
	Values struct {
		declaredValues
		Fixtures map[string][]string `json:"fixtures"`
	} `json:"values"`
	Systems []struct {
		Name    string `json:"name"`
		Owner   string `json:"owner"`
		Process *struct {
			Command []string `json:"command"`
		} `json:"process"`
		API        string              `json:"api"`
		Token      string              `json:"token"`
		Fixtures   []string            `json:"fixtures"`
		Role       string              `json:"role"`
		Ready      string              `json:"ready"`
		Gone       string              `json:"gone"`
		Dependency *declaredDependency `json:"dependency"`
		Poll       string              `json:"poll"`
		Access     string              `json:"access"`
	} `json:"systems"`
	Budgets struct {
		Reads  *int `json:"reads"`
		Writes *int `json:"writes"`
	} `json:"budgets"`
	Timeouts struct {
		Settle     string `json:"settle"`
		Stable     string `json:"stable"`
		BackoffCap string `json:"backoffCap"`
	} `json:"timeouts"`
	Properties []struct {
		Name string `json:"name"`
		Expr string `json:"expr"`
		When string `json:"when"`
	} `json:"properties"`
}

// declaredPrimary is the primary key, which takes no values: they sit at
// the top level.
type declaredPrimary struct {
	Kind     string            `json:"kind"`
	Sample   string            `json:"sample"`
	Ready    string            `json:"ready"`
	Retain   string            `json:"retain"`
	Halted   string            `json:"halted"`
	Periodic *declaredPeriodic `json:"periodic"`
	Owns     []declaredOwns    `json:"owns"`
}

type declaredPeriodic struct {
	Every string   `json:"every"`
	Paths []string `json:"paths"`
}

type declaredOwns struct {
	Kind      string `json:"kind"`
	Name      string `json:"name"`
	Namespace string `json:"namespace"`
}

// declaredEntry is one primaries entry.
type declaredEntry struct {
	declaredPrimary
	Values declaredValues `json:"values"`
}

type declaredValues struct {
	Overlay  map[string]map[string]any `json:"overlay"`
	Distinct []string                  `json:"distinct"`
	Mutate   []string                  `json:"mutate"`
}

type declaredDependency struct {
	Required *bool  `json:"required"`
	Track    string `json:"track"`
}

// Load reads the file strictly: an unknown key is an error that names its
// line. It compiles each entry's expressions, checks that every path exists
// and refuses a kube fixture of a listed kind.
func Load(path string) (*Config, error) {
	c, err := load(path)
	if err != nil {
		return nil, fmt.Errorf("loading config %s: %w", path, err)
	}
	return c, nil
}

func load(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var d declaration
	if err := yaml.UnmarshalStrict(data, &d); err != nil {
		if located := unknownKey(data); located != nil {
			return nil, located
		}
		return nil, err
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return nil, err
	}
	dir := filepath.Dir(abs)
	env := map[string]string{}
	maps.Copy(env, d.Launch.Env)
	// client-go reads an empty KUBECONFIG as unset.
	if env["KUBECONFIG"] == "" {
		env["KUBECONFIG"] = "${systems.kube.kubeconfig}"
	}
	c := &Config{Launch: Launch{Args: d.Launch.Args, Env: env}}

	if d.Launch.Binary == "" {
		return nil, errors.New("launch.binary is required")
	}
	c.Launch.Binary = resolve(dir, d.Launch.Binary)
	if c.Launch.RBAC, err = existing(dir, "launch.rbac", d.Launch.RBAC); err != nil {
		return nil, err
	}
	if c.CRDs, err = existing(dir, "crds", d.CRDs); err != nil {
		return nil, err
	}
	if c.Fixtures, err = existing(dir, "fixtures", d.Fixtures); err != nil {
		return nil, err
	}

	c.Budgets = DefaultBudgets
	for _, b := range []struct {
		key      string
		declared *int
		value    *int
	}{
		{"budgets.reads", d.Budgets.Reads, &c.Budgets.Reads},
		{"budgets.writes", d.Budgets.Writes, &c.Budgets.Writes},
	} {
		if b.declared == nil {
			continue
		}
		if *b.declared <= 0 {
			return nil, fmt.Errorf("%s %d: want a positive count", b.key, *b.declared)
		}
		*b.value = *b.declared
	}

	c.Timeouts = defaultTimeouts
	for _, t := range []struct {
		key      string
		declared string
		value    *time.Duration
	}{
		{"timeouts.settle", d.Timeouts.Settle, &c.Timeouts.Settle},
		{"timeouts.stable", d.Timeouts.Stable, &c.Timeouts.Stable},
		{"timeouts.backoffCap", d.Timeouts.BackoffCap, &c.Timeouts.BackoffCap},
	} {
		if t.declared == "" {
			continue
		}
		parsed, err := time.ParseDuration(t.declared)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", t.key, err)
		}
		if parsed <= 0 {
			return nil, fmt.Errorf("%s %s: want a positive duration", t.key, t.declared)
		}
		*t.value = parsed
	}

	if c.Systems, err = systems(d, dir, c.Timeouts.Stable); err != nil {
		return nil, err
	}
	if c.Primaries, err = primaries(d, dir, c.Fakes()); err != nil {
		return nil, err
	}
	if c.Models, err = models(d, c.ListedKinds()); err != nil {
		return nil, err
	}
	if err := refuseListedFixtures(d.Fixtures, c.Fixtures, c.ListedKinds()); err != nil {
		return nil, err
	}
	if c.References, err = c.references(d.References); err != nil {
		return nil, err
	}
	fixtures := slices.Clone(c.Fixtures)
	for _, s := range c.Systems {
		fixtures = append(fixtures, s.Fixtures...)
	}
	c.FixtureValues = map[string][]string{}
	for file, paths := range d.Values.Fixtures {
		path := resolve(dir, file)
		if !slices.Contains(fixtures, path) {
			return nil, fmt.Errorf("values.fixtures %s: no fixtures key lists this file", file)
		}
		c.FixtureValues[path] = paths
	}
	if c.Properties, err = properties(d); err != nil {
		return nil, err
	}
	if err := c.checkLaunchVars(); err != nil {
		return nil, err
	}
	return c, nil
}

// primaries loads the primaries entries, or primary as the one entry with
// the top-level values.
func primaries(d declaration, dir string, fakes []System) ([]Primary, error) {
	entries, key := d.Primaries, func(i int) string { return fmt.Sprintf("primaries[%d]", i) }
	switch {
	case d.Primary != nil && d.Primaries != nil:
		return nil, errors.New("primary and primaries: set one, not both")
	case d.Primary != nil:
		entries = []declaredEntry{{declaredPrimary: *d.Primary, Values: d.Values.declaredValues}}
		key = func(int) string { return "primary" }
	case len(d.Primaries) == 0:
		return nil, errors.New("primaries or primary is required")
	default:
		for _, v := range []struct {
			key string
			set bool
		}{
			{"overlay", d.Values.Overlay != nil}, {"distinct", d.Values.Distinct != nil}, {"mutate", d.Values.Mutate != nil},
		} {
			if v.set {
				return nil, fmt.Errorf("values.%s: with primaries, each entry takes its own values", v.key)
			}
		}
	}
	var listed []Primary
	for i, declared := range entries {
		p, err := primary(key(i), declared, dir, fakes)
		if err != nil {
			return nil, err
		}
		if j := slices.IndexFunc(listed, func(other Primary) bool { return other.Kind.GroupKind() == p.Kind.GroupKind() }); j >= 0 {
			return nil, fmt.Errorf("%s.kind %s: %s has this group and kind", key(i), declared.Kind, key(j))
		}
		listed = append(listed, p)
	}
	return listed, nil
}

// primary loads one entry. key names it in errors.
func primary(key string, declared declaredEntry, dir string, fakes []System) (Primary, error) {
	if declared.Kind == "" {
		return Primary{}, fmt.Errorf("%s.kind is required", key)
	}
	kind, err := parseGVK(declared.Kind)
	if err != nil {
		return Primary{}, fmt.Errorf("%s.kind %q: %w", key, declared.Kind, err)
	}
	p := Primary{Kind: kind, Ready: declared.Ready, Retain: declared.Retain, Halted: declared.Halted, Values: Values(declared.Values)}
	if declared.Sample == "" {
		return Primary{}, fmt.Errorf("%s.sample is required", key)
	}
	samples, err := existing(dir, key+".sample", []string{declared.Sample})
	if err != nil {
		return Primary{}, err
	}
	p.Sample = samples[0]
	if p.Ready == "" {
		return Primary{}, fmt.Errorf("%s.ready is required", key)
	}
	if p.ready, err = CompileReady(p.Ready); err != nil {
		return Primary{}, fmt.Errorf("%s.ready: %w", key, err)
	}
	if p.Retain != "" {
		if p.retain, err = CompileReady(p.Retain); err != nil {
			return Primary{}, fmt.Errorf("%s.retain: %w", key, err)
		}
	}
	if p.Halted != "" {
		if p.halted, err = CompileReady(p.Halted); err != nil {
			return Primary{}, fmt.Errorf("%s.halted: %w", key, err)
		}
	}
	if p.Periodic, err = periodic(key, declared.Periodic); err != nil {
		return Primary{}, err
	}
	if p.Owns, err = owns(key, declared.Owns, fakes); err != nil {
		return Primary{}, err
	}
	return p, nil
}

// fixtureKey matches a ${key} within one line, as kube expands it in a
// fixture before it parses the file.
var fixtureKey = regexp.MustCompile(`\$\{[^}\n]*\}`)

// refuseListedFixtures refuses a kube fixture of a listed kind. declared
// are the fixtures as the config names them, and paths the files.
func refuseListedFixtures(declared, paths []string, listed []schema.GroupKind) error {
	for i, path := range paths {
		data, err := os.ReadFile(path)
		if err != nil {
			return fmt.Errorf("fixtures %s: %w", declared[i], err)
		}
		data = fixtureKey.ReplaceAllLiteral(data, []byte("key"))
		reader := utilyaml.NewYAMLReader(bufio.NewReader(bytes.NewReader(data)))
		for {
			document, err := reader.Read()
			if errors.Is(err, io.EOF) {
				break
			}
			if err != nil {
				return fmt.Errorf("fixtures %s: %w", declared[i], err)
			}
			var header struct {
				APIVersion string `json:"apiVersion"`
				Kind       string `json:"kind"`
			}
			if err := yaml.Unmarshal(document, &header); err != nil {
				return fmt.Errorf("fixtures %s: %w", declared[i], err)
			}
			if kind := schema.FromAPIVersionAndKind(header.APIVersion, header.Kind).GroupKind(); slices.Contains(listed, kind) {
				return fmt.Errorf("fixtures %s: the file holds a %s CR; create CRs in a plan", declared[i], header.Kind)
			}
		}
	}
	return nil
}

// ListedKinds are the group and kind of each Primaries entry, in order.
func (c *Config) ListedKinds() []schema.GroupKind {
	var kinds []schema.GroupKind
	for _, p := range c.Primaries {
		kinds = append(kinds, p.Kind.GroupKind())
	}
	return kinds
}

// launchVars are the keys each system's Endpoints.Vars holds. A fake with
// a token also holds token.
var launchVars = map[string][]string{"kube": {"kubeconfig", "namespace", "url"}, "fake": {"url"}}

// checkLaunchVars refuses a launch arg or env value whose ${key} names no
// system or no key of one.
func (c *Config) checkLaunchVars() error {
	vars := map[string]string{}
	for _, key := range launchVars["kube"] {
		vars["systems.kube."+key] = ""
	}
	for _, f := range c.Fakes() {
		for _, key := range launchVars["fake"] {
			vars["systems."+f.Name+"."+key] = ""
		}
		if f.Token != "" {
			vars["systems."+f.Name+".token"] = ""
		}
	}
	check := func(where, value string) error {
		for _, key := range keys(value) {
			if _, known := vars[key]; known {
				continue
			}
			system, rest, ok := strings.Cut(strings.TrimPrefix(key, "systems."), ".")
			switch {
			case !strings.HasPrefix(key, "systems.") || !ok:
				return fmt.Errorf("%s: unknown key %s", where, key)
			case system != "kube" && !slices.ContainsFunc(c.Fakes(), func(f System) bool { return f.Name == system }):
				return fmt.Errorf("%s: no system %s", where, system)
			default:
				return fmt.Errorf("%s: system %s has no %s", where, system, rest)
			}
		}
		return nil
	}
	for i, arg := range c.Launch.Args {
		if err := check(fmt.Sprintf("launch args[%d]", i), arg); err != nil {
			return err
		}
	}
	for name, value := range c.Launch.Env {
		if err := check("launch env "+name, value); err != nil {
			return err
		}
	}
	return nil
}

// keys returns each ${key} in s.
func keys(s string) []string {
	var found []string
	for {
		_, rest, ok := strings.Cut(s, "${")
		if !ok {
			return found
		}
		key, after, closed := strings.Cut(rest, "}")
		if !closed {
			return found
		}
		found, s = append(found, key), after
	}
}

func models(d declaration, listed []schema.GroupKind) ([]Model, error) {
	var models []Model
	seen := map[schema.GroupKind]bool{}
	for _, kind := range listed {
		seen[kind] = true
	}
	for i, declared := range d.Models {
		key := fmt.Sprintf("models[%d]", i)
		if declared.Kind == "" {
			return nil, fmt.Errorf("%s.kind is required", key)
		}
		kind, err := parseGVK(declared.Kind)
		if err != nil {
			return nil, fmt.Errorf("%s.kind %q: %w", key, declared.Kind, err)
		}
		if seen[kind.GroupKind()] {
			return nil, fmt.Errorf("%s.kind %s: a primary or another model has this kind", key, declared.Kind)
		}
		seen[kind.GroupKind()] = true
		m := Model{Kind: kind, Stage: ReadyCondition, Status: declared.Status, StuckStatus: declared.StuckStatus}
		if declared.Stages != "" && declared.Stages != ReadyCondition {
			return nil, fmt.Errorf("%s.stages %q: the only stage is %s", key, declared.Stages, ReadyCondition)
		}
		if declared.After == "" {
			return nil, fmt.Errorf("%s.after is required", key)
		}
		if m.After, err = time.ParseDuration(declared.After); err != nil || m.After < 0 {
			return nil, fmt.Errorf("%s.after %q: want a duration of 0 or more", key, declared.After)
		}
		if m.Dependency, err = dependency(key, declared.Dependency); err != nil {
			return nil, err
		}
		models = append(models, m)
	}
	return models, nil
}

func dependency(key string, declared *declaredDependency) (*Dependency, error) {
	if declared == nil {
		return nil, nil
	}
	dep := &Dependency{Required: declared.Required == nil || *declared.Required}
	if declared.Track != "" {
		var err error
		if dep.Track, err = time.ParseDuration(declared.Track); err != nil || dep.Track <= 0 {
			return nil, fmt.Errorf("%s.dependency.track %q: want a positive duration", key, declared.Track)
		}
	}
	return dep, nil
}

func owns(entryKey string, entries []declaredOwns, fakes []System) ([]Owns, error) {
	var owns []Owns
	for i, declared := range entries {
		key := fmt.Sprintf("%s.owns[%d]", entryKey, i)
		if declared.Kind == "" {
			return nil, fmt.Errorf("%s.kind is required", key)
		}
		var o Owns
		if strings.Contains(declared.Kind, ":") {
			fake, collection, err := fakeKind(declared.Kind, fakes)
			if err != nil {
				return nil, fmt.Errorf("%s.kind %s: %w", key, declared.Kind, err)
			}
			if declared.Namespace != "" {
				return nil, fmt.Errorf("%s.namespace: a fake's resources have no namespace", key)
			}
			o.System, o.Collection = fake.Name, collection
		} else {
			kind, err := parseGVK(declared.Kind)
			if err != nil {
				return nil, fmt.Errorf("%s.kind %q: %w", key, declared.Kind, err)
			}
			o.Kind = kind
		}
		if declared.Name == "" {
			return nil, fmt.Errorf("%s.name is required", key)
		}
		if _, err := CompileString(declared.Name); err != nil {
			return nil, fmt.Errorf("%s.name: %w", key, err)
		}
		if declared.Namespace != "" {
			if _, err := CompileString(declared.Namespace); err != nil {
				return nil, fmt.Errorf("%s.namespace: %w", key, err)
			}
		}
		o.Name, o.Namespace = declared.Name, declared.Namespace
		owns = append(owns, o)
	}
	return owns, nil
}

var systemName = regexp.MustCompile(`^[a-z][a-z0-9-]*$`)

func systems(d declaration, dir string, stable time.Duration) ([]System, error) {
	var systems []System
	for i, declared := range d.Systems {
		key := fmt.Sprintf("systems[%d]", i)
		if slices.ContainsFunc(systems, func(s System) bool { return s.Name == declared.Name }) {
			return nil, fmt.Errorf("%s.name %s: another system has this name", key, declared.Name)
		}
		s := System{Name: declared.Name, Owner: declared.Owner, API: declared.API, Role: declared.Role, Ready: declared.Ready, Gone: declared.Gone}
		for _, expr := range []struct {
			key, expr string
			compile   func(string) error
		}{
			{"owner", s.Owner, compileString}, {"role", s.Role, compileString},
			{"ready", s.Ready, compileBool}, {"gone", s.Gone, compileBool},
		} {
			if expr.expr == "" {
				continue
			}
			if err := expr.compile(expr.expr); err != nil {
				return nil, fmt.Errorf("%s.%s: %w", key, expr.key, err)
			}
		}
		if declared.Name == "kube" {
			if declared.Process != nil || declared.API != "" || declared.Role != "" || declared.Ready != "" || declared.Gone != "" ||
				declared.Dependency != nil || declared.Poll != "" || declared.Token != "" || declared.Fixtures != nil || declared.Access != "" {
				return nil, fmt.Errorf("%s: kube takes only name and owner", key)
			}
			systems = append(systems, s)
			continue
		}
		if !systemName.MatchString(declared.Name) {
			return nil, fmt.Errorf("%s.name %s: want a name matching %s", key, declared.Name, systemName)
		}
		if declared.Process == nil || len(declared.Process.Command) == 0 || declared.Process.Command[0] == "" {
			return nil, fmt.Errorf("%s.process.command is required", key)
		}
		command := slices.Clone(declared.Process.Command)
		if strings.Contains(command[0], "/") {
			command[0] = resolve(dir, command[0])
		}
		s.Process = &Process{Command: command, Dir: dir}
		if _, known := APIMaps[s.API]; !known {
			return nil, fmt.Errorf("%s.api %q: want crud, vault or s3", key, s.API)
		}
		s.Token = declared.Token
		if s.Map().Token && s.Token == "" {
			return nil, fmt.Errorf("%s.token is required with api %s", key, s.API)
		}
		var err error
		if s.Fixtures, err = existing(dir, key+".fixtures", declared.Fixtures); err != nil {
			return nil, err
		}
		for _, file := range s.Fixtures {
			if _, err := ReadSystemFixtures(file); err != nil {
				return nil, fmt.Errorf("%s.fixtures: %w", key, err)
			}
		}
		if s.Dependency, err = dependency(key, declared.Dependency); err != nil {
			return nil, err
		}
		if s.Dependency != nil && s.Ready == "" {
			return nil, fmt.Errorf("%s.dependency needs ready", key)
		}
		switch declared.Access {
		case "":
		case "read":
			s.ReadOnly = true
		default:
			return nil, fmt.Errorf("%s.access %q: want read, or none", key, declared.Access)
		}
		s.Poll = stable
		if declared.Poll != "" {
			if s.Poll, err = time.ParseDuration(declared.Poll); err != nil || s.Poll <= 0 {
				return nil, fmt.Errorf("%s.poll %q: want a positive duration", key, declared.Poll)
			}
		}
		systems = append(systems, s)
	}
	return systems, nil
}

func compileString(expr string) error {
	_, err := CompileString(expr)
	return err
}

func compileBool(expr string) error {
	_, err := CompileReady(expr)
	return err
}

func resolve(dir, path string) string {
	if filepath.IsAbs(path) {
		return path
	}
	return filepath.Join(dir, path)
}

// existing resolves paths and requires each to exist.
func existing(dir, key string, paths []string) ([]string, error) {
	var resolved []string
	for _, path := range paths {
		path = resolve(dir, path)
		if _, err := os.Stat(path); err != nil {
			return nil, fmt.Errorf("%s: %w", key, err)
		}
		resolved = append(resolved, path)
	}
	return resolved, nil
}

// apiVersion matches the version names Kubernetes accepts, so that a
// group/Kind typo does not read as a core-group version.
var apiVersion = regexp.MustCompile(`^v[1-9][0-9]*((alpha|beta)[1-9][0-9]*)?$`)

func parseGVK(declared string) (schema.GroupVersionKind, error) {
	malformed := errors.New("want group/version/Kind or version/Kind")
	parts := strings.Split(declared, "/")
	var gvk schema.GroupVersionKind
	switch len(parts) {
	case 2:
		gvk = schema.GroupVersionKind{Version: parts[0], Kind: parts[1]}
	case 3:
		gvk = schema.GroupVersionKind{Group: parts[0], Version: parts[1], Kind: parts[2]}
	default:
		return schema.GroupVersionKind{}, malformed
	}
	if gvk.Kind == "" || (len(parts) == 3 && gvk.Group == "") {
		return schema.GroupVersionKind{}, malformed
	}
	if !apiVersion.MatchString(gvk.Version) {
		return schema.GroupVersionKind{}, fmt.Errorf("%q is not an API version", gvk.Version)
	}
	return gvk, nil
}

// Ready evaluates the Ready of cr's entry over cr, a CR's body. Ready,
// Retain and Halted take the entry of cr's group and kind. With one entry,
// that entry judges any body. A body of no listed kind is an error.
func (c *Config) Ready(cr map[string]any) (bool, error) {
	p, err := c.entry(cr)
	if err != nil {
		return false, err
	}
	if p.ready == nil {
		return false, errors.New("ready is not compiled; load the config with Load")
	}
	return p.ready(cr)
}

// Retain evaluates the Retain of cr's entry over cr, a deleted CR's body.
// It is false when Retain is empty.
func (c *Config) Retain(cr map[string]any) (bool, error) {
	p, err := c.entry(cr)
	if err != nil || p.retain == nil {
		return false, err
	}
	return p.retain(cr)
}

// Halted evaluates the Halted of cr's entry over cr. It is false when
// Halted is empty.
func (c *Config) Halted(cr map[string]any) (bool, error) {
	p, err := c.entry(cr)
	if err != nil || p.halted == nil {
		return false, err
	}
	return p.halted(cr)
}

// entry is the Primaries entry of cr's group and kind, or the only entry.
func (c *Config) entry(cr map[string]any) (Primary, error) {
	if len(c.Primaries) == 1 {
		return c.Primaries[0], nil
	}
	apiVersion, _ := cr["apiVersion"].(string)
	kind, _ := cr["kind"].(string)
	gk := schema.FromAPIVersionAndKind(apiVersion, kind).GroupKind()
	for _, p := range c.Primaries {
		if p.Kind.GroupKind() == gk {
			return p, nil
		}
	}
	return Primary{}, fmt.Errorf("primaries lists no kind %q of apiVersion %q", kind, apiVersion)
}

// Expand replaces each ${key} in s with vars[key]. An unknown key is an
// error.
func Expand(s string, vars map[string]string) (string, error) {
	var out strings.Builder
	for {
		before, rest, found := strings.Cut(s, "${")
		out.WriteString(before)
		if !found {
			return out.String(), nil
		}
		key, after, closed := strings.Cut(rest, "}")
		if !closed {
			return "", fmt.Errorf("%q: ${ has no closing }", s)
		}
		value, known := vars[key]
		if !known {
			return "", fmt.Errorf("%q: unknown key %s", s, key)
		}
		out.WriteString(value)
		s = after
	}
}
