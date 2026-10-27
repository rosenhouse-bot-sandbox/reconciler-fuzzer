package hunt

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/rosenhouse-bot-sandbox/reconciler-fuzzer/internal/check"
	"github.com/rosenhouse-bot-sandbox/reconciler-fuzzer/internal/config"
	"github.com/rosenhouse-bot-sandbox/reconciler-fuzzer/internal/generate"
	"github.com/rosenhouse-bot-sandbox/reconciler-fuzzer/internal/plan"
)

var (
	clean  = check.Finding{Check: "Clean", Summary: "a ConfigMap outlives its Widget", Kind: "kube/configmaps"}
	unique = check.Finding{Check: "Unique", Summary: "two ConfigMaps hold one index", Kind: "kube/configmaps"}
)

// fakeRun records each run's directory, relative to out, and returns the
// findings that finds gives for it.
type fakeRun struct {
	out   string
	finds func(dir string, p *plan.Plan) []check.Finding
	mu    sync.Mutex
	dirs  []string
}

func (f *fakeRun) run(_ context.Context, p *plan.Plan, out string) (check.Report, error) {
	dir, err := filepath.Rel(f.out, out)
	if err != nil {
		return check.Report{}, err
	}
	f.mu.Lock()
	f.dirs = append(f.dirs, dir)
	f.mu.Unlock()
	if err := os.MkdirAll(out, 0o755); err != nil {
		return check.Report{}, err
	}
	return check.Report{Findings: f.finds(dir, p)}, os.WriteFile(filepath.Join(out, "trace.jsonl"), nil, 0o644)
}

func hunt(t *testing.T, seeds []uint64, finds func(dir string, p *plan.Plan) []check.Finding) ([]Found, *fakeRun) {
	t.Helper()
	fake := &fakeRun{out: t.TempDir(), finds: finds}
	found, err := Hunt(context.Background(), Options{ConfigPath: toyConfig, Seeds: slices.Values(seeds), Out: fake.out, Run: fake.run})
	if err != nil {
		t.Fatal(err)
	}
	return found, fake
}

func always(findings ...check.Finding) func(string, *plan.Plan) []check.Finding {
	return func(string, *plan.Plan) []check.Finding { return findings }
}

func TestHuntReplaysAndShrinksEachSignatureOnce(t *testing.T) {
	found, fake := hunt(t, []uint64{4, 5, 6}, always(clean))
	if len(found) != 1 || found[0].Seed != 4 || found[0].Reproduced != Replays || found[0].Finding.Signature() != clean.Signature() {
		t.Fatalf("Hunt found %+v, want the Clean finding of seed 4, reproduced 5 times.", found)
	}
	for _, want := range []string{"seed-4", "seed-4/replay-1", "seed-4/replay-5", "seed-5", "seed-6"} {
		if !slices.Contains(fake.dirs, want) {
			t.Errorf("Hunt ran nothing into %s; it ran %v.", want, fake.dirs)
		}
	}
	for _, dir := range fake.dirs {
		if strings.HasPrefix(dir, "seed-5/") || strings.HasPrefix(dir, "seed-6/") || strings.Contains(dir, "replay-6") {
			t.Errorf("Hunt ran into %s.", dir)
		}
	}
	shrunk, err := plan.Load(filepath.Join(fake.out, "seed-4", "shrunk.json"))
	if err != nil {
		t.Fatal(err)
	}
	if len(shrunk.Steps) != 0 || found[0].ShrunkPath != filepath.Join(fake.out, "seed-4", "shrunk.json") {
		t.Errorf("Hunt shrinks a plan that always fails to %+v at %s, want no steps.", shrunk, found[0].ShrunkPath)
	}
	summary, err := os.ReadFile(filepath.Join(fake.out, "summary.md"))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"3 seeds", "| 4 | Clean | a ConfigMap outlives its Widget | 5/5 | " + found[0].ShrunkPath + " |"} {
		if !strings.Contains(string(summary), want) {
			t.Errorf("summary.md lacks %q:\n%s", want, summary)
		}
	}
}

