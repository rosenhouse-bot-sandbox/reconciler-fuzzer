// Package target loads a target.yaml into a Target, which every other package
// consumes. Every error Load returns is a configuration error, which the CLI
// reports as exit code 2.
package target

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"time"

	rbacv1 "k8s.io/api/rbac/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime/schema"

	"github.com/rosenhouse-bot-sandbox/reconciler-fuzzer/internal/observe"
)

// ReadyFunc is the target's readiness predicate, which G4 judges. An error
// means "not ready"; it is an *EvalError, which a report quotes.
type ReadyFunc func(cr *unstructured.Unstructured) (bool, error)

// PropertyFunc evaluates a declared property over the primary CR and the
// managed objects. An error is a configuration error.
type PropertyFunc func(cr *unstructured.Unstructured, managed []*unstructured.Unstructured) (bool, error)

// EqualFunc compares snapshots taken around a Restart, for G5.
type EqualFunc func(a, b observe.Snapshot) bool

// PropertyWhen says where a property is evaluated.
type PropertyWhen string

const (
	Always     PropertyWhen = "always"
	Checkpoint PropertyWhen = "checkpoint"
	End        PropertyWhen = "end"
)

// Property is a per-target check, identified as P1..Pn.
type Property struct {
	ID, Description string
	Eval            PropertyFunc
	When            PropertyWhen
}

// GenerateSpec constrains sequence generation.
type GenerateSpec struct {
	// Mutate lists dotted paths the generator may change. Empty means every
	// schema path.
	Mutate []string
	// Overlay tightens the CRD schema at a dotted path.
	Overlay map[string]map[string]any
	// MaxCRs bounds the CRs a sequence creates. Zero takes the generator's
	// default.
	MaxCRs int
	// Distinct lists dotted paths at which no two CRs of a sequence hold one
	// value.
	Distinct []string
	// NoFaults disables generated fault injection. A hand-written sequence
	// still carries the faults it declares.
	NoFaults bool
	// Fixtures are the fixtures generation may delete, in the order the
	// target lists them.
	Fixtures []MutableFixture
}

// MutableFixture is a fixture generation may delete, and the strings in it
// generation may set.
type MutableFixture struct {
	GVK    schema.GroupVersionKind
	Name   string
	Mutate []Path
}

// LaunchSpec says how to run the target.
type LaunchSpec struct {
	// Binary is relative to the working directory, or a name on PATH.
	Binary string `json:"binary"`
	// Args and the values of Env carry $KUBECONFIG and $NAMESPACE wherever the
	// launcher must substitute the kubeconfig path and the run namespace.
	Args []string          `json:"args"`
	Env  map[string]string `json:"env"`
}

// Check reports a binary that reconciler-fuzzer could not exec.
func (l LaunchSpec) Check() error {
	_, err := exec.LookPath(l.Binary)
	switch {
	case err == nil:
		return nil
	case errors.Is(err, fs.ErrNotExist) && !filepath.IsAbs(l.Binary):
		wd, _ := os.Getwd()
		return fmt.Errorf("launch.binary: %w; the path is relative to the working directory %s, not to target.yaml", err, wd)
	default:
		return fmt.Errorf("launch.binary: %w", err)
	}
}

// Timeouts are the run's waits, as target.yaml's timeouts declare them.
type Timeouts struct{ Settle, Stable, Delete time.Duration }

// Thresholds hold thresholds.errloop for G6, and thresholds.quiet: the
// requests G1 and the status writes G2 allow in one quiet window.
type Thresholds struct{ ErrLoop, Quiet int }

// Defaults for a target that declares neither block.
var (
	DefaultTimeouts   = Timeouts{Settle: 30 * time.Second, Stable: 10 * time.Second, Delete: 60 * time.Second}
	DefaultThresholds = Thresholds{ErrLoop: 10}
)

// DefaultReady is the readiness predicate of a target that declares none.
const DefaultReady = "has(status.observedGeneration) && status.observedGeneration == metadata.generation"

