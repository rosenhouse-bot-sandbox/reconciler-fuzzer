package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/rosenhouse-bot-sandbox/reconciler-fuzzer/internal/check"
	"github.com/rosenhouse-bot-sandbox/reconciler-fuzzer/internal/plan"
	"github.com/rosenhouse-bot-sandbox/reconciler-fuzzer/internal/run"
	"github.com/rosenhouse-bot-sandbox/reconciler-fuzzer/internal/trace"
	"github.com/rosenhouse-bot-sandbox/reconciler-fuzzer/internal/trace/tracetest"
)

const toyConfig = "../../targets/toy-widget/config.yaml"

const group = "toy.reconciler-fuzzer"

var widget = tracetest.Ref(group+"/widgets", "ns", "widget")

func readyWidget() map[string]any {
	return map[string]any{
		"metadata": map[string]any{"name": "widget", "generation": 1},
		"spec":     map[string]any{"count": 1},
		"status":   map[string]any{"observedGeneration": 1, "ready": 1},
	}
}

func cliOutput(args ...string) (code int, stdout, stderr string) {
	var out, errOut bytes.Buffer
	code = cli(args, &out, &errOut)
	return code, out.String(), errOut.String()
}

func TestUsageErrors(t *testing.T) {
	cases := map[string][]string{
		"no command":        {},
		"unknown command":   {"fuzz"},
		"unknown flag":      {"judge", "--config", toyConfig, "--trace", "t", "--verbose"},
		"missing trace":     {"judge", "--config", toyConfig},
		"extra args":        {"judge", "--config", toyConfig, "--trace", "t", "more"},
		"run missing out":   {"run", "--config", toyConfig, "--plan", "p.json"},
		"run missing plan":  {"run", "--config", toyConfig, "--out", "dir"},
		"run plan and seed": {"run", "--config", toyConfig, "--plan", "p.json", "--seed", "1", "--out", "dir"},
		"run bad seed":      {"run", "--config", toyConfig, "--seed", "x", "--out", "dir"},
		"judge out":         {"judge", "--trace", "t", "--out", "dir"},
		"hunt plan":         {"hunt", "--config", toyConfig, "--seeds", "2", "--plan", "p.json", "--out", "dir"},
		"hunt no seeds":     {"hunt", "--config", toyConfig, "--out", "dir"},
		"hunt missing out":  {"hunt", "--config", toyConfig, "--seeds", "2"},
		"hunt overflow":     {"hunt", "--config", toyConfig, "--from", "5", "--seeds", "18446744073709551615", "--out", "dir"},
		"shrink no plan":    {"shrink", "--config", toyConfig, "--out", "dir"},
		"run extra args":    {"run", "--config", toyConfig, "--plan", "p.json", "--out", "dir", "more"},
	}
	for name, args := range cases {
		t.Run(name, func(t *testing.T) {
			code, stdout, stderr := cliOutput(args...)
			if code != 2 || !strings.Contains(stderr, "usage:") || stdout != "" {
				t.Errorf("cli(%q) = %d, stdout %q, stderr %q; want 2 and usage on stderr", args, code, stdout, stderr)
			}
		})
	}
}

func TestUsageNamesTheMissingFlag(t *testing.T) {
	if code, _, stderr := cliOutput("run", "--config", toyConfig, "--out", "dir"); code != 2 || !strings.Contains(stderr, "exactly one of --plan and --seed") {
		t.Errorf("cli = %d, stderr %q; want 2 naming --plan and --seed", code, stderr)
	}
	if code, _, stderr := cliOutput("shrink", "--config", toyConfig, "--out", "dir"); code != 2 || !strings.Contains(stderr, "--plan is required") {
		t.Errorf("cli = %d, stderr %q; want 2 naming --plan", code, stderr)
	}
}

// fakeRun replaces run.Run with one that records its options and gives
// findings to the plans that hold a delete.
func fakeRun(t *testing.T) *[]run.Options {
	var got []run.Options
	runPlan = func(_ context.Context, o run.Options) (check.Report, error) {
		got = append(got, o)
		for _, step := range o.Plan.Steps {
			for _, a := range step.Actions {
				if a.Do == plan.Delete {
					return check.Report{Findings: []check.Finding{{Check: "Clean", Summary: "a ConfigMap stays"}}}, os.MkdirAll(o.Out, 0o755)
				}
			}
		}
		return check.Report{}, os.MkdirAll(o.Out, 0o755)
	}
	t.Cleanup(func() { runPlan = run.Run })
	return &got
}

