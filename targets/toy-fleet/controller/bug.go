package controller

import "fmt"

// Bug selects the seeded bug the controller runs with. Zero is the correct
// controller. The fleet's bugs start at B100, past the other toys' bugs.
type Bug int

// These seeded bugs break what a Database waits for.
const (
	// B123 readies a Database on a Cluster that is not Ready.
	B123 Bug = 123
	// B124 readies a Database without checking its Secret.
	B124 Bug = 124
	// B125 keeps a Ready Database ready after its Secret is deleted.
	B125 Bug = 125
)

// These seeded bugs break an uninstall.
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

// bugs holds every seeded bug ID.
var bugs = map[Bug]bool{B123: true, B124: true, B125: true, B150: true, B151: true, B152: true}

// ParseBug converts a --bug value into a Bug.
func ParseBug(id int) (Bug, error) {
	if id != 0 && !bugs[Bug(id)] {
		return 0, fmt.Errorf("--bug=%d: want 0 or a seeded bug ID", id)
	}
	return Bug(id), nil
}
