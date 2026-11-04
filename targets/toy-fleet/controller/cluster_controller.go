package controller

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"time"

	k8sstoragev1 "k8s.io/api/storage/v1"
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

// ClusterLabel names a Volume's or StorageClass's Cluster, and
// NamespaceLabel a StorageClass's Cluster's namespace. Version 2 also sets
// VersionLabel to 2, as a release often labels its children with its version.
const (
	ClusterLabel   = "fleet.toy/cluster"
	NamespaceLabel = "fleet.toy/namespace"
	VersionLabel   = "fleet.toy/version"
)

// Provisioner is the provisioner of each Cluster's StorageClass.
const Provisioner = "storage.thirdparty/volumes"

// manualCleanup is the message of a Cluster that B117 stopped finalizing.
const manualCleanup = "a Volume failed; clean up by hand"

// gaveUp starts the message of a Cluster that B133 gave up on.
const gaveUp = "gave up: "

// ClusterReconciler moves a Cluster through its phases and keeps its Volumes.
type ClusterReconciler struct {
	reconciler
	// generated holds, under B132, the names of the Volumes this process made
	// for each Cluster, by index.
	generated map[types.UID][]string
	// suffix ends the name of each Volume this process makes under B135.
	suffix string
	// finalizing holds, under B161, the Clusters this process began to finalize.
	finalizing map[types.NamespacedName]bool
	// storageClasses gives each Cluster a StorageClass. The StorageClass is
	// cluster-scoped, so the garbage collector resolves no ownerReference
	// from it to the namespaced Cluster.
	storageClasses bool
}

func (r *ClusterReconciler) SetupWithManager(m ctrl.Manager) error {
	b := ctrl.NewControllerManagedBy(m).
		For(&fleetv1.Cluster{}).
		Watches(&fleetv1.Database{}, handler.EnqueueRequestsFromMapFunc(func(_ context.Context, o client.Object) []reconcile.Request {
			return []reconcile.Request{{NamespacedName: types.NamespacedName{Namespace: o.GetNamespace(), Name: o.(*fleetv1.Database).Spec.ClusterRef}}}
		}))
	if r.storageClasses {
		b = b.Watches(&k8sstoragev1.StorageClass{}, handler.EnqueueRequestsFromMapFunc(clusterOfStorageClass))
	}
	if r.Bug == B160 {
		return b.Watches(&storagev1.Volume{}, handler.EnqueueRequestsFromMapFunc(clusterOfVolume)).Complete(r)
	}
	return b.Owns(&storagev1.Volume{}).Complete(r)
}

// clusterOfStorageClass maps a StorageClass to the Cluster its labels name.
func clusterOfStorageClass(_ context.Context, o client.Object) []reconcile.Request {
	labels := o.GetLabels()
	if labels[ClusterLabel] == "" {
		return nil
	}
	return []reconcile.Request{{NamespacedName: types.NamespacedName{Namespace: labels[NamespaceLabel], Name: labels[ClusterLabel]}}}
}

// clusterOfVolume maps a Volume to the Cluster its label names.
func clusterOfVolume(_ context.Context, o client.Object) []reconcile.Request {
	name := o.GetLabels()[ClusterLabel]
	if name == "" {
		return nil
	}
	return []reconcile.Request{{NamespacedName: types.NamespacedName{Namespace: o.GetNamespace(), Name: name}}}
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
		return r.finalize(ctx, cluster)
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
	case r.Bug == B133 && strings.HasPrefix(old.Message, gaveUp):
		return ctrl.Result{}, nil
	}
	if err := r.syncStorageClass(ctx, cluster); err != nil {
		return ctrl.Result{}, err
	}
	ready, err := r.syncVolumes(ctx, cluster)
	if err != nil && r.Bug == B133 {
		cluster.Status.Phase, cluster.Status.Message = r.provisioning(), gaveUp+err.Error()
		return ctrl.Result{}, r.writeStatus(ctx, cluster, old, cluster.Status)
	}
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
		volume, err := r.ensureVolume(ctx, cluster, i)
		if err != nil {
			return 0, err
		}
		want[volume.Name] = true
		if r.countsReady(volume) {
			ready++
		}
	}
	for i := range volumes {
		if v := &volumes[i]; !want[v.Name] && r.mine(cluster, v) && v.DeletionTimestamp.IsZero() {
			if err := r.deleteVolume(ctx, v); err != nil {
				return 0, err
			}
			if r.Bug == B132 {
				// B132 forgets the name of each Volume it deletes.
				r.generated[cluster.UID] = slices.DeleteFunc(r.generated[cluster.UID], func(name string) bool { return name == v.Name })
			}
		}
	}
	return ready, nil
}

