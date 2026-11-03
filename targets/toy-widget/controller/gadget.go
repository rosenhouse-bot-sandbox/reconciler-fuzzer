package controller

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	toyv1 "github.com/rosenhouse-bot-sandbox/reconciler-fuzzer/targets/toy-widget/api/v1"
)

const (
	b21Requeue = 100 * time.Millisecond
	b25Requeue = 10 * time.Millisecond
)

// syncGadget creates or updates the Widget's Gadget and returns gadgetReady,
// which is nil without --gadget, and how soon to requeue.
func (r *Reconciler) syncGadget(ctx context.Context, widget *toyv1.Widget) (*bool, time.Duration, error) {
	if !r.Gadget {
		return nil, 0, nil
	}
	if r.Bug == B20 {
		return ptr.To(true), 0, nil // B20: no Gadget at all.
	}
	key := client.ObjectKeyFromObject(widget)
	gadget := &toyv1.Gadget{ObjectMeta: metav1.ObjectMeta{Namespace: key.Namespace, Name: key.Name}}
	if r.Bug == B24 && ptr.Deref(widget.Status.GadgetReady, false) {
		// B24: a Gadget once ready is never recreated.
		if err := r.Get(ctx, key, gadget); apierrors.IsNotFound(err) {
			return widget.Status.GadgetReady, 0, nil
		}
	}
	_, err := controllerutil.CreateOrUpdate(ctx, r.Client, gadget, func() error {
		gadget.Spec.Size = widget.Spec.Count
		return controllerutil.SetControllerReference(widget, gadget, r.Scheme)
	})
	if err != nil {
		return nil, 0, fmt.Errorf("ensuring Gadget %s: %w", gadget.Name, err)
	}
	ready := gadgetIsReady(gadget)
	var requeue time.Duration
	switch r.Bug {
	case B17:
		ready = true // B17: the Gadget's status goes unread.
	case B18:
		if gadgetIsNotReady(gadget) {
			r.ignoreGadgetOf(key)
		}
	case B19:
		ready = ready || ptr.Deref(widget.Status.GadgetReady, false) // B19: once true, always true.
	case B21:
		// B21: a no-op update on every reconcile, and soon again.
		if err := r.Update(ctx, gadget); err != nil {
			return nil, 0, fmt.Errorf("updating Gadget %s: %w", gadget.Name, err)
		}
		requeue = b21Requeue
	case B25:
		if gadgetIsNotReady(gadget) {
			// B25: a write its RBAC forbids, retried at once.
			r.forceGadgetReady(ctx, gadget)
			requeue = b25Requeue
		}
	}
	return &ready, requeue, nil
}

// gadgetIsReady says whether the Gadget's Ready condition is True for its
// current generation.
func gadgetIsReady(gadget *toyv1.Gadget) bool {
	c := meta.FindStatusCondition(gadget.Status.Conditions, "Ready")
	return c != nil && c.Status == metav1.ConditionTrue && c.ObservedGeneration == gadget.Generation
}

// gadgetIsNotReady says whether the Gadget's Ready condition is False.
func gadgetIsNotReady(gadget *toyv1.Gadget) bool {
	return meta.IsStatusConditionFalse(gadget.Status.Conditions, "Ready")
}

func (r *Reconciler) forceGadgetReady(ctx context.Context, gadget *toyv1.Gadget) {
	patch, _ := json.Marshal(map[string]any{"status": map[string]any{"conditions": []metav1.Condition{{
		Type: "Ready", Status: metav1.ConditionTrue, Reason: "Forced", ObservedGeneration: gadget.Generation, LastTransitionTime: metav1.Now(),
	}}}})
	_ = r.Status().Patch(ctx, gadget, client.RawPatch(types.MergePatchType, patch))
}

func (r *Reconciler) ignoreGadgetOf(widget types.NamespacedName) {
	r.ignoredMu.Lock()
	defer r.ignoredMu.Unlock()
	if r.ignored == nil {
		r.ignored = sets.New[types.NamespacedName]()
	}
	r.ignored.Insert(widget)
}

// passesGadgetEvent filters the Gadget watch. Only B18 drops events.
func (r *Reconciler) passesGadgetEvent(gadget client.Object) bool {
	r.ignoredMu.Lock()
	defer r.ignoredMu.Unlock()
	return !r.ignored.Has(client.ObjectKeyFromObject(gadget))
}

// deleteGadget deletes the Widget's Gadget and says whether an uncached
// read finds it gone.
func (r *Reconciler) deleteGadget(ctx context.Context, widget *toyv1.Widget) (bool, error) {
	if !r.Gadget {
		return true, nil
	}
	key := client.ObjectKeyFromObject(widget)
	gadget := &toyv1.Gadget{}
	gone, err := r.gadgetGone(ctx, key, gadget)
	if gone || err != nil {
		return gone, err
	}
	if gadget.DeletionTimestamp.IsZero() {
		if r.Bug == B22 {
			// B22: the Gadget is orphaned, not deleted.
			base := gadget.DeepCopy()
			gadget.OwnerReferences = nil
			if err := r.Patch(ctx, gadget, client.MergeFrom(base)); err != nil {
				return false, fmt.Errorf("orphaning Gadget %s: %w", gadget.Name, err)
			}
			return true, nil
		}
		if err := r.Delete(ctx, gadget); client.IgnoreNotFound(err) != nil {
			return false, fmt.Errorf("deleting Gadget %s: %w", gadget.Name, err)
		}
		if gone, err := r.gadgetGone(ctx, key, gadget); gone || err != nil {
			return gone, err
		}
	}
	if r.Bug == B23 {
		r.giveUpOn(key) // B23: a Gadget still there after being deleted is never waited for.
	}
	return false, nil
}

func (r *Reconciler) gadgetGone(ctx context.Context, key types.NamespacedName, gadget *toyv1.Gadget) (bool, error) {
	err := r.APIReader.Get(ctx, key, gadget)
	if apierrors.IsNotFound(err) {
		return true, nil
	}
	if err != nil {
		return false, fmt.Errorf("reading Gadget %s: %w", key.Name, err)
	}
	return false, nil
}

func (r *Reconciler) giveUpOn(widget types.NamespacedName) {
	if r.gaveUp == nil {
		r.gaveUp = sets.New[types.NamespacedName]()
	}
	r.gaveUp.Insert(widget)
}
