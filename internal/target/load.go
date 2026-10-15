package target

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

	yamlv2 "go.yaml.in/yaml/v2"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime/schema"
	utilyaml "k8s.io/apimachinery/pkg/util/yaml"
	"sigs.k8s.io/yaml"

	"github.com/rosenhouse-bot-sandbox/reconciler-fuzzer/internal/observe"
)

// declaration mirrors target.yaml. Decoding is strict, so a misspelled key is
// a configuration error rather than silence.
type declaration struct {
	Name         string                `json:"name"`
	Version      string                `json:"version"`
	CRDs         []string              `json:"crds"`
	Primary      string                `json:"primary"`
	Sample       string                `json:"sample"`
	Fixtures     []string              `json:"fixtures"`
	Manages      []string              `json:"manages"`
	NotRecreated []string              `json:"notRecreated"`
	RBAC         []string              `json:"rbac"`
	Selector     string                `json:"selector"`
	Ready        string                `json:"ready"`
	Equal        string                `json:"equal"`
	EqualIgnore  []string              `json:"equalIgnore"`
	Properties   []propertyDeclaration `json:"properties"`
	Generate     generateDeclaration   `json:"generate"`
	Launch       LaunchSpec            `json:"launch"`
	Timeouts     timeoutsDeclaration   `json:"timeouts"`
	Thresholds   thresholdsDeclaration `json:"thresholds"`
}

type propertyDeclaration struct {
	ID          string `json:"id"`
	Description string `json:"description"`
	CEL         string `json:"cel"`
	When        string `json:"when"`
}

type generateDeclaration struct {
	Mutate   []string                      `json:"mutate"`
	Overlay  map[string]map[string]any     `json:"overlay"`
	MaxCRs   *int                          `json:"maxCRs"`
	Distinct []string                      `json:"distinct"`
	Faults   *bool                         `json:"faults"`
	Fixtures map[string]fixtureDeclaration `json:"fixtures"`
}

type fixtureDeclaration struct {
	Mutate []string `json:"mutate"`
}

type timeoutsDeclaration struct {
	Settle string `json:"settle"`
	Stable string `json:"stable"`
	Delete string `json:"delete"`
}

type thresholdsDeclaration struct {
	ErrLoop *int `json:"errloop"`
	Quiet   *int `json:"quiet"`
}

// Load reads target.yaml at path. Paths inside it resolve against the file's
// own directory, except launch.binary, which resolves against the working
// directory.
func Load(path string) (*Target, error) {
	loaded, err := load(path)
	if err != nil {
		return nil, fmt.Errorf("loading target %s: %w", path, err)
	}
	return loaded, nil
}

