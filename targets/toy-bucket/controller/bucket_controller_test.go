package controller

import (
	"context"
	"testing"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	toyv1 "github.com/rosenhouse-bot-sandbox/reconciler-fuzzer/targets/toy-bucket/api/v1"
)

const uid = "uid-1"

var key = types.NamespacedName{Namespace: "ns", Name: "b"}

type harness struct {
	t     *testing.T
	cloud *fakeCloud
	kube  client.Client
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	scheme, err := NewScheme()
	if err != nil {
		t.Fatal(err)
	}
	bucket := &toyv1.Bucket{
		ObjectMeta: metav1.ObjectMeta{Name: key.Name, Namespace: key.Namespace, UID: uid, Generation: 1},
		Spec:       toyv1.BucketSpec{Region: "eu"},
	}
	kube := fake.NewClientBuilder().WithScheme(scheme).WithObjects(bucket).WithStatusSubresource(bucket).Build()
	return &harness{t: t, cloud: newFakeCloud(t), kube: kube}
}

func (h *harness) reconciler(bug Bug) *BucketReconciler {
	return &BucketReconciler{Client: h.kube, Cloud: h.cloud.client(), Bug: bug, Poll: time.Second, Nonce: "n1"}
}

func (h *harness) reconcile(r *BucketReconciler) (ctrl.Result, error) {
	return r.Reconcile(context.Background(), ctrl.Request{NamespacedName: key})
}

