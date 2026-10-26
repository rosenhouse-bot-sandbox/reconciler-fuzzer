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
		if apierrors.IsNotFound(err) && r.Bug == B151 {
			return ctrl.Result{}, r.releaseVolumes(ctx, req.NamespacedName)
		}
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	if !cluster.DeletionTimestamp.IsZero() {
		return ctrl.Result{}, r.finalize(ctx, cluster)
	}
	if controllerutil.AddFinalizer(cluster, r.finalizer()) {
		if r.migrates() {
			// One write moves the Cluster to version 2's finalizer.
			controllerutil.RemoveFinalizer(cluster, fleetv1.ClusterFinalizer)
		}
		return ctrl.Result{}, r.Update(ctx, cluster)
	}
	old := cluster.Status
	switch {
	case old.Phase == "":
		cluster.Status.Phase = fleetv1.Pending
		return ctrl.Result{}, r.writeStatus(ctx, cluster, old, cluster.Status)
	case r.Version == 2 && r.Bug == B142 && old.Phase == fleetv1.Provisioning:
		return ctrl.Result{}, nil // B142 leaves a Cluster that version 1 was provisioning.
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

// migrates says whether this version moves the Cluster from version 1's
// finalizer to its own. B140 never removes version 1's.
func (r *ClusterReconciler) migrates() bool { return r.Version == 2 && r.Bug != B140 }

// finalizer is the finalizer this version adds.
func (r *ClusterReconciler) finalizer() string {
	if r.Version == 2 {
		return fleetv1.ClusterFinalizerV2
	}
	return fleetv1.ClusterFinalizer
}

// finalizers are the finalizers this version removes once the Cluster is
// finalized. Version 2 also removes version 1's, which a Cluster deleted
// before the upgrade still carries.
func (r *ClusterReconciler) finalizers() []string {
	if r.migrates() {
		return []string{fleetv1.ClusterFinalizerV2, fleetv1.ClusterFinalizer}
	}
	return []string{r.finalizer()}
}

// provisioning is the phase this version writes while a Volume is not ready.
func (r *ClusterReconciler) provisioning() fleetv1.Phase {
	if r.Version == 2 {
		return fleetv1.Creating
	}
	return fleetv1.Provisioning
}

// renames says whether this version renames the Cluster's Volumes. B141
// then sees only the Volumes it names, and misses version 1's.
func (r *ClusterReconciler) renames() bool { return r.Version == 2 && r.Bug == B141 }

// volumePrefix starts the names of the Volumes this version makes.
func (r *ClusterReconciler) volumePrefix(cluster *fleetv1.Cluster) string {
	if r.renames() {
		return cluster.Name + "-data-"
	}
	return cluster.Name + "-vol-"
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
		name := fmt.Sprintf("%s%d", r.volumePrefix(cluster), i)
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
		if v := &volumes[i]; !want[v.Name] && v.DeletionTimestamp.IsZero() {
			if err := r.Delete(ctx, v); client.IgnoreNotFound(err) != nil {
				return 0, fmt.Errorf("deleting Volume %s: %w", v.Name, err)
			}
		}
	}
	return ready, nil
}

// finalize waits until no Database names the Cluster, deletes its Volumes,
// waits until none remains, and then removes its finalizers.
func (r *ClusterReconciler) finalize(ctx context.Context, cluster *fleetv1.Cluster) error {
	mine := r.finalizers()
	if !slices.ContainsFunc(mine, func(f string) bool { return controllerutil.ContainsFinalizer(cluster, f) }) {
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
		}
	}
	if len(volumes) > 0 && r.Bug != B151 {
		return nil // A Volume's deletion requeues the Cluster.
	}
	for _, f := range mine {
		controllerutil.RemoveFinalizer(cluster, f)
	}
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
		if metav1.IsControlledBy(&v, cluster) && (!r.renames() || strings.HasPrefix(v.Name, r.volumePrefix(cluster))) {
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
