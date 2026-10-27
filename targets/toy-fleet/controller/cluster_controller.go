package controller

import (
	"context"
	"fmt"
	"slices"
	"strings"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	fleetv1 "github.com/rosenhouse-bot-sandbox/reconciler-fuzzer/targets/toy-fleet/api/v1"
	storagev1 "github.com/rosenhouse-bot-sandbox/reconciler-fuzzer/targets/toy-fleet/thirdparty/storage/v1"
)

// ClusterLabel names a Volume's Cluster.
const ClusterLabel = "fleet.toy/cluster"

// ClusterReconciler moves a Cluster through its phases and keeps its Volumes.
type ClusterReconciler struct {
	reconciler
	// suffix ends the name of each Volume this process makes under B135.
	suffix string
}

func (r *ClusterReconciler) SetupWithManager(m ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(m).
		For(&fleetv1.Cluster{}).
		Owns(&storagev1.Volume{}).
		Watches(&fleetv1.Database{}, handler.EnqueueRequestsFromMapFunc(func(_ context.Context, o client.Object) []reconcile.Request {
			return []reconcile.Request{{NamespacedName: types.NamespacedName{Namespace: o.GetNamespace(), Name: o.(*fleetv1.Database).Spec.ClusterRef}}}
		})).
		Complete(r)
}

