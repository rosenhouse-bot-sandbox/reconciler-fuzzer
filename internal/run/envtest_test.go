//go:build envtest

package run_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
	corev1client "k8s.io/client-go/kubernetes/typed/core/v1"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"

	"github.com/rosenhouse-bot-sandbox/reconciler-fuzzer/internal/cluster"
	"github.com/rosenhouse-bot-sandbox/reconciler-fuzzer/internal/launch"
	"github.com/rosenhouse-bot-sandbox/reconciler-fuzzer/internal/observe"
	"github.com/rosenhouse-bot-sandbox/reconciler-fuzzer/internal/proxy"
	"github.com/rosenhouse-bot-sandbox/reconciler-fuzzer/internal/run"
	"github.com/rosenhouse-bot-sandbox/reconciler-fuzzer/internal/target"
)

const (
	repoRoot      = "../.."
	targetYAML    = repoRoot + "/targets/toy-widget/target.yaml"
	pollInterval  = 25 * time.Millisecond
	pollDeadline  = 30 * time.Second
	fixtureName   = "fixture"
	collectedName = "collected"
)

var (
	widgetResource = schema.GroupVersionResource{Group: "toy.reconciler-fuzzer", Version: "v1", Resource: "widgets"}
	configMapKind  = schema.GroupVersionKind{Version: "v1", Kind: "ConfigMap"}
)

