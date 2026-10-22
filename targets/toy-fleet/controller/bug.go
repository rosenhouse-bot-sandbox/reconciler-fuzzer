package controller

import "fmt"

// Bug selects the seeded bug the controller runs with. Zero is the correct
// controller. The fleet's bugs start at B40. B40 to B42 are bugs of
// --version=2 that only CRs made by version 1 show.
type Bug int

const (
	// B40 never moves a v1 Cluster to the v2 finalizer, so its delete hangs.
	B40 Bug = iota + 40
	// B41 sees only Volumes with v2 names, and leaks the v1 ones.
	B41
	// B42 leaves a Cluster in the v1 phase Provisioning.
	B42
)

// bugs holds every seeded bug ID.
var bugs = map[Bug]bool{B40: true, B41: true, B42: true}

// ParseBug converts a --bug value into a Bug.
func ParseBug(id int) (Bug, error) {
	if id != 0 && !bugs[Bug(id)] {
		return 0, fmt.Errorf("--bug=%d: want 0 or a seeded bug ID", id)
	}
	return Bug(id), nil
}