// ensureVolume creates or updates the Cluster's ith Volume. B132 creates a
// Volume it has no name for with generateName.
func (r *ClusterReconciler) ensureVolume(ctx context.Context, cluster *fleetv1.Cluster, i int32) (*storagev1.Volume, error) {
	name := r.volumeName(cluster, i)
	volume := &storagev1.Volume{ObjectMeta: metav1.ObjectMeta{Namespace: cluster.Namespace, Name: name}}
	mutate := func() error {
		volume.Spec.Size = cluster.Spec.VolumeSize
		if volume.Labels == nil {
			volume.Labels = map[string]string{}
		}
		volume.Labels[ClusterLabel] = cluster.Name
		if r.Version == 2 {
			volume.Labels[VersionLabel] = "2"
		}
		if r.Bug == B151 {
			controllerutil.AddFinalizer(volume, VolumeFinalizer)
		}
		if r.Bug == B160 {
			return nil
		}
		return controllerutil.SetControllerReference(cluster, volume, r.Scheme)
	}
	if name == "" {
		volume.GenerateName = r.baseName(cluster, i) + "-"
		if err := mutate(); err != nil {
			return nil, err
		}
		if err := r.Create(ctx, volume); err != nil {
			return nil, fmt.Errorf("creating Volume %s: %w", volume.GenerateName, err)
		}
		if r.generated == nil {
			r.generated = map[types.UID][]string{}
		}
		r.generated[cluster.UID] = append(r.generated[cluster.UID], volume.Name)
		return volume, nil
	}
	if _, err := controllerutil.CreateOrUpdate(ctx, r.Client, volume, mutate); err != nil {
		return nil, fmt.Errorf("ensuring Volume %s: %w", name, err)
	}
	return volume, nil
}

// deleteVolume deletes the Volume. B118 orphans its dependents.
func (r *ClusterReconciler) deleteVolume(ctx context.Context, v *storagev1.Volume) error {
	var opts []client.DeleteOption
	if r.Bug == B118 {
		opts = append(opts, client.PropagationPolicy(metav1.DeletePropagationOrphan))
	}
	if err := r.Delete(ctx, v, opts...); client.IgnoreNotFound(err) != nil {
		return fmt.Errorf("deleting Volume %s: %w", v.Name, err)
	}
	return nil
}

// storageClassOf is the Cluster's StorageClass.
func storageClassOf(cluster *fleetv1.Cluster) *k8sstoragev1.StorageClass {
	return &k8sstoragev1.StorageClass{ObjectMeta: metav1.ObjectMeta{Name: "fleet-" + cluster.Namespace + "-" + cluster.Name}}
}

// syncStorageClass creates the Cluster's StorageClass, when it has one.
func (r *ClusterReconciler) syncStorageClass(ctx context.Context, cluster *fleetv1.Cluster) error {
	if !r.storageClasses {
		return nil
	}
	sc := storageClassOf(cluster)
	if _, err := controllerutil.CreateOrUpdate(ctx, r.Client, sc, func() error {
		sc.Labels = map[string]string{ClusterLabel: cluster.Name, NamespaceLabel: cluster.Namespace}
		sc.Provisioner = Provisioner
		return nil
	}); err != nil {
		return fmt.Errorf("ensuring StorageClass %s: %w", sc.Name, err)
	}
	return nil
}

// deleteStorageClass deletes the Cluster's StorageClass, when it has one.
func (r *ClusterReconciler) deleteStorageClass(ctx context.Context, cluster *fleetv1.Cluster) error {
	if !r.storageClasses {
		return nil
	}
	sc := storageClassOf(cluster)
	if err := r.Delete(ctx, sc); client.IgnoreNotFound(err) != nil {
		return fmt.Errorf("deleting StorageClass %s: %w", sc.Name, err)
	}
	return nil
}

// baseName is the name of the Cluster's ith Volume before any suffix.
func (r *ClusterReconciler) baseName(cluster *fleetv1.Cluster, i int32) string {
	return fmt.Sprintf("%s%d", r.volumePrefix(cluster), i)
}

// volumeName names the Cluster's ith Volume. B132 knows only the names of
// the Volumes this process made, and gives "" for any other.
func (r *ClusterReconciler) volumeName(cluster *fleetv1.Cluster, i int32) string {
	switch r.Bug {
	case B132:
		if names := r.generated[cluster.UID]; int(i) < len(names) {
			return names[i]
		}
		return ""
	case B135:
		return r.baseName(cluster, i) + "-" + r.suffix
	}
	return r.baseName(cluster, i)
}

// mine says whether this process counts the Cluster's Volume as its own.
// B132 and B135 count only the Volumes this process made.
func (r *ClusterReconciler) mine(cluster *fleetv1.Cluster, v *storagev1.Volume) bool {
	switch r.Bug {
	case B132:
		return slices.Contains(r.generated[cluster.UID], v.Name)
	case B135:
		return strings.HasSuffix(v.Name, "-"+r.suffix)
	}
	return true
}

