package main

import (
	"bytes"
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"syscall"
	"testing"
	"time"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/rosenhouse-bot-sandbox/reconciler-fuzzer/internal/proxy"
	"github.com/rosenhouse-bot-sandbox/reconciler-fuzzer/internal/report"
	"github.com/rosenhouse-bot-sandbox/reconciler-fuzzer/internal/run"
	"github.com/rosenhouse-bot-sandbox/reconciler-fuzzer/internal/target"
)

var updateGolden = flag.Bool("update", false, "rewrite the goldens under testdata from what reconciler-fuzzer writes now")

// checkGolden compares what reconciler-fuzzer wrote with the golden file at
// path.
func checkGolden(t *testing.T, path string, written []byte) {
	t.Helper()
	if *updateGolden {
		if err := os.WriteFile(path, written, 0o644); err != nil {
			t.Fatal(err)
		}
		return
	}
	golden, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(written, golden) {
		t.Errorf("reconciler-fuzzer wrote\n%s\nwhich differs from %s. If the change is deliberate, rerun with -update.", written, path)
	}
}

// widgetSequence creates a Widget, faults the ConfigMaps it creates, and
// deletes it.
func widgetSequence(seed int64) run.Sequence {
	widget := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "toy.reconciler-fuzzer/v1", "kind": "Widget",
		"metadata": map[string]any{"name": "widget"}, "spec": map[string]any{"count": int64(2)},
	}}
	fault := &run.Fault{Match: run.Match{Verb: "create", Resource: "configmaps"}, Action: run.Action{Error: 500}, Until: run.Trigger{Count: 2}}
	return run.Sequence{Seed: seed, Target: "toy-widget", Ops: []run.Op{
		{Index: 0, Type: run.OpCreate, Obj: widget},
		{Index: 1, Type: run.OpFault, Fault: fault},
		{Index: 2, Type: run.OpDelete},
	}}
}

// sampleSummary is an invocation of three runs: the first passed through a
// fault and a crash, the second found G3, and the baseline never ran.
func sampleSummary(t *testing.T) *summary {
	t.Helper()
	toy, err := target.Load(toyTargetYAML)
	if err != nil {
		t.Fatal(err)
	}
	start := time.Date(2026, 9, 24, 1, 2, 3, 0, time.UTC)
	opts := options{command: "run", target: toyTargetYAML, launchArgs: []string{"--bug=3", "--name=a b"},
		seed: 7, seedGiven: true, deadline: 5 * time.Minute, deadlineGiven: true}
	runs := []planned{{sequence: widgetSequence(7)}, {sequence: widgetSequence(8)}, {sequence: widgetSequence(0), baseline: true}}
	// reconciler-fuzzer writes UTC whatever the local zone.
	berlin := time.FixedZone("CEST", 2*60*60)
	s := newSummary(opts, toy, runs, start.In(berlin))
	s.ReconcilerFuzzer = "v0.0.0-test"
	s.deadline(opts)

	crashed := start.Add(5 * time.Second)
	s.Runs[0].ran(run.Result{
		Timeline: run.Timeline{
			Ops:         make([]run.AppliedOp, 3),
			Checkpoints: make([]run.Checkpoint, 3),
			Faults:      []run.Window{{Start: start.Add(time.Second), End: crashed}},
			Exits:       []run.Exit{{At: crashed, Err: errors.New("exit status 2"), Said: "panic: lost the lease"}},
		},
		Recorded: run.Input{Requests: []proxy.Request{{Fault: "error 500"}, {Fault: "error 500"}, {}}},
		Notes:    []string{`the target exited during op 1 (fault) with exit status 2 after writing "panic: lost the lease"`},
	}, 21500*time.Millisecond)
	s.Runs[0].Outcome = outcomePassed

	s.Runs[1].ran(run.Result{Timeline: run.Timeline{
		Ops:         make([]run.AppliedOp, 3),
		Checkpoints: make([]run.Checkpoint, 2),
		Faults:      []run.Window{{}},
	}}, 31*time.Second)
	s.Runs[1].found(run.Violation{
		ID: "G3", Statement: "the target deletes what it manages once the CR is deleted",
		At:       start.Add(55 * time.Second),
		Evidence: "the v1/ConfigMap widget-1 was still there 10s after the CR was deleted",
	}, []string{"the proxy applied the fault of op 1 to no request", `P1 "status.ready <= 3 && has(spec)" is not evaluated`},
		filepath.Join("reconciler-fuzzer-out", "20260924T010203Z-7", "run-2"))

	s.finish(context.Background(), exitViolation, start.Add(58*time.Second+123456*time.Nanosecond).In(berlin))
	return s
}

func TestSummaryJSONKeepsItsSchema(t *testing.T) {
	encoded, err := sampleSummary(t).marshal()
	if err != nil {
		t.Fatal(err)
	}

	checkGolden(t, "testdata/summary.golden.json", encoded)
}

