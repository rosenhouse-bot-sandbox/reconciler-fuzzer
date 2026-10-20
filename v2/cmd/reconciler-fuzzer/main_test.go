package main

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/rosenhouse-bot-sandbox/reconciler-fuzzer/v2/run"
	"github.com/rosenhouse-bot-sandbox/reconciler-fuzzer/v2/trace"
	"github.com/rosenhouse-bot-sandbox/reconciler-fuzzer/v2/trace/tracetest"
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
		"no command":       {},
		"unknown command":  {"fuzz"},
		"unknown flag":     {"judge", "--config", toyConfig, "--trace", "t", "--verbose"},
		"missing trace":    {"judge", "--config", toyConfig},
		"extra args":       {"judge", "--config", toyConfig, "--trace", "t", "more"},
		"run missing out":  {"run", "--config", toyConfig, "--plan", "p.json"},
		"run missing plan": {"run", "--config", toyConfig, "--out", "dir"},
		"run extra args":   {"run", "--config", toyConfig, "--plan", "p.json", "--out", "dir", "more"},
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
	if code, _, stderr := cliOutput("run", "--config", toyConfig, "--out", "dir"); code != 2 || !strings.Contains(stderr, "--plan is required") {
		t.Errorf("cli = %d, stderr %q; want 2 naming --plan", code, stderr)
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
	runPlan = func(_ context.Context, o run.Options) (int, error) {
		got = o
		return 2, os.WriteFile(filepath.Join(o.Out, "report.md"), []byte("# Findings\n"), 0o644)
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
	run.Run.Ready, run.Run.Stable, run.Run.Reads, run.Run.Writes = ready, 2*time.Second, 2, 4
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
