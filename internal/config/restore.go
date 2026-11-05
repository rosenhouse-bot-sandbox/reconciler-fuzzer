package config

import (
	"fmt"
	"slices"

	"github.com/rosenhouse-bot-sandbox/reconciler-fuzzer/internal/trace"
)

// notRestored loads the kinds Restore lets stay gone under a damaged root,
// such as a record of a finished operation.
func (c *Config) notRestored(declared []string) ([]trace.Kind, error) {
	var kinds []trace.Kind
	for i, d := range declared {
		key := fmt.Sprintf("notRestored[%d]", i)
		kind, err := parseGVK(d)
		if err != nil {
			return nil, fmt.Errorf("%s %q: %w", key, d, err)
		}
		served, err := c.kinds.serving(kind)
		if err != nil {
			return nil, fmt.Errorf("%s %s: %w", key, d, err)
		}
		if slices.Contains(c.ListedKinds(), kind.GroupKind()) {
			return nil, fmt.Errorf("%s %s: Restore never requires a listed kind back", key, d)
		}
		kinds = append(kinds, trace.Kind{Group: kind.Group, Version: kind.Version, Kind: kind.Kind, Resource: served.resource})
	}
	return kinds, nil
}
