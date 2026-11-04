package controller

import (
	"context"
	"reflect"
	"slices"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	toyv1 "github.com/rosenhouse-bot-sandbox/reconciler-fuzzer/targets/toy-widget/api/v1"
)

// withStaleCopy creates current on a fake API server and returns a client
// whose reads of it return stale instead, as a lagging cache would.
func withStaleCopy(t *testing.T, current, stale client.Object) (server, cached client.Client) {
	t.Helper()
	scheme, err := NewScheme()
	if err != nil {
		t.Fatal(err)
	}
	server = fake.NewClientBuilder().WithScheme(scheme).WithObjects(current).Build()
	stale.SetResourceVersion("1")
	cached = interceptor.NewClient(server.(client.WithWatch), interceptor.Funcs{
		Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, object client.Object, options ...client.GetOption) error {
			if key == client.ObjectKeyFromObject(stale) && reflect.TypeOf(object) == reflect.TypeOf(stale) {
				reflect.ValueOf(object).Elem().Set(reflect.ValueOf(stale.DeepCopyObject()).Elem())
				return nil
			}
			return c.Get(ctx, key, object, options...)
		},
	})
	return server, cached
}

// recreated is a copy of current as it was before a re-create: an older
// instance being deleted that holds finalizer.
func recreated(current client.Object, finalizer string) client.Object {
	stale := current.DeepCopyObject().(client.Object)
	stale.SetUID("old-uid")
	stale.SetFinalizers([]string{finalizer, "other/finalizer"})
	deletedAt := metav1.Now()
	stale.SetDeletionTimestamp(&deletedAt)
	return stale
}

func TestReconcilesFromAStaleCopyKeepTheServersFinalizers(t *testing.T) {
	widget, grant := newWidget(1), newGrant(1)
	widget.Finalizers, grant.Finalizers = []string{"other/finalizer"}, []string{"other/finalizer"}
	addedBefore := func(current client.Object) client.Object {
		stale := current.DeepCopyObject().(client.Object)
		stale.SetFinalizers(nil)
		return stale
	}
	reconcileWidget := func(c, _ client.Client, current client.Object) error {
		_, err := (&Reconciler{Client: c, APIReader: c, Scheme: c.Scheme()}).Reconcile(context.Background(), request(current))
		return err
	}
	reconcileGrant := func(c, server client.Client, current client.Object) error {
		r := &GrantReconciler{Client: c, APIReader: server, Scheme: c.Scheme(), MirrorNamespace: mirror}
		_, err := r.Reconcile(context.Background(), request(current))
		return err
	}

	for _, testCase := range []struct {
		name      string
		current   client.Object
		stale     client.Object
		reconcile func(c, server client.Client, current client.Object) error
	}{
		{"a Widget cached before another finalizer", widget, addedBefore(widget), reconcileWidget},
		{"a Widget cached before its re-create", newWidget(1), recreated(newWidget(1), Finalizer), reconcileWidget},
		{"a Grant cached before another finalizer", grant, addedBefore(grant), reconcileGrant},
		{"a Grant cached before its re-create", newGrant(1), recreated(newGrant(1), GrantFinalizer), reconcileGrant},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			want := testCase.current.GetFinalizers()
			server, cached := withStaleCopy(t, testCase.current.DeepCopyObject().(client.Object), testCase.stale)

			if err := testCase.reconcile(cached, server, testCase.current); err == nil {
				t.Error("The reconcile succeeded, want a conflict.")
			}
			observed := testCase.current.DeepCopyObject().(client.Object)
			if err := server.Get(t.Context(), client.ObjectKeyFromObject(observed), observed); err != nil {
				t.Fatal(err)
			}
			if got := observed.GetFinalizers(); !slices.Equal(got, want) {
				t.Errorf("The server holds finalizers %v, want %v.", got, want)
			}
		})
	}
}

func request(object client.Object) ctrl.Request {
	return ctrl.Request{NamespacedName: client.ObjectKeyFromObject(object)}
}

func TestFinalizerWritesFromACurrentCopySucceed(t *testing.T) {
	c := fakeClient(t, &toyv1.Widget{ObjectMeta: newWidget(1).ObjectMeta})
	widget := readWidget(t, c, newWidget(1))

	if err := addFinalizer(t.Context(), c, widget, Finalizer); err != nil {
		t.Fatal(err)
	}
	if err := addFinalizer(t.Context(), c, widget, "other/finalizer"); err != nil {
		t.Fatal(err)
	}
	if err := removeFinalizer(t.Context(), c, widget, Finalizer); err != nil {
		t.Fatal(err)
	}
	if got := readWidget(t, c, widget).Finalizers; !slices.Equal(got, []string{"other/finalizer"}) {
		t.Errorf("The Widget has finalizers %v, want only other/finalizer.", got)
	}
}