func TestHuntShrinksEachNewSignatureOfASeed(t *testing.T) {
	found, fake := hunt(t, []uint64{1, 2}, func(dir string, _ *plan.Plan) []check.Finding {
		if strings.HasPrefix(dir, "seed-2") {
			return []check.Finding{clean, unique}
		}
		return []check.Finding{clean}
	})
	if len(found) != 2 || found[0].Seed != 1 || found[1].Seed != 2 || found[1].Finding.Check != "Unique" {
		t.Fatalf("Hunt found %+v.", found)
	}
	if want := filepath.Join(fake.out, "seed-2", "shrunk.json"); found[1].ShrunkPath != want {
		t.Errorf("The Unique finding's shrunk plan is %s, want %s.", found[1].ShrunkPath, want)
	}
}

func TestHuntNamesTheShrunkPlanOfEachFindingOfASeed(t *testing.T) {
	found, fake := hunt(t, []uint64{1}, always(clean, unique))
	if len(found) != 2 {
		t.Fatalf("Hunt found %+v.", found)
	}
	for i, name := range []string{"shrunk.json", "shrunk-2.json"} {
		if want := filepath.Join(fake.out, "seed-1", name); found[i].ShrunkPath != want {
			t.Errorf("Finding %d is shrunk to %s, want %s.", i, found[i].ShrunkPath, want)
		}
		if _, err := plan.Load(found[i].ShrunkPath); err != nil {
			t.Error(err)
		}
	}
}

func TestACandidateRunsFourTimesWhenTheFindingReproducedInUnderHalfItsReplays(t *testing.T) {
	for _, testCase := range []struct{ reproduced, runs int }{{1, 4}, {2, 4}, {3, 2}, {5, 2}} {
		found, fake := hunt(t, []uint64{1}, func(dir string, _ *plan.Plan) []check.Finding {
			for i := 1; i <= testCase.reproduced; i++ {
				if dir == "seed-1/replay-"+string(rune('0'+i)) {
					return []check.Finding{clean}
				}
			}
			if dir == "seed-1" {
				return []check.Finding{clean}
			}
			return nil
		})
		runs := 0
		for _, dir := range fake.dirs {
			if strings.HasPrefix(dir, "seed-1/shrink-1-") {
				runs++
			}
		}
		if found[0].Reproduced != testCase.reproduced || runs != testCase.runs {
			t.Errorf("Reproduced %d of %d, the first candidate ran %d times, want %d.", found[0].Reproduced, Replays, runs, testCase.runs)
		}
	}
}

func TestAShrinkCandidateFailsOnTheFirstRunThatFindsTheSignature(t *testing.T) {
	_, fake := hunt(t, []uint64{1}, func(dir string, _ *plan.Plan) []check.Finding {
		if strings.HasSuffix(dir, "-2") {
			return []check.Finding{unique}
		}
		return []check.Finding{clean}
	})
	for _, dir := range fake.dirs {
		if strings.HasPrefix(dir, "seed-1/shrink-") && !strings.HasSuffix(dir, "-1") {
			t.Fatalf("Hunt ran a candidate again after it failed: %s.", dir)
		}
	}
}

func TestHuntKeepsOnlyTheRunsOfFailingCandidates(t *testing.T) {
	_, fake := hunt(t, []uint64{1}, func(dir string, _ *plan.Plan) []check.Finding {
		if dir == "seed-1/shrink-1-1" {
			return nil
		}
		return []check.Finding{clean}
	})
	for dir, kept := range map[string]bool{"seed-1": true, "seed-1/replay-1": true, "seed-1/shrink-1-1": false, "seed-1/shrink-1-2": true} {
		if _, err := os.Stat(filepath.Join(fake.out, dir)); (err == nil) != kept {
			t.Errorf("%s: stat gives %v, want kept %v.", dir, err, kept)
		}
	}
}

