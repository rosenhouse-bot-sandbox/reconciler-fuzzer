package config

import (
	"fmt"
	"regexp"
)

// Known is a finding the target is known to give, such as an upstream bug.
// A finding is known when its check and summary are equal and, if Details is
// set, it has detail lines and each one matches.
type Known struct {
	Check, Summary string
	Details        *regexp.Regexp
	// See says where the finding is described.
	See string
}

func known(d declaration) ([]Known, error) {
	var known []Known
	for i, declared := range d.Known {
		key := fmt.Sprintf("known[%d]", i)
		k := Known{Check: declared.Check, Summary: declared.Summary, See: declared.See}
		switch {
		case k.Check == "":
			return nil, fmt.Errorf("%s.check is required", key)
		case k.Summary == "":
			return nil, fmt.Errorf("%s.summary is required", key)
		case k.See == "":
			return nil, fmt.Errorf("%s.see is required", key)
		}
		if declared.Details != "" {
			var err error
			if k.Details, err = regexp.Compile(declared.Details); err != nil {
				return nil, fmt.Errorf("%s.details: %w", key, err)
			}
		}
		known = append(known, k)
	}
	return known, nil
}
