package target_test

import (
	"errors"
	"strings"
	"testing"

	"github.com/rosenhouse-bot-sandbox/reconciler-fuzzer/internal/target"
)

func loadWith(t *testing.T, extraYAML string) *target.Target {
	t.Helper()
	path := writeTarget(t, minimalTarget+extraYAML, map[string]string{"widget.yaml": sampleWidget})
	loaded, err := target.Load(path)
	if err != nil {
		t.Fatalf("Load rejected the target: %v", err)
	}
	return loaded
}

// TestPredicatesBindMissingFieldsAsEmptyMaps pins the binding of DESIGN.md
// §8.4, which an expression that reads a whole field depends on.
func TestPredicatesBindMissingFieldsAsEmptyMaps(t *testing.T) {
	loaded := loadWith(t, "ready: size(metadata) + size(spec) + size(status) == 0\n"+
		"properties:\n  - id: P1\n    cel: size(status) == 0 && managed.size() == 0\n")
	bare := widgetBare()

	ready, err := loaded.Ready(bare)
	if err != nil {
		t.Fatalf("ready returned an error on an object with no fields: %v", err)
	}
	if !ready {
		t.Error("ready did not see empty maps for the missing fields.")
	}

	held, err := loaded.Properties[0].Eval(bare, nil)
	if err != nil {
		t.Fatalf("P1 returned an error on an object with no fields: %v", err)
	}
	if !held {
		t.Error("P1 did not see an empty map for the missing status.")
	}
}

func TestPredicateEvalErrorsAreInspectable(t *testing.T) {
	loaded := loadWith(t, "ready: status.ready > 0\n"+
		"properties:\n  - id: P1\n    cel: status.ready > managed.size()\n")
	// status.ready holds a string, so both predicates fail at evaluation.
	cr := widgetCR(1, 1, map[string]any{"ready": "many"})

	ready, err := loaded.Ready(cr)
	if ready {
		t.Error("ready returned true although it failed to evaluate.")
	}
	var readyErr *target.EvalError
	if !errors.As(err, &readyErr) {
		t.Fatalf("ready returned %v, which is not an *EvalError.", err)
	}
	if readyErr.Predicate != "ready" || readyErr.Expr != "status.ready > 0" {
		t.Errorf("ready reported predicate %q and expression %q.", readyErr.Predicate, readyErr.Expr)
	}
	if !strings.Contains(readyErr.Error(), readyErr.Expr) {
		t.Errorf("the error %q does not quote the expression.", readyErr)
	}

	_, err = loaded.Properties[0].Eval(cr, nil)
	var propertyErr *target.EvalError
	if !errors.As(err, &propertyErr) {
		t.Fatalf("P1 returned %v, which is not an *EvalError.", err)
	}
	if propertyErr.Predicate != "P1" {
		t.Errorf("P1 reported predicate %q.", propertyErr.Predicate)
	}
}

func TestPropertyWhenDefaultsToCheckpoint(t *testing.T) {
	loaded := loadWith(t, "properties:\n  - id: P1\n    cel: 'true'\n")

	if when := loaded.Properties[0].When; when != target.Checkpoint {
		t.Errorf("Load defaulted when to %q, want %q.", when, target.Checkpoint)
	}
}

// TestPredicateRejectsANonBooleanResult covers an expression that compiles as
// dyn and yields something else, which DESIGN.md §8.4 makes a configuration
// error rather than "not ready".
func TestPredicateRejectsANonBooleanResult(t *testing.T) {
	loaded := loadWith(t, "ready: status.phase\n")

	ready, err := loaded.Ready(widgetCR(1, 1, map[string]any{"phase": "Running"}))
	if ready {
		t.Error("ready returned true for a string result.")
	}
	if !errors.Is(err, target.ErrNotBool) {
		t.Errorf("ready returned %v, which is not ErrNotBool.", err)
	}
}
