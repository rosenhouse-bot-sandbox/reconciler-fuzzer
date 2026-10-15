package target_test

import (
	"strings"
	"testing"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/rosenhouse-bot-sandbox/reconciler-fuzzer/internal/observe"
	"github.com/rosenhouse-bot-sandbox/reconciler-fuzzer/internal/target"
)

// The registry is process-wide, so the hooks register once rather than once
// per run of the tests.
var _ = func() bool {
	target.RegisterReady("alwaysReady", func(*unstructured.Unstructured) (bool, error) { return true, nil })
	target.RegisterEqual("neverEqual", func(a, b observe.Snapshot) bool { return false })
	return true
}()

func TestReadyHook(t *testing.T) {
	path := writeTarget(t, minimalTarget+"ready: 'go:alwaysReady'\n", map[string]string{"widget.yaml": sampleWidget})

	loaded, err := target.Load(path)
	if err != nil {
		t.Fatalf("Load rejected a registered ready hook: %v", err)
	}
	// The hook wins over CEL: this CR fails the default predicate.
	ready, err := loaded.Ready(widgetCR(1, 3, nil))
	if err != nil || !ready {
		t.Errorf("the ready hook returned (%v, %v), want (true, nil).", ready, err)
	}
}

func TestEqualHook(t *testing.T) {
	path := writeTarget(t, minimalTarget+"equal: 'go:neverEqual'\n", map[string]string{"widget.yaml": sampleWidget})

	loaded, err := target.Load(path)
	if err != nil {
		t.Fatalf("Load rejected a registered equal hook: %v", err)
	}
	if loaded.Equal == nil {
		t.Fatal("Load read no equality hook.")
	}
	if loaded.Equal(observe.Snapshot{}, observe.Snapshot{}) {
		t.Error("Load did not use the registered equality hook.")
	}
}

func TestEqualHookRefusesPathsItWouldIgnore(t *testing.T) {
	path := writeTarget(t, minimalTarget+"equal: 'go:neverEqual'\nequalIgnore:\n  - status.lastSyncTime\n", map[string]string{"widget.yaml": sampleWidget})

	_, err := target.Load(path)
	if err == nil || !strings.Contains(err.Error(), "equalIgnore") {
		t.Errorf("Load reported %v for a hook beside paths it would never read, want an error naming equalIgnore.", err)
	}
}

func TestRegisterRejectsADuplicateName(t *testing.T) {
	for _, tc := range []struct {
		name     string
		register func()
	}{
		{"ready", func() {
			target.RegisterReady("alwaysReady", func(*unstructured.Unstructured) (bool, error) { return false, nil })
		}},
		{"equal", func() {
			target.RegisterEqual("neverEqual", func(a, b observe.Snapshot) bool { return true })
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			defer func() {
				if recover() == nil {
					t.Errorf("registering a second %s hook under a name in use was accepted.", tc.name)
				}
			}()

			tc.register()
		})
	}
}