func TestHuntWithoutFindingsSaysSo(t *testing.T) {
	found, fake := hunt(t, []uint64{1, 2}, always())
	summary, err := os.ReadFile(filepath.Join(fake.out, "summary.md"))
	if err != nil || len(found) != 0 || !strings.Contains(string(summary), "No findings over 2 seeds.") {
		t.Errorf("Hunt found %v and wrote %q, %v.", found, summary, err)
	}
}

var boom = errors.New("boom")

func TestHuntRecordsASeedsHarnessErrorAndGoesOn(t *testing.T) {
	fake := &fakeRun{out: t.TempDir(), finds: always(clean)}
	found, err := Hunt(context.Background(), Options{ConfigPath: toyConfig, Seeds: slices.Values([]uint64{1, 2}), Out: fake.out,
		Run: func(ctx context.Context, p *plan.Plan, out string) (check.Report, error) {
			if filepath.Base(out) == "seed-1" {
				return check.Report{}, boom
			}
			return fake.run(ctx, p, out)
		}})
	if err == nil || !strings.Contains(err.Error(), "1 of 2 seeds failed") || len(found) != 1 || found[0].Seed != 2 {
		t.Fatalf("Hunt = %+v, %v; want seed 2's finding and an error for seed 1.", found, err)
	}
	summary, _ := os.ReadFile(filepath.Join(fake.out, "summary.md"))
	if !strings.Contains(string(summary), "- seed 1: boom") {
		t.Errorf("summary.md does not name seed 1's error:\n%s", summary)
	}
}

func TestHuntCountsAFailedReplayOrCandidateAsNotFailing(t *testing.T) {
	fake := &fakeRun{out: t.TempDir(), finds: always(clean)}
	found, err := Hunt(context.Background(), Options{ConfigPath: toyConfig, Seeds: slices.Values([]uint64{1}), Out: fake.out,
		Run: func(ctx context.Context, p *plan.Plan, out string) (check.Report, error) {
			if name := filepath.Base(out); name == "replay-2" || name == "shrink-1-1" || name == "shrink-1-2" {
				return check.Report{}, boom
			}
			return fake.run(ctx, p, out)
		}})
	if err != nil || len(found) != 1 || found[0].Reproduced != Replays-1 || found[0].Shrunk == nil {
		t.Fatalf("Hunt = %+v, %v; want 4 of 5 replays and a shrunk plan.", found, err)
	}
}

func TestHuntDoesNotShrinkAFindingNoReplayReproduced(t *testing.T) {
	found, fake := hunt(t, []uint64{1}, func(dir string, _ *plan.Plan) []check.Finding {
		if dir == "seed-1" {
			return []check.Finding{clean}
		}
		return nil
	})
	for _, dir := range fake.dirs {
		if strings.Contains(dir, "shrink-") {
			t.Fatalf("Hunt ran a shrink candidate into %s.", dir)
		}
	}
	summary, _ := os.ReadFile(filepath.Join(fake.out, "summary.md"))
	if len(found) != 1 || found[0].Shrunk != nil || !strings.Contains(string(summary), "| 0/5 | not shrunk |") {
		t.Errorf("Hunt found %+v and wrote:\n%s", found, summary)
	}
}

func TestHuntWritesTheSummaryOfTheSeedsBeforeACancel(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	fake := &fakeRun{out: t.TempDir(), finds: always(clean)}
	_, err := Hunt(ctx, Options{ConfigPath: toyConfig, Seeds: slices.Values([]uint64{1, 2}), Out: fake.out,
		Run: func(ctx context.Context, p *plan.Plan, out string) (check.Report, error) {
			if filepath.Base(out) == "seed-2" {
				cancel()
				return check.Report{}, ctx.Err()
			}
			return fake.run(ctx, p, out)
		}})
	if !errors.Is(err, context.Canceled) {
		t.Errorf("Hunt returned %v, want the cancel.", err)
	}
	summary, _ := os.ReadFile(filepath.Join(fake.out, "summary.md"))
	if !strings.Contains(string(summary), "over 1 seeds") || !strings.Contains(string(summary), "| 1 | Clean |") {
		t.Errorf("summary.md is:\n%s", summary)
	}
}