func TestSummaryMarkdown(t *testing.T) {
	for _, test := range []struct {
		golden string
		ending func(t *testing.T, s *summary)
	}{
		{"violation", func(*testing.T, *summary) {}},
		{"bare-violation", func(_ *testing.T, s *summary) { s.Runs[1].Violation.Evidence = "" }},
		{"deadline", func(t *testing.T, s *summary) {
			s.Runs[1] = newSummary(options{}, &target.Target{}, []planned{{sequence: widgetSequence(8)}}, s.Start).Runs[0]
			s.Runs[1].Run, s.Runs[1].File = 2, "sequences/a|b.json"
			s.Target.Version, s.DeadlineDerived = "", true
			s.Error = "the derived deadline of 5m0s stopped the invocation after 1 of 3 runs"
			s.finish(t.Context(), exitError, s.Finish)
		}},
		{"interrupted", func(t *testing.T, s *summary) {
			s.Runs[1].Violation, s.Runs[1].Notes, s.Runs[1].Dir = nil, nil, ""
			dir := filepath.Join(t.TempDir(), "run-2")
			if err := os.Mkdir(dir, 0o755); err != nil {
				t.Fatal(err)
			}
			s.Runs[1].stopped(errRunInterrupted, dir)
			delete(s.Runs[2].OpTypes, run.OpFault)
			s.LaunchArgs, s.Deadline = nil, 0
			ctx, cancel := context.WithCancelCause(t.Context())
			cancel(interrupt{syscall.SIGINT})
			s.finish(ctx, exitError, s.Finish)
		}},
		{"run-error", func(t *testing.T, s *summary) {
			s.Runs[1].Violation, s.Runs[1].Notes, s.Runs[1].Dir = nil, nil, ""
			s.Runs[1].stopped(errors.New("op 0 (create): the target is no longer running"), filepath.Join(t.TempDir(), "run-2"))
			s.finish(t.Context(), exitError, s.Finish)
		}},
		{"killed", func(_ *testing.T, s *summary) {
			s.Runs[1] = newSummary(options{}, &target.Target{}, []planned{{sequence: widgetSequence(8)}}, s.Start).Runs[0]
			s.Runs[1].Run, s.Runs[1].Outcome = 2, outcomeUnfinished
			s.Outcome, s.ExitCode = outcomeUnfinished, nil
		}},
	} {
		t.Run(test.golden, func(t *testing.T) {
			s := sampleSummary(t)
			test.ending(t, s)

			checkGolden(t, "testdata/summary."+test.golden+".golden.md", s.markdown())
		})
	}
}

// writtenSummary is summary.json as a consumer reads it.
type writtenSummary struct {
	Schema           int
	Command          string
	ReconcilerFuzzer string
	Target           struct{ Name, Version, File string }
	Seed             int64
	LaunchArgs       []string
	Cluster          string
	Deadline         string
	DeadlineDerived  bool
	Start, Finish    time.Time
	Outcome          string
	ExitCode         int
	Error            string
	Runs             []writtenRun
}

type writtenRun struct {
	Run                                         int
	Seed                                        int64
	File                                        string
	Baseline                                    bool
	Outcome                                     string
	Duration                                    string
	Ops, Applied                                int
	OpTypes                                     map[string]int
	FaultsApplied, FaultedRequests, Checkpoints int
	Exits                                       []struct {
		At          time.Time
		Error, Said string
	}
	Notes     []string
	Violation *struct {
		ID, Statement, Evidence string
		At                      time.Time
	}
	Error    string
	Dir      string
	Sequence json.RawMessage
}

// summaryDir is the directory of the one invocation under out.
func summaryDir(t *testing.T, out string) string {
	t.Helper()
	paths, err := filepath.Glob(filepath.Join(out, "*", "summary.json"))
	if err != nil || len(paths) != 1 {
		t.Fatalf("The invocation wrote the summaries %v, want one: %v", paths, err)
	}
	return filepath.Dir(paths[0])
}

func readSummary(t *testing.T, out string) writtenSummary {
	t.Helper()
	encoded, err := os.ReadFile(filepath.Join(summaryDir(t, out), "summary.json"))
	if err != nil {
		t.Fatal(err)
	}
	decoder := json.NewDecoder(bytes.NewReader(encoded))
	decoder.DisallowUnknownFields()
	var written writtenSummary
	if err := decoder.Decode(&written); err != nil {
		t.Fatalf("summary.json does not decode: %v\n%s", err, encoded)
	}
	return written
}

func marshalled(t *testing.T, sequence run.Sequence) string {
	t.Helper()
	encoded, err := sequence.Marshal()
	if err != nil {
		t.Fatal(err)
	}
	return string(encoded)
}

// sequenceOf reads a run's sequence back in its canonical form.
func sequenceOf(t *testing.T, ran writtenRun) string {
	t.Helper()
	sequence, err := run.UnmarshalSequence(ran.Sequence)
	if err != nil {
		t.Fatalf("Run %d's sequence does not parse: %v", ran.Run, err)
	}
	return marshalled(t, sequence)
}

func TestAPassingInvocationWritesItsSummary(t *testing.T) {
	session := &fakeSession{}
	out := t.TempDir()
	before := time.Now()

	code, stdout, stderr := invokeWith(t, session, countingGenerator(nil, run.OpRestart, run.OpSettle, run.OpSettle),
		"run", "--target", toyTargetYAML, "--out", out, "--runs", "3", "--seed", "42", "--no-baseline", "--deadline", "5m",
		"--launch-arg", "--bug=0")

	if code != exitOK {
		t.Fatalf("reconciler-fuzzer run exited %d: %s", code, stderr)
	}
	written := readSummary(t, out)
	if written.Schema != 2 || written.Command != "run" || written.ReconcilerFuzzer != version() {
		t.Errorf("The summary is schema %d of reconciler-fuzzer %s %q, want schema 2 of reconciler-fuzzer run %q.",
			written.Schema, written.Command, written.ReconcilerFuzzer, version())
	}
	if written.Target.Name != "toy-widget" || written.Target.Version != "dev" || written.Target.File != toyTargetYAML {
		t.Errorf("The summary names the target %+v, want toy-widget dev from %s.", written.Target, toyTargetYAML)
	}
	if written.Seed != 42 || !slices.Equal(written.LaunchArgs, []string{"--bug=0"}) || written.Cluster != "envtest" {
		t.Errorf("The summary says seed %d, launch args %q and cluster %q, want 42, [--bug=0] and envtest.",
			written.Seed, written.LaunchArgs, written.Cluster)
	}
	if written.Deadline != "5m0s" || written.DeadlineDerived {
		t.Errorf("The summary says the deadline is %s, derived %t, want the 5m0s given.", written.Deadline, written.DeadlineDerived)
	}
	if written.Outcome != "passed" || written.ExitCode != exitOK || written.Error != "" {
		t.Errorf("The summary says %s, exit %d, error %q, want passed and exit 0.", written.Outcome, written.ExitCode, written.Error)
	}
	if written.Start.Before(before.Truncate(time.Second)) || written.Finish.Before(written.Start) || written.Finish.After(time.Now()) {
		t.Errorf("The summary says the invocation ran from %v to %v, want inside %v and now.", written.Start, written.Finish, before)
	}
	if len(written.Runs) != 3 {
		t.Fatalf("The summary lists %d runs, want 3.", len(written.Runs))
	}
	for i, ran := range written.Runs {
		if ran.Run != i+1 || ran.Seed != 42+int64(i) || ran.File != "" || ran.Outcome != "passed" || ran.Dir != "" {
			t.Errorf("The summary lists run %d as %+v, want run %d, seed %d, drawn, passed and discarded.", i+1, ran, i+1, 42+i)
		}
		if ran.Ops != 3 || !reflect.DeepEqual(ran.OpTypes, map[string]int{"restart": 1, "settle": 2}) {
			t.Errorf("The summary counts %d ops, %v, want 3: a restart and 2 settles.", ran.Ops, ran.OpTypes)
		}
		if duration, err := time.ParseDuration(ran.Duration); err != nil || duration < 0 || duration != duration.Round(time.Millisecond) {
			t.Errorf("The summary says run %d took %q, want a duration in milliseconds: %v", i+1, ran.Duration, err)
		}
		if got, want := sequenceOf(t, ran), marshalled(t, session.sequences[i]); got != want {
			t.Errorf("The summary holds run %d's sequence as\n%s\nwant what it executed:\n%s", i+1, got, want)
		}
	}
	if _, err := os.Stat(filepath.Join(summaryDir(t, out), "summary.md")); err != nil {
		t.Errorf("The invocation wrote no summary.md: %v", err)
	}
	if want := "every run passed.\n"; !strings.HasSuffix(stdout, want) {
		t.Errorf("reconciler-fuzzer run printed %q, want it to end with %q.", stdout, want)
	}
}