// TestHarness shares one control plane between its cases, because each start
// costs seconds. The last case starts its own, which is what it is about.
func TestHarness(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	binary := buildToy(t)
	testCluster := startCluster(t, loadTarget(t, binary).CRDs)

	// The M2 acceptance criterion of DESIGN.md §10.
	t.Run("runs the toy target and records what it did", func(t *testing.T) {
		toy := loadTarget(t, binary)
		// The harness creates a fixture and marks it reconciler-fuzzer's, which keeps
		// it out of the managed objects (DESIGN.md §6).
		toy.Fixtures = append(toy.Fixtures, fixtureConfigMap())
		dir := t.TempDir()
		h := startHarness(t, ctx, toy, testCluster.Config(), dir)

		widget := createWidget(t, ctx, h, toy)
		requireSettled(t, ctx, h, toy)

		requireReconcileRecorded(t, h.Proxy.Log())
		requirePresent(t, ctx, h, fixtureName)
		requireChildren(t, h, widget, count(t, toy.Sample))
		requireWidgetReady(t, h, toy)

		if err := h.Stop(ctx); err != nil {
			t.Fatalf("Stopping the harness failed: %v", err)
		}
		requireJSONLines(t, filepath.Join(dir, "requests.jsonl"))
		requireJSONLines(t, filepath.Join(dir, "objects.jsonl"))
		requireTargetLog(t, filepath.Join(dir, "target.log"))
		// An empty file says the collector ran and tried no delete.
		if deletes, err := os.ReadFile(filepath.Join(dir, "collector.jsonl")); err != nil || len(deletes) > 0 {
			t.Errorf("collector.jsonl holds %q (%v), want it empty: nothing lost its owner.", deletes, err)
		}
	})

	// What the Runner of M3 builds on: a Restart that G5 compares across, and
	// a namespace that empties itself when the CR goes.
	t.Run("restarts the target and empties the namespace on a delete", func(t *testing.T) {
		toy := loadTarget(t, binary)
		dir := t.TempDir()
		h := startHarness(t, ctx, toy, testCluster.Config(), dir)

		widget := createWidget(t, ctx, h, toy)
		requireSettled(t, ctx, h, toy)
		// A ConfigMap reconciler-fuzzer owns to the Widget without controlling it.
		// The target only ever touches what it controls, so this one leaves the
		// namespace by the collector alone (DESIGN.md §5.8).
		collected := createCollectedConfigMap(t, ctx, h, widget)
		before := convergedState(t, h, toy)

		if err := h.Launcher.Restart(ctx); err != nil {
			t.Fatalf("Restarting the target failed: %v", err)
		}
		requireSettled(t, ctx, h, toy)

		if after := convergedState(t, h, toy); !maps.Equal(before, after) {
			t.Errorf("The restart changed the converged state from\n\t%v\nto\n\t%v", before, after)
		}

		deleted := time.Now()
		deleteWidget(t, ctx, h, widget)
		requireNamespaceEmptied(t, ctx, h, toy, widget)

		if err := h.Stop(ctx); err != nil {
			t.Fatalf("Stopping the harness failed: %v", err)
		}
		requireCollected(t, dir, collected, widget, deleted)
	})

	t.Run("hands the target the run namespace", func(t *testing.T) {
		sh := loadTarget(t, "/bin/sh")
		sh.Launch.Args = []string{"-c", `echo "arg=$1 env=$WATCH_NAMESPACE"; exec sleep 600`, "sh", "--namespace=$NAMESPACE"}
		sh.Launch.Env = map[string]string{"WATCH_NAMESPACE": "$NAMESPACE"}
		dir := t.TempDir()

		h := startHarness(t, ctx, sh, testCluster.Config(), dir)

		want := "arg=--namespace=" + h.Namespace + " env=" + h.Namespace + "\n"
		eventually(t, func() error {
			logged, err := os.ReadFile(filepath.Join(dir, "target.log"))
			if err != nil || string(logged) != want {
				return fmt.Errorf("target.log holds %q (%v), want %q", logged, err, want)
			}
			return nil
		})
		kubeconfig, err := clientcmd.LoadFromFile(filepath.Join(dir, "kubeconfig"))
		if err != nil {
			t.Fatal(err)
		}
		if namespace, _, err := clientcmd.NewDefaultClientConfig(*kubeconfig, nil).Namespace(); namespace != h.Namespace {
			t.Errorf("The kubeconfig names the namespace %q (%v), want %s.", namespace, err, h.Namespace)
		}
	})

	// An interrupt ends the run's context before its teardown stops the harness.
	t.Run("kills the target at once and deletes the namespace under a context that ended", func(t *testing.T) {
		sh := loadTarget(t, "/bin/sh")
		sh.Launch.Args = []string{"-c", `trap "" TERM; echo started; while :; do sleep 0.1; done`}
		dir := t.TempDir()
		h := startHarness(t, ctx, sh, testCluster.Config(), dir)
		eventually(t, func() error {
			if logged, err := os.ReadFile(filepath.Join(dir, "target.log")); !strings.Contains(string(logged), "started") {
				return fmt.Errorf("target.log holds %q (%v)", logged, err)
			}
			return nil
		})
		ended, cancel := context.WithCancel(ctx)
		cancel()

		began := time.Now()
		err := h.Stop(ended)

		if err != nil {
			t.Errorf("Stopping the harness failed: %v", err)
		}
		if took := time.Since(began); took >= launch.DefaultGracePeriod {
			t.Errorf("Stopping the harness took %v, want the target killed at once.", took)
		}
		requireNoRunNamespaceLive(t, ctx, testCluster.Config())
	})

	t.Run("takes back what it started when the target cannot start", func(t *testing.T) {
		toy := loadTarget(t, filepath.Join(t.TempDir(), "absent-binary"))

		h, err := run.Start(ctx, toy, run.Options{Dir: t.TempDir(), Config: testCluster.Config()})

		if h != nil || err == nil {
			t.Fatalf("Start returned (%v, %v), want no harness and an error.", h, err)
		}
		if !strings.Contains(err.Error(), "absent-binary") {
			t.Errorf("Start returned %q, want the binary it could not run named.", err)
		}
		if strings.Contains(err.Error(), "stopping") || strings.Contains(err.Error(), "deleting") {
			t.Errorf("Start returned %q, want a rollback that itself succeeded.", err)
		}
		requireNoRunNamespaceLive(t, ctx, testCluster.Config())
	})

	// envtest runs no controller manager, so it never adds kube-root-ca.crt.
	t.Run("ends when the controller manager adds nothing to the namespace", func(t *testing.T) {
		toy := loadTarget(t, binary)
		opts := run.Options{
			Dir:                     t.TempDir(),
			Config:                  testCluster.Config(),
			ControllerManager:       true,
			NamespaceDefaultsWithin: time.Second,
		}

		h, err := run.Start(ctx, toy, opts)

		if h != nil {
			_ = h.Stop(context.Background())
		}
		if err == nil || !strings.Contains(err.Error(), "v1/ConfigMap kube-root-ca.crt in the run namespace within 1s") {
			t.Fatalf("Start returned %v, want an error naming the v1/ConfigMap kube-root-ca.crt it waited 1s for.", err)
		}
		requireNoRunNamespaceLive(t, ctx, testCluster.Config())
	})

	// A run given no cluster starts one and installs the target's CRDs in it.
	// Everything the run then starts resolves those kinds, so nothing comes up
	// unless the run built its mapper after the install.
	t.Run("starts against a cluster of its own", func(t *testing.T) {
		toy := loadTarget(t, binary)

		h, err := run.Start(ctx, toy, run.Options{Dir: t.TempDir()})

		if err != nil {
			t.Fatalf("Starting the harness failed: %v", err)
		}
		if err := h.Stop(ctx); err != nil {
			t.Errorf("Stopping the harness failed: %v", err)
		}
	})
}

