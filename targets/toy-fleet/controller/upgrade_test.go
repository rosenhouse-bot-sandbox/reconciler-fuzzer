package controller

import (
	"slices"
	"testing"

	apierrors "k8s.io/apimachinery/pkg/api/errors"

	fleetv1 "github.com/rosenhouse-bot-sandbox/reconciler-fuzzer/targets/toy-fleet/api/v1"
)

// as switches f to the controllers of version, with bug.
func (f *fleet) as(version int, bug Bug) *fleet {
	base := reconciler{Client: f.c, APIReader: f.c, Scheme: f.c.Scheme(), Bug: bug, Version: version}
	f.clusters, f.databases, f.users = &ClusterReconciler{base}, &DatabaseReconciler{base}, &UserReconciler{base}
	return f
}

// version1Cluster returns a fleet whose Cluster version 1 made, Ready when
// ready and Provisioning otherwise.
func version1Cluster(t *testing.T, ready bool) *fleet {
	f := newFleet(t, clusterObj())
	f.settle(f.clusters, "cluster")
	if ready {
		f.readyVolumes()
		f.settle(f.clusters, "cluster")
	}
	return f
}

func (f *fleet) sortedVolumes() []string {
	f.t.Helper()
	return slices.Sorted(slices.Values(f.volumes()))
}

func TestVersion2RenamesItsFinalizerAndPhaseButNotItsVolumes(t *testing.T) {
	f := newFleet(t, clusterObj()).as(2, 0)
	f.settle(f.clusters, "cluster")
	if got := f.sortedVolumes(); !slices.Equal(got, []string{"cluster-vol-0 small", "cluster-vol-1 small"}) {
		t.Errorf("Version 2 made the Volumes %q.", got)
	}
	if c := f.cluster(); c.Status.Phase != fleetv1.Creating || !slices.Equal(c.Finalizers, []string{fleetv1.ClusterFinalizerV2}) {
		t.Errorf("Version 2 left the Cluster %+v with finalizers %v.", c.Status, c.Finalizers)
	}
}

func TestVersion2TakesOverAVersion1ClusterAndDeletesIt(t *testing.T) {
	f := version1Cluster(t, true).as(2, 0)
	f.settle(f.clusters, "cluster")
	if c := f.cluster(); c.Status.Phase != fleetv1.Ready || !slices.Equal(c.Finalizers, []string{fleetv1.ClusterFinalizerV2}) {
		t.Errorf("Version 2 left the Cluster %+v with finalizers %v.", c.Status, c.Finalizers)
	}
	if got := f.sortedVolumes(); !slices.Equal(got, []string{"cluster-vol-0 small", "cluster-vol-1 small"}) {
		t.Errorf("Version 2 left the Volumes %q, want version 1's.", got)
	}
	f.delete(f.cluster())
	f.settle(f.clusters, "cluster")
	if err := f.get("cluster", &fleetv1.Cluster{}); !apierrors.IsNotFound(err) {
		t.Errorf("The deleted Cluster gives %v, want not found.", err)
	}
}

func TestVersion2ReleasesAClusterDeletedUnderVersion1(t *testing.T) {
	f := version1Cluster(t, true)
	f.delete(f.cluster())
	f.as(2, 0).settle(f.clusters, "cluster")
	if err := f.get("cluster", &fleetv1.Cluster{}); !apierrors.IsNotFound(err) {
		t.Errorf("The deleted Cluster gives %v, want not found.", err)
	}
}

func TestVersion2FinishesAVersion1ClusterStillProvisioning(t *testing.T) {
	f := version1Cluster(t, false).as(2, 0)
	f.settle(f.clusters, "cluster")
	f.readyVolumes()
	f.settle(f.clusters, "cluster")
	if c := f.cluster(); c.Status.Phase != fleetv1.Ready {
		t.Errorf("The Cluster is %+v, want Ready.", c.Status)
	}
}

func TestB140KeepsTheVersion1Finalizer(t *testing.T) {
	f := version1Cluster(t, true).as(2, B140)
	f.settle(f.clusters, "cluster")
	f.delete(f.cluster())
	f.settle(f.clusters, "cluster")
	if c := f.cluster(); !slices.Equal(c.Finalizers, []string{fleetv1.ClusterFinalizer}) {
		t.Errorf("The deleted Cluster has finalizers %v, want version 1's.", c.Finalizers)
	}
}

func TestB141MakesVersion2VolumesBesideVersion1Ones(t *testing.T) {
	f := version1Cluster(t, true).as(2, B141)
	f.settle(f.clusters, "cluster")
	want := []string{"cluster-data-0 small", "cluster-data-1 small", "cluster-vol-0 small", "cluster-vol-1 small"}
	if got := f.sortedVolumes(); !slices.Equal(got, want) {
		t.Errorf("The Volumes are %q, want %q.", got, want)
	}
}

// A version without B141 counts every Volume the Cluster controls.
func TestAFixedVersionDeletesTheVolumesB141Made(t *testing.T) {
	f := version1Cluster(t, true).as(2, B141)
	f.settle(f.clusters, "cluster")
	f.as(2, 0).settle(f.clusters, "cluster")
	if got := f.sortedVolumes(); !slices.Equal(got, []string{"cluster-vol-0 small", "cluster-vol-1 small"}) {
		t.Errorf("The Volumes are %q, want only those version 2 names.", got)
	}
}

func TestB142ReadiesAClusterThatVersion2Made(t *testing.T) {
	f := newFleet(t, clusterObj()).as(2, B142)
	f.settle(f.clusters, "cluster")
	f.readyVolumes()
	f.settle(f.clusters, "cluster")
	if c := f.cluster(); c.Status.Phase != fleetv1.Ready {
		t.Errorf("The Cluster is %+v, want Ready.", c.Status)
	}
}

func TestB142LeavesAVersion1ClusterProvisioning(t *testing.T) {
	f := version1Cluster(t, false).as(2, B142)
	f.settle(f.clusters, "cluster")
	f.readyVolumes()
	f.settle(f.clusters, "cluster")
	if c := f.cluster(); c.Status.Phase != fleetv1.Provisioning {
		t.Errorf("The Cluster is %+v, want Provisioning.", c.Status)
	}
}

// B140 to B142 are bugs of version 2, so version 1 runs correctly under them.
func TestVersion1IgnoresTheVersion2Bugs(t *testing.T) {
	for _, bug := range []Bug{B140, B141, B142} {
		f := newBuggyFleet(t, bug, clusterObj())
		f.settle(f.clusters, "cluster")
		f.readyVolumes()
		f.settle(f.clusters, "cluster")
		if c := f.cluster(); c.Status.Phase != fleetv1.Ready || !slices.Equal(c.Finalizers, []string{fleetv1.ClusterFinalizer}) {
			t.Errorf("B%d left the Cluster %+v with finalizers %v, want Ready under version 1's finalizer.", bug, c.Status, c.Finalizers)
		}
		if got := f.sortedVolumes(); !slices.Equal(got, []string{"cluster-vol-0 small", "cluster-vol-1 small"}) {
			t.Errorf("B%d made the Volumes %q, want version 1's.", bug, got)
		}
		f.delete(f.cluster())
		f.settle(f.clusters, "cluster")
		if err := f.get("cluster", &fleetv1.Cluster{}); !apierrors.IsNotFound(err) {
			t.Errorf("B%d: the deleted Cluster gives %v, want not found.", bug, err)
		}
	}
}
