package controller

import (
	"context"
	"crypto/rand"
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
type ClusterReconciler struct {
	reconciler
	// memory holds what the seeded bugs remember between reconciles.
	memory map[types.UID]*clusterMemory
	// nonce is this process's Volume name suffix under
	// RenamedVolumesAfterRestart.
	nonce string
}

type clusterMemory struct {
	regressedAt int64                // PhaseRegresses
	volumes     []string             // DuplicateVolumesAfterRestart
	spec        *fleetv1.ClusterSpec // IgnoresSpecDuringProvisioning
}

func newClusterReconciler(base reconciler) *ClusterReconciler {
	return &ClusterReconciler{reconciler: base, memory: map[types.UID]*clusterMemory{}, nonce: strings.ToLower(rand.Text()[:5])}
}

func (r *ClusterReconciler) remember(cluster *fleetv1.Cluster) *clusterMemory {
	m := r.memory[cluster.UID]
	if m == nil {
		m = &clusterMemory{}
		r.memory[cluster.UID] = m
	}
	return m
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
	if r.Bug == StuckAfterCreateError && strings.HasPrefix(old.Message, gaveUp) {
		return ctrl.Result{}, nil
	}
	spec := r.specToProvision(cluster)
	ready, err := r.syncVolumes(ctx, cluster, spec)
	if err != nil && r.Bug == StuckAfterCreateError {
		cluster.Status.Phase, cluster.Status.Message = fleetv1.Provisioning, gaveUp+err.Error()
		return ctrl.Result{}, r.writeStatus(ctx, cluster, old, cluster.Status)
	}
	if err != nil {
		return ctrl.Result{}, err
	}
	status := fleetv1.ClusterStatus{ReadyVolumes: ready}
	status.ObservedGeneration = cluster.Generation
	status.Phase, status.Message = fleetv1.Provisioning, fmt.Sprintf("%d of %d Volumes ready", ready, spec.Volumes)
	if ready == spec.Volumes {
		status.Phase, status.Message = fleetv1.Ready, ""
	}
	if m := r.remember(cluster); r.Bug == PhaseRegresses && status.Phase == fleetv1.Ready &&
		old.Phase == fleetv1.Ready && old.ObservedGeneration == cluster.Generation && m.regressedAt != cluster.Generation {
		m.regressedAt = cluster.Generation
		status.Phase, status.Message = fleetv1.Provisioning, "resyncing"
	}
	cluster.Status = status
	return ctrl.Result{}, r.writeStatus(ctx, cluster, old, status)
}

// gaveUp starts the message of a Cluster StuckAfterCreateError abandoned.
const gaveUp = "gave up: "

// specToProvision is the Cluster's spec, or under
// IgnoresSpecDuringProvisioning the one it saw when it last left Ready.
func (r *ClusterReconciler) specToProvision(cluster *fleetv1.Cluster) fleetv1.ClusterSpec {
	if r.Bug != IgnoresSpecDuringProvisioning {
		return cluster.Spec
	}
	m := r.remember(cluster)
	if m.spec == nil || cluster.Status.Phase == fleetv1.Ready && cluster.Status.ObservedGeneration != cluster.Generation {
		m.spec = cluster.Spec.DeepCopy()
	}
	return *m.spec
}

// volumeName names the Cluster's ith Volume, or is empty for a new Volume
// under DuplicateVolumesAfterRestart.
func (r *ClusterReconciler) volumeName(cluster *fleetv1.Cluster, i int32) string {
	switch r.Bug {
	case DuplicateVolumesAfterRestart:
		if m := r.remember(cluster); int(i) < len(m.volumes) {
			return m.volumes[i]
		}
		return ""
	case RenamedVolumesAfterRestart:
		return fmt.Sprintf("%s-vol-%d-%s", cluster.Name, i, r.nonce)
	}
	return fmt.Sprintf("%s-vol-%d", cluster.Name, i)
}

// knows says whether this process may delete the Volume. Only the
// restart bugs forget Volumes.
func (r *ClusterReconciler) knows(cluster *fleetv1.Cluster, v *storagev1.Volume) bool {
	switch r.Bug {
	case DuplicateVolumesAfterRestart:
		return slices.Contains(r.remember(cluster).volumes, v.Name)
	case RenamedVolumesAfterRestart:
		return strings.HasSuffix(v.Name, "-"+r.nonce)
	}
	return true
}

// syncVolumes creates, updates and deletes the Cluster's Volumes to match
// spec, and counts those ready.
func (r *ClusterReconciler) syncVolumes(ctx context.Context, cluster *fleetv1.Cluster, spec fleetv1.ClusterSpec) (int32, error) {
	volumes, err := r.ownedVolumes(ctx, r.Client, cluster)
	if err != nil {
		return 0, err
	}
	want := map[string]bool{}
	var ready int32
	for i := range spec.Volumes {
		name := r.volumeName(cluster, i)
		volume := &storagev1.Volume{ObjectMeta: metav1.ObjectMeta{Namespace: cluster.Namespace, Name: name}}
		if name == "" {
			volume.GenerateName = fmt.Sprintf("%s-vol-%d-", cluster.Name, i)
		}
		mutate := func() error {
			volume.Spec.Size = spec.VolumeSize
			if volume.Labels == nil {
				volume.Labels = map[string]string{}
			}
			volume.Labels[ClusterLabel] = cluster.Name
			return controllerutil.SetControllerReference(cluster, volume, r.Scheme)
		}
		if name == "" {
			if err := mutate(); err != nil {
				return 0, err
			}
			if err := r.Create(ctx, volume); err != nil {
				return 0, fmt.Errorf("creating Volume %s: %w", volume.GenerateName, err)
			}
			m := r.remember(cluster)
			m.volumes = append(m.volumes, volume.Name)
		} else if _, err := controllerutil.CreateOrUpdate(ctx, r.Client, volume, mutate); err != nil {
			return 0, fmt.Errorf("ensuring Volume %s: %w", name, err)
		}
		want[volume.Name] = true
		if volumeReady(volume) || r.Bug == ReadyBeforeVolumes {
			ready++
		}
	}
	for i := range volumes {
		if v := &volumes[i]; !want[v.Name] && r.knows(cluster, v) && v.DeletionTimestamp.IsZero() {
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
	if !controllerutil.ContainsFinalizer(cluster, fleetv1.ClusterFinalizer) {
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
	controllerutil.RemoveFinalizer(cluster, fleetv1.ClusterFinalizer)
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
