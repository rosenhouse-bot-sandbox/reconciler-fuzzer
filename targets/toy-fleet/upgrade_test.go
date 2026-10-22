package main_test

import (
	"path/filepath"
	"testing"

	"github.com/rosenhouse-bot-sandbox/reconciler-fuzzer/internal/config"
	"github.com/rosenhouse-bot-sandbox/reconciler-fuzzer/internal/plan"
)

func TestUpgradeConfigsLoad(t *testing.T) {
	for _, path := range []string{"upgrade/config.yaml", "upgrade/properties.yaml"} {
		if _, err := config.Load(path); err != nil {
			t.Error(err)
		}
	}
}

func TestUpgradePlansLoad(t *testing.T) { loadAll(t, "upgrade/plans/*.json") }

// TestRestartActionsLoadWithUpgradeFields wants a restart that names the
// controller's new args and the CRDs it installs first, as an upgrade does.
// With overlap, the new process starts that long before the old one stops,
// as in a rolling update.
func TestRestartActionsLoadWithUpgradeFields(t *testing.T) { loadAll(t, "upgrade/proposed/*.json") }

func loadAll(t *testing.T, glob string) {
	paths, err := filepath.Glob(glob)
	if err != nil || len(paths) == 0 {
		t.Fatalf("%s matches %v, %v", glob, paths, err)
	}
	for _, path := range paths {
		if _, err := plan.Load(path); err != nil {
			t.Error(err)
		}
	}
}
