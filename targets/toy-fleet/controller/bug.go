package controller

import "fmt"

// Bug selects the seeded bug the controller runs with. Zero is the correct
// controller. The fleet's bugs start at B40.
type Bug int

const (
	// B50 strips its finalizers from deleting CRs when it shuts down.
	B50 Bug = 50
	// B51 puts a finalizer on each Volume, and releases a deleted Cluster
	// before its Volumes, whose finalizers it strips only afterwards.
	B51 Bug = 51
	// B52 errors when a dependent kind is not served, as after its CRD is
	// deleted, so the parent's deletion never ends.
	B52 Bug = 52
)

// bugs holds every seeded bug ID.
var bugs = map[Bug]bool{B50: true, B51: true, B52: true}

// ParseBug converts a --bug value into a Bug.
func ParseBug(id int) (Bug, error) {
	if id != 0 && !bugs[Bug(id)] {
		return 0, fmt.Errorf("--bug=%d: want 0 or a seeded bug ID", id)
	}
	return Bug(id), nil
}
