//go:build envtest

package cluster_test

import (
	"context"
	"slices"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"

	"github.com/rosenhouse-bot-sandbox/reconciler-fuzzer/internal/cluster"
)

var (
	configMapKind  = schema.GroupVersionKind{Version: "v1", Kind: "ConfigMap"}
	gadgetKind     = schema.GroupVersionKind{Group: "test.reconciler-fuzzer", Version: "v1", Kind: "Gadget"}
	gadgetResource = schema.GroupVersionResource{Group: "test.reconciler-fuzzer", Version: "v1", Resource: "gadgets"}
)

// collectionBudget is the bound DESIGN.md §5.8 puts on the collector: it
// deletes within a second of the owner's deletion event.
const collectionBudget = time.Second

func TestCollector(t *testing.T) {
	c, err := cluster.Start(cluster.Options{CRDPaths: []string{"testdata"}})
	if err != nil {
		t.Fatalf("Start returned an error: %v", err)
	}
	t.Cleanup(func() {
		if err := c.Stop(); err != nil {
			t.Errorf("Stop returned an error: %v", err)
		}
	})
	client, err := kubernetes.NewForConfig(c.Config())
	if err != nil {
		t.Fatalf("Building a client failed: %v", err)
	}
	dynamicClient, err := dynamic.NewForConfig(c.Config())
	if err != nil {
		t.Fatalf("Building a dynamic client failed: %v", err)
	}
	gadgets := func(namespace string) dynamic.ResourceInterface {
		return dynamicClient.Resource(gadgetResource).Namespace(namespace)
	}

	t.Run("collects a child once its owner is gone", func(t *testing.T) {
		namespace := newNamespace(t, client)
		parent := createConfigMap(t, client, namespace, "parent")
		child := createConfigMap(t, client, namespace, "child", ownerOf(parent))
		startCollector(t, c.Config(), namespace, configMapKind)

		deleteConfigMap(t, client, namespace, parent.Name)

		waitGone(t, client, namespace, child.Name)
	})

	t.Run("keeps what it must not collect", func(t *testing.T) {
		namespace := newNamespace(t, client)
		gadget := createGadget(t, gadgets(namespace), "gadget")
		parent := createConfigMap(t, client, namespace, "parent")
		canary := createConfigMap(t, client, namespace, "canary", ownerOf(parent))
		ownerless := createConfigMap(t, client, namespace, "ownerless")
		unwatchedOwner := createConfigMap(t, client, namespace, "unwatched-owner", secretOwner("absent"))
		unservedVersion := createConfigMap(t, client, namespace, "unserved-version", gadgetOwner(gadget, "v1beta9"))
		collector := startCollector(t, c.Config(), namespace, configMapKind, gadgetKind)

		// The canary goes in a sweep that follows both deletions.
		deleteGadget(t, gadgets(namespace), gadget.GetName())
		deleteConfigMap(t, client, namespace, parent.Name)

		waitGone(t, client, namespace, canary.Name)
		requirePresent(t, client, namespace, ownerless.Name)
		requirePresent(t, client, namespace, unwatchedOwner.Name)
		requirePresent(t, client, namespace, unservedVersion.Name)
		unresolved := collector.Stop()
		want := []cluster.Unresolved{
			{DependentKind: configMapKind, DependentName: unservedVersion.Name,
				OwnerKind: schema.GroupVersionKind{Group: gadgetKind.Group, Version: "v1beta9", Kind: gadgetKind.Kind}, OwnerName: gadget.GetName(),
				Unserved: true},
			{DependentKind: configMapKind, DependentName: unwatchedOwner.Name,
				OwnerKind: schema.GroupVersionKind{Version: "v1", Kind: "Secret"}, OwnerName: "absent"},
		}
		if !slices.Equal(unresolved, want) {
			t.Errorf("Stop returned %+v, want %+v.", unresolved, want)
		}
	})

	// An ownerReference may name any version the API server serves.
	t.Run("collects a child whose owner reference names another served version", func(t *testing.T) {
		namespace := newNamespace(t, client)
		gadget := createGadget(t, gadgets(namespace), "gadget")
		child := createConfigMap(t, client, namespace, "child", gadgetOwner(gadget, "v1alpha1"))
		startCollector(t, c.Config(), namespace, configMapKind, gadgetKind)

		deleteGadget(t, gadgets(namespace), gadget.GetName())

		waitGone(t, client, namespace, child.Name)
	})

	t.Run("collects a child once its last owner is gone", func(t *testing.T) {
		namespace := newNamespace(t, client)
		first := createConfigMap(t, client, namespace, "first-owner")
		second := createConfigMap(t, client, namespace, "second-owner")
		child := createConfigMap(t, client, namespace, "child", ownerOf(first), ownerOf(second))
		canary := createConfigMap(t, client, namespace, "canary", ownerOf(first))
		startCollector(t, c.Config(), namespace, configMapKind)

		deleteConfigMap(t, client, namespace, first.Name)

		waitGone(t, client, namespace, canary.Name)
		requirePresent(t, client, namespace, child.Name)

		deleteConfigMap(t, client, namespace, second.Name)

		waitGone(t, client, namespace, child.Name)
	})

	// The owner is recreated before the collector starts, so the child can
	// only be collected by the UID comparison, never by the owner's absence.
	t.Run("collects a child whose owner's name was reused", func(t *testing.T) {
		namespace := newNamespace(t, client)
		parent := createConfigMap(t, client, namespace, "parent")
		deleteConfigMap(t, client, namespace, parent.Name)
		recreated := createConfigMap(t, client, namespace, "parent")
		child := createConfigMap(t, client, namespace, "child", ownerOf(parent))

		startCollector(t, c.Config(), namespace, configMapKind)

		waitGone(t, client, namespace, child.Name)
		requirePresent(t, client, namespace, recreated.Name)
	})

	// A kind given without a version resolves to the version the API server
	// serves, which is the version an ownerReference names.
	t.Run("collects a child when the kind carries no version", func(t *testing.T) {
		namespace := newNamespace(t, client)
		parent := createConfigMap(t, client, namespace, "parent")
		child := createConfigMap(t, client, namespace, "child", ownerOf(parent))
		startCollector(t, c.Config(), namespace, schema.GroupVersionKind{Kind: "ConfigMap"})

		deleteConfigMap(t, client, namespace, parent.Name)

		waitGone(t, client, namespace, child.Name)
	})

	t.Run("refuses a cluster-scoped kind", func(t *testing.T) {
		clusterScoped := schema.GroupVersionKind{Version: "v1", Kind: "Namespace"}

		collector, err := cluster.StartCollector(c.Config(), cluster.CollectorOptions{
			Namespace: newNamespace(t, client),
			Kinds:     []schema.GroupVersionKind{clusterScoped},
			Mapper:    restMapper(t, c.Config()),
		})

		if err == nil {
			collector.Stop()
			t.Fatal("StartCollector accepted a cluster-scoped kind.")
		}
		if !strings.Contains(err.Error(), "cluster-scoped") || !strings.Contains(err.Error(), clusterScoped.Kind) {
			t.Errorf("StartCollector returned %q, which does not say that %s is cluster-scoped.", err, clusterScoped)
		}
	})
}

