//go:build envtest

package main

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/config"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"

	"github.com/rosenhouse-bot-sandbox/reconciler-fuzzer/internal/cluster"
	toyv1 "github.com/rosenhouse-bot-sandbox/reconciler-fuzzer/targets/toy-widget/api/v1"
	"github.com/rosenhouse-bot-sandbox/reconciler-fuzzer/targets/toy-widget/controller"
)

// settleInterval is long enough for the toy's workqueue to drain; it converges
// in milliseconds.
const settleInterval = time.Second

// TestSeededBugs runs each bug of DESIGN.md §9.1 against one control plane and
// asserts how it deviates from the correct controller. Each subtest owns a
// namespace, and the reconcilers of a subtest stop when it ends.
func TestSeededBugs(t *testing.T) {
	ctx := t.Context()
	testCluster := startCluster(t)
	c := uncachedClient(t, testCluster)

	t.Run("B1 reports its children ready before it creates them", func(t *testing.T) {
		namespace := createNamespace(t, ctx, c)
		runReconciler(t, testCluster, namespace, controller.B1, func(r *controller.Reconciler) { r.B1Hold = time.Second })

		widget := createWidgetIn(t, ctx, c, namespace, "w", 3)

		eventually(t, func() error {
			names, err := configMapNames(ctx, c, namespace)
			if err != nil {
				return err
			}
			if len(names) > 0 {
				return fmt.Errorf("the namespace already holds the ConfigMaps %v", names)
			}
			if ready := readWidget(t, ctx, c, widget).Status.Ready; ready != 3 {
				return fmt.Errorf("status.ready is %d, want the premature 3", ready)
			}
			return nil
		})
	})

	t.Run("B2 creates another set of children on every reconcile", func(t *testing.T) {
		namespace := createNamespace(t, ctx, c)
		widget := createWidgetIn(t, ctx, c, namespace, "w", 2)
		reconciler := directReconciler(t, testCluster, controller.B2)

		mustReconcile(t, reconciler, widget)
		mustReconcile(t, reconciler, widget)

		if names := mustListConfigMaps(t, ctx, c, namespace); len(names) != 4 {
			t.Errorf("Two reconciles left the ConfigMaps %v, want 4 of them for a count of 2.", names)
		}
	})

	t.Run("B3 orphans the first child and leaves it behind", func(t *testing.T) {
		namespace := createNamespace(t, ctx, c)
		runReconciler(t, testCluster, namespace, controller.B3)
		widget := createWidgetIn(t, ctx, c, namespace, "w", 2)

		requireConfigMaps(t, ctx, c, namespace, "w-0", "w-1")
		requireStatus(t, ctx, c, widget, 2) // The Widget converges: the orphan counts by name.
		if owner := metav1.GetControllerOf(readConfigMap(t, ctx, c, namespace, "w-0")); owner != nil {
			t.Errorf("w-0 is controlled by %v, want no ownerReference at all.", owner)
		}
		if owner := metav1.GetControllerOf(readConfigMap(t, ctx, c, namespace, "w-1")); owner == nil {
			t.Error("w-1 carries no ownerReference, want the Widget as its controller.")
		}

		if err := c.Delete(ctx, widget); err != nil {
			t.Fatal(err)
		}

		eventually(t, func() error {
			err := c.Get(ctx, client.ObjectKeyFromObject(widget), &toyv1.Widget{})
			if !apierrors.IsNotFound(err) {
				return fmt.Errorf("the Widget is still there: %v", err)
			}
			return nil
		})
		requireConfigMaps(t, ctx, c, namespace, "w-0")
	})

	t.Run("B4 takes the count from the status", func(t *testing.T) {
		namespace := createNamespace(t, ctx, c)
		widget := createWidgetIn(t, ctx, c, namespace, "w", 3)
		setReady(t, ctx, c, widget, 1)
		reconciler := directReconciler(t, testCluster, controller.B4)

		mustReconcile(t, reconciler, widget)

		if names, want := mustListConfigMaps(t, ctx, c, namespace), []string{"w-0"}; !slices.Equal(names, want) {
			t.Errorf("The reconcile left the ConfigMaps %v, want %v from status.ready.", names, want)
		}
	})

	t.Run("B5 requeues forever on the missing child", func(t *testing.T) {
		namespace := createNamespace(t, ctx, c)
		logs, _ := runReconciler(t, testCluster, namespace, controller.B5)

		createWidgetIn(t, ctx, c, namespace, "w", 1)

		eventually(t, func() error {
			if failures := strings.Count(logs.String(), "Reconciler error"); failures < 3 {
				return fmt.Errorf("the manager logged %d reconcile errors, want the loop to keep failing", failures)
			}
			if !strings.Contains(logs.String(), "not found") {
				return fmt.Errorf("the manager logged no NotFound:\n%s", logs.String())
			}
			return nil
		})
		if names := mustListConfigMaps(t, ctx, c, namespace); len(names) != 0 {
			t.Errorf("The reconciler left the ConfigMaps %v, want none: the Get never gives way to a create.", names)
		}
	})

	t.Run("B6 keeps re-triggering itself with a fresh lastSyncTime", func(t *testing.T) {
		namespace := createNamespace(t, ctx, c)
		runReconciler(t, testCluster, namespace, controller.B6)
		widget := createWidgetIn(t, ctx, c, namespace, "w", 1)
		requireChildren(t, ctx, c, widget, 1)

		// Nothing touches the Widget from here on, so only the controller's own
		// writes can move it on.
		converged := readWidget(t, ctx, c, widget)
		eventually(t, func() error {
			stamped := readWidget(t, ctx, c, widget)
			if stamped.Status.LastSyncTime == nil {
				return fmt.Errorf("status.lastSyncTime is unset")
			}
			if stamped.ResourceVersion == converged.ResourceVersion {
				return fmt.Errorf("the Widget rests at resourceVersion %s with lastSyncTime %v",
					converged.ResourceVersion, stamped.Status.LastSyncTime)
			}
			if converged.Status.LastSyncTime != nil && !stamped.Status.LastSyncTime.After(converged.Status.LastSyncTime.Time) {
				return fmt.Errorf("status.lastSyncTime is still %v", converged.Status.LastSyncTime)
			}
			return nil
		})
	})

	t.Run("B7 keeps the surplus children on scale down", func(t *testing.T) {
		namespace := createNamespace(t, ctx, c)
		runReconciler(t, testCluster, namespace, controller.B7)
		widget := createWidgetIn(t, ctx, c, namespace, "w", 3)
		requireChildren(t, ctx, c, widget, 3)

		setCount(t, ctx, c, widget, 1)

		requireObservedGeneration(t, ctx, c, widget)
		if names, want := mustListConfigMaps(t, ctx, c, namespace), []string{"w-0", "w-1", "w-2"}; !slices.Equal(names, want) {
			t.Errorf("Scaling down to 1 left the ConfigMaps %v, want %v.", names, want)
		}
		if ready := readWidget(t, ctx, c, widget).Status.Ready; ready != 3 {
			t.Errorf("status.ready is %d, want the 3 ConfigMaps that are still there.", ready)
		}
	})

	t.Run("B8 never notices a child deleted behind its back", func(t *testing.T) {
		namespace := createNamespace(t, ctx, c)
		runReconciler(t, testCluster, namespace, controller.B8)
		widget := createWidgetIn(t, ctx, c, namespace, "w", 1)
		requireChildren(t, ctx, c, widget, 1)
		requireSettled(t, ctx, c, widget)

		if err := c.Delete(ctx, readConfigMap(t, ctx, c, namespace, "w-0")); err != nil {
			t.Fatal(err)
		}

		// The correct controller recreates the child in milliseconds.
		consistently(t, 2*time.Second, func() error {
			err := c.Get(ctx, client.ObjectKey{Namespace: namespace, Name: "w-0"}, &corev1.ConfigMap{})
			if !apierrors.IsNotFound(err) {
				return fmt.Errorf("w-0 is back: %v", err)
			}
			return nil
		})
	})

	t.Run("B9 releases the Widget and leaves its children behind", func(t *testing.T) {
		namespace := createNamespace(t, ctx, c)
		runReconciler(t, testCluster, namespace, controller.B9)
		widget := createWidgetIn(t, ctx, c, namespace, "w", 2)
		requireConfigMaps(t, ctx, c, namespace, "w-0", "w-1")

		if err := c.Delete(ctx, widget); err != nil {
			t.Fatal(err)
		}

		eventually(t, func() error {
			err := c.Get(ctx, client.ObjectKeyFromObject(widget), &toyv1.Widget{})
			if !apierrors.IsNotFound(err) {
				return fmt.Errorf("the Widget is still there: %v", err)
			}
			return nil
		})
		for _, name := range []string{"w-0", "w-1"} {
			if owner := metav1.GetControllerOf(readConfigMap(t, ctx, c, namespace, name)); owner != nil {
				t.Errorf("%s is controlled by %v, want the orphan no path cleans up.", name, owner)
			}
		}
	})

	t.Run("B10 loses the status of a Widget it did not create children for", func(t *testing.T) {
		namespace := createNamespace(t, ctx, c)
		_, stopReconciler := runReconciler(t, testCluster, namespace, controller.B10)
		widget := createWidgetIn(t, ctx, c, namespace, "w", 3)
		requireChildren(t, ctx, c, widget, 3)
		requireStatus(t, ctx, c, widget, 3)

		stopReconciler() // The restart loses the in-memory flag.
		runReconciler(t, testCluster, namespace, controller.B10)
		setCount(t, ctx, c, widget, 1)

		requireChildren(t, ctx, c, widget, 1)
		consistently(t, 500*time.Millisecond, func() error {
			if ready := readWidget(t, ctx, c, widget).Status.Ready; ready != 3 {
				return fmt.Errorf("status.ready is %d, want it left stale at 3", ready)
			}
			return nil
		})
	})

	t.Run("B14 leaves its children on a label the ConfigMap no longer holds", func(t *testing.T) {
		for _, test := range []struct {
			name string
			bug  controller.Bug
		}{
			{"the correct controller", 0},
			{"B14", controller.B14},
		} {
			t.Run(test.name, func(t *testing.T) {
				namespace := createNamespace(t, ctx, c)
				config := &corev1.ConfigMap{
					ObjectMeta: metav1.ObjectMeta{Namespace: namespace, Name: "config"},
					Data:       map[string]string{"label": "red"},
				}
				if err := c.Create(ctx, config); err != nil {
					t.Fatal(err)
				}
				runReconciler(t, testCluster, namespace, test.bug, func(r *controller.Reconciler) { r.LabelFrom = "config" })
				widget := createWidgetIn(t, ctx, c, namespace, "w", 1)
				requireLabel(t, ctx, c, namespace, "red")
				requireSettled(t, ctx, c, widget)

				config.Data["label"] = "blue"
				if err := c.Update(ctx, config); err != nil {
					t.Fatal(err)
				}

				if test.bug == 0 {
					requireLabel(t, ctx, c, namespace, "blue")
					return
				}
				consistently(t, 2*time.Second, func() error {
					if label := readConfigMap(t, ctx, c, namespace, "w-0").Data["label"]; label != "red" {
						return fmt.Errorf("w-0 carries the label %q", label)
					}
					return nil
				})
			})
		}
	})

	t.Run("B13 keeps a deleted Widget and its children", func(t *testing.T) {
		namespace := createNamespace(t, ctx, c)
		runReconciler(t, testCluster, namespace, controller.B13)
		widget := createWidgetIn(t, ctx, c, namespace, "w", 1)
		requireChildren(t, ctx, c, widget, 1)
		requireSettled(t, ctx, c, widget)

		if err := c.Delete(ctx, widget); err != nil {
			t.Fatal(err)
		}

		// The correct controller releases the Widget in milliseconds.
		consistently(t, 2*time.Second, func() error {
			held := &toyv1.Widget{}
			if err := c.Get(ctx, client.ObjectKeyFromObject(widget), held); err != nil {
				return fmt.Errorf("reading the Widget: %w", err)
			}
			if !slices.Contains(held.Finalizers, controller.Finalizer) {
				return fmt.Errorf("the Widget carries the finalizers %v", held.Finalizers)
			}
			if names, err := configMapNames(ctx, c, namespace); err != nil || !slices.Equal(names, []string{"w-0"}) {
				return fmt.Errorf("the namespace holds the ConfigMaps %v (%v), want w-0", names, err)
			}
			return nil
		})
	})

	t.Run("B15 lets the first of two Widgets keep the ConfigMap both name", func(t *testing.T) {
		namespace := createNamespace(t, ctx, c)
		runReconciler(t, testCluster, namespace, controller.B15)
		first := createWidgetIn(t, ctx, c, namespace, "w", 1)
		requireStatus(t, ctx, c, first, 1)

		second := createWidgetIn(t, ctx, c, namespace, "w2", 1)

		consistently(t, 2*time.Second, func() error {
			if names, err := configMapNames(ctx, c, namespace); err != nil || !slices.Equal(names, []string{"widget-0"}) {
				return fmt.Errorf("the namespace holds the ConfigMaps %v (%v), want widget-0 alone", names, err)
			}
			if observed := readWidget(t, ctx, c, second).Status.ObservedGeneration; observed != 0 {
				return fmt.Errorf("w2 observed generation %d, want none: its reconcile fails", observed)
			}
			return nil
		})
	})
}

