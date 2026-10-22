package controller

import "fmt"

// Bug selects the seeded bug the controller runs with. Zero is the correct
// controller. The fleet's bugs start at B40.
type Bug int

// Startup bugs, which main applies.
const (
	// B70 exits before starting the manager.
	B70 Bug = 70
	// B71 ignores KUBECONFIG and dials localhost:8080.
	B71 Bug = 71
)

// B72 counts a Volume ready on a Ready condition of an older generation.
const B72 Bug = 72

// bugs holds every seeded bug ID.
var bugs = map[Bug]bool{B70: true, B71: true, B72: true}

// ParseBug converts a --bug value into a Bug.
func ParseBug(id int) (Bug, error) {
	if id != 0 && !bugs[Bug(id)] {
		return 0, fmt.Errorf("--bug=%d: want 0 or a seeded bug ID", id)
	}
	return Bug(id), nil
}
