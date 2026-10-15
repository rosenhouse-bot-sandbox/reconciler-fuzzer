//go:build envtest

package main

import (
	"context"
	"fmt"
	"maps"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/util/retry"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"

	"github.com/rosenhouse-bot-sandbox/reconciler-fuzzer/internal/cluster"
	toyv1 "github.com/rosenhouse-bot-sandbox/reconciler-fuzzer/targets/toy-widget/api/v1"
	"github.com/rosenhouse-bot-sandbox/reconciler-fuzzer/targets/toy-widget/controller"
)

const (
	pollInterval = 25 * time.Millisecond
	pollDeadline = 20 * time.Second
)

// TestWidgetController exercises the controller against one control plane,
// because each start costs seconds.
func TestWidgetController(t *testing.T) {
	ctx := t.Context()
	c, logs := startController(t, ctx)

	// The lifecycle subtests share one Widget and run in order, driving it
	// through the operations the harness performs.
	t.Run("lifecycle", func(t *testing.T) {
		widget := createWidget(t, ctx, c, 3)

		t.Run("creates one controlled ConfigMap per count", func(t *testing.T) {
			requireChildren(t, ctx, c, widget, 3)
			requireStatus(t, ctx, c, widget, 3)
		})

		t.Run("creates the missing children on scale up", func(t *testing.T) {
			setCount(t, ctx, c, widget, 5)

			requireChildren(t, ctx, c, widget, 5)
			requireStatus(t, ctx, c, widget, 5)
		})

		t.Run("deletes the surplus children on scale down", func(t *testing.T) {
			setCount(t, ctx, c, widget, 1)

			requireChildren(t, ctx, c, widget, 1)
			requireStatus(t, ctx, c, widget, 1)
		})

		t.Run("recreates a child deleted behind its back", func(t *testing.T) {
			deleted := &corev1.ConfigMap{}
			key := client.ObjectKey{Namespace: widget.Namespace, Name: widget.Name + "-0"}
			if err := c.Get(ctx, key, deleted); err != nil {
				t.Fatal(err)
			}
			if err := c.Delete(ctx, deleted); err != nil {
				t.Fatal(err)
			}

			eventually(t, func() error {
				recreated := &corev1.ConfigMap{}
				if err := c.Get(ctx, key, recreated); err != nil {
					return err
				}
				if recreated.UID == deleted.UID {
					return fmt.Errorf("ConfigMap %s is still the deleted one", key.Name)
				}
				return nil
			})
			requireChildren(t, ctx, c, widget, 1)
		})

		t.Run("holds no children at count zero", func(t *testing.T) {
			setCount(t, ctx, c, widget, 0)

			requireChildren(t, ctx, c, widget, 0)
			requireStatus(t, ctx, c, widget, 0)
			requireStatusFields(t, ctx, c, widget)
		})

		t.Run("deletes its children and itself on delete", func(t *testing.T) {
			setCount(t, ctx, c, widget, 2)
			requireChildren(t, ctx, c, widget, 2)

			if err := c.Delete(ctx, widget); err != nil {
				t.Fatal(err)
			}

			requireGone(t, ctx, c, widget)
			requireChildren(t, ctx, c, widget, 0)
		})
	})

	// A Widget born at count zero is ready the moment its status lands, and a
	// target is only ready when every field its predicate reads is there (§8.1).
	t.Run("writes status.ready for a Widget created at count zero", func(t *testing.T) {
		widget := createWidget(t, ctx, c, 0)

		requireStatus(t, ctx, c, widget, 0)
		requireStatusFields(t, ctx, c, widget)
	})

	t.Run("controls only its own children", func(t *testing.T) {
		namespace := createNamespace(t, ctx, c)
		mine := createWidgetIn(t, ctx, c, namespace, "mine", 2)
		theirs := createWidgetIn(t, ctx, c, namespace, "theirs", 3)
		requireControlledChildren(t, ctx, c, mine, 2)
		requireControlledChildren(t, ctx, c, theirs, 3)
		requireStatus(t, ctx, c, theirs, 3)
		undisturbed, err := controlledChildren(ctx, c, theirs)
		if err != nil {
			t.Fatal(err)
		}

		if err := c.Delete(ctx, mine); err != nil {
			t.Fatal(err)
		}

		requireGone(t, ctx, c, mine)
		requireChildren(t, ctx, c, theirs, 3)
		requireStatus(t, ctx, c, theirs, 3)
		intact, err := controlledChildren(ctx, c, theirs)
		if err != nil {
			t.Fatal(err)
		}
		if !maps.Equal(intact, undisturbed) {
			t.Errorf("The other Widget's ConfigMaps are now %v, want the untouched %v", intact, undisturbed)
		}
	})

	t.Run("rejects a count above the maximum", func(t *testing.T) {
		requireCountRejected(t, ctx, c, 11)
	})

	t.Run("rejects a negative count", func(t *testing.T) {
		requireCountRejected(t, ctx, c, -1)
	})

	// A reconcile that conflicts with its own last write still converges, so
	// only the log shows it.
	t.Run("logs no reconcile error", func(t *testing.T) {
		if logged := logs.String(); strings.Contains(logged, "Reconciler error") {
			t.Errorf("The manager logged a reconcile error:\n%s", logged)
		}
	})
}

