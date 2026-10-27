package controller

import "fmt"

// Bug selects the seeded bug the controller runs with. Zero is the correct
// controller. The fleet's bugs start at B100, past the other toys' bugs.
type Bug int

const (
	// B116 removes every finalizer from a deleting Volume while its Cluster is
	// deleted.
	B116 Bug = 116
	// B135 ends each Volume's name with a suffix that each process picks, and
	// deletes only Volumes with its own suffix. So a restart makes a second
	// set.
	B135 Bug = 135
	// B150 strips its finalizers from deleting CRs when it shuts down.
	B150 Bug = 150
	// B151 puts a finalizer on each Volume, and releases a deleted Cluster
	// before its Volumes, whose finalizers it strips only afterwards.
	B151 Bug = 151
	// B152 errors when a dependent kind is not served, as after its CRD is
	// deleted, so the parent's deletion never ends.
	B152 Bug = 152
)

// bugs holds every seeded bug ID.
var bugs = map[Bug]bool{B116: true, B135: true, B150: true, B151: true, B152: true}

// ParseBug converts a --bug value into a Bug.
func ParseBug(id int) (Bug, error) {
	if id != 0 && !bugs[Bug(id)] {
		return 0, fmt.Errorf("--bug=%d: want 0 or a seeded bug ID", id)
	}
	return Bug(id), nil
}