func TestTheSummaryTimesTheInvocationAndEachRun(t *testing.T) {
	berlin := time.FixedZone("CEST", 2*60*60)
	clock := time.Date(2026, 9, 24, 3, 2, 3, 0, berlin)
	began := clock
	session := &fakeSession{now: func() time.Time { return clock }}
	session.after = func() { clock = clock.Add(1500 * time.Millisecond) }
	out := t.TempDir()

	code, _, stderr := invoke(t, session, "run", "--target", toyTargetYAML, "--out", out, "--runs", "2", "--seed", "1", "--no-baseline")

	if code != exitOK {
		t.Fatalf("reconciler-fuzzer run exited %d: %s", code, stderr)
	}
	written := readSummary(t, out)
	if !written.Start.Equal(began) || !written.Finish.Equal(began.Add(3*time.Second)) || written.Start.Location() != time.UTC {
		t.Errorf("The summary says the invocation ran from %v to %v, want the 3s from %v, in UTC.", written.Start, written.Finish, began.UTC())
	}
	for _, ran := range written.Runs {
		if ran.Duration != "1.5s" {
			t.Errorf("The summary says run %d took %q, want 1.5s.", ran.Run, ran.Duration)
		}
	}
}

func TestAPassingRunReconcilerFuzzerCannotDiscardStopsTheInvocation(t *testing.T) {
	out := t.TempDir()
	session := &fakeSession{discarding: errors.New("discarding the passing run: device or resource busy")}

	code, _, stderr := invoke(t, session, "run", "--target", toyTargetYAML, "--out", out, "--runs", "2", "--seed", "1")

	if code != exitError {
		t.Fatalf("reconciler-fuzzer run exited %d, want %d: %s", code, exitError, stderr)
	}
	written := readSummary(t, out)
	if written.Outcome != "error" || written.Error != session.discarding.Error() {
		t.Errorf("The summary says %s on %q, want an error on %q.", written.Outcome, written.Error, session.discarding)
	}
	if outcomes := []string{written.Runs[0].Outcome, written.Runs[1].Outcome}; !slices.Equal(outcomes, []string{"passed", "not run"}) {
		t.Errorf("The summary lists the runs as %q, want run 1 passed and run 2 not run.", outcomes)
	}
}

func TestASummaryReconcilerFuzzerCannotWriteOnlyWarns(t *testing.T) {
	var taken []string
	session := &fakeSession{}
	// A directory in the way fails the write.
	session.after = func() {
		taken = nil
		for _, name := range []string{"summary.json", "summary.md"} {
			path := filepath.Join(filepath.Dir(session.dirs[0]), name)
			_ = os.Remove(path)
			if err := os.Mkdir(path, 0o755); err != nil {
				t.Fatal(err)
			}
			taken = append(taken, path)
		}
	}

	code, _, stderr := invoke(t, session, "run", "--target", toyTargetYAML, "--out", t.TempDir(), "--runs", "2")

	if code != exitOK {
		t.Errorf("reconciler-fuzzer run exited %d, want %d: the runs passed.", code, exitOK)
	}
	for _, path := range taken {
		if n := strings.Count(stderr, "reconciler-fuzzer: writing "+path); n != 1 {
			t.Errorf("reconciler-fuzzer run printed\n%s\nwhich says %d times that it could not write %s, want once.", stderr, n, path)
		}
	}
}

func TestAClusterThatDoesNotStopOnlyWarns(t *testing.T) {
	session := &fakeSession{unclosed: errors.New("the control plane did not stop")}

	code, _, stderr := invoke(t, session, "run", "--target", toyTargetYAML, "--out", t.TempDir(), "--runs", "1")

	if code != exitOK {
		t.Errorf("reconciler-fuzzer run exited %d, want %d: the run passed.", code, exitOK)
	}
	if want := "reconciler-fuzzer: the control plane did not stop\n"; !strings.Contains(stderr, want) {
		t.Errorf("reconciler-fuzzer run printed %q on stderr, want %q.", stderr, want)
	}
}