// runReconciler runs one seeded bug's reconciler over one namespace and returns
// the manager's log. The manager stops when the subtest ends, or when the
// returned function is called.
func runReconciler(t *testing.T, testCluster *cluster.Cluster, namespace string, bug controller.Bug, configure ...func(*controller.Reconciler)) (*managerLog, func()) {
	t.Helper()
	logs := &managerLog{}
	scheme, err := controller.NewScheme()
	if err != nil {
		t.Fatal(err)
	}
	sharedName := true // Every bug runs the same controller in this one process.
	manager, err := ctrl.NewManager(testCluster.Config(), ctrl.Options{
		Scheme:     scheme,
		Metrics:    metricsserver.Options{BindAddress: "0"},
		Logger:     zap.New(zap.WriteTo(logs)),
		Cache:      cache.Options{DefaultNamespaces: map[string]cache.Config{namespace: {}}},
		Controller: config.Controller{SkipNameValidation: &sharedName},
	})
	if err != nil {
		t.Fatalf("Creating the manager for B%d failed: %v", bug, err)
	}
	reconciler := &controller.Reconciler{
		Client:    manager.GetClient(),
		APIReader: manager.GetAPIReader(),
		Scheme:    manager.GetScheme(),
		Bug:       bug,
	}
	for _, apply := range configure {
		apply(reconciler)
	}
	if err := reconciler.SetupWithManager(manager); err != nil {
		t.Fatalf("Setting up B%d failed: %v", bug, err)
	}

	ctx, cancel := context.WithCancel(t.Context())
	stopped := make(chan error, 1)
	go func() { stopped <- manager.Start(ctx) }()
	stop := sync.OnceFunc(func() {
		cancel()
		if err := <-stopped; err != nil {
			t.Errorf("The manager for B%d returned an error: %v", bug, err)
		}
	})
	t.Cleanup(stop)
	return logs, stop
}

