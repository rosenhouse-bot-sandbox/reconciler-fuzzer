//go:build envtest

package kube

import (
	"context"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/watch"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/clientcmd"

	"github.com/rosenhouse-bot-sandbox/reconciler-fuzzer/internal/plan"
	"github.com/rosenhouse-bot-sandbox/reconciler-fuzzer/internal/system"
)

func TestAHoldWatchHoldsTheControllersEventsUntilItEnds(t *testing.T) {
	ctx := context.Background()
	k := New(Options{CRDs: []string{toy + "crds"}, RBAC: []string{toy + "rbac/role.yaml"}})
	endpoints, err := k.Start(ctx, &system.Env{Record: (&events{}).record, Dir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { k.Stop(ctx) })
	config, err := clientcmd.BuildConfigFromFlags("", endpoints.Vars["kubeconfig"])
	if err != nil {
		t.Fatal(err)
	}
	configMaps := kubernetes.NewForConfigOrDie(config).CoreV1().ConfigMaps(k.Namespace())
	list, err := configMaps.List(ctx, metav1.ListOptions{})
	if err != nil {
		t.Fatal(err)
	}
	watcher, err := configMaps.Watch(ctx, metav1.ListOptions{ResourceVersion: list.ResourceVersion})
	if err != nil {
		t.Fatal(err)
	}
	defer watcher.Stop()

	end, err := endpoints.Controller.Fault(plan.Fault{HoldWatch: true, Match: plan.Match{Resource: "configmaps"}})
	if err != nil {
		t.Fatal(err)
	}
	names := []string{"first", "second", "third"}
	for _, name := range names {
		if _, err := k.Create(ctx, map[string]any{"apiVersion": "v1", "kind": "ConfigMap", "metadata": map[string]any{"name": name}}); err != nil {
			t.Fatal(err)
		}
	}
	select {
	case e := <-watcher.ResultChan():
		t.Fatalf("Got a %s event during the hold.", e.Type)
	case <-time.After(500 * time.Millisecond):
	}

	end()
	for _, want := range names {
		select {
		case e := <-watcher.ResultChan():
			if got := e.Object.(metav1.Object).GetName(); e.Type != watch.Added || got != want {
				t.Fatalf("Got %s of %s, want ADDED of %s.", e.Type, got, want)
			}
		case <-time.After(5 * time.Second):
			t.Fatalf("Got no event of %s after the hold.", want)
		}
	}
}