func TestTheSummaryNamesTheSequenceFilesItRan(t *testing.T) {
	out := t.TempDir()
	path := writeSequence(t, 8675309)
	junit := filepath.Join(t.TempDir(), "junit.xml")

	code, _, stderr := invoke(t, &fakeSession{}, "replay", "--target", toyTargetYAML, "--out", out, "--kubeconfig", "kind.kubeconfig",
		"--junit", junit, path)

	if code != exitOK {
		t.Fatalf("reconciler-fuzzer replay exited %d: %s", code, stderr)
	}
	if suite, _ := readJUnit(t, junit); len(suite.Cases) != 1 || suite.Cases[0].Name != "run 1: "+path {
		t.Errorf("The JUnit testcases are %+v, want one named for %s.", suite.Cases, path)
	}
	written := readSummary(t, out)
	if written.Command != "replay" || written.Cluster != "kubeconfig" || written.Seed != 8675309 {
		t.Errorf("The summary says reconciler-fuzzer %s on %s with seed %d, want replay on kubeconfig with 8675309.",
			written.Command, written.Cluster, written.Seed)
	}
	if !written.DeadlineDerived || written.Deadline == "" {
		t.Errorf("The summary says the deadline is %q, derived %t, want the one reconciler-fuzzer derived.", written.Deadline, written.DeadlineDerived)
	}
	if len(written.Runs) != 1 || written.Runs[0].File != path || written.Runs[0].Seed != 8675309 {
		t.Errorf("The summary lists %+v, want the one run of %s.", written.Runs, path)
	}
	if md, _ := os.ReadFile(filepath.Join(summaryDir(t, out), "summary.md")); !strings.Contains(string(md), " on the --kubeconfig cluster.") {
		t.Errorf("summary.md is\n%s\nwant it to say reconciler-fuzzer ran on the --kubeconfig cluster.", md)
	}
}

// The summary's seed is the one its directory is named for.
func TestTheSummaryGivesTheSeedTheCallerGave(t *testing.T) {
	out := t.TempDir()

	code, _, stderr := invoke(t, &fakeSession{}, "run", "--target", toyTargetYAML, "--out", out, "--seed", "5", writeSequence(t, 8675309))

	if code != exitOK {
		t.Fatalf("reconciler-fuzzer run exited %d: %s", code, stderr)
	}
	written := readSummary(t, out)
	if !strings.HasSuffix(summaryDir(t, out), "-5") || written.Seed != 5 || written.Runs[0].Seed != 8675309 {
		t.Errorf("%s holds a summary of seed %d, with run 1 of seed %d, want seed 5 and 8675309.",
			summaryDir(t, out), written.Seed, written.Runs[0].Seed)
	}
}

// A failing run's summary says what its report says, which may be of the
// minimized sequence's run.
func TestTheSummaryCarriesTheReportsViolationAndNotes(t *testing.T) {
	drawn := run.Violation{ID: "G4", Statement: "the target converges", Evidence: "the settle wait after op 2 expired"}
	minimized := run.Violation{ID: "G4", Statement: "the target converges", Evidence: "the settle wait after op 0 expired"}
	onRestart := func(candidate run.Sequence, _ string) *run.Violation {
		if slices.ContainsFunc(candidate.Ops, func(op run.Op) bool { return op.Type == run.OpRestart }) {
			return &minimized
		}
		return nil
	}
	workloads, err := target.Load(workloadsTargetYAML)
	if err != nil {
		t.Fatal(err)
	}
	envtestWarning := envtestLimit(options{}, workloads)
	for _, test := range []struct {
		name      string
		session   func(cancel context.CancelCauseFunc) *fakeSession
		target    string
		args      []string
		violation run.Violation
		notes     []string
	}{
		{name: "a G4 of a sequence file on envtest",
			session: func(context.CancelCauseFunc) *fakeSession {
				return &fakeSession{results: []run.Result{{Violation: &drawn, Notes: []string{"a note of the run"}}}}
			},
			target:    workloadsTargetYAML,
			args:      []string{writeSequence(t, 1)},
			violation: drawn, notes: []string{"a note of the run", envtestWarning}},
		{name: "a G4 of a minimized sequence on envtest",
			session: func(context.CancelCauseFunc) *fakeSession {
				return &fakeSession{results: []run.Result{{Violation: &drawn}}, fails: onRestart, notes: []string{"a note of the minimized run"}}
			},
			target:    workloadsTargetYAML,
			args:      []string{"--runs", "1", "--seed", "1"},
			violation: minimized, notes: []string{"a note of the minimized run", envtestWarning}},
		{name: "a sequence file",
			session: func(context.CancelCauseFunc) *fakeSession {
				return &fakeSession{results: []run.Result{{Violation: &drawn, Notes: []string{"a note of the run"}}}}
			},
			args:      []string{writeSequence(t, 1)},
			violation: drawn, notes: []string{"a note of the run"}},
		{name: "a minimized sequence",
			session: func(context.CancelCauseFunc) *fakeSession {
				return &fakeSession{results: []run.Result{{Violation: &drawn, Notes: []string{"a note of the drawn run"}}},
					fails: onRestart, notes: []string{"a note of the minimized run"}}
			},
			args:      []string{"--runs", "1", "--seed", "1"},
			violation: minimized, notes: []string{"a note of the minimized run"}},
		{name: "minimization an interrupt ended",
			session: func(cancel context.CancelCauseFunc) *fakeSession {
				s := &fakeSession{results: []run.Result{{Violation: &drawn}}}
				s.after = func() { cancel(interrupt{syscall.SIGINT}) }
				return s
			},
			args:      []string{"--runs", "1", "--seed", "1"},
			violation: drawn, notes: []string{"an interrupt ended minimization before it found a smaller sequence: this is the sequence reconciler-fuzzer generated"}},
	} {
		t.Run(test.name, func(t *testing.T) {
			ctx, cancel := context.WithCancelCause(t.Context())
			defer cancel(nil)
			out := t.TempDir()
			generate := countingGenerator(nil, run.OpSettle, run.OpRestart, run.OpSettle)
			session := test.session(cancel)

			invokeCtx(t, ctx, session, generate, slices.Concat([]string{"run", "--target", cmp.Or(test.target, toyTargetYAML), "--out", out}, test.args)...)

			ran := readSummary(t, out).Runs[0]
			if ran.Violation == nil || ran.Violation.Evidence != test.violation.Evidence || !slices.Equal(ran.Notes, test.notes) {
				t.Errorf("The summary lists run 1's violation as %+v and its notes as %q, want %+v and %q.",
					ran.Violation, ran.Notes, test.violation, test.notes)
			}
			if reported := reportedNotes(t, session.dirs[0]); !slices.Equal(ran.Notes, reported) {
				t.Errorf("The summary lists run 1's notes as %q, and its report %q.", ran.Notes, reported)
			}
		})
	}
}

