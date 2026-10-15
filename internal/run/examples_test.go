package run

import (
	"io/fs"
	"path/filepath"
	"strings"
	"testing"

	"github.com/rosenhouse-bot-sandbox/reconciler-fuzzer/internal/observe"
	"github.com/rosenhouse-bot-sandbox/reconciler-fuzzer/internal/target"
)

// The Runner rejects a bad sequence before its first op where it can, and at
// the op otherwise. This test catches both for every example sequence.
func TestEveryExampleSequenceSuitsItsTarget(t *testing.T) {
	targets, err := filepath.Glob("../../examples/*/target.yaml")
	if err != nil || len(targets) == 0 {
		t.Fatalf("found targets %v: %v", targets, err)
	}
	for _, path := range targets {
		declared, err := target.Load(path)
		if err != nil {
			t.Fatal(err)
		}
		hunted := 0
		err = filepath.WalkDir(filepath.Join(filepath.Dir(path), "sequences"), func(file string, _ fs.DirEntry, err error) error {
			if err != nil || !strings.HasSuffix(file, ".json") {
				return err
			}
			if filepath.Base(filepath.Dir(file)) == "hunt" {
				hunted++
			}
			sequence, err := ReadSequence(file)
			if err != nil {
				t.Errorf("%s: %v", file, err)
				return nil
			}
			checkSequenceSuits(t, declared, file, sequence)
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
		if hunted == 0 {
			t.Errorf("%s has no hunt families under sequences/hunt", filepath.Dir(path))
		}
	}
}

// A findings draft keeps the sequence that replays its find on an example.
func TestEveryFindingSequenceSuitsItsExample(t *testing.T) {
	paths, err := filepath.Glob("../../docs/findings/*/sequence.json")
	if err != nil || len(paths) == 0 {
		t.Fatalf("found finding sequences %v: %v; delete this test with the last findings draft", paths, err)
	}
	for _, path := range paths {
		sequence, err := ReadSequence(path)
		if err != nil {
			t.Errorf("%s: %v", path, err)
			continue
		}
		declared, err := target.Load(filepath.Join("../../examples", sequence.Target, "target.yaml"))
		if err != nil {
			t.Errorf("%s: %v", path, err)
			continue
		}
		checkSequenceSuits(t, declared, path, sequence)
	}
}

func checkSequenceSuits(t *testing.T, declared *target.Target, file string, sequence Sequence) {
	t.Helper()
	if err := validateRun(declared, sequence, Options{Dir: t.TempDir(), Check: Engine{}}); err != nil {
		t.Errorf("%s: %v", file, err)
	}
	fixtures := map[string]bool{}
	for _, fixture := range declared.Fixtures {
		fixtures[observe.KindName(fixture.GroupVersionKind())+" "+fixture.GetName()] = true
	}
	for _, op := range sequence.Ops {
		switch op.Type {
		case OpDeleteManaged:
			if _, err := managedKind(declared, op.Kind); err != nil {
				t.Errorf("%s: op %d: %v", file, op.Index, err)
			}
		case OpUpdateFixture, OpDeleteFixture:
			if !fixtures[op.fixture()] {
				t.Errorf("%s: op %d acts on %s, which the target declares no fixture of", file, op.Index, op.fixture())
			}
		}
	}
}
