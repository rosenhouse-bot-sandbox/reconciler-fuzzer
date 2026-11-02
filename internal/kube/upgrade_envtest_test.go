//go:build envtest

package kube

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"

	"github.com/rosenhouse-bot-sandbox/reconciler-fuzzer/internal/system"
	"github.com/rosenhouse-bot-sandbox/reconciler-fuzzer/internal/trace"
)

func TestInstallCRDsUpdatesACRDAndWatchesANewOne(t *testing.T) {
	ctx := context.Background()
	recorded := &events{}
	k := New(Options{CRDs: []string{"testdata/upgrade/v1"}, RBAC: []string{toy + "rbac/role.yaml"}})
	if _, err := k.Start(ctx, &system.Env{Record: recorded.record, Dir: t.TempDir()}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { k.Stop(ctx) })
	kinds := k.Kinds()
	ref, err := k.Create(ctx, map[string]any{"apiVersion": "upgrade.reconciler-fuzzer/v1", "kind": "Thing",
		"metadata": map[string]any{"name": "t"}, "spec": map[string]any{"size": int64(1)}})
	if err != nil {
		t.Fatal(err)
	}

	if err := k.InstallCRDs(ctx, []string{"testdata/upgrade/v2"}); err != nil {
		t.Fatal(err)
	}

	// The observer's watch keeps the old schema until the server ends it.
	tiered := func(e trace.Event) bool {
		var body struct{ Spec struct{ Tier string } }
		return e.Version != nil && e.Version.Ref == ref && json.Unmarshal(e.Version.Body, &body) == nil && body.Spec.Tier == "standard"
	}
	deadline := time.Now().Add(10 * time.Second)
	for size := 2; !recorded.holds(tiered); size++ {
		if time.Now().After(deadline) {
			t.Fatal("No version of the Thing has the new schema's default spec.tier.")
		}
		if err := k.Patch(ctx, ref, fmt.Appendf(nil, `{"spec":{"size":%d}}`, size)); err != nil {
			t.Fatal(err)
		}
		time.Sleep(100 * time.Millisecond)
	}
	gadgets := schema.GroupVersionResource{Group: "upgrade.reconciler-fuzzer", Version: "v1", Resource: "gadgets"}
	gadget := &unstructured.Unstructured{Object: map[string]any{"apiVersion": "upgrade.reconciler-fuzzer/v1", "kind": "Gadget", "metadata": map[string]any{"name": "g"}}}
	// No listener sees this create, so only the observer's watch of the new kind records it.
	if _, err := dynamic.NewForConfigOrDie(k.admin).Resource(gadgets).Namespace(k.Namespace()).Create(ctx, gadget, metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	recorded.await(t, "version of the Gadget", func(e trace.Event) bool { return e.Version != nil && e.Version.Ref.Resource == "gadgets" })
	if got := k.Kinds(); !reflect.DeepEqual(got, kinds) {
		t.Errorf("Kinds = %v after InstallCRDs, want %v as Start left them.", got, kinds)
	}

	if err := k.InstallCRDs(ctx, []string{"testdata/upgrade/missing"}); err == nil {
		t.Error("InstallCRDs of a missing path succeeded.")
	}

	// The harness acts on a kind through its preferred version, which v3 changes.
	if err := k.InstallCRDs(ctx, []string{"testdata/upgrade/v3"}); err != nil {
		t.Fatal(err)
	}
	things := schema.GroupVersionResource{Group: "upgrade.reconciler-fuzzer", Resource: "things"}
	if gvr, err := k.Mapper().ResourceFor(things); err != nil || gvr.Version != "v2" {
		t.Errorf("Things map to %v, %v after InstallCRDs; want version v2.", gvr, err)
	}
}

func TestInstallCRDsStopsWaitingForANewKindsListWithItsContext(t *testing.T) {
	ctx := context.Background()
	k := New(Options{CRDs: []string{"testdata/upgrade/v1"}, RBAC: []string{toy + "rbac/role.yaml"}})
	if _, err := k.Start(ctx, &system.Env{Record: (&events{}).record, Dir: t.TempDir()}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { k.Stop(ctx) })
	canceled, cancel := context.WithCancel(ctx)
	cancel()
	if err := k.InstallCRDs(canceled, []string{"testdata/upgrade/v2"}); !errors.Is(err, context.Canceled) {
		t.Errorf("InstallCRDs = %v, want the context's error from the wait for the Gadgets' list.", err)
	}
}

// The observer cannot follow a kind to another version, so such an install
// fails and changes nothing.
func TestInstallCRDsRefusesCRDsThatStopServingAWatchedVersion(t *testing.T) {
	ctx := context.Background()
	k := New(Options{CRDs: []string{"testdata/upgrade/v1"}, RBAC: []string{toy + "rbac/role.yaml"}})
	if _, err := k.Start(ctx, &system.Env{Record: (&events{}).record, Dir: t.TempDir()}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { k.Stop(ctx) })
	err := k.InstallCRDs(ctx, []string{"testdata/upgrade/unserved"})
	if want := "things.upgrade.reconciler-fuzzer CRD stops serving v1, which the harness watches"; err == nil || !strings.Contains(err.Error(), want) {
		t.Errorf("InstallCRDs = %v, want an error that says %q.", err, want)
	}
	if _, err := k.Create(ctx, map[string]any{"apiVersion": "upgrade.reconciler-fuzzer/v1", "kind": "Thing", "metadata": map[string]any{"name": "t"}}); err != nil {
		t.Errorf("The refused install changed the CRD: %v", err)
	}
}
