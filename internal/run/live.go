package run

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"slices"
	"strings"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/util/retry"

	"github.com/rosenhouse-bot-sandbox/reconciler-fuzzer/internal/cluster"
	"github.com/rosenhouse-bot-sandbox/reconciler-fuzzer/internal/launch"
	"github.com/rosenhouse-bot-sandbox/reconciler-fuzzer/internal/observe"
	"github.com/rosenhouse-bot-sandbox/reconciler-fuzzer/internal/proxy"
	"github.com/rosenhouse-bot-sandbox/reconciler-fuzzer/internal/target"
)

// clearFinalizers is the merge patch the teardown forces on an object whose
// finalizers never cleared.
var clearFinalizers = []byte(`{"metadata":{"finalizers":null}}`)

// liveRun is the Runner's harness over a started run. Its writes go straight
// to the API server, never through the proxy.
type liveRun struct {
	h         *Harness
	target    *target.Target
	client    dynamic.Interface
	resources map[schema.GroupVersionKind]schema.GroupVersionResource
	// emptied are the kinds the teardown deletes and forces finalizers off:
	// the target's and its fixtures'.
	emptied []schema.GroupVersionKind
}

var _ harness = (*liveRun)(nil)

func newLiveRun(h *Harness, t *target.Target) (*liveRun, error) {
	client, err := dynamic.NewForConfig(h.Config)
	if err != nil {
		return nil, fmt.Errorf("building reconciler-fuzzer's dynamic client: %w", err)
	}
	emptied := t.WatchedKinds()
	for _, fixture := range t.Fixtures {
		gvk := fixture.GroupVersionKind()
		if slices.Contains(t.ClusterFixtures, gvk) {
			continue // Cluster-scoped fixtures are cleaned up per invocation, not per run.
		}
		if !slices.Contains(emptied, gvk) {
			emptied = append(emptied, gvk)
		}
	}
	resources := map[schema.GroupVersionKind]schema.GroupVersionResource{}
	for _, gvk := range emptied {
		mapping, err := h.mapper.RESTMapping(gvk.GroupKind(), gvk.Version)
		if err != nil {
			return nil, fmt.Errorf("resolving the resource of %s: %w", observe.KindName(gvk), err)
		}
		resources[gvk] = mapping.Resource
	}
	return &liveRun{h: h, target: t, client: client, resources: resources, emptied: emptied}, nil
}

func (l *liveRun) of(gvk schema.GroupVersionKind) dynamic.ResourceInterface {
	return l.client.Resource(l.resources[gvk]).Namespace(l.h.Namespace)
}

func (l *liveRun) crs() dynamic.ResourceInterface { return l.of(l.target.Primary) }

func (l *liveRun) namespace() string { return l.h.Namespace }

func (l *liveRun) now() time.Time { return time.Now() }

func (l *liveRun) settle(ctx context.Context, owed func() time.Time, floor time.Time) (bool, error) {
	return l.h.settleFrom(ctx, owed, floor)
}

func (l *liveRun) sleep(ctx context.Context, d time.Duration) error { return sleep(ctx, d) }

func (l *liveRun) restart(ctx context.Context) error { return l.h.Launcher.Restart(ctx) }

func (l *liveRun) addFault(spec proxy.FaultSpec) proxy.FaultID { return l.h.Proxy.AddFault(spec) }

func (l *liveRun) removeFault(id proxy.FaultID) { l.h.Proxy.RemoveFault(id) }

func (l *liveRun) clearFaults() { l.h.Proxy.ClearFaults() }

func (l *liveRun) faultWindow(id proxy.FaultID) proxy.FaultWindow { return l.h.Proxy.Window(id) }

func (l *liveRun) held(before time.Time) (bool, time.Time) { return l.h.Proxy.Held(before) }

func (l *liveRun) servedResources() ([]metav1.APIResource, error) {
	return cluster.ServedResources(l.h.Config)
}

// createCR creates the op's object as the primary CR and tells the Observer
// reconciler-fuzzer created it, so that it never counts as managed.
func (l *liveRun) createCR(ctx context.Context, obj *unstructured.Unstructured) error {
	cr := obj.DeepCopy()
	switch gvk := cr.GroupVersionKind(); {
	case gvk.Empty():
		cr.SetGroupVersionKind(l.target.Primary)
	case gvk != l.target.Primary:
		return fmt.Errorf("the op creates a %s, and the target's primary CR is a %s",
			observe.KindName(gvk), observe.KindName(l.target.Primary))
	}
	cr.SetNamespace(l.h.Namespace)
	if _, err := l.crs().Create(ctx, cr, metav1.CreateOptions{}); err != nil {
		return fmt.Errorf("creating the CR: %w", err)
	}
	l.h.Observer.Exclude(l.target.Primary, cr.GetName())
	return nil
}

