// Package controller reconciles Buckets with buckets in a cloud.
package controller

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"net/http"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	toyv1 "github.com/rosenhouse-bot-sandbox/reconciler-fuzzer/targets/toy-bucket/api/v1"
)

// Finalizer names the cleanup path a deleted Bucket runs.
const Finalizer = "bucket.reconciler-fuzzer/cleanup"

// OwnerLabel is the bucket label that holds its Bucket's UID.
const OwnerLabel = "k8s-uid"

// nameFreed is how long the controller waits for a deleting bucket to free
// its name.
const nameFreed = 250 * time.Millisecond

// NewScheme returns a scheme holding the core Kubernetes types and the Bucket API.
func NewScheme() (*runtime.Scheme, error) {
	scheme := runtime.NewScheme()
	for _, add := range []func(*runtime.Scheme) error{clientgoscheme.AddToScheme, toyv1.AddToScheme} {
		if err := add(scheme); err != nil {
			return nil, fmt.Errorf("building the scheme: %w", err)
		}
	}
	return scheme, nil
}

// BucketReconciler keeps one cloud bucket per Bucket.
type BucketReconciler struct {
	client.Client
	Cloud *Cloud
	Bug   Bug
	// Halt stops reconciling a Bucket, except its deletion, after a create
	// of unknown outcome.
	Halt bool
	// Poll is how often the controller reads a ready bucket.
	Poll time.Duration
	// Nonce is B32's per-process suffix of bucket names.
	Nonce string
}

// NewNonce returns a random suffix for B32 and B31.
func NewNonce() string { return rand.Text()[:8] }

func (r *BucketReconciler) SetupWithManager(m ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(m).For(&toyv1.Bucket{}).Complete(r)
}

func (r *BucketReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	result, err := r.reconcile(ctx, req)
	if r.Bug == B35 && statusCode(err) == http.StatusServiceUnavailable {
		return ctrl.Result{RequeueAfter: 10 * time.Millisecond}, nil
	}
	return result, err
}

func (r *BucketReconciler) reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	var b toyv1.Bucket
	if err := r.Get(ctx, req.NamespacedName, &b); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	if !b.DeletionTimestamp.IsZero() {
		return r.finalize(ctx, &b)
	}
	if r.Halt && meta.IsStatusConditionTrue(b.Status.Conditions, toyv1.CreatePending) {
		return ctrl.Result{}, nil
	}
	if controllerutil.AddFinalizer(&b, Finalizer) {
		if err := r.Update(ctx, &b); err != nil {
			return ctrl.Result{}, err
		}
	}
	bucket, wait, err := r.live(ctx, &b)
	if err != nil || wait > 0 || bucket == nil {
		return ctrl.Result{RequeueAfter: wait}, err
	}
	if bucket.Region != b.Spec.Region {
		patched, err := r.Cloud.Patch(ctx, bucket.ID, b.Spec.Region)
		if err != nil {
			return ctrl.Result{}, err
		}
		bucket = &patched
	}
	ready := bucket.State == Ready || (r.Bug == B33 && bucket.State == Creating)
	if err := r.writeStatus(ctx, &b, func(s *toyv1.BucketStatus) {
		s.BucketID, s.Ready, s.ObservedGeneration = bucket.ID, ready, b.Generation
	}); err != nil {
		return ctrl.Result{}, err
	}
	if !ready {
		return ctrl.Result{RequeueAfter: r.Poll / 4}, nil
	}
	return ctrl.Result{RequeueAfter: r.Poll}, nil
}