// directReconciler returns a reconciler without a manager, so that a test
// controls how often it runs.
func directReconciler(t *testing.T, testCluster *cluster.Cluster, bug controller.Bug) *controller.Reconciler {
	t.Helper()
	c := uncachedClient(t, testCluster)
	return &controller.Reconciler{Client: c, APIReader: c, Scheme: c.Scheme(), Bug: bug}
}

func mustReconcile(t *testing.T, r *controller.Reconciler, widget *toyv1.Widget) {
	t.Helper()
	if _, err := r.Reconcile(t.Context(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(widget)}); err != nil {
		t.Fatalf("Reconcile returned an error: %v", err)
	}
}

func readWidget(t *testing.T, ctx context.Context, c client.Client, widget *toyv1.Widget) *toyv1.Widget {
	t.Helper()
	observed := &toyv1.Widget{}
	if err := c.Get(ctx, client.ObjectKeyFromObject(widget), observed); err != nil {
		t.Fatal(err)
	}
	return observed
}

func readConfigMap(t *testing.T, ctx context.Context, c client.Client, namespace, name string) *corev1.ConfigMap {
	t.Helper()
	configMap := &corev1.ConfigMap{}
	if err := c.Get(ctx, client.ObjectKey{Namespace: namespace, Name: name}, configMap); err != nil {
		t.Fatal(err)
	}
	return configMap
}