func TestRunDrawsThePlanOfASeed(t *testing.T) {
	got := fakeRun(t)
	out := t.TempDir()
	if err := os.WriteFile(filepath.Join(out, "report.md"), []byte("No findings.\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	code, stdout, stderr := cliOutput("run", "--config", toyConfig, "--seed", "3", "--out", out, "--launch-arg", "--bug=3")
	if code == 2 || stdout != "No findings.\n" || len(*got) != 1 {
		t.Fatalf("cli = %d, stdout %q, stderr %q, runs %d", code, stdout, stderr, len(*got))
	}
	o := (*got)[0]
	if o.Plan == nil || o.Plan.Seed != 3 || len(o.Plan.Steps) < 3 || o.PlanPath != "" || o.LaunchArgs[0] != "--bug=3" {
		t.Errorf("run.Options = %+v, want the plan of seed 3.", o)
	}
}

func TestHuntRunsTheSeedsAndPrintsTheSummary(t *testing.T) {
	got := fakeRun(t)
	out := t.TempDir()
	code, stdout, stderr := cliOutput("hunt", "--config", toyConfig, "--seeds", "3", "--from", "5", "--out", out, "--launch-arg", "--bug=9")
	var seeds []uint64
	for _, o := range *got {
		if o.ConfigPath != toyConfig || o.LaunchArgs[0] != "--bug=9" {
			t.Fatalf("run.Options = %+v.", o)
		}
		if filepath.Dir(o.Out) == out {
			seeds = append(seeds, o.Plan.Seed)
		}
	}
	if !reflect.DeepEqual(seeds, []uint64{5, 6, 7}) {
		t.Errorf("hunt ran seeds %v, want 5 to 7.", seeds)
	}
	summary, _ := os.ReadFile(filepath.Join(out, "summary.md"))
	if code != 1 || stdout != string(summary) || !strings.Contains(stdout, "over 3 seeds") || !strings.Contains(stdout, "a ConfigMap stays") {
		t.Errorf("cli = %d, stdout %q, stderr %q; want 1 and the summary", code, stdout, stderr)
	}
}

func TestHuntExits2WhenEverySeedFails(t *testing.T) {
	runPlan = func(context.Context, run.Options) (check.Report, error) {
		return check.Report{}, errors.New("no controller")
	}
	t.Cleanup(func() { runPlan = run.Run })
	code, stdout, stderr := cliOutput("hunt", "--config", toyConfig, "--seeds", "2", "--out", t.TempDir())
	if code != 2 || !strings.Contains(stdout, "seed 2: no controller") || !strings.Contains(stderr, "2 of 2 seeds failed") {
		t.Errorf("cli = %d, stdout %q, stderr %q; want 2 and the failed seeds", code, stdout, stderr)
	}
}

func TestHuntExits1WhenASeedFindsAndAnotherFails(t *testing.T) {
	runPlan = func(_ context.Context, o run.Options) (check.Report, error) {
		if filepath.Base(o.Out) == "seed-1" {
			return check.Report{}, errors.New("no controller")
		}
		return check.Report{Findings: []check.Finding{{Check: "Clean"}}}, os.MkdirAll(o.Out, 0o755)
	}
	t.Cleanup(func() { runPlan = run.Run })
	code, _, stderr := cliOutput("hunt", "--config", toyConfig, "--seeds", "2", "--out", t.TempDir())
	if code != 1 || !strings.Contains(stderr, "1 of 2 seeds failed") {
		t.Errorf("cli = %d, stderr %q; want 1 and the failed seed", code, stderr)
	}
}

// interruptAt fakes runs that find something, until the run into a
// directory named dir. That run interrupts this process.
func interruptAt(t *testing.T, dir string) {
	runPlan = func(ctx context.Context, o run.Options) (check.Report, error) {
		if filepath.Base(o.Out) == dir {
			if err := syscall.Kill(os.Getpid(), syscall.SIGINT); err != nil {
				return check.Report{}, err
			}
			<-ctx.Done()
			return check.Report{}, ctx.Err()
		}
		return check.Report{Findings: []check.Finding{{Check: "Clean"}}}, os.MkdirAll(o.Out, 0o755)
	}
	t.Cleanup(func() { runPlan = run.Run })
}

func TestHuntCutShortByAnInterruptStillReportsItsFinds(t *testing.T) {
	interruptAt(t, "seed-2")
	code, stdout, stderr := cliOutput("hunt", "--config", toyConfig, "--seeds", "3", "--out", t.TempDir())
	if code != 1 || !strings.Contains(stdout, "over 1 seeds") || !strings.Contains(stderr, "context canceled") {
		t.Errorf("cli = %d, stdout %q, stderr %q; want 1 and the find of seed 1", code, stdout, stderr)
	}
}

func TestHuntLogsEachRun(t *testing.T) {
	fakeRun(t)
	_, _, stderr := cliOutput("hunt", "--config", toyConfig, "--seeds", "2", "--out", t.TempDir())
	if !strings.Contains(stderr, "seed 1: ") || !strings.Contains(stderr, "seed 2: ") {
		t.Errorf("stderr is %q, want a line for each seed", stderr)
	}
}

func TestJudgeRefusesAConfigOfAnotherPrimary(t *testing.T) {
	path := filepath.Join(t.TempDir(), "trace.jsonl")
	rec, err := trace.NewRecorder(path)
	if err != nil {
		t.Fatal(err)
	}
	rec.Record(tracetest.Run("cert-manager.io", "certificates"))
	if err := rec.Close(); err != nil {
		t.Fatal(err)
	}
	code, _, stderr := cliOutput("judge", "--config", toyConfig, "--trace", path)
	if code != 2 || !strings.Contains(stderr, "certificates") || !strings.Contains(stderr, "widgets") {
		t.Errorf("cli = %d, stderr %q; want 2 naming both primaries", code, stderr)
	}
}

func TestShrinkWritesTheShrunkPlan(t *testing.T) {
	fakeRun(t)
	out := t.TempDir()
	code, stdout, stderr := cliOutput("shrink", "--config", toyConfig, "--plan", "../../targets/toy-widget/plans/b13.json", "--out", out)
	shrunk, err := plan.Load(filepath.Join(out, "shrunk.json"))
	if code != 1 || err != nil || !strings.Contains(stdout, "shrunk.json") {
		t.Fatalf("cli = %d, stdout %q, stderr %q, %v", code, stdout, stderr, err)
	}
	if len(shrunk.Steps) != 2 {
		t.Errorf("b13 shrinks to %+v, want its create and delete.", shrunk)
	}
}

func TestShrinkCutShortByAnInterruptPrintsTheShrinkSoFar(t *testing.T) {
	interruptAt(t, "shrink-1-1")
	out := t.TempDir()
	code, stdout, stderr := cliOutput("shrink", "--config", toyConfig, "--plan", "../../targets/toy-widget/plans/b13.json", "--out", out)
	if code != 1 || !strings.Contains(stdout, filepath.Join(out, "shrunk.json")+" (cut short)") || !strings.Contains(stderr, "context canceled") {
		t.Errorf("cli = %d, stdout %q, stderr %q; want 1 and the shrink so far", code, stdout, stderr)
	}
}

func TestShrinkExits2WhenItsPlanFailsToRun(t *testing.T) {
	runPlan = func(context.Context, run.Options) (check.Report, error) {
		return check.Report{}, errors.New("no controller")
	}
	t.Cleanup(func() { runPlan = run.Run })
	out := t.TempDir()
	if err := os.WriteFile(filepath.Join(out, "summary.md"), []byte("an earlier run's summary\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	code, stdout, stderr := cliOutput("shrink", "--config", toyConfig, "--plan", "../../targets/toy-widget/plans/b13.json", "--out", out)
	if code != 2 || stdout != "" || !strings.Contains(stderr, "no controller") {
		t.Errorf("cli = %d, stdout %q, stderr %q; want 2 and only the error", code, stdout, stderr)
	}
}

func TestShrinkExits0WhenItsPlanFindsNothing(t *testing.T) {
	runPlan = func(_ context.Context, o run.Options) (check.Report, error) {
		return check.Report{}, os.MkdirAll(o.Out, 0o755)
	}
	t.Cleanup(func() { runPlan = run.Run })
	code, stdout, stderr := cliOutput("shrink", "--config", toyConfig, "--plan", "../../targets/toy-widget/plans/b13.json", "--out", t.TempDir())
	if code != 0 || stdout != "No findings over 1 plan.\n" {
		t.Errorf("cli = %d, stdout %q, stderr %q; want 0 and a summary with no findings", code, stdout, stderr)
	}
}

func TestJudgeHarnessErrors(t *testing.T) {
	code, _, stderr := cliOutput("judge", "--config", toyConfig, "--trace", filepath.Join(t.TempDir(), "missing.jsonl"))
	if code != 2 || !strings.Contains(stderr, "missing.jsonl") {
		t.Errorf("cli = %d, stderr %q; want 2 naming the trace", code, stderr)
	}
}

func TestRunPassesOptionsAndPrintsTheReport(t *testing.T) {
	out := t.TempDir()
	var got run.Options
	runPlan = func(_ context.Context, o run.Options) (check.Report, error) {
		got = o
		return check.Report{Findings: make([]check.Finding, 2)}, os.WriteFile(filepath.Join(o.Out, "report.md"), []byte("# Findings\n"), 0o644)
	}
	t.Cleanup(func() { runPlan = run.Run })
	code, stdout, stderr := cliOutput("run", "--config", "c.yaml", "--plan", "p.json", "--out", out, "--launch-arg", "--bug=3", "--launch-arg", "-v")
	want := run.Options{ConfigPath: "c.yaml", PlanPath: "p.json", Out: out, LaunchArgs: []string{"--bug=3", "-v"}}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("run.Options = %+v, want %+v", got, want)
	}
	if code != 1 || stdout != "# Findings\n" {
		t.Errorf("cli = %d, stdout %q, stderr %q; want 1 and the report", code, stdout, stderr)
	}
}

func TestRunHarnessError(t *testing.T) {
	code, _, stderr := cliOutput("run", "--config", toyConfig, "--plan", filepath.Join(t.TempDir(), "missing.json"), "--out", t.TempDir())
	if code != 2 || !strings.Contains(stderr, "missing.json") {
		t.Errorf("cli = %d, stderr %q; want 2 naming the plan", code, stderr)
	}
}

func TestJudgeReportsACrash(t *testing.T) {
	path := filepath.Join(t.TempDir(), "trace.jsonl")
	rec, err := trace.NewRecorder(path)
	if err != nil {
		t.Fatal(err)
	}
	rec.Record(tracetest.Run(group, "widgets"))
	rec.Record(tracetest.Version(widget, "u1", "1", readyWidget()))
	rec.Record(tracetest.Start(1))
	rec.Record(tracetest.Phase(0, trace.FaultPeriod))
	rec.Record(tracetest.EndFaults(0, rec.Now()+time.Second, false))
	rec.Record(tracetest.Exit("exit status 2", false, "panic: boom"))
	rec.Record(tracetest.Phase(0, trace.TimedOut))
	if err := rec.Close(); err != nil {
		t.Fatal(err)
	}
	code, stdout, stderr := cliOutput("judge", "--config", toyConfig, "--trace", filepath.Dir(path))
	if code != 1 || !strings.Contains(stdout, "## Crash: the controller exited 1 time") || !strings.Contains(stdout, "panic: boom") {
		t.Errorf("cli = %d, stdout %q, stderr %q; want 1 and a Crash finding", code, stdout, stderr)
	}
}

// cleanTrace writes a trace that settles under the toy config, and whose run
// event records ready.
func cleanTrace(t *testing.T, ready string) string {
	var b tracetest.Builder
	run := tracetest.Run(group, "widgets")
	run.Run.Primaries[0].Ready, run.Run.Stable, run.Run.Reads, run.Run.Writes = ready, 2*time.Second, 2, 4
	updated := tracetest.Call(trace.Controller, "update", widget, 200, "2")
	b.Add(run, tracetest.Start(1), tracetest.Phase(0, trace.FaultPeriod)).
		At(100*time.Millisecond).Add(tracetest.Call(trace.Harness, "create", widget, 201, "1"), tracetest.Version(widget, "u1", "1", map[string]any{"spec": map[string]any{"count": 1}})).
		At(500*time.Millisecond).Add(updated, tracetest.Version(widget, "u1", "2", readyWidget())).
		At(time.Second).Add(tracetest.EndFaults(0, 10*time.Second, false)).
		At(3 * time.Second).Add(tracetest.Phase(0, trace.Settled)).
		At(5 * time.Second).Add(tracetest.Phase(0, trace.Confirmed))
	var lines bytes.Buffer
	for _, e := range b.Events() {
		line, err := json.Marshal(e)
		if err != nil {
			t.Fatal(err)
		}
		lines.Write(append(line, '\n'))
	}
	path := filepath.Join(t.TempDir(), "trace.jsonl")
	if err := os.WriteFile(path, lines.Bytes(), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestJudgeCleanTrace(t *testing.T) {
	code, stdout, stderr := cliOutput("judge", "--config", toyConfig, "--trace", cleanTrace(t, ""))
	if code != 0 || stdout != "No findings.\n" {
		t.Errorf("cli = %d, stdout %q, stderr %q; want 0 and no findings", code, stdout, stderr)
	}
}

func TestJudgeReadsTheSpecFromTheTraceUnlessGivenAConfig(t *testing.T) {
	path := cleanTrace(t, "false")
	if code, stdout, stderr := cliOutput("judge", "--trace", path); code != 1 || !strings.Contains(stdout, "is not ready") {
		t.Errorf("cli = %d, stdout %q, stderr %q; want 1 under the trace's ready", code, stdout, stderr)
	}
	if code, stdout, stderr := cliOutput("judge", "--trace", path, "--config", toyConfig); code != 0 {
		t.Errorf("cli = %d, stdout %q, stderr %q; want 0 under the config's ready", code, stdout, stderr)
	}
}
