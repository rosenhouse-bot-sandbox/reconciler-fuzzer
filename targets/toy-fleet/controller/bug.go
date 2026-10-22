package controller

import "fmt"

// Bug selects the seeded bug the controller runs with. Zero is the correct
// controller. The fleet's bugs start at B40.
type Bug int

// The fleet's finalizer-order bugs.
const (
	// ReleaseBeforeVolumesGo removes the Cluster's finalizer once it has
	// deleted its Volumes, without waiting for them to go.
	ReleaseBeforeVolumesGo Bug = 15
	// StripVolumeFinalizers removes every finalizer from a deleting Volume.
	StripVolumeFinalizers Bug = 16
	// GiveUpOnStuckVolume stops finalizing a Cluster for good once a Volume
	// it would delete reports Ready False.
	GiveUpOnStuckVolume Bug = 17
	// OrphanVolumes deletes Volumes with Orphan propagation.
	OrphanVolumes Bug = 18
)

// bugs holds every seeded bug ID.
var bugs = map[Bug]bool{ReleaseBeforeVolumesGo: true, StripVolumeFinalizers: true, GiveUpOnStuckVolume: true, OrphanVolumes: true}

// ParseBug converts a --bug value into a Bug.
func ParseBug(id int) (Bug, error) {
	if id != 0 && !bugs[Bug(id)] {
		return 0, fmt.Errorf("--bug=%d: want 0 or a seeded bug ID", id)
	}
	return Bug(id), nil
}
