package controller

import "fmt"

// Bug selects the seeded bug the controller runs with. Zero is the correct
// controller. The fleet's bugs start at B100, past the other toys' bugs.
type Bug int

const (
	// B116 removes every finalizer from a deleting Volume while its Cluster is
	// deleted.
	B116 Bug = 116
	// B150 strips its finalizers from deleting CRs when it shuts down.
	B150 Bug = 150
	// B151 puts a finalizer on each Volume, and releases a deleted Cluster
	// before its Volumes, whose finalizers it strips only afterwards.
	B151 Bug = 151
	// B152 errors when a dependent kind is not served, as after its CRD is
	// deleted, so the parent's deletion never ends.
	B152 Bug = 152
	// B160 gives Volumes no owner, and releases a deleted Cluster before it
	// deletes them. A kill between the two leaks them.
	B160 Bug = 160
	// B161 finalizes only Clusters whose deletion this process started.
	B161 Bug = 161
)

// bugs holds every seeded bug ID.
var bugs = map[Bug]bool{B116: true, B150: true, B151: true, B152: true, B160: true, B161: true}

// ParseBug converts a --bug value into a Bug.
func ParseBug(id int) (Bug, error) {
	if id != 0 && !bugs[Bug(id)] {
		return 0, fmt.Errorf("--bug=%d: want 0 or a seeded bug ID", id)
	}
	return Bug(id), nil
}