// requireNoRunNamespaceLive asserts that every namespace a run created has been
// deleted, which is how a teardown shows from outside.
func requireNoRunNamespaceLive(t *testing.T, ctx context.Context, config *rest.Config) {
	t.Helper()
	client, err := kubernetes.NewForConfig(config)
	if err != nil {
		t.Fatalf("Building a client failed: %v", err)
	}
	namespaces, err := client.CoreV1().Namespaces().List(ctx, metav1.ListOptions{})
	if err != nil {
		t.Fatalf("Listing the namespaces failed: %v", err)
	}
	for _, namespace := range namespaces.Items {
		// The prefix run.newNamespaceName gives a run namespace.
		if strings.HasPrefix(namespace.Name, "reconciler-fuzzer-run-") && namespace.DeletionTimestamp == nil {
			t.Errorf("The namespace %s is still live: the run did not take it back.", namespace.Name)
		}
	}
}

// buildToy builds the target the harness launches as a process.
func buildToy(t *testing.T) string {
	t.Helper()
	if _, err := exec.LookPath("go"); err != nil {
		t.Skipf("The toy target cannot be built without the go tool: %v", err)
	}
	binary := filepath.Join(t.TempDir(), "toy-widget")
	build := exec.Command("go", "build", "-o", binary, "./targets/toy-widget")
	build.Dir = repoRoot
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("Building the toy target failed: %v\n%s", err, out)
	}
	return binary
}

func loadTarget(t *testing.T, binary string) *target.Target {
	t.Helper()
	toy, err := target.Load(targetYAML)
	if err != nil {
		t.Fatalf("Loading the toy target failed: %v", err)
	}
	toy.Launch.Binary = binary // The test builds its own, rather than expecting bin/.
	return toy
}

// Each envtest test starts its own cluster and calls t.Parallel, so the
// package takes as long as its longest test.
func startCluster(t *testing.T, crds []string) *cluster.Cluster {
	t.Helper()
	c, err := cluster.Start(cluster.Options{CRDPaths: crds})
	if err != nil {
		t.Fatalf("Starting the test cluster failed: %v", err)
	}
	t.Cleanup(func() {
		if err := c.Stop(); err != nil {
			t.Errorf("Stopping the test cluster failed: %v", err)
		}
	})
	return c
}

func startHarness(t *testing.T, ctx context.Context, toy *target.Target, config *rest.Config, dir string) *run.Harness {
	t.Helper()
	h, err := run.Start(ctx, toy, run.Options{Dir: dir, Seed: 1, Config: config})
	if err != nil {
		t.Fatalf("Starting the harness failed: %v", err)
	}
	t.Cleanup(func() {
		// The test's context is already cancelled by now.
		if err := h.Stop(context.Background()); err != nil {
			t.Errorf("Stopping the harness failed: %v", err)
		}
	})
	return h
}