func TestHuntKeepsAFindWhoseReplaysWereCutShort(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	fake := &fakeRun{out: t.TempDir(), finds: always(clean)}
	found, err := Hunt(ctx, Options{ConfigPath: toyConfig, Seeds: slices.Values([]uint64{1}), Out: fake.out,
		Run: func(ctx context.Context, p *plan.Plan, out string) (check.Report, error) {
			if filepath.Base(out) == "replay-2" {
				cancel()
				return check.Report{}, ctx.Err()
			}
			return fake.run(ctx, p, out)
		}})
	summary, _ := os.ReadFile(filepath.Join(fake.out, "summary.md"))
	if !errors.Is(err, context.Canceled) || len(found) != 1 || !strings.Contains(string(summary), "over 1 seed") ||
		!strings.Contains(string(summary), "| 1 | Clean |") {
		t.Errorf("Hunt = %+v, %v, and wrote:\n%s", found, err, summary)
	}
}

// A hunt cut short while shrinking, as by a CI timeout, keeps the smallest
// failing plan so far and counts the seed that found it.
func TestHuntKeepsTheShrinkSoFarOnACancel(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var smallest *plan.Plan
	fake := &fakeRun{out: t.TempDir(), finds: func(dir string, p *plan.Plan) []check.Finding {
		if dir == "seed-4/shrink-2-1" {
			smallest = p
		}
		return []check.Finding{clean}
	}}
	found, err := Hunt(ctx, Options{ConfigPath: toyConfig, Seeds: slices.Values([]uint64{4}), Out: fake.out,
		Run: func(ctx context.Context, p *plan.Plan, out string) (check.Report, error) {
			if filepath.Base(out) == "shrink-3-1" {
				cancel()
				return check.Report{}, ctx.Err()
			}
			return fake.run(ctx, p, out)
		}})
	summary, _ := os.ReadFile(filepath.Join(fake.out, "summary.md"))
	shrunk := filepath.Join(fake.out, "seed-4", "shrunk.json")
	if !errors.Is(err, context.Canceled) || len(found) != 1 || !found[0].CutShort || found[0].ShrunkPath != shrunk ||
		!strings.Contains(string(summary), "over 1 seed") || !strings.Contains(string(summary), "| 5/5 | "+shrunk+" (cut short) |") {
		t.Errorf("Hunt = %+v, %v, and wrote:\n%s", found, err, summary)
	}
	if saved, err := plan.Load(shrunk); err != nil || encoded(t, saved) != encoded(t, smallest) {
		t.Errorf("shrunk.json holds %s, %v; want the last failing candidate, %s.", encoded(t, saved), err, encoded(t, smallest))
	}
}

func TestHuntRemovesTheRunOfASeedWithNothingNew(t *testing.T) {
	_, fake := hunt(t, []uint64{1, 2}, always(clean))
	for dir, kept := range map[string]bool{"seed-1": true, "seed-2": false} {
		if _, err := os.Stat(filepath.Join(fake.out, dir)); (err == nil) != kept {
			t.Errorf("%s: stat gives %v, want kept %v.", dir, err, kept)
		}
	}
}

func TestHuntLogsEachRun(t *testing.T) {
	fake := &fakeRun{out: t.TempDir(), finds: always(clean)}
	var log strings.Builder
	if _, err := Hunt(context.Background(), Options{ConfigPath: toyConfig, Seeds: slices.Values([]uint64{1}), Out: fake.out, Run: fake.run, Log: &log}); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"seed 1: 1 new finding", "seed 1, replay 5: reproduced", "seed 1, shrink candidate 1: fails"} {
		if !strings.Contains(log.String(), want) {
			t.Errorf("The log lacks %q:\n%s", want, log.String())
		}
	}
}