func newNamespace(t *testing.T, client kubernetes.Interface) string {
	t.Helper()
	namespace := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{GenerateName: "collector-"}}
	created, err := client.CoreV1().Namespaces().Create(context.Background(), namespace, metav1.CreateOptions{})
	if err != nil {
		t.Fatalf("Creating a namespace failed: %v", err)
	}
	return created.Name
}

func startCollector(t *testing.T, config *rest.Config, namespace string, kinds ...schema.GroupVersionKind) *cluster.Collector {
	t.Helper()
	collector, err := cluster.StartCollector(config, cluster.CollectorOptions{
		Namespace: namespace,
		Kinds:     kinds,
		Mapper:    restMapper(t, config),
	})
	if err != nil {
		t.Fatalf("StartCollector returned an error: %v", err)
	}
	t.Cleanup(func() { collector.Stop() })
	return collector
}

func restMapper(t *testing.T, config *rest.Config) meta.RESTMapper {
	t.Helper()
	mapper, err := cluster.NewRESTMapper(config)
	if err != nil {
		t.Fatalf("Building the RESTMapper failed: %v", err)
	}
	return mapper
}

func createConfigMap(t *testing.T, client kubernetes.Interface, namespace, name string, owners ...metav1.OwnerReference) *corev1.ConfigMap {
	t.Helper()
	configMap := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: name, OwnerReferences: owners}}
	created, err := client.CoreV1().ConfigMaps(namespace).Create(context.Background(), configMap, metav1.CreateOptions{})
	if err != nil {
		t.Fatalf("Creating the ConfigMap %s failed: %v", name, err)
	}
	return created
}

