package controller

import (
	"context"
	"fmt"
	"slices"
	"strings"

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
type ClusterReconciler struct{ reconciler }

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
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	if !cluster.DeletionTimestamp.IsZero() {
		return ctrl.Result{}, r.finalize(ctx, cluster)
	}
	migrated := r.Version == 2 && r.Bug != B40 && controllerutil.RemoveFinalizer(cluster, fleetv1.ClusterFinalizer)
	if controllerutil.AddFinalizer(cluster, r.finalizer()) || migrated {
		return ctrl.Result{}, r.Update(ctx, cluster)
	}
	old := cluster.Status
	switch old.Phase {
	case "":
		cluster.Status.Phase = fleetv1.Pending
		return ctrl.Result{}, r.writeStatus(ctx, cluster, old, cluster.Status)
	case fleetv1.Provisioning:
		if r.Version == 2 && r.Bug == B42 {
			return ctrl.Result{}, nil
		}
	}
	ready, err := r.syncVolumes(ctx, cluster)
	if err != nil {
		return ctrl.Result{}, err
	}
	status := fleetv1.ClusterStatus{ReadyVolumes: ready}
	status.ObservedGeneration = cluster.Generation
	status.Phase, status.Message = r.provisioning(), fmt.Sprintf("%d of %d Volumes ready", ready, cluster.Spec.Volumes)
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
		name := r.volumeName(cluster, i, volumes)
		want[name] = true
		volume := &storagev1.Volume{ObjectMeta: metav1.ObjectMeta{Namespace: cluster.Namespace, Name: name}}
		if _, err := controllerutil.CreateOrUpdate(ctx, r.Client, volume, func() error {
			volume.Spec.Size = cluster.Spec.VolumeSize
			if volume.Labels == nil {
				volume.Labels = map[string]string{}
			}
			volume.Labels[ClusterLabel] = cluster.Name
			return controllerutil.SetControllerReference(cluster, volume, r.Scheme)
		}); err != nil {
			return 0, fmt.Errorf("ensuring Volume %s: %w", name, err)
		}
		if volumeReady(volume) {
			ready++
		}
	}
	for i := range volumes {
		if v := &volumes[i]; !want[v.Name] && v.DeletionTimestamp.IsZero() {
			if err := r.Delete(ctx, v); client.IgnoreNotFound(err) != nil {
				return 0, fmt.Errorf("deleting Volume %s: %w", v.Name, err)
			}
		}
	}
	return ready, nil
}

// finalize waits for the Cluster's Databases to go, deletes its Volumes,
// waits for them to go, and then releases the Cluster.
func (r *ClusterReconciler) finalize(ctx context.Context, cluster *fleetv1.Cluster) error {
	if !controllerutil.ContainsFinalizer(cluster, r.finalizer()) && (r.Bug == B40 || !controllerutil.ContainsFinalizer(cluster, fleetv1.ClusterFinalizer)) {
		return nil
	}
	databases := &fleetv1.DatabaseList{}
	if err := r.APIReader.List(ctx, databases, client.InNamespace(cluster.Namespace)); err != nil {
		return fmt.Errorf("listing Databases: %w", err)
	}
	var dependents int
	for _, db := range databases.Items {
		if db.Spec.ClusterRef == cluster.Name {
			dependents++
		}
	}
	volumes, err := r.ownedVolumes(ctx, r.APIReader, cluster)
	if err != nil {
		return err
	}
	old := cluster.Status
	cluster.Status.Phase = fleetv1.Deleting
	switch {
	case dependents > 0:
		cluster.Status.Message = fmt.Sprintf("waiting for %d Databases to go", dependents)
	case len(volumes) > 0:
		cluster.Status.Message = fmt.Sprintf("waiting for %d Volumes to go", len(volumes))
	default:
		cluster.Status.Message = ""
	}
	if err := r.writeStatus(ctx, cluster, old, cluster.Status); err != nil {
		return err
	}
	if dependents > 0 {
		return nil // A Database's deletion requeues the Cluster.
	}
	for i := range volumes {
		if v := &volumes[i]; v.DeletionTimestamp.IsZero() {
			if err := r.Delete(ctx, v); client.IgnoreNotFound(err) != nil {
				return fmt.Errorf("deleting Volume %s: %w", v.Name, err)
			}
		}
	}
	if len(volumes) > 0 {
		return nil // A Volume's deletion requeues the Cluster.
	}
	controllerutil.RemoveFinalizer(cluster, r.finalizer())
	if r.Bug != B40 {
		controllerutil.RemoveFinalizer(cluster, fleetv1.ClusterFinalizer)
	}
	return client.IgnoreNotFound(r.Update(ctx, cluster))
}

// ownedVolumes lists, through reader, the Volumes the Cluster controls.
func (r *ClusterReconciler) ownedVolumes(ctx context.Context, reader client.Reader, cluster *fleetv1.Cluster) ([]storagev1.Volume, error) {
	list := &storagev1.VolumeList{}
	if err := reader.List(ctx, list, client.InNamespace(cluster.Namespace), client.MatchingLabels{ClusterLabel: cluster.Name}); err != nil {
		return nil, fmt.Errorf("listing Volumes: %w", err)
	}
	var owned []storagev1.Volume
	for _, v := range list.Items {
		if metav1.IsControlledBy(&v, cluster) && (r.Bug != B41 || strings.HasPrefix(v.Name, cluster.Name+"-data-")) {
			owned = append(owned, v)
		}
	}
	return owned, nil
}

func (r *ClusterReconciler) finalizer() string {
	if r.Version == 2 {
		return fleetv1.ClusterFinalizerV2
	}
	return fleetv1.ClusterFinalizer
}

func (r *ClusterReconciler) provisioning() fleetv1.Phase {
	if r.Version == 2 {
		return fleetv1.Creating
	}
	return fleetv1.Provisioning
}

// volumeName names the Cluster's ith Volume. Version 2 keeps a v1 name
// that one of volumes holds.
func (r *ClusterReconciler) volumeName(cluster *fleetv1.Cluster, i int32, volumes []storagev1.Volume) string {
	v1 := fmt.Sprintf("%s-vol-%d", cluster.Name, i)
	if r.Version != 2 || slices.ContainsFunc(volumes, func(v storagev1.Volume) bool { return v.Name == v1 }) {
		return v1
	}
	return fmt.Sprintf("%s-data-%d", cluster.Name, i)
}

// volumeReady says whether the Volume's Ready condition is True for its
// current generation.
func volumeReady(v *storagev1.Volume) bool {
	c := meta.FindStatusCondition(v.Status.Conditions, "Ready")
	return v.DeletionTimestamp.IsZero() && c != nil && c.Status == metav1.ConditionTrue && c.ObservedGeneration == v.Generation
}
