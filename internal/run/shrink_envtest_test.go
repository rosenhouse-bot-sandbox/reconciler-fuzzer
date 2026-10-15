//go:build envtest

package run_test

import (
	"context"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/rosenhouse-bot-sandbox/reconciler-fuzzer/internal/generate"
	"github.com/rosenhouse-bot-sandbox/reconciler-fuzzer/internal/run"
	"github.com/rosenhouse-bot-sandbox/reconciler-fuzzer/internal/target"
)

// theSeedThatDrawsAB2Reproducer draws more than three ops, so the shrink pass
// has something to do. A correct toy passes the sequence it draws, so the
// failure the test finds is the seeded bug's.
const theSeedThatDrawsAB2Reproducer = 2

// The acceptance of DESIGN.md §10 M5: with --bug=2 the harness finds a failure
// in a sequence it drew and shrinks it to three ops or fewer.
func TestShrinkingAGeneratedFailure(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	correct := loadTarget(t, buildToy(t))
	// The toy converges in milliseconds, and a shrink pass replays many times,
	// so the waits are cut to what keeps the tier inside its budget (§11).
	correct.Timeouts = target.Timeouts{Settle: time.Second, Stable: 500 * time.Millisecond, Delete: 3 * time.Second}
	buggy := *correct
	buggy.Launch.Args = append(append([]string{}, correct.Launch.Args...), "--bug=2")
	testCluster := startCluster(t, correct.CRDs)
	dir := t.TempDir()
	runs := 0
	execute := func(ctx context.Context, against *target.Target, sequence run.Sequence) (run.Result, error) {
		runs++
		return run.Run(ctx, against, sequence, run.Options{
			Dir: filepath.Join(dir, "run", strconv.Itoa(runs)), Config: testCluster.Config(), Check: run.Engine{},
		})
	}

	generator, err := generate.New(correct, generate.Options{})
	if err != nil {
		t.Fatalf("Reading the toy's schema failed: %v", err)
	}
	drawn, err := generator.Draw(theSeedThatDrawsAB2Reproducer)
	if err != nil {
		t.Fatalf("Drawing a sequence failed: %v", err)
	}
	if len(drawn.Ops) <= 3 {
		t.Fatalf("Seed %d draws %d ops, so shrinking to three proves nothing. Re-pin the seed.",
			theSeedThatDrawsAB2Reproducer, len(drawn.Ops))
	}
	found, err := execute(ctx, &buggy, drawn)
	if err != nil {
		t.Fatalf("The generated run failed: %v", err)
	}
	if found.Violation == nil {
		t.Fatalf("The generated run found nothing, and the toy runs with bug 2.")
	}

	shrunk := run.Shrink(ctx, drawn, *found.Violation, func(ctx context.Context, candidate run.Sequence) (run.Result, error) {
		return execute(ctx, &buggy, candidate)
	})

	if len(shrunk.Ops) > 3 {
		t.Errorf("Shrink returned %d ops, want the three or fewer M5 accepts: %v", len(shrunk.Ops), shrunk.Ops)
	}
	if err := shrunk.Validate(); err != nil {
		t.Errorf("Shrink returned a sequence that does not validate: %v", err)
	}
	again, err := execute(ctx, &buggy, shrunk)
	if err != nil {
		t.Fatalf("Replaying the minimized sequence failed: %v", err)
	}
	if again.Violation == nil || again.Violation.ID != found.Violation.ID {
		t.Errorf("The minimized sequence reported %+v, want the %s the drawn sequence failed.",
			again.Violation, found.Violation.ID)
	}
	// Without this, a harness or toy fault that fails every run would pass the
	// test above while proving nothing about bug 2.
	clean, err := execute(ctx, correct, shrunk)
	if err != nil {
		t.Fatalf("Replaying the minimized sequence against the correct toy failed: %v", err)
	}
	if clean.Violation != nil {
		t.Errorf("The minimized sequence reports %+v against a toy with no seeded bug, so it reproduces something other than bug 2.",
			clean.Violation)
	}
}
