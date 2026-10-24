package controller

import "fmt"

// Bug selects the seeded bug the controller runs with. Zero is the correct
// controller. The fleet's bugs start at B100, past the other toys' bugs.
type Bug int

const (
	B150 Bug = 150
	B151 Bug = 151
	B152 Bug = 152
)

// bugs holds every seeded bug ID.
var bugs = map[Bug]bool{}

// ParseBug converts a --bug value into a Bug.
func ParseBug(id int) (Bug, error) {
	if id != 0 && !bugs[Bug(id)] {
		return 0, fmt.Errorf("--bug=%d: want 0 or a seeded bug ID", id)
	}
	return Bug(id), nil
}