func load(path string) (*Target, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var declared declaration
	if err := yaml.UnmarshalStrict(data, &declared); err != nil {
		if located := unknownKey(data); located != nil {
			return nil, located
		}
		return nil, err
	}
	dir := filepath.Dir(path)

	loaded := &Target{
		Name:    declared.Name,
		Version: declared.Version,
		Generate: GenerateSpec{
			Mutate:   declared.Generate.Mutate,
			Overlay:  declared.Generate.Overlay,
			Distinct: declared.Generate.Distinct,
		},
		Launch: declared.Launch,
	}
	if loaded.Name == "" {
		return nil, errors.New("name is required")
	}
	if maxCRs := declared.Generate.MaxCRs; maxCRs != nil {
		if *maxCRs < 1 {
			return nil, fmt.Errorf("generate.maxCRs %d: a sequence creates at least 1 CR", *maxCRs)
		}
		loaded.Generate.MaxCRs = *maxCRs
	}
	if declared.Generate.Faults != nil && !*declared.Generate.Faults {
		loaded.Generate.NoFaults = true
	}
	for _, crd := range declared.CRDs {
		crdPath := resolve(dir, crd)
		if _, err := os.Stat(crdPath); err != nil {
			return nil, fmt.Errorf("crds: %w", err)
		}
		loaded.CRDs = append(loaded.CRDs, crdPath)
	}

	if err := loadRBAC(dir, declared.RBAC, loaded); err != nil {
		return nil, err
	}

	if declared.Primary == "" {
		return nil, errors.New("primary is required")
	}
	if loaded.Primary, err = parseGVK(declared.Primary); err != nil {
		return nil, fmt.Errorf("primary %q: %w", declared.Primary, err)
	}
	for _, managed := range declared.Manages {
		gvk, err := parseGVK(managed)
		if err != nil {
			return nil, fmt.Errorf("manages %q: %w", managed, err)
		}
		loaded.Manages = append(loaded.Manages, gvk)
	}
	for _, kind := range declared.NotRecreated {
		gvk, err := parseGVK(kind)
		if err != nil {
			return nil, fmt.Errorf("notRecreated %q: %w", kind, err)
		}
		if !slices.Contains(loaded.Manages, gvk) {
			return nil, fmt.Errorf("notRecreated %q: the target does not list it under manages", kind)
		}
		loaded.NotRecreated = append(loaded.NotRecreated, gvk)
	}

	if declared.Sample == "" {
		return nil, errors.New("sample is required")
	}
	samplePath := resolve(dir, declared.Sample)
	sample, err := loadObjects(samplePath)
	if err != nil {
		return nil, fmt.Errorf("sample: %w", err)
	}
	if len(sample) != 1 {
		return nil, fmt.Errorf("sample %s: holds %d objects, want the primary CR alone", samplePath, len(sample))
	}
	loaded.Sample = sample[0]
	if gvk := loaded.Sample.GroupVersionKind(); gvk != loaded.Primary {
		return nil, fmt.Errorf("sample %s: holds %s, not the primary %s", samplePath, gvk, loaded.Primary)
	}
	if loaded.Sample.GetName() == "" {
		return nil, fmt.Errorf("sample %s: holds no metadata.name; give it one, since each CR a sequence creates is named after it", samplePath)
	}
	fileOf := map[*unstructured.Unstructured]string{}
	for _, fixture := range declared.Fixtures {
		objects, err := loadObjects(resolve(dir, fixture))
		if err != nil {
			return nil, fmt.Errorf("fixture: %w", err)
		}
		for _, object := range objects {
			fileOf[object] = resolve(dir, fixture)
		}
		loaded.Fixtures = append(loaded.Fixtures, objects...)
		if drawn, mutable := declared.Generate.Fixtures[fixture]; mutable {
			fixtures, err := mutableFixtures(fixture, objects, drawn.Mutate)
			if err != nil {
				return nil, err
			}
			loaded.Generate.Fixtures = append(loaded.Generate.Fixtures, fixtures...)
		}
	}
	for _, file := range slices.Sorted(maps.Keys(declared.Generate.Fixtures)) {
		if !slices.Contains(declared.Fixtures, file) {
			return nil, fmt.Errorf("generate.fixtures %s: fixtures lists no such file", file)
		}
	}
	crds, err := ReadCRDs(loaded.CRDs)
	if err != nil {
		return nil, fmt.Errorf("crds: %w", err)
	}
	scope := scopeAtLoad(crds)
	if err := loaded.checkScopes(scope); err != nil {
		if misplaced := (*misplacedFixture)(nil); errors.As(err, &misplaced) {
			return nil, fmt.Errorf("fixture %s: %w", fileOf[misplaced.fixture], err)
		}
		if clusterNS := (*clusterFixtureNamespace)(nil); errors.As(err, &clusterNS) {
			return nil, fmt.Errorf("fixture %s: %w", fileOf[clusterNS.fixture], err)
		}
		return nil, err
	}
	for _, fixture := range loaded.Fixtures {
		gvk := fixture.GroupVersionKind()
		if namespaced, known := scope(gvk); known && !namespaced && !slices.Contains(loaded.ClusterFixtures, gvk) {
			loaded.ClusterFixtures = append(loaded.ClusterFixtures, gvk)
		}
	}
	for _, file := range slices.Sorted(maps.Keys(declared.Generate.Fixtures)) {
		for _, fixture := range loaded.Fixtures {
			if fileOf[fixture] != resolve(dir, file) {
				continue
			}
			gvk := fixture.GroupVersionKind()
			namespaced, known := scope(gvk)
			if known && !namespaced {
				return nil, fmt.Errorf("generate.fixtures %s: the %s %s is cluster-scoped, and fixture ops would change what every run shares",
					file, observe.KindName(gvk), fixture.GetName())
			}
		}
	}

	if declared.Selector != "" {
		if loaded.Selector, err = labels.Parse(declared.Selector); err != nil {
			return nil, fmt.Errorf("selector %q: %w", declared.Selector, err)
		}
	}

	if declared.Ready == "" {
		declared.Ready = DefaultReady
	}
	loaded.ReadyExpr = declared.Ready
	if loaded.Ready, err = readyPredicate(declared.Ready); err != nil {
		return nil, fmt.Errorf("ready: %w", err)
	}
	if name, isHook := hookName(declared.Equal); isHook {
		if loaded.Equal, err = equalHook(name); err != nil {
			return nil, fmt.Errorf("equal: %w", err)
		}
	} else if declared.Equal != "" {
		return nil, fmt.Errorf("equal %q: equality is not CEL; it takes a go:<name> hook", declared.Equal)
	}
	for _, text := range declared.EqualIgnore {
		path, err := ParsePath(text)
		if err != nil {
			return nil, fmt.Errorf("equalIgnore %q: %w", text, err)
		}
		loaded.EqualIgnore = append(loaded.EqualIgnore, path)
	}
	if loaded.Equal != nil && len(loaded.EqualIgnore) > 0 {
		return nil, errors.New("equalIgnore does nothing beside an equal hook, which replaces the default equality")
	}

	seen := map[string]bool{}
	for _, declaredProperty := range declared.Properties {
		property, err := loadProperty(declaredProperty)
		if err != nil {
			return nil, err
		}
		if seen[property.ID] {
			return nil, fmt.Errorf("property %s: the id is declared twice", property.ID)
		}
		seen[property.ID] = true
		loaded.Properties = append(loaded.Properties, property)
	}

	if loaded.Launch.Binary == "" {
		return nil, errors.New("launch.binary is required")
	}
	if err := checkEnv(data, loaded.Launch.Env); err != nil {
		return nil, fmt.Errorf("launch.env: %w", err)
	}

	if loaded.Timeouts, err = timeouts(declared.Timeouts); err != nil {
		return nil, err
	}
	if loaded.Thresholds, err = thresholds(declared.Thresholds); err != nil {
		return nil, err
	}
	return loaded, nil
}

