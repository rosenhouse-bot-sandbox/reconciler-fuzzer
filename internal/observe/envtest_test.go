//go:build envtest

package observe_test

import (
	"context"
	"encoding/hex"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"

	"github.com/rosenhouse-bot-sandbox/reconciler-fuzzer/internal/cluster"
	"github.com/rosenhouse-bot-sandbox/reconciler-fuzzer/internal/observe"
)

const (
	pollInterval = 25 * time.Millisecond
	pollDeadline = 20 * time.Second
	widgetUID    = types.UID("uid-of-the-widget")
)

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

func startCluster(t *testing.T) (*cluster.Cluster, *kubernetes.Clientset) {
	t.Helper()
	c, err := cluster.Start(cluster.Options{})
	if err != nil {
		t.Fatalf("Starting the cluster failed: %v", err)
	}
	t.Cleanup(func() {
		if err := c.Stop(); err != nil {
			t.Errorf("Stopping the cluster failed: %v", err)
		}
	})
	client, err := kubernetes.NewForConfig(c.Config())
	if err != nil {
		t.Fatalf("Building a client failed: %v", err)
	}
	return c, client
}

func restMapper(t *testing.T, config *rest.Config) meta.RESTMapper {
	t.Helper()
	mapper, err := cluster.NewRESTMapper(config)
	if err != nil {
		t.Fatalf("Building the RESTMapper failed: %v", err)
	}
	return mapper
}

func createNamespace(t *testing.T, ctx context.Context, client *kubernetes.Clientset) string {
	t.Helper()
	ns, err := client.CoreV1().Namespaces().Create(ctx,
		&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{GenerateName: "reconciler-fuzzer-run-"}}, metav1.CreateOptions{})
	if err != nil {
		t.Fatalf("Creating the run namespace failed: %v", err)
	}
	return ns.Name
}

func writeConfigMap(t *testing.T, ctx context.Context, client *kubernetes.Clientset, ns, name, value string) {
	t.Helper()
	cm := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
		Data:       map[string]string{"value": value},
	}
	existing, err := client.CoreV1().ConfigMaps(ns).Get(ctx, name, metav1.GetOptions{})
	if err == nil {
		existing.Data = cm.Data
		if _, err := client.CoreV1().ConfigMaps(ns).Update(ctx, existing, metav1.UpdateOptions{}); err != nil {
			t.Fatalf("Updating the ConfigMap %s failed: %v", name, err)
		}
		return
	}
	cm.OwnerReferences = []metav1.OwnerReference{{
		APIVersion: "toy.reconciler-fuzzer/v1", Kind: "Widget", Name: "widget", UID: widgetUID,
	}}
	if _, err := client.CoreV1().ConfigMaps(ns).Create(ctx, cm, metav1.CreateOptions{}); err != nil {
		t.Fatalf("Creating the ConfigMap %s failed: %v", name, err)
	}
}

// requireVersions waits for the history of one object to hold exactly want
// versions, so a duplicate fails the test as surely as a missing one.
func requireVersions(t *testing.T, obs *observe.Observer, key observe.Key, want int) []observe.Version {
	t.Helper()
	var history []observe.Version
	eventually(t, func() error {
		history = obs.History(key)
		if len(history) != want {
			return fmt.Errorf("the history of %s holds %d versions, want %d", key.Name, len(history), want)
		}
		return nil
	})
	return history
}