func deleteConfigMap(t *testing.T, client kubernetes.Interface, namespace, name string) {
	t.Helper()
	if err := client.CoreV1().ConfigMaps(namespace).Delete(context.Background(), name, metav1.DeleteOptions{}); err != nil {
		t.Fatalf("Deleting the ConfigMap %s failed: %v", name, err)
	}
}

func ownerOf(configMap *corev1.ConfigMap) metav1.OwnerReference {
	return metav1.OwnerReference{APIVersion: "v1", Kind: "ConfigMap", Name: configMap.Name, UID: configMap.UID}
}

func createGadget(t *testing.T, gadgets dynamic.ResourceInterface, name string) *unstructured.Unstructured {
	t.Helper()
	gadget := &unstructured.Unstructured{}
	gadget.SetGroupVersionKind(gadgetKind)
	gadget.SetName(name)
	created, err := gadgets.Create(context.Background(), gadget, metav1.CreateOptions{})
	if err != nil {
		t.Fatalf("Creating the Gadget %s failed: %v", name, err)
	}
	return created
}

func deleteGadget(t *testing.T, gadgets dynamic.ResourceInterface, name string) {
	t.Helper()
	if err := gadgets.Delete(context.Background(), name, metav1.DeleteOptions{}); err != nil {
		t.Fatalf("Deleting the Gadget %s failed: %v", name, err)
	}
}

// gadgetOwner names the Gadget at the version given.
func gadgetOwner(gadget *unstructured.Unstructured, version string) metav1.OwnerReference {
	return metav1.OwnerReference{
		APIVersion: gadgetKind.Group + "/" + version,
		Kind:       gadgetKind.Kind,
		Name:       gadget.GetName(),
		UID:        gadget.GetUID(),
	}
}

// secretOwner names an owner of a kind the collector does not watch.
func secretOwner(name string) metav1.OwnerReference {
	return metav1.OwnerReference{
		APIVersion: "v1",
		Kind:       "Secret",
		Name:       name,
		UID:        "8a1d0f2c-5b3e-4d7a-9c6f-1e2b3c4d5e6f",
	}
}

func waitGone(t *testing.T, client kubernetes.Interface, namespace, name string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), collectionBudget)
	defer cancel()
	gone := func(ctx context.Context) (bool, error) {
		_, err := client.CoreV1().ConfigMaps(namespace).Get(ctx, name, metav1.GetOptions{})
		return apierrors.IsNotFound(err), nil
	}
	if err := wait.PollUntilContextCancel(ctx, 10*time.Millisecond, true, gone); err != nil {
		t.Fatalf("The collector left the ConfigMap %s in place: %v", name, err)
	}
}

func requirePresent(t *testing.T, client kubernetes.Interface, namespace, name string) {
	t.Helper()
	if _, err := client.CoreV1().ConfigMaps(namespace).Get(context.Background(), name, metav1.GetOptions{}); err != nil {
		t.Fatalf("Reading the ConfigMap %s failed: %v", name, err)
	}
}