func setReady(t *testing.T, ctx context.Context, c client.Client, widget *toyv1.Widget, ready int32) {
	t.Helper()
	patched := widget.DeepCopy()
	patched.Status.Ready = ready
	if err := c.Status().Patch(ctx, patched, client.MergeFrom(widget)); err != nil {
		t.Fatalf("Seeding status.ready with %d failed: %v", ready, err)
	}
}

// configMapNames returns the sorted names of the ConfigMaps in a namespace.
func configMapNames(ctx context.Context, c client.Client, namespace string) ([]string, error) {
	configMaps := &corev1.ConfigMapList{}
	if err := c.List(ctx, configMaps, client.InNamespace(namespace)); err != nil {
		return nil, err
	}
	names := make([]string, 0, len(configMaps.Items))
	for _, configMap := range configMaps.Items {
		names = append(names, configMap.Name)
	}
	slices.Sort(names)
	return names, nil
}

func mustListConfigMaps(t *testing.T, ctx context.Context, c client.Client, namespace string) []string {
	t.Helper()
	names, err := configMapNames(ctx, c, namespace)
	if err != nil {
		t.Fatal(err)
	}
	return names
}

func requireConfigMaps(t *testing.T, ctx context.Context, c client.Client, namespace string, want ...string) {
	t.Helper()
	eventually(t, func() error {
		names, err := configMapNames(ctx, c, namespace)
		if err != nil {
			return err
		}
		if !slices.Equal(names, want) {
			return fmt.Errorf("the namespace holds the ConfigMaps %v, want %v", names, want)
		}
		return nil
	})
}