// mutableFixtures reads the strings generation may set in each object of a
// fixture file.
func mutableFixtures(file string, objects []*unstructured.Unstructured, declared []string) ([]MutableFixture, error) {
	var paths []Path
	for _, text := range declared {
		path, err := ParsePath(text)
		if err == nil && slices.ContainsFunc(path, func(step Step) bool { return step.Each }) {
			err = errors.New("name one string, not [*]")
		}
		if err != nil {
			return nil, fmt.Errorf("generate.fixtures %s: mutate %q: %w", file, text, err)
		}
		paths = append(paths, path)
	}
	fixtures := make([]MutableFixture, len(objects))
	for i, object := range objects {
		fixtures[i] = MutableFixture{GVK: object.GroupVersionKind(), Name: object.GetName(), Mutate: paths}
		if fixtures[i].Name == "" {
			return nil, fmt.Errorf("generate.fixtures %s: a %s there sets no metadata.name, and a fixture op names its fixture",
				file, observe.KindName(fixtures[i].GVK))
		}
		for _, path := range paths {
			if value, _, _ := unstructured.NestedFieldNoCopy(object.Object, path.Keys()...); !isString(value) {
				return nil, fmt.Errorf("generate.fixtures %s: the %s %s holds no string at %s",
					file, observe.KindName(fixtures[i].GVK), fixtures[i].Name, path)
			}
		}
	}
	return fixtures, nil
}

func isString(value any) bool {
	_, is := value.(string)
	return is
}

func readyPredicate(declared string) (ReadyFunc, error) {
	if name, isHook := hookName(declared); isHook {
		return readyHook(name)
	}
	return compileReady(declared)
}

func loadProperty(declared propertyDeclaration) (Property, error) {
	if declared.ID == "" {
		return Property{}, errors.New("property: id is required")
	}
	when := PropertyWhen(declared.When)
	switch when {
	case "":
		when = Checkpoint
	case Always, Checkpoint, End:
	default:
		return Property{}, fmt.Errorf("property %s: when %q is not one of %s, %s, %s",
			declared.ID, declared.When, Always, Checkpoint, End)
	}
	if declared.CEL == "" {
		return Property{}, fmt.Errorf("property %s: cel is required", declared.ID)
	}
	eval, err := compileProperty(declared.ID, declared.CEL)
	if err != nil {
		return Property{}, fmt.Errorf("property %s: %w", declared.ID, err)
	}
	return Property{ID: declared.ID, Description: declared.Description, Eval: eval, When: when}, nil
}