// Target is a controller under test, its CRDs, and how to exercise it.
type Target struct {
	Name, Version string
	CRDs          []string
	Primary       schema.GroupVersionKind
	Sample        *unstructured.Unstructured
	Fixtures      []*unstructured.Unstructured
	// ClusterFixtures are the GVKs of fixtures that are cluster-scoped. A run
	// skips them (the invocation applies them once), and a sequence that
	// mutates or deletes one is a configuration error.
	ClusterFixtures []schema.GroupVersionKind
	Manages         []schema.GroupVersionKind
	Roles           []rbacv1.Role
	ClusterRoles    []rbacv1.ClusterRole
	// NotRecreated are the managed kinds the target leaves deleted, which G7
	// does not require back.
	NotRecreated []schema.GroupVersionKind
	Selector     labels.Selector
	Ready        ReadyFunc
	// ReadyExpr is the text Ready came from: the declared CEL, the default, or
	// go:<name>.
	ReadyExpr string
	// Equal is nil unless the target names a hook. The invariant engine then
	// applies the default equality together with EqualIgnore.
	Equal       EqualFunc
	EqualIgnore []Path
	Properties  []Property
	Generate    GenerateSpec
	Launch      LaunchSpec
	Timeouts    Timeouts
	Thresholds  Thresholds
}

// CheckScopes judges each kind of the target's by the scope the mapper serves
// it at. It leaves a kind the mapper does not know to the run.
func (t *Target) CheckScopes(mapper meta.RESTMapper) error {
	return t.checkScopes(func(gvk schema.GroupVersionKind) (bool, bool) {
		mapping, err := mapper.RESTMapping(gvk.GroupKind(), gvk.Version)
		if err != nil {
			return false, false
		}
		return mapping.Scope.Name() == meta.RESTScopeNameNamespace, true
	})
}

// checkScopes refuses cluster-scoped primary and managed kinds, but allows
// cluster-scoped fixtures. It then refuses a namespaced fixture that sets a
// namespace, and a cluster-scoped fixture that sets one.
func (t *Target) checkScopes(scope scopeFunc) error {
	clusterScoped := func(gvk schema.GroupVersionKind) bool {
		namespaced, known := scope(gvk)
		return known && !namespaced
	}
	var found []string
	if clusterScoped(t.Primary) {
		found = append(found, "the primary "+observe.KindName(t.Primary))
	}
	for _, gvk := range t.Manages {
		if clusterScoped(gvk) {
			found = append(found, "the managed "+observe.KindName(gvk))
		}
	}
	if len(found) > 0 {
		return fmt.Errorf("a run owns one namespace, so reconciler-fuzzer cannot test these cluster-scoped kinds: %s", strings.Join(found, ", "))
	}
	for _, fixture := range t.Fixtures {
		gvk := fixture.GroupVersionKind()
		switch {
		case clusterScoped(gvk) && fixture.GetNamespace() != "":
			return &clusterFixtureNamespace{fixture}
		case clusterScoped(gvk):
			// Cluster-scoped fixtures are allowed.
		default:
			if namespaced, _ := scope(gvk); namespaced && fixture.GetNamespace() != "" {
				return &misplacedFixture{fixture}
			}
		}
	}
	return nil
}

// misplacedFixture is a fixture of a namespaced kind that sets a namespace.
type misplacedFixture struct{ fixture *unstructured.Unstructured }

func (m *misplacedFixture) Error() string {
	namespace := m.fixture.GetNamespace()
	return fmt.Sprintf("the fixture %s %s sets metadata.namespace %s; drop it, because reconciler-fuzzer creates fixtures in each run's own namespace, and the target may look for this one in %s",
		observe.KindName(m.fixture.GroupVersionKind()), m.fixture.GetName(), namespace, namespace)
}

// clusterFixtureNamespace is a cluster-scoped fixture that sets a namespace.
type clusterFixtureNamespace struct{ fixture *unstructured.Unstructured }

func (c *clusterFixtureNamespace) Error() string {
	return fmt.Sprintf("the fixture %s %s sets metadata.namespace %s; an %s has no namespace; drop it",
		observe.KindName(c.fixture.GroupVersionKind()), c.fixture.GetName(), c.fixture.GetNamespace(), c.fixture.GetKind())
}

// WatchedKinds are the kinds reconciler-fuzzer watches: the primary CR and
// every managed kind. The collector resolves an owner only among them, and
// managed objects are commonly owned by the CR.
func (t *Target) WatchedKinds() []schema.GroupVersionKind {
	kinds := []schema.GroupVersionKind{t.Primary}
	for _, gvk := range t.Manages {
		if !slices.Contains(kinds, gvk) {
			kinds = append(kinds, gvk)
		}
	}
	return kinds
}
