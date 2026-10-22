package controller

import "fmt"

// Bug selects the seeded bug the controller runs with. Zero is the correct
// controller. The fleet's bugs start at B40.
type Bug int

const (
	// B60 gives Volumes no owner, and releases a deleted Cluster before it
	// deletes them. A kill between the two leaks them.
	B60 Bug = 60
	// B61 finalizes only Clusters whose deletion this process started.
	B61 Bug = 61
	// B62 requeues a terminating Cluster at once while its Volumes go.
	B62 Bug = 62
)

// bugs holds every seeded bug ID.
var bugs = map[Bug]bool{B60: true, B61: true, B62: true}

// ParseBug converts a --bug value into a Bug.
func ParseBug(id int) (Bug, error) {
	if id != 0 && !bugs[Bug(id)] {
		return 0, fmt.Errorf("--bug=%d: want 0 or a seeded bug ID", id)
	}
	return Bug(id), nil
}
