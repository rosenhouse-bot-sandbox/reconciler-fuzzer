// Package config loads the target config: one YAML file that describes the
// controller to the harness.
package config

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"time"

	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/yaml"
)

// Config is a loaded target config. Every path is absolute, resolved against
// the config file's directory.
type Config struct {
	Launch   Launch
	CRDs     []string
	Primary  Primary
	Fixtures []string
	Values   Values
	Budgets  Budgets
	Timeouts Timeouts

	ready, retain func(self map[string]any) (bool, error)
}

// Values say how the generator draws CRs and fixture changes. Paths are
// dotted, such as spec.count.
type Values struct {
	// Overlay maps a path to schema keywords that narrow the CRD's.
	Overlay map[string]map[string]any
	// Distinct paths hold a different value in each CR of a plan.
	Distinct []string
	// Mutate paths are the ones updates change.
	Mutate []string
	// Fixtures maps a fixture file, as an absolute path, to the string
	// paths changeFixture may set.
	Fixtures map[string][]string
}

// Launch says how to run the controller. Args and Env may hold
// ${systems.<name>.<key>}.
type Launch struct {
	Binary string
	Args   []string
	Env    map[string]string
	// RBAC holds Role and ClusterRole manifests that the controller's
	// identity is bound to.
	RBAC []string
}

// Primary describes the CR kind the harness creates, changes and deletes.
type Primary struct {
	// Kind is written group/version/Kind, or version/Kind for the core group.
	Kind   schema.GroupVersionKind
	Sample string
	// Ready is CEL over self, the CR.
	Ready string
	// Retain is CEL over self, a deleted CR. Where it holds, Clean allows
	// the CR's resources to stay. It is empty when unset.
	Retain string
}

// Budgets cap the controller's requests per live CR per stable period, not
// per poll period: the Kubernetes system has no poll.
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
	CRDs    []string `json:"crds"`
	Primary struct {
		Kind   string `json:"kind"`
		Sample string `json:"sample"`
		Ready  string `json:"ready"`
		Retain string `json:"retain"`
	} `json:"primary"`
	Fixtures []string `json:"fixtures"`
	Values   struct {
		Overlay  map[string]map[string]any `json:"overlay"`
		Distinct []string                  `json:"distinct"`
		Mutate   []string                  `json:"mutate"`
		Fixtures map[string][]string       `json:"fixtures"`
	} `json:"values"`
	Budgets struct {
		Reads  *int `json:"reads"`
		Writes *int `json:"writes"`
	} `json:"budgets"`
	Timeouts struct {
		Settle     string `json:"settle"`
		Stable     string `json:"stable"`
		BackoffCap string `json:"backoffCap"`
	} `json:"timeouts"`
}

// Load reads the file strictly: an unknown key is an error that names its
// line. It compiles Ready and checks that every path exists.
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
	c := &Config{Launch: Launch{Args: d.Launch.Args, Env: d.Launch.Env}}

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

	if d.Primary.Kind == "" {
		return nil, errors.New("primary.kind is required")
	}
	if c.Primary.Kind, err = parseGVK(d.Primary.Kind); err != nil {
		return nil, fmt.Errorf("primary.kind %q: %w", d.Primary.Kind, err)
	}
	if d.Primary.Sample == "" {
		return nil, errors.New("primary.sample is required")
	}
	samples, err := existing(dir, "primary.sample", []string{d.Primary.Sample})
	if err != nil {
		return nil, err
	}
	c.Primary.Sample = samples[0]
	if d.Primary.Ready == "" {
		return nil, errors.New("primary.ready is required")
	}
	c.Primary.Ready = d.Primary.Ready
	if c.ready, err = CompileReady(d.Primary.Ready); err != nil {
		return nil, fmt.Errorf("primary.ready: %w", err)
	}
	if c.Primary.Retain = d.Primary.Retain; c.Primary.Retain != "" {
		if c.retain, err = CompileReady(c.Primary.Retain); err != nil {
			return nil, fmt.Errorf("primary.retain: %w", err)
		}
	}

	c.Values = Values{Overlay: d.Values.Overlay, Distinct: d.Values.Distinct, Mutate: d.Values.Mutate, Fixtures: map[string][]string{}}
	for file, paths := range d.Values.Fixtures {
		path := resolve(dir, file)
		if !slices.Contains(c.Fixtures, path) {
			return nil, fmt.Errorf("values.fixtures %s: fixtures lists no such file", file)
		}
		c.Values.Fixtures[path] = paths
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
	return c, nil
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

// Ready evaluates Primary.Ready over obj, a CR's body.
func (c *Config) Ready(obj map[string]any) (bool, error) {
	if c.ready == nil {
		return false, errors.New("ready is not compiled; load the config with Load")
	}
	return c.ready(obj)
}

// Retain evaluates Primary.Retain over cr, a deleted CR's body. It is false
// when Retain is empty.
func (c *Config) Retain(cr map[string]any) (bool, error) {
	if c.retain == nil {
		return false, nil
	}
	return c.retain(cr)
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
