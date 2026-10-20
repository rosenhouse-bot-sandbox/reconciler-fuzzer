package controller

import (
	"strconv"
	"strings"
	"testing"
)

func TestParseBugAcceptsEveryCatalogEntry(t *testing.T) {
	// The bugs map in ../bugmatrix_test.go ends at B25. Deriving the range
	// from MaxBug alone would accept a MaxBug that had lost an entry.
	if MaxBug != int(B25) {
		t.Fatalf("MaxBug is %d, and the bugs map in ../bugmatrix_test.go ends at B25.", MaxBug)
	}
	for id := 0; id <= MaxBug; id++ {
		bug, err := ParseBug(id)
		if err != nil {
			t.Errorf("ParseBug(%d) returned an error: %v", id, err)
		}
		if int(bug) != id {
			t.Errorf("ParseBug(%d) returned bug %d.", id, bug)
		}
	}
}

func TestParseBugRejectsIDsOutsideTheCatalog(t *testing.T) {
	for _, id := range []int{-1, MaxBug + 1} {
		_, err := ParseBug(id)
		if err == nil {
			t.Fatalf("ParseBug(%d) accepted an ID outside the catalog.", id)
		}
		if !strings.Contains(err.Error(), strconv.Itoa(MaxBug)) {
			t.Errorf("ParseBug(%d) returned %q, which does not name the valid range.", id, err)
		}
	}
}