// TestObserverRecordsTheRunNamespace drives one ConfigMap through the versions
// a target's child passes through, against one control plane.
func TestObserverRecordsTheRunNamespace(t *testing.T) {
	ctx := t.Context()
	c, client := startCluster(t)
	ns := createNamespace(t, ctx, client)

	obs, err := observe.Start(c.Config(), observe.Options{
		Namespace: ns,
		Kinds:     []schema.GroupVersionKind{configMapGVK},
		Manages:   []schema.GroupVersionKind{configMapGVK},
		Mapper:    restMapper(t, c.Config()),
	})
	if err != nil {
		t.Fatalf("Starting the observer failed: %v", err)
	}
	t.Cleanup(obs.Stop)
	if err := obs.WaitForSync(ctx); err != nil {
		t.Fatalf("The observer never synced: %v", err)
	}

	child := observe.Key{GVK: configMapGVK, Namespace: ns, Name: "child"}
	writeConfigMap(t, ctx, client, ns, child.Name, "first")
	requireVersions(t, obs, child, 1)

	writeConfigMap(t, ctx, client, ns, child.Name, "second")
	requireVersions(t, obs, child, 2)
	beforeTheLastUpdate := time.Now()

	writeConfigMap(t, ctx, client, ns, child.Name, "third")
	requireVersions(t, obs, child, 3)

	t.Run("the snapshot before the last update holds the earlier content", func(t *testing.T) {
		snapshot := obs.SnapshotAt(beforeTheLastUpdate)
		if len(snapshot) != 1 {
			t.Fatalf("The snapshot holds %d objects, want the one ConfigMap.", len(snapshot))
		}
		if got := dataValue(t, snapshot[0].Object); got != "second" {
			t.Errorf("The snapshot holds data %q, want the content before the last update.", got)
		}
		if got := dataValue(t, obs.Current(configMapGVK)[0].Object); got != "third" {
			t.Errorf("Current holds data %q, want the latest content.", got)
		}
	})

	t.Run("an object reconciler-fuzzer created is not managed", func(t *testing.T) {
		obs.Exclude(configMapGVK, "fixture")
		writeConfigMap(t, ctx, client, ns, "fixture", "applied by reconciler-fuzzer")
		requireVersions(t, obs, observe.Key{GVK: configMapGVK, Namespace: ns, Name: "fixture"}, 1)

		if got := names(obs.Managed()); !slices.Equal(got, []string{"child"}) {
			t.Errorf("Managed returned %v, want only the object reconciler-fuzzer did not create.", got)
		}
		if got := names(obs.ManagedBy(widgetUID)); !slices.Equal(got, []string{"child"}) {
			t.Errorf("ManagedBy returned %v, want the object whose ownerReference names the widget.", got)
		}
	})

	t.Run("a cluster-scoped kind is refused", func(t *testing.T) {
		// Its informer would list a namespaced path that does not exist and
		// never sync.
		_, err := observe.Start(c.Config(), observe.Options{
			Namespace: ns,
			Kinds:     []schema.GroupVersionKind{{Version: "v1", Kind: "Namespace"}},
			Mapper:    restMapper(t, c.Config()),
		})
		if err == nil {
			t.Fatal("Start accepted a cluster-scoped kind.")
		}
	})

	t.Run("the deletion is the last version", func(t *testing.T) {
		if err := client.CoreV1().ConfigMaps(ns).Delete(ctx, child.Name, metav1.DeleteOptions{}); err != nil {
			t.Fatalf("Deleting the ConfigMap failed: %v", err)
		}
		history := requireVersions(t, obs, child, 4)

		recorded := resourceVersions(history)
		if len(slices.Compact(slices.Clone(recorded))) != len(recorded) {
			t.Errorf("The history holds resourceVersions %v, want four distinct ones.", recorded)
		}
		if !slices.IsSortedFunc(history, func(a, b observe.Version) int { return a.Time.Compare(b.Time) }) {
			t.Error("The versions are not in the order they arrived.")
		}
		if !history[3].Deleted || slices.ContainsFunc(history[:3], func(v observe.Version) bool { return v.Deleted }) {
			t.Error("The deletion is not recorded as the last version and only as the last version.")
		}
		if got := names(obs.Managed()); len(got) != 0 {
			t.Errorf("Managed returned %v after the only managed object was deleted.", got)
		}
	})
}

// The API server serves stringData as base64 data.
func TestObjectsJSONLNamesASecretsKeysAndNotItsValues(t *testing.T) {
	ctx := t.Context()
	c, client := startCluster(t)
	ns := createNamespace(t, ctx, client)

	obs, err := observe.Start(c.Config(), observe.Options{
		Namespace: ns,
		Kinds:     []schema.GroupVersionKind{secretGVK},
		Manages:   []schema.GroupVersionKind{secretGVK},
		Mapper:    restMapper(t, c.Config()),
	})
	if err != nil {
		t.Fatalf("Starting the observer failed: %v", err)
	}
	t.Cleanup(obs.Stop)
	if err := obs.WaitForSync(ctx); err != nil {
		t.Fatalf("The observer never synced: %v", err)
	}
	if _, err := client.CoreV1().Secrets(ns).Create(ctx, &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "creds", Namespace: ns},
		StringData: map[string]string{"token": "s3cr3t"},
	}, metav1.CreateOptions{}); err != nil {
		t.Fatalf("Creating the Secret failed: %v", err)
	}
	requireVersions(t, obs, observe.Key{GVK: secretGVK, Namespace: ns, Name: "creds"}, 1)

	var written strings.Builder
	if err := obs.WriteHistory(&written); err != nil {
		t.Fatalf("WriteHistory returned an error: %v", err)
	}
	if !strings.Contains(written.String(), `"token":"[redacted 6 bytes`) {
		t.Errorf("objects.jsonl does not mark the Secret's token: %s", written.String())
	}
	for _, value := range []string{"s3cr3t", "czNjcjN0", hex.EncodeToString([]byte("s3cr3t"))} {
		if strings.Contains(written.String(), value) {
			t.Errorf("objects.jsonl holds the Secret's value %q: %s", value, written.String())
		}
	}
}