func requireSettled(t *testing.T, ctx context.Context, h *run.Harness, toy *target.Target) {
	t.Helper()
	converged, err := h.Settle(ctx, nil)
	if err != nil {
		t.Fatalf("The settle wait failed: %v", err)
	}
	if !converged {
		t.Fatalf("The run did not converge within T_settle of %v.", toy.Timeouts.Settle)
	}
}

// reconciler-fuzzer writes directly to the API server, never through the proxy
// (DESIGN.md §5.3).
func widgets(t *testing.T, h *run.Harness) dynamic.ResourceInterface {
	t.Helper()
	client, err := dynamic.NewForConfig(h.Config)
	if err != nil {
		t.Fatalf("Building reconciler-fuzzer's dynamic client failed: %v", err)
	}
	return client.Resource(widgetResource).Namespace(h.Namespace)
}

func configMaps(t *testing.T, h *run.Harness) corev1client.ConfigMapInterface {
	t.Helper()
	client, err := kubernetes.NewForConfig(h.Config)
	if err != nil {
		t.Fatalf("Building reconciler-fuzzer's client failed: %v", err)
	}
	return client.CoreV1().ConfigMaps(h.Namespace)
}

func createWidget(t *testing.T, ctx context.Context, h *run.Harness, toy *target.Target) *unstructured.Unstructured {
	t.Helper()
	widget := toy.Sample.DeepCopy()
	widget.SetNamespace(h.Namespace)
	created, err := widgets(t, h).Create(ctx, widget, metav1.CreateOptions{})
	if err != nil {
		t.Fatalf("Creating the sample Widget failed: %v", err)
	}
	return created
}

func deleteWidget(t *testing.T, ctx context.Context, h *run.Harness, widget *unstructured.Unstructured) {
	t.Helper()
	if err := widgets(t, h).Delete(ctx, widget.GetName(), metav1.DeleteOptions{}); err != nil {
		t.Fatalf("Deleting the Widget failed: %v", err)
	}
}

func fixtureConfigMap() *unstructured.Unstructured {
	fixture := &unstructured.Unstructured{Object: map[string]any{"data": map[string]any{"fixture": "true"}}}
	fixture.SetGroupVersionKind(configMapKind)
	fixture.SetName(fixtureName)
	return fixture
}

// fixtureSecret is of a kind the toy does not declare.
func fixtureSecret() *unstructured.Unstructured {
	fixture := &unstructured.Unstructured{}
	fixture.SetGroupVersionKind(schema.GroupVersionKind{Version: "v1", Kind: "Secret"})
	fixture.SetName(fixtureName)
	return fixture
}

func createCollectedConfigMap(t *testing.T, ctx context.Context, h *run.Harness, widget *unstructured.Unstructured) *corev1.ConfigMap {
	t.Helper()
	owned := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{
		Name: collectedName,
		OwnerReferences: []metav1.OwnerReference{{
			APIVersion: widget.GetAPIVersion(),
			Kind:       widget.GetKind(),
			Name:       widget.GetName(),
			UID:        widget.GetUID(),
		}},
	}}
	created, err := configMaps(t, h).Create(ctx, owned, metav1.CreateOptions{})
	if err != nil {
		t.Fatalf("Creating the owned ConfigMap failed: %v", err)
	}
	h.Observer.Exclude(configMapKind, collectedName)
	return created
}