// patchCR applies the op's JSON merge patch to the CR. The target writes the
// CR's status while the run reads it, so a conflict is retried. A patch of
// status does not take, because status is a subresource.
func (l *liveRun) patchCR(ctx context.Context, name string, patch map[string]any) error {
	err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
		current, err := l.crs().Get(ctx, name, metav1.GetOptions{})
		if err != nil {
			return err
		}
		patched := &unstructured.Unstructured{Object: MergePatch(current.Object, patch)}
		_, err = l.crs().Update(ctx, patched, metav1.UpdateOptions{})
		return err
	})
	if err != nil {
		return fmt.Errorf("patching the CR %s: %w", name, err)
	}
	return nil
}

func (l *liveRun) deleteCR(ctx context.Context, name string) error {
	if err := l.crs().Delete(ctx, name, metav1.DeleteOptions{}); err != nil && !apierrors.IsNotFound(err) {
		return fmt.Errorf("deleting the CR %s: %w", name, err)
	}
	return nil
}

func (l *liveRun) awaitCRGone(ctx context.Context, name string, until func() time.Time) (bool, error) {
	gone, err := l.await(ctx, until, func() (bool, error) {
		_, err := l.crs().Get(ctx, name, metav1.GetOptions{})
		if apierrors.IsNotFound(err) {
			return true, nil
		}
		return false, err
	})
	if err != nil {
		return false, fmt.Errorf("waiting for the CR %s to go: %w", name, err)
	}
	return gone, nil
}

// managedObjects names the managed objects of one kind in the order a
// deleteManaged op's index counts in: creationTimestamp, then name.
func (l *liveRun) managedObjects(gvk schema.GroupVersionKind) []string {
	var managed []observe.Version
	for _, version := range l.h.Observer.Managed() {
		if version.GVK == gvk {
			managed = append(managed, version)
		}
	}
	slices.SortFunc(managed, func(a, b observe.Version) int {
		return cmp.Or(
			a.Object.GetCreationTimestamp().Time.Compare(b.Object.GetCreationTimestamp().Time),
			strings.Compare(a.Name, b.Name),
		)
	})
	names := make([]string, len(managed))
	for i, version := range managed {
		names[i] = version.Name
	}
	return names
}

func (l *liveRun) deleteManaged(ctx context.Context, gvk schema.GroupVersionKind, name string) (bool, error) {
	switch err := l.of(gvk).Delete(ctx, name, metav1.DeleteOptions{}); {
	case apierrors.IsNotFound(err):
		return false, nil
	case err != nil:
		return false, fmt.Errorf("deleting the managed %s %s: %w", observe.KindName(gvk), name, err)
	}
	return true, nil
}

func (l *liveRun) patchFixture(ctx context.Context, gvk schema.GroupVersionKind, name string, patch map[string]any) error {
	data, err := json.Marshal(patch)
	if err == nil {
		_, err = l.of(gvk).Patch(ctx, name, types.MergePatchType, data, metav1.PatchOptions{})
	}
	if err != nil {
		return fmt.Errorf("patching the fixture %s %s: %w", observe.KindName(gvk), name, err)
	}
	return nil
}

// deleteFixture deletes the fixture and waits timeouts.delete for it to go,
// so that reconciler-fuzzer can create it again.
func (l *liveRun) deleteFixture(ctx context.Context, gvk schema.GroupVersionKind, name string) error {
	fixtures := l.of(gvk)
	if err := fixtures.Delete(ctx, name, metav1.DeleteOptions{}); err != nil {
		return fmt.Errorf("deleting the fixture %s %s: %w", observe.KindName(gvk), name, err)
	}
	var held []string
	deadline := time.Now().Add(l.target.Timeouts.Delete)
	gone, err := l.await(ctx, func() time.Time { return deadline }, func() (bool, error) {
		fixture, err := fixtures.Get(ctx, name, metav1.GetOptions{})
		if err == nil {
			held = fixture.GetFinalizers()
		}
		return fixtureGone(fixture, err)
	})
	switch {
	case err != nil:
		return fmt.Errorf("waiting for the fixture %s %s to go: %w", observe.KindName(gvk), name, err)
	case !gone:
		return fmt.Errorf("the fixture %s %s was still there %v (timeouts.delete) after reconciler-fuzzer deleted it, held by the finalizers %v",
			observe.KindName(gvk), name, l.target.Timeouts.Delete, held)
	}
	return nil
}

// fixtureGone reads a Get of a fixture reconciler-fuzzer deleted. An object
// that is not being deleted is another under the fixture's name.
func fixtureGone(fixture *unstructured.Unstructured, err error) (bool, error) {
	switch {
	case apierrors.IsNotFound(err):
		return true, nil
	case err != nil:
		return false, err
	}
	return fixture.GetDeletionTimestamp() == nil, nil
}

