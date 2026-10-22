package controller

import "fmt"

// Bug selects the seeded bug the controller runs with. Zero is the correct
// controller. The fleet's bugs are B20 to B29 and from B40.
type Bug int

// Seeded bugs in the ordering across the fleet's kinds.
const (
	// B20 releases a Cluster while Databases still name it.
	B20 Bug = 20
	// B21 releases a Database while Users still name it.
	B21 Bug = 21
	// B22 does not requeue Users when their Database changes.
	B22 Bug = 22
	// B23 readies a Database on a Cluster that is not Ready.
	B23 Bug = 23
)

// bugs holds every seeded bug ID.
var bugs = map[Bug]bool{B20: true, B21: true, B22: true, B23: true}

// ParseBug converts a --bug value into a Bug.
func ParseBug(id int) (Bug, error) {
	if id != 0 && !bugs[Bug(id)] {
		return 0, fmt.Errorf("--bug=%d: want 0 or a seeded bug ID", id)
	}
	return Bug(id), nil
}