// requireCollected asserts that collector.jsonl in dir records the collector's
// delete of collected after the deletion of widget. Any other line is a delete
// that lost a race and ended not found.
func requireCollected(t *testing.T, dir string, collected *corev1.ConfigMap, widget *unstructured.Unstructured, after time.Time) {
	t.Helper()
	written, err := os.ReadFile(filepath.Join(dir, "collector.jsonl"))
	if err != nil {
		t.Fatalf("The harness wrote no collector.jsonl: %v", err)
	}
	var deletes []map[string]any
	for decoder := json.NewDecoder(bytes.NewReader(written)); decoder.More(); {
		var line map[string]any
		if err := decoder.Decode(&line); err != nil {
			t.Fatalf("collector.jsonl does not parse: %v\n%s", err, written)
		}
		if line["result"] != "not found" {
			deletes = append(deletes, line)
		}
	}
	if len(deletes) != 1 {
		t.Fatalf("collector.jsonl holds\n%s\nwant one delete of %s, and at most lost races besides.", written, collectedName)
	}
	tried, err := time.Parse(time.RFC3339Nano, fmt.Sprint(deletes[0]["time"]))
	if err != nil || !tried.After(after) {
		t.Errorf("collector.jsonl says the delete was tried at %v (%v), want after the Widget's deletion at %v.",
			deletes[0]["time"], err, after)
	}
	delete(deletes[0], "time")
	want := map[string]any{
		"kind": "v1/ConfigMap", "namespace": collected.Namespace, "name": collectedName,
		"uid": string(collected.UID), "resourceVersion": collected.ResourceVersion,
		"owners": []any{map[string]any{
			"apiVersion": "toy.reconciler-fuzzer/v1", "kind": "Widget", "name": widget.GetName(), "uid": string(widget.GetUID()), "gone": "not found",
		}},
		"result": "deleted",
	}
	if !reflect.DeepEqual(deletes[0], want) {
		t.Errorf("collector.jsonl records\n\t%v\nwant\n\t%v", deletes[0], want)
	}
}

// requireReconcileRecorded asserts that the target's traffic reached the API
// server through the proxy, which is the signal G1 and G6 read.
func requireReconcileRecorded(t *testing.T, log []proxy.Request) {
	t.Helper()
	watched := map[string]bool{}
	created := 0
	for _, request := range log {
		switch {
		case request.Verb == "watch":
			watched[request.Resource] = true
		case request.Verb == "create" && request.Resource == "configmaps":
			created++
		}
	}
	for _, resource := range []string{"widgets", "configmaps"} {
		if !watched[resource] {
			t.Errorf("The request log holds no watch on %s: %v", resource, log)
		}
	}
	if created < 3 {
		t.Errorf("The request log holds %d creates of configmaps, want the Widget's 3 children.", created)
	}
}

func requirePresent(t *testing.T, ctx context.Context, h *run.Harness, name string) {
	t.Helper()
	if _, err := configMaps(t, h).Get(ctx, name, metav1.GetOptions{}); err != nil {
		t.Errorf("The run namespace holds no ConfigMap %s: %v", name, err)
	}
}

// requireChildren asserts what the Observer holds: one ConfigMap per index,
// controlled by the Widget, and nothing the target did not create.
func requireChildren(t *testing.T, h *run.Harness, widget *unstructured.Unstructured, want int64) {
	t.Helper()
	var names []string
	for _, child := range h.Observer.Managed() {
		names = append(names, child.Name)
		requireControlledBy(t, child, widget)
	}
	var wanted []string
	for index := range want {
		wanted = append(wanted, fmt.Sprintf("%s-%d", widget.GetName(), index))
	}
	if !slices.Equal(names, wanted) {
		t.Errorf("The Observer holds the managed objects %v, want %v.", names, wanted)
	}
}

func requireControlledBy(t *testing.T, child observe.Version, widget *unstructured.Unstructured) {
	t.Helper()
	controller := metav1.GetControllerOf(child.Object)
	if controller == nil || controller.UID != widget.GetUID() {
		t.Errorf("The ConfigMap %s carries the ownerReferences %v, want the Widget controlling it.",
			child.Name, child.OwnerReferences)
	}
}