func (r *ClusterReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	cluster := &fleetv1.Cluster{}
	if err := r.Get(ctx, req.NamespacedName, cluster); err != nil {
		if apierrors.IsNotFound(err) && r.Bug == B151 {
			return ctrl.Result{}, r.releaseVolumes(ctx, req.NamespacedName)
		}
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	if !cluster.DeletionTimestamp.IsZero() {
		return ctrl.Result{}, r.finalize(ctx, cluster)
	}
	if controllerutil.AddFinalizer(cluster, fleetv1.ClusterFinalizer) {
		return ctrl.Result{}, r.Update(ctx, cluster)
	}
	old := cluster.Status
	if old.Phase == "" {
		cluster.Status.Phase = fleetv1.Pending
		return ctrl.Result{}, r.writeStatus(ctx, cluster, old, cluster.Status)
	}
	ready, err := r.syncVolumes(ctx, cluster)
	if err != nil {
		return ctrl.Result{}, err
	}
	status := fleetv1.ClusterStatus{ReadyVolumes: ready}
	status.ObservedGeneration = cluster.Generation
	status.Phase, status.Message = fleetv1.Provisioning, fmt.Sprintf("%d of %d Volumes ready", ready, cluster.Spec.Volumes)
	if ready == cluster.Spec.Volumes {
		status.Phase, status.Message = fleetv1.Ready, ""
	}
	cluster.Status = status
	return ctrl.Result{}, r.writeStatus(ctx, cluster, old, status)
}

// syncVolumes creates, updates and deletes the Cluster's Volumes to match its
// spec, and counts those ready.
func (r *ClusterReconciler) syncVolumes(ctx context.Context, cluster *fleetv1.Cluster) (int32, error) {
	volumes, err := r.ownedVolumes(ctx, r.Client, cluster)
	if err != nil {
		return 0, err
	}
	want := map[string]bool{}
	var ready int32
	for i := range cluster.Spec.Volumes {
		name := r.volumeName(cluster, i)
		want[name] = true
		volume := &storagev1.Volume{ObjectMeta: metav1.ObjectMeta{Namespace: cluster.Namespace, Name: name}}
		if _, err := controllerutil.CreateOrUpdate(ctx, r.Client, volume, func() error {
			volume.Spec.Size = cluster.Spec.VolumeSize
			if volume.Labels == nil {
				volume.Labels = map[string]string{}
			}
			volume.Labels[ClusterLabel] = cluster.Name
			if r.Bug == B151 {
				controllerutil.AddFinalizer(volume, VolumeFinalizer)
			}
			return controllerutil.SetControllerReference(cluster, volume, r.Scheme)
		}); err != nil {
			return 0, fmt.Errorf("ensuring Volume %s: %w", name, err)
		}
		if volumeReady(volume) {
			ready++
		}
	}
	for i := range volumes {
		if v := &volumes[i]; !want[v.Name] && r.mine(v) && v.DeletionTimestamp.IsZero() {
			if err := r.Delete(ctx, v); client.IgnoreNotFound(err) != nil {
				return 0, fmt.Errorf("deleting Volume %s: %w", v.Name, err)
			}
		}
	}
	return ready, nil
}

// volumeName names the Cluster's ith Volume.
func (r *ClusterReconciler) volumeName(cluster *fleetv1.Cluster, i int32) string {
	name := fmt.Sprintf("%s-vol-%d", cluster.Name, i)
	if r.Bug == B135 {
		return name + "-" + r.suffix
	}
	return name
}

// mine says whether this process counts the Volume as its own. B135 counts
// only the Volumes with its suffix.
func (r *ClusterReconciler) mine(v *storagev1.Volume) bool {
	return r.Bug != B135 || strings.HasSuffix(v.Name, "-"+r.suffix)
}

// finalize waits until no Database names the Cluster, deletes its Volumes,
// waits until none remains, and then removes its finalizer.
func (r *ClusterReconciler) finalize(ctx context.Context, cluster *fleetv1.Cluster) error {
	if !controllerutil.ContainsFinalizer(cluster, fleetv1.ClusterFinalizer) {
		return nil
	}
	databases := &fleetv1.DatabaseList{}
	if err := r.APIReader.List(ctx, databases, client.InNamespace(cluster.Namespace)); err != nil && !r.unserved(err) {
		return fmt.Errorf("listing Databases: %w", err)
	}
	named := slices.ContainsFunc(databases.Items, func(db fleetv1.Database) bool { return db.Spec.ClusterRef == cluster.Name })
	volumes, err := r.ownedVolumes(ctx, r.APIReader, cluster)
	if err != nil {
		return err
	}
	old := cluster.Status
	cluster.Status.Phase = fleetv1.Deleting
	switch {
	case named:
		cluster.Status.Message = "waiting until no Database names the Cluster"
	case len(volumes) > 0:
		cluster.Status.Message = "waiting until no Volume of the Cluster remains"
	default:
		cluster.Status.Message = ""
	}
	if err := r.writeStatus(ctx, cluster, old, cluster.Status); err != nil {
		return err
	}
	if named {
		return nil // A Database's deletion requeues the Cluster.
	}
	for i := range volumes {
		if v := &volumes[i]; v.DeletionTimestamp.IsZero() {
			if err := r.Delete(ctx, v); client.IgnoreNotFound(err) != nil {
				return fmt.Errorf("deleting Volume %s: %w", v.Name, err)
			}
		} else if r.Bug == B116 {
			v.Finalizers = nil
			if err := r.Update(ctx, v); client.IgnoreNotFound(err) != nil {
				return fmt.Errorf("stripping Volume %s: %w", v.Name, err)
			}
		}
	}
	if len(volumes) > 0 && r.Bug != B151 {
		return nil // A Volume's deletion requeues the Cluster.
	}
	controllerutil.RemoveFinalizer(cluster, fleetv1.ClusterFinalizer)
	return client.IgnoreNotFound(r.Update(ctx, cluster))
}

// releaseVolumes strips VolumeFinalizer from the gone Cluster's Volumes.
func (r *ClusterReconciler) releaseVolumes(ctx context.Context, cluster types.NamespacedName) error {
	list := &storagev1.VolumeList{}
	if err := r.List(ctx, list, client.InNamespace(cluster.Namespace), client.MatchingLabels{ClusterLabel: cluster.Name}); err != nil {
		return fmt.Errorf("listing Volumes: %w", err)
	}
	for i := range list.Items {
		if v := &list.Items[i]; controllerutil.RemoveFinalizer(v, VolumeFinalizer) {
			if err := r.Update(ctx, v); client.IgnoreNotFound(err) != nil {
				return fmt.Errorf("releasing Volume %s: %w", v.Name, err)
			}
		}
	}
	return nil
}

// ownedVolumes lists, through reader, the Volumes the Cluster controls.
func (r *ClusterReconciler) ownedVolumes(ctx context.Context, reader client.Reader, cluster *fleetv1.Cluster) ([]storagev1.Volume, error) {
	list := &storagev1.VolumeList{}
	if err := reader.List(ctx, list, client.InNamespace(cluster.Namespace), client.MatchingLabels{ClusterLabel: cluster.Name}); err != nil {
		return nil, fmt.Errorf("listing Volumes: %w", err)
	}
	var owned []storagev1.Volume
	for _, v := range list.Items {
		if metav1.IsControlledBy(&v, cluster) {
			owned = append(owned, v)
		}
	}
	return owned, nil
}

// volumeReady says whether the Volume's Ready condition is True for its
// current generation.
func volumeReady(v *storagev1.Volume) bool {
	c := meta.FindStatusCondition(v.Status.Conditions, "Ready")
	return v.DeletionTimestamp.IsZero() && c != nil && c.Status == metav1.ConditionTrue && c.ObservedGeneration == v.Generation
}