// settle reconciles until the Bucket is ready or a deadline passes.
func (h *harness) settle(r *BucketReconciler) *toyv1.Bucket {
	h.t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		_, err := h.reconcile(r)
		b := h.bucket()
		if err == nil && b.Status.Ready {
			return b
		}
		if time.Now().After(deadline) {
			h.t.Fatalf("not ready: %+v, %v", b.Status, err)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func (h *harness) bucket() *toyv1.Bucket {
	h.t.Helper()
	var b toyv1.Bucket
	if err := h.kube.Get(context.Background(), key, &b); err != nil {
		h.t.Fatal(err)
	}
	return &b
}

// live lists the cloud's buckets that are not deleting.
func (h *harness) live() []CloudBucket {
	h.t.Helper()
	all, err := h.cloud.client().list(context.Background())
	if err != nil {
		h.t.Fatal(err)
	}
	var live []CloudBucket
	for _, b := range all {
		if b.State != Deleting {
			live = append(live, b)
		}
	}
	return live
}

// waitGone waits until the cloud lists no bucket at all.
func (h *harness) waitGone() {
	h.t.Helper()
	for deadline := time.Now().Add(2 * time.Second); ; time.Sleep(10 * time.Millisecond) {
		all, err := h.cloud.client().list(context.Background())
		if err == nil && len(all) == 0 {
			return
		}
		if time.Now().After(deadline) {
			h.t.Fatalf("buckets remain: %+v, %v", all, err)
		}
	}
}

// killedAfterCreate lets r create the bucket, then forgets its id, as a
// process killed before it recorded the id would.
func (h *harness) killedAfterCreate(r *BucketReconciler) {
	h.t.Helper()
	if _, err := h.reconcile(r); err != nil {
		h.t.Fatal(err)
	}
	b := h.bucket()
	b.Status = toyv1.BucketStatus{}
	if err := h.kube.Status().Update(context.Background(), b); err != nil {
		h.t.Fatal(err)
	}
}

func (h *harness) deleteCR() {
	h.t.Helper()
	if err := h.kube.Delete(context.Background(), h.bucket()); err != nil {
		h.t.Fatal(err)
	}
}

func (h *harness) crGone() bool {
	var b toyv1.Bucket
	return apierrors.IsNotFound(h.kube.Get(context.Background(), key, &b))
}

func TestB0CreatesOneBucketAndReportsReadyOnlyOnceItIsReady(t *testing.T) {
	h := newHarness(t)
	r := h.reconciler(0)
	if _, err := h.reconcile(r); err != nil {
		t.Fatal(err)
	}
	b := h.bucket()
	live := h.live()
	if len(live) != 1 || live[0].Name != "b-"+uid || live[0].Labels["k8s-uid"] != uid || live[0].Region != "eu" {
		t.Fatalf("buckets %+v, want one named b-%s labelled k8s-uid and in eu", live, uid)
	}
	if b.Status.BucketID != live[0].ID || b.Status.Ready {
		t.Errorf("status %+v after the create, want the bucket's id and not ready", b.Status)
	}
	b = h.settle(r)
	if b.Status.ObservedGeneration != 1 || len(h.live()) != 1 || h.live()[0].State != Ready {
		t.Errorf("status %+v, buckets %+v", b.Status, h.live())
	}
	result, err := h.reconcile(r)
	if err != nil || result.RequeueAfter != time.Second {
		t.Errorf("a ready Bucket reconciles to %+v, %v; want a poll in 1s", result, err)
	}
}

func TestB0PatchesTheRegion(t *testing.T) {
	h := newHarness(t)
	r := h.reconciler(0)
	h.settle(r)
	b := h.bucket()
	b.Spec.Region, b.Generation = "us", 2
	if err := h.kube.Update(context.Background(), b); err != nil {
		t.Fatal(err)
	}
	b = h.settle(r)
	if live := h.live(); len(live) != 1 || live[0].Region != "us" || b.Status.ObservedGeneration != b.Generation {
		t.Errorf("buckets %+v, status %+v, want one in us at generation %d", live, b.Status, b.Generation)
	}
}

func TestB0AdoptsTheBucketAfterAnUnknownOutcome(t *testing.T) {
	h := newHarness(t)
	h.cloud.set(func(c *fakeCloud) { c.dropResponses = 1 })
	if _, err := h.reconcile(h.reconciler(0)); err != nil {
		t.Fatal(err)
	}
	if live := h.live(); len(live) != 1 || h.bucket().Status.BucketID != live[0].ID {
		t.Errorf("buckets %+v, status %+v; want one, recorded in the same reconcile", live, h.bucket().Status)
	}
}

func TestB0AdoptsTheBucketAKilledProcessCreated(t *testing.T) {
	h := newHarness(t)
	h.killedAfterCreate(h.reconciler(0))
	h.settle(&BucketReconciler{Client: h.kube, Cloud: h.cloud.client(), Poll: time.Second, Nonce: "n2"})
	if live := h.live(); len(live) != 1 {
		t.Errorf("buckets %+v, want one", live)
	}
}

func TestB0WaitsForADeletingBucketToFreeItsName(t *testing.T) {
	h := newHarness(t)
	r := h.reconciler(0)
	h.killedAfterCreate(r)
	h.cloud.fake.Close() // holds the bucket in DELETING
	if _, err := h.cloud.client().Delete(context.Background(), h.live()[0].ID); err != nil {
		t.Fatal(err)
	}
	result, err := h.reconcile(r)
	if err != nil || result.RequeueAfter != nameFreed || h.bucket().Status.BucketID != "" {
		t.Errorf("reconciled to %+v, %v, status %+v; want a wait and no id", result, err, h.bucket().Status)
	}
}

func TestB0RecreatesADeletedBucket(t *testing.T) {
	h := newHarness(t)
	r := h.reconciler(0)
	old := h.settle(r).Status.BucketID
	if _, err := h.cloud.client().Delete(context.Background(), old); err != nil {
		t.Fatal(err)
	}
	h.waitGone()
	b := h.settle(r)
	if live := h.live(); len(live) != 1 || b.Status.BucketID == old || live[0].ID != b.Status.BucketID {
		t.Errorf("buckets %+v, status %+v, want one new bucket", live, b.Status)
	}
}

func TestB0DeletesTheBucketBehindAFinalizer(t *testing.T) {
	h := newHarness(t)
	r := h.reconciler(0)
	h.settle(r)
	h.deleteCR()
	if _, err := h.reconcile(r); err != nil {
		t.Fatal(err)
	}
	if !h.crGone() || len(h.live()) != 0 {
		t.Errorf("CR gone %v, live buckets %+v; want the CR gone and no bucket", h.crGone(), h.live())
	}
}

func TestB0BacksOffThroughTheRateLimiterOn503(t *testing.T) {
	h := newHarness(t)
	h.cloud.set(func(c *fakeCloud) { c.unavailable = true })
	if result, err := h.reconcile(h.reconciler(0)); err == nil || result.RequeueAfter != 0 {
		t.Errorf("a 503 reconciles to %+v, %v; want an error for the rate limiter", result, err)
	}
}

func TestB30LeaksTheBucket(t *testing.T) {
	h := newHarness(t)
	r := h.reconciler(B30)
	h.settle(r)
	h.deleteCR()
	if _, err := h.reconcile(r); err != nil {
		t.Fatal(err)
	}
	if !h.crGone() || len(h.live()) != 1 {
		t.Errorf("CR gone %v, live buckets %+v; want the CR gone and its bucket left", h.crGone(), h.live())
	}
}

func TestB31CreatesASecondBucketAfterAnUnknownOutcome(t *testing.T) {
	h := newHarness(t)
	h.cloud.set(func(c *fakeCloud) { c.dropResponses = 1 })
	h.settle(h.reconciler(B31))
	if live := h.live(); len(live) != 2 {
		t.Errorf("buckets %+v, want two", live)
	}
}

func TestB32CreatesASecondBucketAfterARestart(t *testing.T) {
	h := newHarness(t)
	h.killedAfterCreate(h.reconciler(B32))
	h.settle(&BucketReconciler{Client: h.kube, Cloud: h.cloud.client(), Bug: B32, Poll: time.Second, Nonce: "n2"})
	if live := h.live(); len(live) != 2 {
		t.Errorf("buckets %+v, want two", live)
	}
}

func TestB33ReportsReadyWhileCreating(t *testing.T) {
	h := newHarness(t)
	if _, err := h.reconcile(h.reconciler(B33)); err != nil {
		t.Fatal(err)
	}
	if b := h.bucket(); !b.Status.Ready {
		t.Errorf("status %+v right after the create, want ready", b.Status)
	}
}

func TestB34NeverRecreatesADeletedBucket(t *testing.T) {
	h := newHarness(t)
	r := h.reconciler(B34)
	h.settle(r)
	if _, err := h.cloud.client().Delete(context.Background(), h.bucket().Status.BucketID); err != nil {
		t.Fatal(err)
	}
	h.waitGone()
	for range 3 {
		if _, err := h.reconcile(r); err != nil {
			t.Fatal(err)
		}
	}
	if live := h.live(); len(live) != 0 || !h.bucket().Status.Ready {
		t.Errorf("buckets %+v, status %+v; want none and still ready", live, h.bucket().Status)
	}
}

func TestB35RetriesA503In10ms(t *testing.T) {
	h := newHarness(t)
	h.cloud.set(func(c *fakeCloud) { c.unavailable = true })
	if result, err := h.reconcile(h.reconciler(B35)); err != nil || result.RequeueAfter != 10*time.Millisecond {
		t.Errorf("a 503 reconciles to %+v, %v; want a requeue in 10ms", result, err)
	}
}

func TestHaltStopsAfterAnUnknownOutcomeAndStillDeletesByName(t *testing.T) {
	h := newHarness(t)
	r := h.reconciler(0)
	r.Halt = true
	h.cloud.set(func(c *fakeCloud) { c.dropResponses = 1 })
	if _, err := h.reconcile(r); err != nil {
		t.Fatal(err)
	}
	if !meta.IsStatusConditionTrue(h.bucket().Status.Conditions, toyv1.CreatePending) {
		t.Fatalf("conditions %+v, want CreatePending", h.bucket().Status.Conditions)
	}
	for range 3 {
		if _, err := h.reconcile(r); err != nil {
			t.Fatal(err)
		}
	}
	if posts := h.cloud.postCount(); posts != 1 || h.bucket().Status.BucketID != "" {
		t.Errorf("%d POSTs, status %+v; want one POST and no id", posts, h.bucket().Status)
	}
	h.deleteCR()
	if _, err := h.reconcile(r); err != nil {
		t.Fatal(err)
	}
	if !h.crGone() || len(h.live()) != 0 {
		t.Errorf("CR gone %v, live buckets %+v; want the CR gone and no bucket", h.crGone(), h.live())
	}
}

func TestB0ReportsNotReadyWhileItsBucketIsDeleting(t *testing.T) {
	h := newHarness(t)
	r := h.reconciler(0)
	h.settle(r)
	h.cloud.fake.Close() // holds the bucket in DELETING
	if _, err := h.cloud.client().Delete(context.Background(), h.bucket().Status.BucketID); err != nil {
		t.Fatal(err)
	}
	result, err := h.reconcile(r)
	if err != nil || result.RequeueAfter != nameFreed || h.bucket().Status.Ready {
		t.Errorf("reconciled to %+v, %v, status %+v; want a wait and not ready", result, err, h.bucket().Status)
	}
}