// finalize waits until no Database names the Cluster, deletes its Volumes,
// waits until none remains, deletes its StorageClass, and then removes its
// finalizers.
func (r *ClusterReconciler) finalize(ctx context.Context, cluster *fleetv1.Cluster) (ctrl.Result, error) {
	mine := r.finalizers()
	if !slices.ContainsFunc(mine, func(f string) bool { return controllerutil.ContainsFinalizer(cluster, f) }) {
		return ctrl.Result{}, nil
	}
	if r.Bug == B117 && cluster.Status.Message == manualCleanup {
		return ctrl.Result{}, nil
	}
	if r.Bug == B161 {
		key := client.ObjectKeyFromObject(cluster)
		if cluster.Status.Phase == fleetv1.Deleting && !r.finalizing[key] {
			return ctrl.Result{}, nil
		}
		if r.finalizing == nil {
			r.finalizing = map[types.NamespacedName]bool{}
		}
		r.finalizing[key] = true
	}
	databases := &fleetv1.DatabaseList{}
	if err := r.APIReader.List(ctx, databases, client.InNamespace(cluster.Namespace)); err != nil && !r.unserved(err) {
		return ctrl.Result{}, fmt.Errorf("listing Databases: %w", err)
	}
	named := r.Bug != B120 && slices.ContainsFunc(databases.Items, func(db fleetv1.Database) bool { return db.Spec.ClusterRef == cluster.Name })
	volumes, err := r.ownedVolumes(ctx, r.APIReader, cluster)
	if err != nil {
		return ctrl.Result{}, err
	}
	old := cluster.Status
	cluster.Status.Phase = fleetv1.Deleting
	failed := r.Bug == B117 && slices.ContainsFunc(volumes, volumeFailed)
	switch {
	case named:
		cluster.Status.Message = "waiting until no Database names the Cluster"
	case failed:
		cluster.Status.Message = manualCleanup
	case len(volumes) > 0:
		cluster.Status.Message = "waiting until no Volume of the Cluster remains"
	default:
		cluster.Status.Message = ""
	}
	if err := r.writeStatus(ctx, cluster, old, cluster.Status); err != nil {
		return ctrl.Result{}, err
	}
	if named || failed {
		return ctrl.Result{}, nil // A Database's deletion requeues the Cluster.
	}
	if r.Bug == B160 {
		if err := r.release(ctx, cluster); err != nil {
			return ctrl.Result{}, err
		}
	}
	for i := range volumes {
		if v := &volumes[i]; v.DeletionTimestamp.IsZero() || r.Bug == B162 {
			if err := r.deleteVolume(ctx, v); err != nil {
				return ctrl.Result{}, err
			}
		} else if r.Bug == B116 {
			v.Finalizers = nil
			if err := r.Update(ctx, v); client.IgnoreNotFound(err) != nil {
				return ctrl.Result{}, fmt.Errorf("stripping Volume %s: %w", v.Name, err)
			}
		}
	}
	switch {
	case r.Bug == B160:
		return ctrl.Result{}, nil // It released the Cluster above.
	case len(volumes) > 0 && r.Bug == B162:
		return ctrl.Result{RequeueAfter: time.Millisecond}, nil
	case len(volumes) > 0 && r.Bug != B151 && r.Bug != B115:
		return ctrl.Result{}, nil // A Volume's deletion requeues the Cluster.
	case r.Bug == B119:
		if err := r.release(ctx, cluster); err != nil {
			return ctrl.Result{}, err
		}
		return ctrl.Result{}, r.deleteStorageClass(ctx, cluster)
	}
	if err := r.deleteStorageClass(ctx, cluster); err != nil {
		return ctrl.Result{}, err
	}
	return ctrl.Result{}, r.release(ctx, cluster)
}

// release removes this version's finalizers from the Cluster.
func (r *ClusterReconciler) release(ctx context.Context, cluster *fleetv1.Cluster) error {
	for _, f := range r.finalizers() {
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

// ownedVolumes lists, through reader, the Volumes the Cluster controls, or
// under B160 those its label names.
func (r *ClusterReconciler) ownedVolumes(ctx context.Context, reader client.Reader, cluster *fleetv1.Cluster) ([]storagev1.Volume, error) {
	list := &storagev1.VolumeList{}
	if err := reader.List(ctx, list, client.InNamespace(cluster.Namespace), client.MatchingLabels{ClusterLabel: cluster.Name}); err != nil {
		return nil, fmt.Errorf("listing Volumes: %w", err)
	}
	var owned []storagev1.Volume
	for _, v := range list.Items {
		if (metav1.IsControlledBy(&v, cluster) || r.Bug == B160) && (!r.renames() || strings.HasPrefix(v.Name, r.volumePrefix(cluster))) {
			owned = append(owned, v)
		}
	}
	return owned, nil
}

// volumeFailed says whether the Volume's Ready condition is False.
func volumeFailed(v storagev1.Volume) bool {
	return meta.IsStatusConditionFalse(v.Status.Conditions, "Ready")
}

// countsReady says whether the Cluster counts the Volume ready: live, with a
// Ready condition True for its current generation. B131 counts every Volume,
// and B172 a Ready condition of any generation.
func (r *ClusterReconciler) countsReady(v *storagev1.Volume) bool {
	if r.Bug == B131 {
		return true
	}
	c := meta.FindStatusCondition(v.Status.Conditions, "Ready")
	return v.DeletionTimestamp.IsZero() && c != nil && c.Status == metav1.ConditionTrue &&
		(c.ObservedGeneration == v.Generation || r.Bug == B172)
}
