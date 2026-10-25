package controller

import "fmt"

// Bug selects the seeded bug the controller runs with. Zero is the correct
// controller. The fleet's bugs start at B100, past the other toys' bugs.
type Bug int

const (
	// B150 strips its finalizers from deleting CRs when it shuts down.
	B150 Bug = 150
	// B151 puts a finalizer on each Volume, and releases a deleted Cluster
	// before its Volumes, whose finalizers it strips only afterwards.
	B151 Bug = 151
	// B152 errors when a dependent kind is not served, as after its CRD is
	// deleted, so the parent's deletion never ends.
	B152 Bug = 152
)

// B140 to B142 are bugs of version 2 that show only on a Cluster that
// version 1 made.
const (
	// B140 never removes the version 1 finalizer, so the Cluster's delete
	// hangs.
	B140 Bug = 140
	// B141 names its Volumes <cluster>-data-<i> and sees only those, so it
	// makes new ones and leaves the version 1 ones.
	B141 Bug = 141
	// B142 leaves the Cluster in the version 1 phase Provisioning.
	B142 Bug = 142
)

// bugs holds every seeded bug ID.
var bugs = map[Bug]bool{B150: true, B151: true, B152: true}

// ParseBug converts a --bug value into a Bug.
func ParseBug(id int) (Bug, error) {
	if id != 0 && !bugs[Bug(id)] {
		return 0, fmt.Errorf("--bug=%d: want 0 or a seeded bug ID", id)
	}
	return Bug(id), nil
}
