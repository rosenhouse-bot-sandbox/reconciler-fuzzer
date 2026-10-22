package main_test

import (
	"testing"

	"github.com/rosenhouse-bot-sandbox/reconciler-fuzzer/internal/plan"
)

// TestAPlanCanUpgradeTheController wants a restart that starts another
// binary or other args, as an upgrade does.
func TestAPlanCanUpgradeTheController(t *testing.T) {
	if _, err := plan.Load("gaps/proposed/upgrade.json"); err != nil {
		t.Error(err)
	}
}