func (l *liveRun) createFixture(ctx context.Context, fixture *unstructured.Unstructured) error {
	gvk := fixture.GroupVersionKind()
	_, err := l.of(gvk).Create(ctx, fixture, metav1.CreateOptions{})
	switch {
	case apierrors.IsAlreadyExists(err):
		return fmt.Errorf("something created the fixture %s %s again after reconciler-fuzzer deleted it, so reconciler-fuzzer cannot restore it",
			observe.KindName(gvk), fixture.GetName())
	case err != nil:
		return fmt.Errorf("restoring the fixture %s %s: %w", observe.KindName(gvk), fixture.GetName(), err)
	}
	return nil
}

func (l *liveRun) managedCount() int { return len(l.h.Observer.Managed()) }

// awaitClean waits for the target to remove what it manages and for the CR to
// go, which is what G3 requires within timeouts.delete.
func (l *liveRun) awaitClean(ctx context.Context, within time.Duration) (bool, error) {
	deadline := time.Now().Add(within)
	return l.await(ctx, func() time.Time { return deadline }, func() (bool, error) {
		empty := len(l.h.Observer.Managed()) == 0 && len(l.h.Observer.Current(l.target.Primary)) == 0
		return empty, nil
	})
}

// forceFinalizers takes every finalizer left in the run namespace off, and
// names the objects it took them from.
func (l *liveRun) forceFinalizers(ctx context.Context) ([]string, error) {
	var forced []string
	var failures []error
	for _, gvk := range l.emptied {
		list, err := l.of(gvk).List(ctx, metav1.ListOptions{})
		if err != nil {
			failures = append(failures, fmt.Errorf("listing the %s left behind: %w", observe.KindName(gvk), err))
			continue
		}
		for _, object := range list.Items {
			if len(object.GetFinalizers()) == 0 {
				continue
			}
			_, err := l.of(gvk).Patch(ctx, object.GetName(), types.MergePatchType, clearFinalizers, metav1.PatchOptions{})
			switch {
			case apierrors.IsNotFound(err):
			case err != nil:
				failures = append(failures, fmt.Errorf("clearing the finalizers of %s %s: %w", observe.KindName(gvk), object.GetName(), err))
			default:
				forced = append(forced, observe.KindName(gvk)+" "+object.GetName())
			}
		}
	}
	return forced, errors.Join(failures...)
}

// empty deletes what the run left in the namespace.
func (l *liveRun) empty(ctx context.Context) error {
	var failures []error
	for _, gvk := range l.emptied {
		err := l.of(gvk).DeleteCollection(ctx, metav1.DeleteOptions{}, metav1.ListOptions{})
		if err != nil && !apierrors.IsNotFound(err) {
			failures = append(failures, fmt.Errorf("deleting the %s left behind: %w", observe.KindName(gvk), err))
		}
	}
	return errors.Join(failures...)
}

func (l *liveRun) requests() []proxy.Request { return l.h.Proxy.Log() }

func (l *liveRun) objects() *observe.Store { return l.h.Observer.Store }

func (l *liveRun) targetStatus() launch.Status { return l.h.Launcher.Status() }

// supervise records why the target stopped before the launcher restarts it.
// Each process writes to one log, so an exit is quoted from what the log
// gained since the exit before it.
func (l *liveRun) supervise(ctx context.Context) {
	l.h.Launcher.Supervise(ctx, func(exit error, restart time.Time) {
		l.h.mu.Lock()
		defer l.h.mu.Unlock()
		said, end := whyItStopped(filepath.Join(l.h.dir, targetLogFile), l.h.logQuoted)
		l.h.logQuoted = end
		l.h.exited = append(l.h.exited, Exit{At: time.Now(), Err: exit, Said: said, Restart: restart})
	})
}

func (l *liveRun) exits() []Exit {
	l.h.mu.Lock()
	defer l.h.mu.Unlock()
	return slices.Clone(l.h.exited)
}

func (l *liveRun) stop(ctx context.Context) error { return l.h.Stop(ctx) }

func (l *liveRun) unresolvedOwners() []cluster.Unresolved { return l.h.unresolved }

// await polls until the condition holds or the instant until returns has
// passed, and reports whether it held.
func (l *liveRun) await(ctx context.Context, until func() time.Time, condition func() (bool, error)) (bool, error) {
	for {
		held, err := condition()
		if err != nil || held {
			return held, err
		}
		if !time.Now().Before(until()) {
			return false, nil
		}
		if err := sleep(ctx, settlePoll); err != nil {
			return false, err
		}
	}
}