// startController brings up a control plane holding the Widget CRD, runs the
// reconciler against it, and returns a client that bypasses the controller's
// cache along with the manager's log.
func startController(t *testing.T, ctx context.Context) (client.Client, *managerLog) {
	t.Helper()
	logs := &managerLog{}
	ctrl.SetLogger(zap.New(zap.WriteTo(logs)))

	testCluster := startCluster(t)
	scheme, err := controller.NewScheme()
	if err != nil {
		t.Fatal(err)
	}
	manager, err := ctrl.NewManager(testCluster.Config(), ctrl.Options{
		Scheme:  scheme,
		Metrics: metricsserver.Options{BindAddress: "0"},
		Logger:  zap.New(zap.WriteTo(logs)),
	})
	if err != nil {
		t.Fatalf("Creating the manager failed: %v", err)
	}
	reconciler := &controller.Reconciler{
		Client:    manager.GetClient(),
		APIReader: manager.GetAPIReader(),
		Scheme:    manager.GetScheme(),
	}
	if err := reconciler.SetupWithManager(manager); err != nil {
		t.Fatalf("Setting up the controller failed: %v", err)
	}

	stopped := make(chan error, 1)
	go func() { stopped <- manager.Start(ctx) }()
	t.Cleanup(func() {
		if err := <-stopped; err != nil {
			t.Errorf("The manager returned an error: %v", err)
		}
	})

	return uncachedClient(t, testCluster), logs
}

// startCluster brings up a control plane holding the Widget CRD.
func startCluster(t *testing.T) *cluster.Cluster {
	t.Helper()
	testCluster, err := cluster.Start(cluster.Options{CRDPaths: []string{"crds"}})
	if err != nil {
		t.Fatalf("Starting the test cluster failed: %v", err)
	}
	t.Cleanup(func() {
		if err := testCluster.Stop(); err != nil {
			t.Errorf("Stopping the test cluster failed: %v", err)
		}
	})
	return testCluster
}

// uncachedClient returns a client that bypasses every controller cache.
func uncachedClient(t *testing.T, testCluster *cluster.Cluster) client.Client {
	t.Helper()
	scheme, err := controller.NewScheme()
	if err != nil {
		t.Fatal(err)
	}
	c, err := client.New(testCluster.Config(), client.Options{Scheme: scheme})
	if err != nil {
		t.Fatalf("Building a client failed: %v", err)
	}
	return c
}

// managerLog collects what the manager writes from its own goroutines.
type managerLog struct {
	mu      sync.Mutex
	written strings.Builder
}

func (l *managerLog) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.written.Write(p)
}

func (l *managerLog) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.written.String()
}

func createNamespace(t *testing.T, ctx context.Context, c client.Client) string {
	t.Helper()
	namespace := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{GenerateName: "widget-"}}
	if err := c.Create(ctx, namespace); err != nil {
		t.Fatalf("Creating a namespace failed: %v", err)
	}
	return namespace.Name
}