// live returns the Bucket's live bucket, creating or adopting one, or how
// long to wait first. A nil bucket with no wait means the Bucket halted.
func (r *BucketReconciler) live(ctx context.Context, b *toyv1.Bucket) (*CloudBucket, time.Duration, error) {
	if id := b.Status.BucketID; id != "" {
		got, err := r.Cloud.Get(ctx, id)
		switch {
		case err == nil && got.State != Deleting:
			return &got, 0, nil
		case r.Bug == B34 && (err == nil || IsNotFound(err)):
			return nil, r.Poll, nil
		case err == nil:
			return nil, nameFreed, r.writeStatus(ctx, b, func(s *toyv1.BucketStatus) { s.Ready = false })
		case !IsNotFound(err):
			return nil, 0, err
		}
	}
	name := r.name(b)
	created, err := r.Cloud.Create(ctx, CloudBucket{Name: name, Region: b.Spec.Region, Labels: map[string]string{OwnerLabel: string(b.UID)}})
	switch {
	case err == nil:
	case IsUnknownOutcome(err) && r.Halt:
		return nil, 0, r.writeStatus(ctx, b, func(s *toyv1.BucketStatus) {
			meta.SetStatusCondition(&s.Conditions, metav1.Condition{
				Type: toyv1.CreatePending, Status: metav1.ConditionTrue, Reason: "UnknownOutcome",
				Message: "A create of " + name + " may have committed.", ObservedGeneration: b.Generation,
			})
		})
	case IsUnknownOutcome(err) && r.Bug == B31:
		name = "b-" + NewNonce()
		if created, err = r.Cloud.Create(ctx, CloudBucket{Name: name, Region: b.Spec.Region, Labels: map[string]string{OwnerLabel: string(b.UID)}}); err != nil {
			return nil, 0, err
		}
	case IsUnknownOutcome(err) || IsConflict(err):
		found, findErr := r.adoptable(ctx, name)
		if findErr != nil {
			return nil, 0, errors.Join(err, findErr)
		}
		if found == nil && IsConflict(err) {
			return nil, nameFreed, nil
		}
		if found == nil {
			return nil, 0, err
		}
		created = *found
	default:
		return nil, 0, err
	}
	return &created, 0, r.writeStatus(ctx, b, func(s *toyv1.BucketStatus) { s.BucketID, s.Ready = created.ID, false })
}

// adoptable finds the live bucket named name, or nil.
func (r *BucketReconciler) adoptable(ctx context.Context, name string) (*CloudBucket, error) {
	found, err := r.Cloud.FindByName(ctx, name)
	for _, f := range found {
		if f.State != Deleting {
			return &f, err
		}
	}
	return nil, err
}

func (r *BucketReconciler) name(b *toyv1.Bucket) string {
	if r.Bug == B32 {
		return "b-" + string(b.UID) + "-" + r.Nonce
	}
	return "b-" + string(b.UID)
}

// finalize deletes the Bucket's bucket, by its id or else by name, then
// removes the finalizer.
func (r *BucketReconciler) finalize(ctx context.Context, b *toyv1.Bucket) (ctrl.Result, error) {
	if !controllerutil.ContainsFinalizer(b, Finalizer) {
		return ctrl.Result{}, nil
	}
	if r.Bug != B30 {
		ids := []string{b.Status.BucketID}
		if b.Status.BucketID == "" {
			found, err := r.Cloud.FindByName(ctx, r.name(b))
			if err != nil {
				return ctrl.Result{}, err
			}
			ids = nil
			for _, f := range found {
				ids = append(ids, f.ID)
			}
		}
		for _, id := range ids {
			got, err := r.Cloud.Delete(ctx, id)
			if err != nil && !IsNotFound(err) {
				return ctrl.Result{}, err
			}
			if err == nil && got.State != Deleting {
				return ctrl.Result{RequeueAfter: nameFreed}, nil
			}
		}
	}
	controllerutil.RemoveFinalizer(b, Finalizer)
	return ctrl.Result{}, r.Update(ctx, b)
}

// writeStatus patches the status that change makes, if it differs.
func (r *BucketReconciler) writeStatus(ctx context.Context, b *toyv1.Bucket, change func(*toyv1.BucketStatus)) error {
	before := b.DeepCopy()
	change(&b.Status)
	if equalStatus(before.Status, b.Status) {
		return nil
	}
	err := r.Status().Patch(ctx, b, client.MergeFrom(before))
	if apierrors.IsNotFound(err) {
		return nil
	}
	return err
}

func equalStatus(a, b toyv1.BucketStatus) bool {
	if a.Ready != b.Ready || a.BucketID != b.BucketID || a.ObservedGeneration != b.ObservedGeneration || len(a.Conditions) != len(b.Conditions) {
		return false
	}
	for i := range a.Conditions {
		x, y := a.Conditions[i], b.Conditions[i]
		if x.Type != y.Type || x.Status != y.Status || x.Reason != y.Reason || x.Message != y.Message || x.ObservedGeneration != y.ObservedGeneration {
			return false
		}
	}
	return true
}