func reportedNotes(t *testing.T, dir string) []string {
	t.Helper()
	encoded, err := os.ReadFile(filepath.Join(dir, report.JSONFile))
	if err != nil {
		t.Fatal(err)
	}
	var reported struct{ Notes []string }
	if err := json.Unmarshal(encoded, &reported); err != nil {
		t.Fatal(err)
	}
	return reported.Notes
}

func TestARunThatMadeNoDirectoryNamesNone(t *testing.T) {
	out := t.TempDir()
	junit := filepath.Join(t.TempDir(), "junit.xml")
	session := &fakeSession{failures: []error{errors.New("the sequence is for another target")}, writesNothing: true}

	code, _, stderr := invoke(t, session, "replay", "--target", toyTargetYAML, "--out", out, "--junit", junit, writeSequence(t, 1))

	if code != exitError {
		t.Fatalf("reconciler-fuzzer replay exited %d, want %d: %s", code, exitError, stderr)
	}
	if ran := readSummary(t, out).Runs[0]; ran.Outcome != "error" || ran.Dir != "" {
		t.Errorf("The summary lists the run as %s in %q, want an error and no directory.", ran.Outcome, ran.Dir)
	}
	if suite, _ := readJUnit(t, junit); suite.Cases[0].Error == nil || suite.Cases[0].Error.Body != "" {
		t.Errorf("The JUnit testcase errs with %+v, want no directory named.", suite.Cases[0].Error)
	}
}

// Faults the proxy never applied changed nothing, so the summary counts them
// apart from those it did.
func TestTheSummaryCountsFaults(t *testing.T) {
	at := time.Date(2026, 9, 24, 1, 2, 3, 0, time.UTC)
	result := run.Result{
		Timeline: run.Timeline{
			Ops:         make([]run.AppliedOp, 2),
			Checkpoints: make([]run.Checkpoint, 4),
			Faults:      []run.Window{{Start: at, End: at.Add(time.Second)}, {}},
			Exits: []run.Exit{
				{At: at, Err: errors.New("exit status 2"), Said: "panic: runtime error: integer divide by zero"},
				{At: at.Add(time.Second)},
			},
		},
		Recorded: run.Input{Requests: []proxy.Request{{Fault: "error 500"}, {}, {Fault: "delay 1s"}}},
		Notes:    []string{"the target exited during op 1 (fault) with exit status 2", "the proxy applied the fault of op 1 to no request"},
	}
	session := &fakeSession{results: []run.Result{result}}
	out := t.TempDir()
	fault := &run.Fault{Action: run.Action{Error: 500}, Until: run.Trigger{Count: 3}}
	path := filepath.Join(t.TempDir(), "faults.json")
	if err := run.WriteSequence(path, run.Sequence{Seed: 1, Target: "toy-widget", Ops: []run.Op{
		{Index: 0, Type: run.OpFault, Fault: fault}, {Index: 1, Type: run.OpFault, Fault: fault}, {Index: 2, Type: run.OpSettle},
	}}); err != nil {
		t.Fatal(err)
	}

	code, _, stderr := invoke(t, session, "replay", "--target", toyTargetYAML, "--out", out, path)

	if code != exitOK {
		t.Fatalf("reconciler-fuzzer run exited %d: %s", code, stderr)
	}
	ran := readSummary(t, out).Runs[0]
	if ran.Ops != 3 || ran.Applied != 2 || ran.OpTypes["fault"] != 2 {
		t.Errorf("The summary says the run applied %d of %d ops, %d of them faults, want 2 of 3, and 2 faults.",
			ran.Applied, ran.Ops, ran.OpTypes["fault"])
	}
	if ran.FaultsApplied != 1 || ran.FaultedRequests != 2 || ran.Checkpoints != 4 {
		t.Errorf("The summary says the proxy applied %d faults to %d requests, over %d checkpoints, want 1, 2 and 4.",
			ran.FaultsApplied, ran.FaultedRequests, ran.Checkpoints)
	}
	if !slices.Equal(ran.Notes, result.Notes) {
		t.Errorf("The summary carries the notes %q, want %q.", ran.Notes, result.Notes)
	}
	if len(ran.Exits) != 2 {
		t.Fatalf("The summary lists the exits %+v, want the target's 2.", ran.Exits)
	}
	first, second := ran.Exits[0], ran.Exits[1]
	if !first.At.Equal(at) || first.Error != "exit status 2" || first.Said != result.Timeline.Exits[0].Said {
		t.Errorf("The summary lists the first exit as %+v, want %+v.", first, result.Timeline.Exits[0])
	}
	if !second.At.Equal(at.Add(time.Second)) || second.Error != "" || second.Said != "" {
		t.Errorf("The summary lists the second exit as %+v, want one at %v with no error.", second, at.Add(time.Second))
	}
}

// A second signal kills reconciler-fuzzer at once, and stopping the control
// plane takes seconds, so the summary is written first.
func TestTheSummaryIsWrittenBeforeTheClusterStops(t *testing.T) {
	out := t.TempDir()
	var written writtenSummary
	session := &fakeSession{closing: func() { written = readSummary(t, out) }}

	code, _, stderr := invoke(t, session, "run", "--target", toyTargetYAML, "--out", out, "--runs", "1")

	if code != exitOK {
		t.Fatalf("reconciler-fuzzer run exited %d: %s", code, stderr)
	}
	if !session.closed || written.Outcome != "passed" {
		t.Errorf("reconciler-fuzzer closed the session %t, with the summary saying %q, want it closed after the summary said passed.",
			session.closed, written.Outcome)
	}
}