func widgetIn(namespace, name string, count int32) *toyv1.Widget {
	return &toyv1.Widget{
		ObjectMeta: metav1.ObjectMeta{Namespace: namespace, Name: name},
		Spec:       toyv1.WidgetSpec{Count: count},
	}
}

func createWidgetIn(t *testing.T, ctx context.Context, c client.Client, namespace, name string, count int32) *toyv1.Widget {
	t.Helper()
	widget := widgetIn(namespace, name, count)
	if err := c.Create(ctx, widget); err != nil {
		t.Fatalf("Creating the Widget %s failed: %v", name, err)
	}
	return widget
}

// createWidget puts a Widget in a namespace of its own.
func createWidget(t *testing.T, ctx context.Context, c client.Client, count int32) *toyv1.Widget {
	t.Helper()
	return createWidgetIn(t, ctx, c, createNamespace(t, ctx, c), "w", count)
}

func setCount(t *testing.T, ctx context.Context, c client.Client, widget *toyv1.Widget, count int32) {
	t.Helper()
	err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
		if err := c.Get(ctx, client.ObjectKeyFromObject(widget), widget); err != nil {
			return err
		}
		widget.Spec.Count = count
		return c.Update(ctx, widget)
	})
	if err != nil {
		t.Fatalf("Setting spec.count to %d failed: %v", count, err)
	}
}

func requireCountRejected(t *testing.T, ctx context.Context, c client.Client, count int32) {
	t.Helper()
	widget := widgetIn(createNamespace(t, ctx, c), "w", count)
	if err := c.Create(ctx, widget); !apierrors.IsInvalid(err) {
		t.Errorf("Creating a Widget with count %d returned %v, want the API server to reject it.", count, err)
	}
}

func requireChildren(t *testing.T, ctx context.Context, c client.Client, widget *toyv1.Widget, count int32) {
	t.Helper()
	eventually(t, func() error { return childrenMatch(ctx, c, widget, count) })
}

// childrenMatch reports whether the Widget's namespace holds exactly the
// ConfigMaps its count requires.
func childrenMatch(ctx context.Context, c client.Client, widget *toyv1.Widget, count int32) error {
	configMaps := &corev1.ConfigMapList{}
	if err := c.List(ctx, configMaps, client.InNamespace(widget.Namespace)); err != nil {
		return err
	}
	byName := map[string]corev1.ConfigMap{}
	for _, configMap := range configMaps.Items {
		byName[configMap.Name] = configMap
	}

	want := childNames(widget, count)
	if got := slices.Sorted(maps.Keys(byName)); !slices.Equal(got, want) {
		return fmt.Errorf("the namespace holds the ConfigMaps %v, want %v", got, want)
	}
	for index, name := range want {
		if err := childMatches(byName[name], widget, index); err != nil {
			return err
		}
	}
	return nil
}

// requireControlledChildren waits until the Widget controls exactly the
// ConfigMaps its count requires, whatever else shares the namespace.
func requireControlledChildren(t *testing.T, ctx context.Context, c client.Client, widget *toyv1.Widget, count int32) {
	t.Helper()
	want := childNames(widget, count)
	eventually(t, func() error {
		controlled, err := controlledChildren(ctx, c, widget)
		if err != nil {
			return err
		}
		if got := slices.Sorted(maps.Keys(controlled)); !slices.Equal(got, want) {
			return fmt.Errorf("the Widget %s controls the ConfigMaps %v, want %v", widget.Name, got, want)
		}
		return nil
	})
}

func childNames(widget *toyv1.Widget, count int32) []string {
	names := make([]string, count)
	for index := range count {
		names[index] = fmt.Sprintf("%s-%d", widget.Name, index)
	}
	return names
}

// controlledChildren returns the UIDs, by name, of the ConfigMaps the Widget
// controls.
func controlledChildren(ctx context.Context, c client.Client, widget *toyv1.Widget) (map[string]types.UID, error) {
	configMaps := &corev1.ConfigMapList{}
	if err := c.List(ctx, configMaps, client.InNamespace(widget.Namespace)); err != nil {
		return nil, err
	}
	controlled := map[string]types.UID{}
	for _, configMap := range configMaps.Items {
		if owner := metav1.GetControllerOf(&configMap); owner != nil && owner.UID == widget.UID {
			controlled[configMap.Name] = configMap.UID
		}
	}
	return controlled, nil
}

