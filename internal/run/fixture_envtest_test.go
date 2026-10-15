//go:build envtest

package run_test

import (
	"bytes"
	"encoding/json"
	"os"
	"slices"
	"strconv"
	"testing"

	"github.com/rosenhouse-bot-sandbox/reconciler-fuzzer/internal/generate"
	"github.com/rosenhouse-bot-sandbox/reconciler-fuzzer/internal/run"
	"github.com/rosenhouse-bot-sandbox/reconciler-fuzzer/internal/target"
)

// lastFixtureSeed is the last seed fixtureSequences draws.
const lastFixtureSeed = 60

// seedThatFindsB14 draws an updateFixture and then a restart.
const seedThatFindsB14 = 41

// The unit tier pins what this test draws under goldenFixtureTarget in
// goldenDraws.
const (
	goldenDraws         = "internal/generate/testdata/draws.golden.json"
	goldenFixtureTarget = "toy-widget with a label fixture"
)

// A correct toy passes what generation draws on its fixtures, so the fixture
// ops find bugs rather than harness artefacts.
func TestGeneratedFixtureOps(t *testing.T) {
	t.Parallel()
	toy := loadTarget(t, buildToy(t))
	toy.Generate.Fixtures = []target.MutableFixture{{
		GVK: configMapKind, Name: "widget-config", Mutate: []target.Path{target.MustParsePath("data.label")},
	}}
	testCluster := startCluster(t, toy.CRDs)
	generator, err := generate.New(toy, generate.Options{})
	if err != nil {
		t.Fatalf("Reading the toy's schema failed: %v", err)
	}
	execute := func(t *testing.T, against *target.Target, sequence run.Sequence) run.Result {
		t.Helper()
		result, err := run.Run(t.Context(), against, sequence, run.Options{
			Dir: t.TempDir(), Config: testCluster.Config(), Check: run.Engine{},
		})
		if err != nil {
			t.Fatalf("Seed %d failed to run: %v", sequence.Seed, err)
		}
		return result
	}
	sequences := fixtureSequences(t, generator)

	t.Run("passes the correct toy", func(t *testing.T) {
		for _, sequence := range sequences {
			t.Run(strconv.FormatInt(sequence.Seed, 10), func(t *testing.T) {
				t.Parallel()
				if result := execute(t, toy, sequence); result.Violation != nil {
					t.Errorf("Seed %d reported %s, want none: the toy runs without a bug.", sequence.Seed, result.Violation)
				}
			})
		}
	})

	t.Run("finds B14", func(t *testing.T) {
		buggy := *toy
		buggy.Launch.Args = append(slices.Clone(toy.Launch.Args), "--bug=14")
		i := slices.IndexFunc(sequences, func(sequence run.Sequence) bool { return sequence.Seed == seedThatFindsB14 })
		if i < 0 {
			t.Fatalf("Seed %d draws no fixture op. Re-pin the seed.", seedThatFindsB14)
		}

		result := execute(t, &buggy, sequences[i])

		if result.Violation == nil || result.Violation.ID != "G5" {
			t.Errorf("Seed %d reported %v, want G5: B14 misses the label until a restart.", seedThatFindsB14, result.Violation)
		}
	})
}

// fixtureSequences are the sequences seeds 1 to lastFixtureSeed draw that act
// on a fixture.
func fixtureSequences(t *testing.T, generator *generate.Generator) []run.Sequence {
	t.Helper()
	golden := goldenFixtureDraws(t)
	var sequences []run.Sequence
	drawn := map[run.OpType]int{}
	for seed := int64(1); seed <= lastFixtureSeed; seed++ {
		sequence, err := generator.Draw(seed)
		if err != nil {
			t.Fatalf("Drawing seed %d failed: %v", seed, err)
		}
		if !sameJSON(t, golden[strconv.FormatInt(seed, 10)], sequence) {
			t.Fatalf("Seed %d draws a sequence that %s does not record under %q.", seed, goldenDraws, goldenFixtureTarget)
		}
		acts := false
		for _, op := range sequence.Ops {
			if op.Type == run.OpUpdateFixture || op.Type == run.OpDeleteFixture {
				drawn[op.Type]++
				acts = true
			}
		}
		if acts {
			sequences = append(sequences, sequence)
		}
	}
	if drawn[run.OpUpdateFixture] < 5 || drawn[run.OpDeleteFixture] < 5 {
		t.Fatalf("Seeds 1 to %d draw %v, want at least five of each fixture op.", lastFixtureSeed, drawn)
	}
	return sequences
}

func goldenFixtureDraws(t *testing.T) map[string]json.RawMessage {
	t.Helper()
	content, err := os.ReadFile(repoRoot + "/" + goldenDraws)
	if err != nil {
		t.Fatal(err)
	}
	var golden map[string]map[string]json.RawMessage
	if err := json.Unmarshal(content, &golden); err != nil {
		t.Fatalf("Reading %s failed: %v", goldenDraws, err)
	}
	return golden[goldenFixtureTarget]
}

func sameJSON(t *testing.T, recorded json.RawMessage, sequence run.Sequence) bool {
	t.Helper()
	drawn, err := sequence.Marshal()
	if err != nil {
		t.Fatalf("Marshalling seed %d failed: %v", sequence.Seed, err)
	}
	var compactRecorded, compactDrawn bytes.Buffer
	return json.Compact(&compactRecorded, recorded) == nil && json.Compact(&compactDrawn, drawn) == nil &&
		bytes.Equal(compactRecorded.Bytes(), compactDrawn.Bytes())
}