// checkEnv compares env, as decoded, with the text of target.yaml. Decoding
// reads an unquoted 0022 as the number 18 and ON as true.
func checkEnv(data []byte, env map[string]string) error {
	var written struct {
		Launch struct {
			Env map[string]string `yaml:"env"`
		} `yaml:"launch"`
	}
	if err := yamlv2.Unmarshal(data, &written); err != nil {
		return err
	}
	for _, name := range slices.Sorted(maps.Keys(written.Launch.Env)) {
		value, decoded := env[name]
		switch {
		case !decoded:
			return fmt.Errorf("YAML reads the name %s as something else; quote it", name)
		case value != written.Launch.Env[name]:
			return fmt.Errorf("YAML reads %s: %s as %s; quote the value", name, written.Launch.Env[name], value)
		case name == "KUBECONFIG":
			return errors.New("reconciler-fuzzer sets KUBECONFIG itself, to the kubeconfig it writes")
		case name == "" || strings.ContainsAny(name, "=\x00"):
			return fmt.Errorf("%q is not a variable name", name)
		case strings.Contains(value, "\x00"):
			return fmt.Errorf("the value of %s holds a NUL", name)
		}
	}
	if len(env) != len(written.Launch.Env) {
		return errors.New("write launch and env in lower case")
	}
	return nil
}

func timeouts(declared timeoutsDeclaration) (Timeouts, error) {
	parsed := DefaultTimeouts
	var err error
	if parsed.Settle, err = duration("settle", declared.Settle, parsed.Settle); err != nil {
		return Timeouts{}, err
	}
	if parsed.Stable, err = duration("stable", declared.Stable, parsed.Stable); err != nil {
		return Timeouts{}, err
	}
	if parsed.Delete, err = duration("delete", declared.Delete, parsed.Delete); err != nil {
		return Timeouts{}, err
	}
	// A settle wait ends once the Ready predicate holds and nothing has
	// changed for stable, within settle. Where stable is the wider of the
	// two, no wait can end that way, and every run reports G4 against a
	// target that did nothing wrong.
	if parsed.Stable >= parsed.Settle {
		return Timeouts{}, fmt.Errorf("timeouts: stable %s is not shorter than settle %s, and a settle wait has to observe stable of quiet inside settle",
			parsed.Stable, parsed.Settle)
	}
	return parsed, nil
}

func duration(field, declared string, fallback time.Duration) (time.Duration, error) {
	if declared == "" {
		return fallback, nil
	}
	parsed, err := time.ParseDuration(declared)
	if err != nil {
		return 0, fmt.Errorf("timeouts: %s %q: %w", field, declared, err)
	}
	if parsed <= 0 {
		return 0, fmt.Errorf("timeouts: %s %q must be positive", field, declared)
	}
	return parsed, nil
}

func thresholds(declared thresholdsDeclaration) (Thresholds, error) {
	parsed := DefaultThresholds
	if declared.ErrLoop != nil {
		parsed.ErrLoop = *declared.ErrLoop
	}
	if parsed.ErrLoop <= 0 {
		return Thresholds{}, fmt.Errorf("thresholds: errloop %d must be positive", parsed.ErrLoop)
	}
	if declared.Quiet != nil {
		parsed.Quiet = *declared.Quiet
	}
	if parsed.Quiet < 0 {
		return Thresholds{}, fmt.Errorf("thresholds: quiet %d must not be negative", parsed.Quiet)
	}
	return parsed, nil
}

// resolve makes path relative to dir, the directory holding target.yaml.
func resolve(dir, path string) string {
	if filepath.IsAbs(path) {
		return path
	}
	return filepath.Join(dir, path)
}

// apiVersion matches the version names Kubernetes accepts, so that a
// group/Kind typo does not read as a core-group version.
var apiVersion = regexp.MustCompile(`^v[1-9][0-9]*((alpha|beta)[1-9][0-9]*)?$`)

// parseGVK reads group/version/Kind, or version/Kind for the core group.
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

// loadObjects reads every document of a YAML file.
func loadObjects(path string) ([]*unstructured.Unstructured, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	documents := utilyaml.NewYAMLReader(bufio.NewReader(bytes.NewReader(data)))
	var objects []*unstructured.Unstructured
	for {
		document, err := documents.Read()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("%s: %w", path, err)
		}
		asJSON, err := yaml.YAMLToJSON(document)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", path, err)
		}
		if string(asJSON) == "null" { // a document holding only comments
			continue
		}
		object := &unstructured.Unstructured{}
		if err := object.UnmarshalJSON(asJSON); err != nil {
			return nil, fmt.Errorf("%s: %w", path, err)
		}
		objects = append(objects, object)
	}
	if len(objects) == 0 {
		return nil, fmt.Errorf("%s: holds no object", path)
	}
	return objects, nil
}
