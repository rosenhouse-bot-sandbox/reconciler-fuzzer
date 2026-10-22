package controller

import "fmt"

// Bug selects the seeded bug the controller runs with. Zero is the correct
// controller.
type Bug int

// Cluster phase bugs.
const (
	// PhaseRegresses drops a Ready Cluster to Provisioning once per
	// generation, on the next resync.
	PhaseRegresses Bug = 30
	// ReadyBeforeVolumes counts every Volume as ready.
	ReadyBeforeVolumes Bug = 31
	// DuplicateVolumesAfterRestart names Volumes with generateName and
	// remembers them only in memory, so a restart creates a second set.
	DuplicateVolumesAfterRestart Bug = 32
	// StuckAfterCreateError gives up for good when a Volume write fails.
	StuckAfterCreateError Bug = 33
	// IgnoresSpecDuringProvisioning keeps provisioning the spec it first
	// saw, and reports each new generation as observed.
	IgnoresSpecDuringProvisioning Bug = 34
	// RenamedVolumesAfterRestart is DuplicateVolumesAfterRestart with
	// fixed names that carry a per-process suffix.
	RenamedVolumesAfterRestart Bug = 35
)

// bugs holds every seeded bug ID.
var bugs = map[Bug]bool{
	PhaseRegresses: true, ReadyBeforeVolumes: true, DuplicateVolumesAfterRestart: true,
	StuckAfterCreateError: true, IgnoresSpecDuringProvisioning: true, RenamedVolumesAfterRestart: true,
}

// ParseBug converts a --bug value into a Bug.
func ParseBug(id int) (Bug, error) {
	if id != 0 && !bugs[Bug(id)] {
		return 0, fmt.Errorf("--bug=%d: want 0 or a seeded bug ID", id)
	}
	return Bug(id), nil
}