func TestHuntRunsTheGeneratedPlanOfEachSeed(t *testing.T) {
	var plans []*plan.Plan
	hunt(t, []uint64{3, 9}, func(_ string, p *plan.Plan) []check.Finding {
		plans = append(plans, p)
		return nil
	})
	if len(plans) != 2 || plans[0].Seed != 3 || plans[1].Seed != 9 || len(plans[0].Steps) < 3 {
		t.Errorf("Hunt ran %+v.", plans)
	}
}

func TestShrinkPlanRunsReplaysAndShrinksAPlanFile(t *testing.T) {
	out := t.TempDir()
	path := filepath.Join(t.TempDir(), "plan.json")
	if err := plan.Save(path, parsePlan(t, `{"steps":[{"actions":[`+create+`]},{"actions":[{"do":"restart"}]}]}`)); err != nil {
		t.Fatal(err)
	}
	fake := &fakeRun{out: out, finds: func(_ string, p *plan.Plan) []check.Finding {
		if len(p.Steps) == 2 {
			return []check.Finding{clean}
		}
		return nil
	}}
	found, err := ShrinkPlan(context.Background(), ShrinkOptions{ConfigPath: toyConfig, PlanPath: path, Out: out, Run: fake.run})
	if err != nil {
		t.Fatal(err)
	}
	if len(found) != 1 || found[0].Reproduced != Replays || found[0].ShrunkPath != filepath.Join(out, "shrunk.json") {
		t.Fatalf("ShrinkPlan found %+v.", found)
	}
	if !slices.Contains(fake.dirs, ".") || !slices.Contains(fake.dirs, "replay-5") {
		t.Errorf("ShrinkPlan ran into %v, want the plan into out and its replays.", fake.dirs)
	}
	if _, err := os.Stat(filepath.Join(out, "summary.md")); err != nil {
		t.Error(err)
	}
}

func TestShrinkPlanReportsTheShrinkSoFarOnACancel(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	out := t.TempDir()
	path := filepath.Join(t.TempDir(), "plan.json")
	if err := plan.Save(path, parsePlan(t, `{"steps":[{"actions":[`+create+`]},{"actions":[{"do":"restart"}]}]}`)); err != nil {
		t.Fatal(err)
	}
	fake := &fakeRun{out: out, finds: always(clean)}
	found, err := ShrinkPlan(ctx, ShrinkOptions{ConfigPath: toyConfig, PlanPath: path, Out: out,
		Run: func(ctx context.Context, p *plan.Plan, dir string) (check.Report, error) {
			if filepath.Base(dir) == "shrink-1-1" {
				cancel()
				return check.Report{}, ctx.Err()
			}
			return fake.run(ctx, p, dir)
		}})
	summary, _ := os.ReadFile(filepath.Join(out, "summary.md"))
	if !errors.Is(err, context.Canceled) || len(found) != 1 || !strings.Contains(string(summary), "| 5/5 | "+filepath.Join(out, "shrunk.json")+" (cut short) |") {
		t.Errorf("ShrinkPlan = %+v, %v, and wrote:\n%s", found, err, summary)
	}
}

func TestShrinkPlanResetsACreateTowardTheSample(t *testing.T) {
	out := t.TempDir()
	path := filepath.Join(t.TempDir(), "plan.json")
	const widget = `{"steps":[{"actions":[{"do":"create","slot":"a","object":{"apiVersion":"toy.reconciler-fuzzer/v1","kind":"Widget","metadata":{"name":"widget"},"spec":{"count":%d}}}]}]}`
	if err := plan.Save(path, parsePlan(t, fmt.Sprintf(widget, 5))); err != nil {
		t.Fatal(err)
	}
	fake := &fakeRun{out: out, finds: func(_ string, p *plan.Plan) []check.Finding {
		if created, _ := hasCreate(context.Background(), p); created {
			return []check.Finding{clean}
		}
		return nil
	}}
	found, err := ShrinkPlan(context.Background(), ShrinkOptions{ConfigPath: toyConfig, PlanPath: path, Out: out, Run: fake.run})
	if err != nil {
		t.Fatal(err)
	}
	if want := fmt.Sprintf(widget, 3); len(found) != 1 || encoded(t, found[0].Shrunk) != want {
		t.Errorf("ShrinkPlan found %+v, want the create shrunk to %s.", found, want)
	}
}