// unfinishedSummary reads the summary of an invocation that has not finished,
// which says neither when it finished nor what it exited with.
func unfinishedSummary(t *testing.T, out string) writtenSummary {
	t.Helper()
	encoded, err := os.ReadFile(filepath.Join(summaryDir(t, out), "summary.json"))
	if err != nil {
		t.Fatal(err)
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(encoded, &fields); err != nil {
		t.Fatal(err)
	}
	for _, field := range []string{"finish", "exitCode"} {
		if said, ok := fields[field]; ok {
			t.Errorf("The unfinished summary says %s is %s.", field, said)
		}
	}
	return readSummary(t, out)
}

func runOutcomes(written writtenSummary) []string {
	var outcomes []string
	for _, ran := range written.Runs {
		outcomes = append(outcomes, ran.Outcome)
	}
	return outcomes
}

// A signal reconciler-fuzzer cannot catch, or a crash, leaves the summary of
// the runs that finished.
func TestTheSummaryRecordsEachRunAsItStarts(t *testing.T) {
	out := t.TempDir()
	junit := filepath.Join(t.TempDir(), "junit.xml")
	var planned []string
	var during [][]string
	var suites []parsedSuite
	session := &fakeSession{}
	session.opening = func() {
		written := unfinishedSummary(t, out)
		planned = append(runOutcomes(written), written.Outcome)
	}
	session.after = func() {
		written := unfinishedSummary(t, out)
		during = append(during, append(runOutcomes(written), written.Outcome))
		suite, _ := readJUnit(t, junit)
		suites = append(suites, suite)
	}

	code, _, stderr := invoke(t, session, "run", "--target", toyTargetYAML, "--out", out, "--runs", "2", "--seed", "1", "--no-baseline", "--junit", junit)

	if code != exitOK {
		t.Fatalf("reconciler-fuzzer run exited %d: %s", code, stderr)
	}
	if want := []string{"not run", "not run", "unfinished"}; !slices.Equal(planned, want) {
		t.Errorf("Before the cluster started, the summary said %q, want %q.", planned, want)
	}
	want := [][]string{{"unfinished", "not run", "unfinished"}, {"passed", "unfinished", "unfinished"}}
	if !reflect.DeepEqual(during, want) {
		t.Errorf("During each run, the summary said %q, want %q.", during, want)
	}
	if len(suites) != 2 {
		t.Fatalf("reconciler-fuzzer ran %d times, want 2.", len(suites))
	}
	for i, suite := range suites {
		if timeOf(suite.Time) != "no time" {
			t.Errorf("During run %d, the JUnit testsuite took %s, want no time, since reconciler-fuzzer had not finished.", i+1, timeOf(suite.Time))
		}
	}
	var cases []string
	for _, c := range suites[1].Cases {
		switch {
		case c.Error != nil:
			cases = append(cases, fmt.Sprintf("%s: %s %s", c.Name, c.Error.Type, c.Error.Message))
		default:
			cases = append(cases, c.Name+": passed")
		}
	}
	if want := []string{"run 1: seed 1: passed", "run 2: seed 2: unfinished reconciler-fuzzer did not finish this run",
		"reconciler-fuzzer: unfinished reconciler-fuzzer did not finish"}; !slices.Equal(cases, want) {
		t.Errorf("During run 2, the JUnit testcases were %q, want %q.", cases, want)
	}
}

// Minimizing can take minutes, and a kill then leaves the violation the run
// found.
func TestTheSummaryRecordsAViolationBeforeMinimizing(t *testing.T) {
	out := t.TempDir()
	junit := filepath.Join(t.TempDir(), "junit.xml")
	g4 := run.Violation{ID: "G4", Statement: "the target converges", Evidence: "the settle wait after op 2 expired"}
	session := &fakeSession{results: []run.Result{{Violation: &g4, Notes: []string{"a note of the run"}}}}
	var minimizing []writtenRun
	var md string
	var failure *parsedProblem
	session.fails = func(_ run.Sequence, dir string) *run.Violation {
		if filepath.Base(dir) == shrinkDir && len(minimizing) == 0 {
			minimizing = append(minimizing, unfinishedSummary(t, out).Runs[0])
			written, _ := os.ReadFile(filepath.Join(summaryDir(t, out), "summary.md"))
			md = string(written)
			suite, _ := readJUnit(t, junit)
			failure = suite.Cases[0].Failure
		}
		return nil
	}
	workloads, err := target.Load(workloadsTargetYAML)
	if err != nil {
		t.Fatal(err)
	}
	notes := []string{"a note of the run", envtestLimit(options{}, workloads)}

	code, _, stderr := invokeWith(t, session, countingGenerator(nil, run.OpSettle, run.OpRestart, run.OpSettle),
		"run", "--target", workloadsTargetYAML, "--out", out, "--runs", "2", "--seed", "1", "--junit", junit)

	if code != exitViolation {
		t.Fatalf("reconciler-fuzzer run exited %d, want %d: %s", code, exitViolation, stderr)
	}
	if len(minimizing) == 0 {
		t.Fatal("reconciler-fuzzer replayed nothing to minimize run 1.")
	}
	ran := minimizing[0]
	if ran.Outcome != "violation" || ran.Violation == nil || ran.Violation.ID != g4.ID || ran.Violation.Evidence != g4.Evidence ||
		ran.Dir != "run-1" || !slices.Equal(ran.Notes, notes) {
		t.Errorf("While reconciler-fuzzer minimized run 1, the summary listed it as %s in %q, with the violation %+v and the notes %q, "+
			"want %s in run-1, with %+v and %q.", ran.Outcome, ran.Dir, ran.Violation, ran.Notes, "violation", g4, notes)
	}
	// The report comes once the pass ends.
	if !strings.Contains(md, "`run-1/` holds the evidence.\n") {
		t.Errorf("While reconciler-fuzzer minimized run 1, summary.md was\n%s\nwant it to say run-1/ holds the evidence, and no report yet.", md)
	}
	if failure == nil || failure.Type != g4.ID || !strings.HasSuffix(failure.Body, "run-1 holds the evidence.") {
		t.Errorf("While reconciler-fuzzer minimized run 1, its JUnit testcase failed with %+v, want %s, and run-1 holding the evidence and no report yet.",
			failure, g4.ID)
	}
}

// reconciler-fuzzer runs the minimized sequence again into the run's directory,
// which then no longer holds the run's evidence alone.
func TestTheSummarySaysWhenReconcilerFuzzerRunsTheMinimizedSequenceAgain(t *testing.T) {
	out := t.TempDir()
	junit := filepath.Join(t.TempDir(), "junit.xml")
	g4 := run.Violation{ID: "G4", Statement: "the target converges"}
	session := &fakeSession{}
	var md string
	var failure *parsedProblem
	session.fails = func(sequence run.Sequence, dir string) *run.Violation {
		if rerun := len(session.dirs) > 1 && dir == session.dirs[0]; rerun {
			written, _ := os.ReadFile(filepath.Join(summaryDir(t, out), "summary.md"))
			md = string(written)
			suite, _ := readJUnit(t, junit)
			failure = suite.Cases[0].Failure
		}
		if len(sequence.Ops) < 2 {
			return nil
		}
		return &g4
	}

	code, _, stderr := invokeWith(t, session, countingGenerator(nil, run.OpSettle, run.OpSettle, run.OpSettle),
		"run", "--target", toyTargetYAML, "--out", out, "--runs", "1", "--seed", "1", "--junit", junit)

	if code != exitViolation {
		t.Fatalf("reconciler-fuzzer run exited %d, want %d: %s", code, exitViolation, stderr)
	}
	if md == "" {
		t.Fatal("reconciler-fuzzer never ran the minimized sequence again.")
	}
	if want := "`run-1/` holds a partial run of the minimized sequence.\n"; !strings.Contains(md, want) {
		t.Errorf("While reconciler-fuzzer ran the minimized sequence again, summary.md was\n%s\nwant it to say %q.", md, want)
	}
	if want := "run-1 holds a partial run of the minimized sequence."; failure == nil || !strings.HasSuffix(failure.Body, want) {
		t.Errorf("While reconciler-fuzzer ran the minimized sequence again, its JUnit testcase failed with %+v, want a body that ends %q.", failure, want)
	}
	written, err := os.ReadFile(filepath.Join(summaryDir(t, out), "summary.md"))
	if want := "`run-1/` holds the report and the evidence.\n"; err != nil || !strings.Contains(string(written), want) {
		t.Errorf("Once reconciler-fuzzer finished, summary.md was\n%s\nwant it to say %q.", written, want)
	}
}

// reconciler-fuzzer dies of a signal that arrives as the cluster stops, so the
// summary has to say so.
func TestAnInterruptAfterEveryRunPassedRewritesTheSummary(t *testing.T) {
	ctx, cancel := context.WithCancelCause(t.Context())
	defer cancel(nil)
	finished := time.Date(2026, 9, 24, 1, 2, 3, 0, time.UTC)
	clock := finished
	session := &fakeSession{now: func() time.Time { return clock }}
	session.closing = func() {
		clock = clock.Add(time.Minute)
		cancel(interrupt{syscall.SIGTERM})
	}
	out := t.TempDir()
	junit := filepath.Join(t.TempDir(), "junit.xml")

	code, _, stderr := invokeCtx(t, ctx, session, countingGenerator(nil),
		"run", "--target", toyTargetYAML, "--out", out, "--runs", "2", "--seed", "1", "--junit", junit)

	want := 128 + int(syscall.SIGTERM)
	if code != want {
		t.Fatalf("reconciler-fuzzer run exited %d, want %d: %s", code, want, stderr)
	}
	written := readSummary(t, out)
	if written.Outcome != "interrupted" || written.ExitCode != want || written.Error != "an interrupt arrived after every run passed" {
		t.Errorf("The summary says %s, exit %d, on %q, want interrupted, exit %d, after every run passed.",
			written.Outcome, written.ExitCode, written.Error, want)
	}
	if !written.Finish.Equal(finished) {
		t.Errorf("The summary says the invocation finished at %v, want %v, before the cluster stopped.", written.Finish, finished)
	}
	if md, _ := os.ReadFile(filepath.Join(summaryDir(t, out), "summary.md")); !strings.Contains(string(md), fmt.Sprintf("exited %d.", want)) {
		t.Errorf("summary.md is\n%s\nwant it to say reconciler-fuzzer exited %d.", md, want)
	}
	suite, _ := readJUnit(t, junit)
	if i := slices.IndexFunc(suite.Cases, func(c parsedCase) bool { return c.Name == "reconciler-fuzzer" }); i < 0 || suite.Cases[i].Error == nil ||
		suite.Cases[i].Error.Type != "interrupted" {
		t.Errorf("The JUnit testcases are %+v, want an interrupted one named reconciler-fuzzer.", suite.Cases)
	}
}

func TestEveryExitPathWritesTheSummary(t *testing.T) {
	g3 := run.Violation{ID: "G3", Statement: "the target deletes what it manages", Evidence: "the v1/ConfigMap widget-0 was still there"}
	stopping := func(cancel context.CancelCauseFunc, sig syscall.Signal, s *fakeSession) *fakeSession {
		s.after = func() { cancel(interrupt{sig}) }
		return s
	}
	for _, test := range []struct {
		name    string
		session func(cancel context.CancelCauseFunc) *fakeSession
		args    []string
		code    int
		outcome string
		// err is what stopped the invocation where no run did.
		err string
		// ran is the error of the run that failed.
		ran  string
		runs []string
	}{
		{name: "a violation",
			session: func(context.CancelCauseFunc) *fakeSession {
				return &fakeSession{results: []run.Result{{}, {Violation: &g3}}}
			},
			code: exitViolation, outcome: "violation", runs: []string{"passed", "violation", "not run"}},
		{name: "a harness error",
			session: func(context.CancelCauseFunc) *fakeSession {
				return &fakeSession{failures: []error{nil, errors.New("the control plane did not start")}}
			},
			code: exitError, outcome: "error", ran: "the control plane did not start", runs: []string{"passed", "error", "not run"}},
		{name: "a deadline between runs",
			session: func(context.CancelCauseFunc) *fakeSession { return &fakeSession{} },
			args:    []string{"--deadline", "1ns"},
			code:    exitError, outcome: "error", err: "the --deadline of 1ns stopped the invocation after 1 of 3 runs",
			runs: []string{"passed", "not run", "not run"}},
		{name: "an interrupt between runs",
			session: func(cancel context.CancelCauseFunc) *fakeSession {
				return stopping(cancel, syscall.SIGINT, &fakeSession{})
			},
			code: 128 + int(syscall.SIGINT), outcome: "interrupted", err: "an interrupt stopped the invocation after 1 of 3 runs",
			runs: []string{"passed", "not run", "not run"}},
		{name: "an interrupt during a run",
			session: func(cancel context.CancelCauseFunc) *fakeSession {
				return stopping(cancel, syscall.SIGTERM, &fakeSession{failures: []error{fmt.Errorf("op 0 (settle): %w", context.Canceled)}})
			},
			code: 128 + int(syscall.SIGTERM), outcome: "interrupted", ran: "an interrupt stopped the run",
			runs: []string{"interrupted", "not run", "not run"}},
		{name: "an interrupt while minimizing",
			session: func(cancel context.CancelCauseFunc) *fakeSession {
				return stopping(cancel, syscall.SIGINT, &fakeSession{results: []run.Result{{Violation: &g3}}})
			},
			code: 128 + int(syscall.SIGINT), outcome: "interrupted", runs: []string{"violation", "not run", "not run"}},
		{name: "a target the cluster refuses",
			session: func(context.CancelCauseFunc) *fakeSession {
				return &fakeSession{refused: errors.New("these are cluster-scoped: the managed rbac.authorization.k8s.io/v1/ClusterRole")}
			},
			code: exitError, outcome: "error", err: "these are cluster-scoped", runs: []string{"not run", "not run", "not run"}},
		{name: "a cluster that does not start",
			session: func(context.CancelCauseFunc) *fakeSession {
				return &fakeSession{unopened: errors.New("unable to start the control plane")}
			},
			code: exitError, outcome: "error", err: "unable to start the control plane", runs: []string{"not run", "not run", "not run"}},
	} {
		t.Run(test.name, func(t *testing.T) {
			ctx, cancel := context.WithCancelCause(t.Context())
			defer cancel(nil)
			out := t.TempDir()
			args := slices.Concat([]string{"run", "--target", toyTargetYAML, "--out", out, "--runs", "3", "--seed", "7", "--no-baseline"}, test.args)

			code, _, stderr := invokeCtx(t, ctx, test.session(cancel), countingGenerator(nil), args...)

			if code != test.code {
				t.Fatalf("reconciler-fuzzer run exited %d, want %d: %s", code, test.code, stderr)
			}
			written := readSummary(t, out)
			if written.Outcome != test.outcome || written.ExitCode != test.code {
				t.Errorf("The summary says %s, exit %d, want %s, exit %d.", written.Outcome, written.ExitCode, test.outcome, test.code)
			}
			if said := written.Error; (test.err == "") != (said == "") || !strings.Contains(said, test.err) {
				t.Errorf("The summary says the invocation stopped on %q, want %q.", said, test.err)
			}
			var outcomes []string
			for _, ran := range written.Runs {
				outcomes = append(outcomes, ran.Outcome)
			}
			if !slices.Equal(outcomes, test.runs) {
				t.Fatalf("The summary lists the runs as %q, want %q.", outcomes, test.runs)
			}
			for _, ran := range written.Runs {
				checkRunSummary(t, summaryDir(t, out), ran, g3, test.ran)
			}
			md, err := os.ReadFile(filepath.Join(summaryDir(t, out), "summary.md"))
			if err != nil {
				t.Fatalf("The invocation wrote no summary.md: %v", err)
			}
			for _, want := range []string{": " + test.outcome + "\n", test.err, test.ran} {
				if !strings.Contains(string(md), want) {
					t.Errorf("summary.md is\n%s\nwant it to say %q.", md, want)
				}
			}
		})
	}
}

// checkRunSummary checks that a failed run names its directory and says why
// it failed, and that a run that did not fail names neither.
func checkRunSummary(t *testing.T, dir string, ran writtenRun, violation run.Violation, err string) {
	t.Helper()
	failed := ran.Outcome != "passed" && ran.Outcome != "not run"
	if want := fmt.Sprintf("run-%d", ran.Run); failed && ran.Dir != want {
		t.Errorf("The summary says run %d's directory is %q, want %q.", ran.Run, ran.Dir, want)
	}
	if !failed && (ran.Dir != "" || ran.Error != "" || ran.Violation != nil) {
		t.Errorf("The summary lists run %d, which %s, as %+v.", ran.Run, ran.Outcome, ran)
	}
	if failed {
		if _, statErr := os.Stat(filepath.Join(dir, ran.Dir)); statErr != nil {
			t.Errorf("Run %d's directory is not where the summary says: %v", ran.Run, statErr)
		}
	}
	switch ran.Outcome {
	case "violation":
		if ran.Violation == nil || ran.Violation.ID != violation.ID || ran.Violation.Statement != violation.Statement ||
			ran.Violation.Evidence != violation.Evidence || ran.Error != "" {
			t.Errorf("The summary lists run %d's violation as %+v, want %+v.", ran.Run, ran.Violation, violation)
		}
	case "error", "interrupted":
		if !strings.Contains(ran.Error, err) || ran.Violation != nil {
			t.Errorf("The summary says run %d failed on %q, want %q.", ran.Run, ran.Error, err)
		}
	}
}
