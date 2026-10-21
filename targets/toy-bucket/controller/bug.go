package controller

import "fmt"

// Bug selects the seeded bug the controller runs with. Zero is the correct
// controller. The bucket toy's bugs follow the Widget toy's B1 to B29.
type Bug int

const (
	// B30 leaks the bucket on CR delete.
	B30 Bug = iota + 30
	// B31 retries a create of unknown outcome under a new random name.
	B31
	// B32 names its bucket with a per-process nonce.
	B32
	// B33 reports ready right after the create.
	B33
	// B34 never recreates a missing bucket.
	B34
	// B35 retries a 503 every 10 ms.
	B35
)

// ParseBug converts a --bug value into a Bug.
func ParseBug(id int) (Bug, error) {
	if id != 0 && (id < int(B30) || id > int(B35)) {
		return 0, fmt.Errorf("--bug=%d: want 0, or a bug ID from %d to %d", id, B30, B35)
	}
	return Bug(id), nil
}