func TestSummaryShowsNoSeedForAPlanWithout(t *testing.T) {
	got := Summary("1 plan", []Found{{Finding: clean}, {Finding: clean, Seed: 4}}, nil)
	if !strings.Contains(got, "| - | Clean |") || !strings.Contains(got, "| 4 | Clean |") {
		t.Errorf("Summary is %q.", got)
	}
}

func TestHuntLogsAReplayErrorOnlyWhenThereIsOne(t *testing.T) {
	fake := &fakeRun{out: t.TempDir(), finds: always(clean)}
	var log strings.Builder
	_, err := Hunt(context.Background(), Options{ConfigPath: toyConfig, Seeds: slices.Values([]uint64{1}), Out: fake.out, Log: &log,
		Run: func(ctx context.Context, p *plan.Plan, out string) (check.Report, error) {
			if filepath.Base(out) == "replay-2" {
				return check.Report{}, boom
			}
			return fake.run(ctx, p, out)
		}})
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"seed 1: 1 new finding\n", "replay 1: reproduced 1 of 1\n", "replay 2: reproduced 0 of 1, boom\n"} {
		if !strings.Contains(log.String(), want) {
			t.Errorf("The log lacks %q:\n%s", want, log.String())
		}
	}
}

func TestSummaryEscapesPipes(t *testing.T) {
	got := Summary("1 plan", []Found{{Finding: check.Finding{Check: "Clean", Summary: "a|b"}}}, nil)
	if !strings.Contains(got, `| a\|b |`) {
		t.Errorf("Summary is %q.", got)
	}
}

func TestLoadSamplesReadsEachEntrysSampleInOrder(t *testing.T) {
	cfg, err := config.Load("../generate/testdata/fleet/config.yaml")
	if err != nil {
		t.Fatal(err)
	}
	samples, err := loadSamples(cfg)
	var kinds []any
	for _, sample := range samples {
		kinds = append(kinds, sample["kind"])
	}
	if err != nil || !slices.Equal(kinds, []any{"Cluster", "Database"}) {
		t.Errorf("loadSamples = samples of %v, %v; want a Cluster's and a Database's.", kinds, err)
	}
}

func TestAShrinkStopsAfterShrinkCandidates(t *testing.T) {
	cfg, err := config.Load(toyConfig)
	if err != nil {
		t.Fatal(err)
	}
	g, err := generate.New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	samples, err := loadSamples(cfg)
	if err != nil {
		t.Fatal(err)
	}
	seed := uint64(1)
	for ; ; seed++ {
		if p, _ := g.Plan(seed); len(Candidates(p, samples)) > ShrinkCandidates {
			break
		}
	}
	found, fake := hunt(t, []uint64{seed}, func(dir string, _ *plan.Plan) []check.Finding {
		if strings.Contains(dir, "shrink") {
			return nil
		}
		return []check.Finding{clean}
	})
	tried := map[string]bool{}
	for _, dir := range fake.dirs {
		if rest, ok := strings.CutPrefix(filepath.Base(dir), "shrink-"); ok {
			c, _, _ := strings.Cut(rest, "-")
			tried[c] = true
		}
	}
	if len(tried) != ShrinkCandidates || len(found) != 1 || found[0].Shrunk == nil {
		t.Errorf("The shrink of seed %d tried %d candidates and found %+v, want %d and the plan kept.", seed, len(tried), found, ShrinkCandidates)
	}
}