// requireLabel waits until the child w-0 carries the label.
func requireLabel(t *testing.T, ctx context.Context, c client.Client, namespace, want string) {
	t.Helper()
	eventually(t, func() error {
		child := &corev1.ConfigMap{}
		if err := c.Get(ctx, client.ObjectKey{Namespace: namespace, Name: "w-0"}, child); err != nil {
			return err
		}
		if label := child.Data["label"]; label != want {
			return fmt.Errorf("w-0 carries the label %q, want %q", label, want)
		}
		return nil
	})
}

// requireObservedGeneration waits until the controller has acted on the
// Widget's current generation.
func requireObservedGeneration(t *testing.T, ctx context.Context, c client.Client, widget *toyv1.Widget) {
	t.Helper()
	eventually(t, func() error {
		observed := readWidget(t, ctx, c, widget)
		if observed.Status.ObservedGeneration != observed.Generation {
			return fmt.Errorf("status.observedGeneration is %d, want the generation %d",
				observed.Status.ObservedGeneration, observed.Generation)
		}
		return nil
	})
}

// requireSettled waits for the reconciles the Widget's own writes queued to
// drain, so that nothing is left in flight to act on a later change.
func requireSettled(t *testing.T, ctx context.Context, c client.Client, widget *toyv1.Widget) {
	t.Helper()
	requireObservedGeneration(t, ctx, c, widget)
	time.Sleep(settleInterval)
}

// consistently fails as soon as check fails, and returns once it has held for d.
func consistently(t *testing.T, d time.Duration, check func() error) {
	t.Helper()
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		if err := check(); err != nil {
			t.Fatalf("The condition stopped holding: %v", err)
		}
		time.Sleep(pollInterval)
	}
}