func requireWidgetReady(t *testing.T, h *run.Harness, toy *target.Target) {
	t.Helper()
	observed := h.Observer.Current(toy.Primary)
	if len(observed) != 1 {
		t.Fatalf("The Observer holds %d Widgets, want the one reconciler-fuzzer created.", len(observed))
	}
	widget := observed[0].Object
	if ready, want := status(t, widget), count(t, toy.Sample); ready != want {
		t.Errorf("The observed Widget reports status.ready %d, want %d.", ready, want)
	}
	switch holds, err := toy.Ready(widget); {
	case err != nil:
		t.Errorf("The target's Ready predicate failed on the observed Widget: %v", err)
	case !holds:
		t.Errorf("The target's Ready predicate does not hold on the converged Widget %v.", widget.Object)
	}
}

// convergedState is what a Restart must not change: the managed objects with
// their identities, and what the CR reports.
func convergedState(t *testing.T, h *run.Harness, toy *target.Target) map[string]string {
	t.Helper()
	state := map[string]string{}
	for _, managed := range h.Observer.Managed() {
		state[managed.Name] = string(managed.UID)
	}
	for _, widget := range h.Observer.Current(toy.Primary) {
		state[widget.Name+".status.ready"] = fmt.Sprint(status(t, widget.Object))
	}
	return state
}

// requireNamespaceEmptied asserts the two cleanup paths of DESIGN.md §9: the
// finalizer takes the children the Widget controls, and the collector takes
// what only carries an ownerReference. The fixtures stay.
func requireNamespaceEmptied(t *testing.T, ctx context.Context, h *run.Harness, toy *target.Target, widget *unstructured.Unstructured) {
	t.Helper()
	eventually(t, func() error {
		remaining, err := configMaps(t, h).List(ctx, metav1.ListOptions{})
		if err != nil {
			return err
		}
		var names []string
		for _, configMap := range remaining.Items {
			if !slices.ContainsFunc(toy.Fixtures, func(fixture *unstructured.Unstructured) bool { return fixture.GetName() == configMap.Name }) {
				names = append(names, configMap.Name)
			}
		}
		if len(names) > 0 {
			return fmt.Errorf("the namespace still holds the ConfigMaps %v", names)
		}
		if _, err := widgets(t, h).Get(ctx, widget.GetName(), metav1.GetOptions{}); !apierrors.IsNotFound(err) {
			return fmt.Errorf("the Widget is still present: %v", err)
		}
		return nil
	})
}

// requireTargetLog asserts that the target's own output reached target.log
// (DESIGN.md §5.1).
func requireTargetLog(t *testing.T, path string) {
	t.Helper()
	log, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("Reading the target's log failed: %v", err)
	}
	if len(log) == 0 {
		t.Errorf("%s is empty: the target's output went elsewhere.", path)
	}
}

func requireJSONLines(t *testing.T, path string) {
	t.Helper()
	file, err := os.Open(path)
	if err != nil {
		t.Fatalf("Opening the recording failed: %v", err)
	}
	defer file.Close()
	lines := 0
	for decoder := json.NewDecoder(file); ; lines++ {
		var record map[string]any
		err := decoder.Decode(&record)
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatalf("Line %d of %s does not parse: %v", lines+1, path, err)
		}
	}
	if lines == 0 {
		t.Errorf("%s holds no record.", path)
	}
}

func count(t *testing.T, widget *unstructured.Unstructured) int64 {
	t.Helper()
	return field(t, widget, "spec", "count")
}

func status(t *testing.T, widget *unstructured.Unstructured) int64 {
	t.Helper()
	return field(t, widget, "status", "ready")
}

func field(t *testing.T, widget *unstructured.Unstructured, path ...string) int64 {
	t.Helper()
	value, found, err := unstructured.NestedInt64(widget.Object, path...)
	if err != nil || !found {
		t.Fatalf("The Widget carries no %v: found %t, %v", path, found, err)
	}
	return value
}

func eventually(t *testing.T, condition func() error) {
	t.Helper()
	deadline := time.Now().Add(pollDeadline)
	err := condition()
	for err != nil && time.Now().Before(deadline) {
		time.Sleep(pollInterval)
		err = condition()
	}
	if err != nil {
		t.Fatalf("The condition never held: %v", err)
	}
}