func childMatches(child corev1.ConfigMap, widget *toyv1.Widget, index int) error {
	if want := map[string]string{"index": strconv.Itoa(index)}; !maps.Equal(child.Data, want) {
		return fmt.Errorf("ConfigMap %s holds the data %v, want %v", child.Name, child.Data, want)
	}
	want := metav1.OwnerReference{
		APIVersion: toyv1.GroupVersion.String(),
		Kind:       "Widget",
		Name:       widget.Name,
		UID:        widget.UID,
	}
	owner := metav1.GetControllerOf(&child)
	if owner == nil || owner.APIVersion != want.APIVersion || owner.Kind != want.Kind ||
		owner.Name != want.Name || owner.UID != want.UID {
		return fmt.Errorf("ConfigMap %s carries the ownerReferences %v, want %v as its controller",
			child.Name, child.OwnerReferences, want)
	}
	return nil
}

func requireStatus(t *testing.T, ctx context.Context, c client.Client, widget *toyv1.Widget, ready int32) {
	t.Helper()
	eventually(t, func() error {
		observed := &toyv1.Widget{}
		if err := c.Get(ctx, client.ObjectKeyFromObject(widget), observed); err != nil {
			return err
		}
		if observed.Status.Ready != ready {
			return fmt.Errorf("status.ready is %d, want %d", observed.Status.Ready, ready)
		}
		if observed.Status.ObservedGeneration != observed.Generation {
			return fmt.Errorf("status.observedGeneration is %d, want the generation %d",
				observed.Status.ObservedGeneration, observed.Generation)
		}
		if !slices.Contains(observed.Finalizers, controller.Finalizer) {
			return fmt.Errorf("the Widget carries the finalizers %v, want %s",
				observed.Finalizers, controller.Finalizer)
		}
		return nil
	})
}

// requireStatusFields guards the ready predicate of DESIGN.md §9, which reaches
// for status.ready and status.observedGeneration behind has(). A zero that the
// API drops is a Widget that never becomes ready, and a field a seeded bug
// writes does not belong here.
func requireStatusFields(t *testing.T, ctx context.Context, c client.Client, widget *toyv1.Widget) {
	t.Helper()
	observed := &unstructured.Unstructured{}
	observed.SetGroupVersionKind(toyv1.GroupVersion.WithKind("Widget"))
	if err := c.Get(ctx, client.ObjectKeyFromObject(widget), observed); err != nil {
		t.Fatal(err)
	}
	status, _, err := unstructured.NestedMap(observed.Object, "status")
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"observedGeneration", "ready"}
	if got := slices.Sorted(maps.Keys(status)); !slices.Equal(got, want) {
		t.Errorf("The stored status holds the fields %v, want %v.", got, want)
	}
}

// requireGone waits for the Widget and every ConfigMap it controls to disappear.
func requireGone(t *testing.T, ctx context.Context, c client.Client, widget *toyv1.Widget) {
	t.Helper()
	eventually(t, func() error {
		err := c.Get(ctx, client.ObjectKeyFromObject(widget), &toyv1.Widget{})
		if !apierrors.IsNotFound(err) {
			return fmt.Errorf("the Widget %s is still there: %v", widget.Name, err)
		}
		left, err := controlledChildren(ctx, c, widget)
		if err != nil {
			return err
		}
		if len(left) > 0 {
			return fmt.Errorf("the Widget %s still controls the ConfigMaps %v",
				widget.Name, slices.Sorted(maps.Keys(left)))
		}
		return nil
	})
}

// eventually polls check until it passes or the deadline expires.
func eventually(t *testing.T, check func() error) {
	t.Helper()
	deadline := time.Now().Add(pollDeadline)
	for {
		err := check()
		if err == nil {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("The condition never held: %v", err)
		}
		time.Sleep(pollInterval)
	}
}
