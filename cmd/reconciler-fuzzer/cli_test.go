package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"syscall"
	"testing"
	"time"

	"k8s.io/apimachinery/pkg/runtime/schema"

	"github.com/rosenhouse-bot-sandbox/reconciler-fuzzer/internal/invariant"
	"github.com/rosenhouse-bot-sandbox/reconciler-fuzzer/internal/observe"
	"github.com/rosenhouse-bot-sandbox/reconciler-fuzzer/internal/proxy"
	"github.com/rosenhouse-bot-sandbox/reconciler-fuzzer/internal/report"
	"github.com/rosenhouse-bot-sandbox/reconciler-fuzzer/internal/run"
	"github.com/rosenhouse-bot-sandbox/reconciler-fuzzer/internal/target"
)

const (
	toyTargetYAML       = "../../targets/toy-widget/target.yaml"
	rulesTargetYAML     = "../../internal/generate/testdata/rules/target.yaml"
	workloadsTargetYAML = "testdata/workloads/target.yaml"
)

// fakeSession executes nothing: it records what the CLI asked for and answers
// from results.
type fakeSession struct {
	results  []run.Result
	failures []error
	// fails answers every execute the results do not, which is how a shrink
	// pass is given its verdicts. It reads the directory too, because that is
	// what tells a replay of the pass from the run of the minimized sequence.
	fails func(sequence run.Sequence, dir string) *run.Violation
	// notes are what a run fails answers for notes.
	notes []string
	// applies is how many ops a run fails answers for applied. Nil applies
	// none.
	applies func(sequence run.Sequence) int
	// after runs once a sequence has executed, which is where a test expires
	// the deadline.
	after func()
	// writesNothing leaves the run directory unmade, as a run that fails
	// before it starts does.
	writesNothing bool
	// stderrAtOpen is what reconciler-fuzzer had printed on stderr when it opened
	// the session.
	stderrAtOpen string
	sequences    []run.Sequence
	checks       []run.Checker
	dirs         []string
	args         [][]string
	closed       bool
	// refused is what vet answers.
	refused error
	// unopened fails opening the session.
	unopened error
	// opening runs as reconciler-fuzzer opens the session.
	opening func()
	// closing runs as the session closes.
	closing func()
	// unclosed is what close returns.
	unclosed error
	// deadlines are when each execute's context ends.
	deadlines []time.Time
	// now is reconciler-fuzzer's clock, where it is not the real one.
	now func() time.Time
	// discarding fails discarding a passing run.
	discarding error
	// printed is what reconciler-fuzzer has printed on stdout so far.
	printed func() string
}

func (s *fakeSession) vet(*target.Target) error { return s.refused }

func (s *fakeSession) prepare(context.Context, *target.Target) error { return nil }

func (s *fakeSession) execute(ctx context.Context, t *target.Target, sequence run.Sequence, dir string, check run.Checker) (run.Result, error) {
	s.sequences = append(s.sequences, sequence)
	deadline, _ := ctx.Deadline()
	s.deadlines = append(s.deadlines, deadline)
	s.dirs = append(s.dirs, dir)
	s.args = append(s.args, t.Launch.Args)
	s.checks = append(s.checks, check)
	if !s.writesNothing {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return run.Result{}, err
		}
	}
	if s.after != nil {
		s.after()
	}
	n := len(s.sequences) - 1
	var failure error
	if n < len(s.failures) {
		failure = s.failures[n]
	}
	if n < len(s.results) {
		return s.results[n], failure
	}
	if s.fails != nil {
		result := run.Result{Violation: s.fails(sequence, dir), Notes: s.notes}
		if s.applies != nil {
			result.Timeline.Ops = make([]run.AppliedOp, s.applies(sequence))
		}
		return result, failure
	}
	return run.Result{}, failure
}

func (s *fakeSession) close() error {
	if s.closing != nil {
		s.closing()
	}
	s.closed = true
	return s.unclosed
}

// invoke runs the CLI over args with a fake session, and returns its exit code
// and what it wrote.
func invoke(t *testing.T, fake *fakeSession, args ...string) (int, string, string) {
	t.Helper()
	return invokeWith(t, fake, countingGenerator(nil), args...)
}

func invokeWith(t *testing.T, fake *fakeSession, newGenerator func(*target.Target) (Generator, error), args ...string) (int, string, string) {
	t.Helper()
	return invokeCtx(t, t.Context(), fake, newGenerator, args...)
}

// invokeCtx is invokeWith under a context the test controls, for the deadline.
func invokeCtx(t *testing.T, ctx context.Context, fake *fakeSession,
	newGenerator func(*target.Target) (Generator, error), args ...string) (int, string, string) {
	t.Helper()
	var stdout, stderr bytes.Buffer
	fake.printed = stdout.String
	c := newCLI(&stdout, &stderr)
	c.open = func(options, *target.Target) (session, error) {
		fake.stderrAtOpen = stderr.String()
		if fake.opening != nil {
			fake.opening()
		}
		if fake.unopened != nil {
			return nil, fake.unopened
		}
		return fake, nil
	}
	c.newGenerator = newGenerator
	if fake.now != nil {
		c.now = fake.now
	}
	if fake.discarding != nil {
		c.discard = func(*run.Output, int) error { return fake.discarding }
	}
	return c.main(ctx, args), stdout.String(), stderr.String()
}

// countingGenerator draws sequences of the ops given, or of one settle, and
// records every seed it was asked for.
func countingGenerator(seeds *[]int64, ops ...run.OpType) func(*target.Target) (Generator, error) {
	if len(ops) == 0 {
		ops = []run.OpType{run.OpSettle}
	}
	return func(t *target.Target) (Generator, error) {
		return drawing(func(seed int64) (run.Sequence, error) {
			if seeds != nil {
				*seeds = append(*seeds, seed)
			}
			sequence := run.Sequence{Seed: seed, Target: t.Name}
			for i, opType := range ops {
				sequence.Ops = append(sequence.Ops, run.Op{Index: i, Type: opType})
			}
			return sequence, nil
		}), nil
	}
}

// drawing is a Generator that draws with the func. Its baseline is one settle.
type drawing func(seed int64) (run.Sequence, error)

func (d drawing) Draw(seed int64) (run.Sequence, error) { return d(seed) }

func (drawing) Baseline() (run.Sequence, error) {
	return run.Sequence{Target: "toy-widget", Ops: []run.Op{{Type: run.OpSettle}}}, nil
}

func (drawing) LeftAlone() []string { return nil }

// writeSequence writes a one-op sequence for the toy target.
func writeSequence(t *testing.T, seed int64) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "sequence.json")
	sequence := run.Sequence{Seed: seed, Target: "toy-widget", Ops: []run.Op{{Type: run.OpSettle}}}
	if err := run.WriteSequence(path, sequence); err != nil {
		t.Fatalf("Writing the test sequence failed: %v", err)
	}
	return path
}

func TestVersionPrintsTheVersion(t *testing.T) {
	code, stdout, _ := invoke(t, &fakeSession{}, "version")

	if code != exitOK {
		t.Errorf("reconciler-fuzzer version exited %d, want %d.", code, exitOK)
	}
	if !strings.HasPrefix(stdout, "reconciler-fuzzer ") {
		t.Errorf("reconciler-fuzzer version printed %q.", stdout)
	}
}

func TestConfigurationErrorsExitTwo(t *testing.T) {
	sequence := writeSequence(t, 1)
	for _, test := range []struct {
		name string
		args []string
		want string
	}{
		{name: "no command", args: nil, want: "run"},
		{name: "an unknown command", args: []string{"frobnicate"}, want: "frobnicate"},
		{name: "an unknown flag", args: []string{"run", "--target", toyTargetYAML, "--loud"}, want: "loud"},
		{name: "no target", args: []string{"run", sequence}, want: "--target"},
		{name: "a target that does not load", args: []string{"replay", "--target", "absent.yaml", sequence}, want: "absent.yaml"},
		{name: "a sequence that does not load", args: []string{"replay", "--target", toyTargetYAML, "absent.json"}, want: "absent.json"},
		{name: "replay without a sequence", args: []string{"replay", "--target", toyTargetYAML}, want: "sequence"},
		{name: "replay with two sequences", args: []string{"replay", "--target", toyTargetYAML, sequence, sequence}, want: "one sequence"},
		{name: "--runs with a named sequence", args: []string{"run", "--target", toyTargetYAML, "--runs", "5", sequence}, want: "--runs"},
		{name: "--no-baseline with a named sequence", args: []string{"run", "--target", toyTargetYAML, "--no-baseline", sequence}, want: "--no-baseline"},
		{name: "no runs at all", args: []string{"run", "--target", toyTargetYAML, "--runs", "0"}, want: "--runs"},
		{name: "a deadline of no time", args: []string{"replay", "--target", toyTargetYAML, "--deadline", "0s", sequence},
			want: "--deadline is 0s, and an invocation needs time to run: leave the flag out, and reconciler-fuzzer derives one\n"},
		{name: "a deadline in the past", args: []string{"matrix", "--target", toyTargetYAML, "--sequences", ".", "--deadline", "-1m"}, want: "--deadline is -1m0s"},
	} {
		t.Run(test.name, func(t *testing.T) {
			code, _, stderr := invoke(t, &fakeSession{}, test.args...)

			if code != exitError {
				t.Errorf("reconciler-fuzzer %v exited %d, want %d.", test.args, code, exitError)
			}
			if !strings.Contains(stderr, test.want) {
				t.Errorf("reconciler-fuzzer %v reported %q, want it to name %q.", test.args, stderr, test.want)
			}
		})
	}
}

func TestRunGeneratesOneSequencePerRunThenTheBaseline(t *testing.T) {
	session := &fakeSession{}
	var seeds []int64

	code, stdout, stderr := invokeWith(t, session, countingGenerator(&seeds),
		"run", "--target", toyTargetYAML, "--out", t.TempDir(), "--runs", "3", "--seed", "42")

	if code != exitOK {
		t.Fatalf("reconciler-fuzzer run exited %d: %s", code, stderr)
	}
	if want := []int64{42, 43, 44}; !slices.Equal(seeds, want) {
		t.Errorf("The generator drew the seeds %v, want %v: one run per seed, from the one given.", seeds, want)
	}
	baseline, _ := drawing(nil).Baseline()
	if len(session.sequences) != 4 || !reflect.DeepEqual(session.sequences[3], baseline) {
		t.Errorf("The session executed %v, want one sequence per run and then the baseline.", session.sequences)
	}
	want := "run 1: seed 42, generated\nrun 2: seed 43, generated\nrun 3: seed 44, generated\nrun 4: baseline\nevery run passed.\n"
	if !strings.HasSuffix(stdout, want) {
		t.Errorf("reconciler-fuzzer run printed %q, want it to end %q.", stdout, want)
	}
}

func TestNoBaselineRunsOnlyTheDrawnSequences(t *testing.T) {
	session := &fakeSession{}

	code, stdout, stderr := invoke(t, session, "run", "--target", toyTargetYAML, "--out", t.TempDir(), "--runs", "2", "--no-baseline")

	if code != exitOK {
		t.Fatalf("reconciler-fuzzer run exited %d: %s", code, stderr)
	}
	if len(session.sequences) != 2 || strings.Contains(stdout, "baseline") {
		t.Errorf("The session executed %d sequences and printed %q, want the two drawn alone.", len(session.sequences), stdout)
	}
}

// DESIGN.md §11: seeds are always printed, so that every failure is
// reproducible from the seed and the sequence.
func TestRunWithoutASeedPicksOneAndPrintsIt(t *testing.T) {
	session := &fakeSession{}
	var seeds []int64

	code, stdout, stderr := invokeWith(t, session, countingGenerator(&seeds),
		"run", "--target", toyTargetYAML, "--out", t.TempDir(), "--runs", "1")

	if code != exitOK {
		t.Fatalf("reconciler-fuzzer run exited %d: %s", code, stderr)
	}
	if len(seeds) != 1 || seeds[0] == 0 {
		t.Fatalf("The generator drew the seeds %v, want one the CLI picked.", seeds)
	}
	if !strings.Contains(stdout, fmt.Sprint(seeds[0])) {
		t.Errorf("reconciler-fuzzer run printed %q, want the seed %d it picked.", stdout, seeds[0])
	}
}

func TestANamedSequenceTakesPrecedenceOverGeneration(t *testing.T) {
	session := &fakeSession{}
	var seeds []int64

	code, _, stderr := invokeWith(t, session, countingGenerator(&seeds),
		"run", "--target", toyTargetYAML, "--out", t.TempDir(), writeSequence(t, 8675309))

	if code != exitOK {
		t.Fatalf("reconciler-fuzzer run exited %d: %s", code, stderr)
	}
	if len(seeds) != 0 {
		t.Errorf("The generator drew %v, want nothing: the caller named the sequence.", seeds)
	}
	if len(session.sequences) != 1 || session.sequences[0].Seed != 8675309 {
		t.Errorf("The session executed %v, want the named sequence.", session.sequences)
	}
}

// withBaseline is a Generator whose baseline is the sequence given.
type withBaseline struct {
	drawing
	baseline run.Sequence
}

func (g withBaseline) Baseline() (run.Sequence, error) { return g.baseline, nil }

// failingBaseline is a Generator whose baseline fails with the error given.
type failingBaseline struct {
	drawing
	err error
}

func (g failingBaseline) Baseline() (run.Sequence, error) { return run.Sequence{}, g.err }

func TestAFailingBaselineIsMinimizedAndReportedAsTheBaseline(t *testing.T) {
	g4 := run.Violation{ID: "G4", Statement: "the target converges"}
	session := &fakeSession{fails: func(candidate run.Sequence, _ string) *run.Violation {
		if slices.ContainsFunc(candidate.Ops, func(op run.Op) bool { return op.Type == run.OpRestart }) {
			return &g4
		}
		return nil
	}}
	baseline := run.Sequence{Target: "toy-widget", Ops: []run.Op{{Type: run.OpSettle}, {Index: 1, Type: run.OpRestart}, {Index: 2, Type: run.OpSettle}}}
	generate := func(t *target.Target) (Generator, error) {
		drawn, _ := countingGenerator(nil)(t)
		return withBaseline{drawn.(drawing), baseline}, nil
	}

	code, stdout, _ := invokeWith(t, session, generate, "run", "--target", toyTargetYAML, "--out", t.TempDir(), "--runs", "1")

	if code != exitViolation {
		t.Fatalf("reconciler-fuzzer run exited %d, want %d.", code, exitViolation)
	}
	dir := session.dirs[1]
	written, err := run.ReadSequence(filepath.Join(dir, sequenceFile))
	if err != nil {
		t.Fatalf("The run directory holds no minimized sequence: %v", err)
	}
	if want := []run.OpType{run.OpRestart, run.OpSettle}; !slices.Equal(opTypesOf(written), want) {
		t.Errorf("The run directory holds the sequence %v, want the minimized %v.", written.Ops, want)
	}
	if !strings.Contains(stdout, "run 2: baseline\n") {
		t.Errorf("reconciler-fuzzer run printed %q, want run 2 named the baseline.", stdout)
	}
	md, err := os.ReadFile(filepath.Join(dir, "report.md"))
	if err != nil || !strings.Contains(string(md), "with the baseline") {
		t.Errorf("report.md is\n%s\nwant it to name the baseline: %v", md, err)
	}
}

func opTypesOf(s run.Sequence) []run.OpType {
	var types []run.OpType
	for _, op := range s.Ops {
		types = append(types, op.Type)
	}
	return types
}

func TestRunShrinksTheFailingSequence(t *testing.T) {
	violation := run.Violation{ID: "G4", Statement: "the target converges", Evidence: "the settle wait after op 2 expired"}
	minimized := run.Violation{ID: "G4", Statement: "the target converges", Evidence: "the settle wait after op 0 expired"}
	session := &fakeSession{
		results: []run.Result{{Violation: &violation}},
		// Only the restart is needed to fail, so the pass removes every other
		// op but the settle that a sequence cannot end without (DESIGN.md §7).
		fails: func(candidate run.Sequence, _ string) *run.Violation {
			if slices.ContainsFunc(candidate.Ops, func(op run.Op) bool { return op.Type == run.OpRestart }) {
				return &minimized
			}
			return nil
		},
	}
	generate := countingGenerator(nil, run.OpSettle, run.OpRestart, run.OpSettle)

	code, stdout, _ := invokeWith(t, session, generate,
		"run", "--target", toyTargetYAML, "--out", t.TempDir(), "--runs", "1", "--seed", "42")

	if code != exitViolation {
		t.Fatalf("reconciler-fuzzer run exited %d, want %d.", code, exitViolation)
	}
	written, err := run.ReadSequence(filepath.Join(session.dirs[0], sequenceFile))
	if err != nil {
		t.Fatalf("The run directory holds no minimized sequence: %v", err)
	}
	if want := []run.OpType{run.OpRestart, run.OpSettle}; !slices.Equal(opTypesOf(written), want) {
		t.Errorf("The run directory holds the sequence %v, want the minimized %v.", written.Ops, want)
	}
	if !strings.Contains(stdout, "G4") || !strings.Contains(stdout, "2 ops") {
		t.Errorf("reconciler-fuzzer run printed %q, want the violation and what it shrank the sequence to.", stdout)
	}
	if !strings.Contains(stdout, minimized.Evidence) || strings.Contains(stdout, violation.Evidence) {
		t.Errorf("reconciler-fuzzer run printed %q, want the evidence of the run the directory holds.", stdout)
	}
	last := len(session.sequences) - 1
	if session.dirs[last] != session.dirs[0] || len(session.sequences[last].Ops) != 2 {
		t.Errorf("The last run executed %d ops in %s, want the minimized sequence in the run directory: its evidence is what the run directory reports.",
			len(session.sequences[last].Ops), session.dirs[last])
	}
	if _, err := os.Stat(filepath.Join(session.dirs[0], shrinkDir)); !os.IsNotExist(err) {
		t.Errorf("The run directory keeps the shrink pass's replays: %v", err)
	}
}

// Minimizing can take minutes, so a reader learns of the failure before the
// first replay. A single op has nothing to minimize.
func TestADrawnRunSaysItFailedBeforeItMinimizes(t *testing.T) {
	for _, test := range []struct {
		name string
		ops  []run.OpType
		says string
	}{
		{"two ops", []run.OpType{run.OpSettle, run.OpSettle}, "run 1: G3 failed, and minimizing its 2 ops can take minutes.\n"},
		{"one op", []run.OpType{run.OpSettle}, ""},
	} {
		t.Run(test.name, func(t *testing.T) {
			violation := run.Violation{ID: "G3", Statement: "the CR's children go with it"}
			session := &fakeSession{fails: func(run.Sequence, string) *run.Violation { return &violation }}
			var atFirstReplay string
			session.after = func() {
				if len(session.sequences) == 2 {
					atFirstReplay = session.printed()
				}
			}

			code, stdout, stderr := invokeWith(t, session, countingGenerator(nil, test.ops...),
				"run", "--target", toyTargetYAML, "--out", t.TempDir(), "--runs", "1", "--seed", "42")

			if code != exitViolation {
				t.Fatalf("reconciler-fuzzer run exited %d: %s", code, stderr)
			}
			if test.says == "" {
				if strings.Contains(stdout, "minimizing its") {
					t.Errorf("reconciler-fuzzer run printed\n%s\nand a single op has nothing to minimize.", stdout)
				}
				return
			}
			if !strings.HasSuffix(atFirstReplay, "run 1: seed 42, generated\n"+test.says) {
				t.Errorf("reconciler-fuzzer run had printed\n%s\nby the first replay, want it to end with %q.", atFirstReplay, test.says)
			}
		})
	}
}

// A report says the sequence it carries is the one reconciler-fuzzer drew,
// because §5.7 says a report carries the minimized sequence and this one is not
// it (D31).
func TestTheReportSaysWhenTheDeadlineLeftTheSequenceUnminimized(t *testing.T) {
	ctx, expire := context.WithCancel(t.Context())
	violation := run.Violation{ID: "G4", Statement: "the target converges"}
	session := &fakeSession{fails: func(run.Sequence, string) *run.Violation { return &violation }}
	// The deadline passes before the first candidate is replayed, so the pass
	// never finds anything smaller.
	session.after = expire
	generate := countingGenerator(nil, run.OpSettle, run.OpSettle, run.OpSettle)

	code, _, stderr := invokeCtx(t, ctx, session, generate,
		"run", "--target", toyTargetYAML, "--out", t.TempDir(), "--runs", "1", "--seed", "42")

	if code != exitViolation {
		t.Fatalf("reconciler-fuzzer run exited %d, want %d: %s", code, exitViolation, stderr)
	}
	report, err := os.ReadFile(filepath.Join(session.dirs[0], report.MarkdownFile))
	if err != nil {
		t.Fatal(err)
	}
	if want := "the deadline ended minimization"; !strings.Contains(string(report), want) {
		t.Errorf("The report is\n%s\nwant it to say %q.", report, want)
	}
}

// DESIGN.md §11: the run directory reports the sequence its recordings are of.
// A pass the deadline cut short never ran the smaller sequence, so what the
// directory holds is still the run of the sequence reconciler-fuzzer drew.
func TestADeadlineDuringTheShrinkPassLeavesTheDrawnSequence(t *testing.T) {
	ctx, expire := context.WithCancel(t.Context())
	violation := run.Violation{ID: "G4", Statement: "the target converges"}
	session := &fakeSession{fails: func(run.Sequence, string) *run.Violation { return &violation }}
	// The run itself, then one candidate the pass accepts, then the deadline.
	session.after = func() {
		if len(session.sequences) == 2 {
			expire()
		}
	}
	generate := countingGenerator(nil, run.OpSettle, run.OpSettle, run.OpSettle)

	code, stdout, stderr := invokeCtx(t, ctx, session, generate,
		"run", "--target", toyTargetYAML, "--out", t.TempDir(), "--runs", "1", "--seed", "42")

	if code != exitViolation {
		t.Fatalf("reconciler-fuzzer run exited %d, want %d.", code, exitViolation)
	}
	written, err := run.ReadSequence(filepath.Join(session.dirs[0], sequenceFile))
	if err != nil {
		t.Fatalf("The run directory holds no sequence: %v", err)
	}
	if len(written.Ops) != len(session.sequences[0].Ops) {
		t.Errorf("The run directory holds %d ops, want the %d of the sequence its recordings are of.",
			len(written.Ops), len(session.sequences[0].Ops))
	}
	if !strings.Contains(stderr, "deadline") {
		t.Errorf("reconciler-fuzzer run printed %q on stderr, want the deadline that ended the pass.", stderr)
	}
	if !strings.Contains(stdout, ops(written)) {
		t.Errorf("reconciler-fuzzer run printed %q, want the %s the directory holds.", stdout, ops(written))
	}
	// §11: the shrinker reports the smallest failing sequence it found, which
	// no recording here is of, so it keeps its own file.
	smaller, err := run.ReadSequence(filepath.Join(session.dirs[0], shrunkFile))
	if err != nil {
		t.Fatalf("The pass found a smaller sequence and left no %s: %v", shrunkFile, err)
	}
	if len(smaller.Ops) >= len(written.Ops) {
		t.Errorf("%s holds %d ops, want fewer than the drawn sequence's %d.",
			shrunkFile, len(smaller.Ops), len(written.Ops))
	}
}

// DESIGN.md §11: each failing run writes report.json and report.md beside the
// recordings they describe (§5.7).
func TestAFailingRunWritesItsReport(t *testing.T) {
	violation := run.Violation{
		ID: "G4", Statement: "the target converges", Evidence: "the settle wait expired",
		At:       time.Date(2026, 9, 21, 5, 59, 8, 980624165, time.UTC),
		Requests: []proxy.Request{{Verb: "get", Path: "/api/v1/namespaces/ns/configmaps/w-0", Status: 404}},
		Versions: []observe.Version{{Key: observe.Key{
			GVK:  schema.GroupVersionKind{Version: "v1", Kind: "ConfigMap"},
			Name: "left-behind",
		}, ResourceVersion: "12"}},
	}
	note := "G3 is not evaluated for the deletion of widget"
	session := &fakeSession{results: []run.Result{{Violation: &violation, Notes: []string{note}}}}

	code, stdout, stderr := invokeWith(t, session, countingGenerator(nil),
		"run", "--target", toyTargetYAML, "--out", t.TempDir(), "--runs", "1", "--seed", "42",
		"--kubeconfig", "kind.kubeconfig", "--launch-arg", "--bug=7")

	if code != exitViolation {
		t.Fatalf("reconciler-fuzzer run exited %d: %s", code, stderr)
	}
	if !strings.Contains(stdout, "at 2026-09-21T05:59:08.980624165Z") {
		t.Errorf("reconciler-fuzzer printed\n%s\nwant the instant the violation carries.", stdout)
	}
	for _, name := range []string{report.JSONFile, report.MarkdownFile} {
		if _, err := os.Stat(filepath.Join(session.dirs[0], name)); err != nil {
			t.Fatalf("The run directory holds no %s: %v", name, err)
		}
	}
	written, err := os.ReadFile(filepath.Join(session.dirs[0], report.MarkdownFile))
	if err != nil {
		t.Fatal(err)
	}
	want := []string{violation.ID, violation.Statement, note}
	// The command has to carry what selected this run, or it replays something
	// else (DESIGN.md §5.7).
	want = append(want, "reconciler-fuzzer replay --target "+toyTargetYAML+" --kubeconfig kind.kubeconfig --launch-arg --bug=7 "+
		filepath.Join(session.dirs[0], sequenceFile))
	// §5.7 also asks for what ran, the seed, the instant it judged, and the
	// evidence the check named.
	want = append(want, version(), "seed 42", "2026-09-21T05:59:08.980624165Z",
		violation.Requests[0].Path, violation.Versions[0].Name)
	for _, want := range want {
		if !strings.Contains(string(written), want) {
			t.Errorf("The report is\n%s\nwant it to carry %q.", written, want)
		}
	}
}

// A run ends at its first violation, so its report says how far it got. The
// ops after it never ran, and a reader who counts them all has been told the
// wrong thing (DESIGN.md §5.7).
func TestTheReportSaysHowFarTheRunGot(t *testing.T) {
	violation := run.Violation{ID: "G4", Statement: "the target converges"}
	session := &fakeSession{results: []run.Result{{
		Violation: &violation,
		Timeline:  run.Timeline{Ops: []run.AppliedOp{{Op: run.Op{Index: 0, Type: run.OpCreate}}}},
	}}}
	generate := countingGenerator(nil, run.OpSettle, run.OpSettle, run.OpSettle)

	code, _, stderr := invokeWith(t, session, generate,
		"run", "--target", toyTargetYAML, "--out", t.TempDir(), "--runs", "1", "--seed", "42")

	if code != exitViolation {
		t.Fatalf("reconciler-fuzzer run exited %d: %s", code, stderr)
	}
	written, err := os.ReadFile(filepath.Join(session.dirs[0], report.MarkdownFile))
	if err != nil {
		t.Fatal(err)
	}
	if want := "applied 1 of the sequence's 3 ops"; !strings.Contains(string(written), want) {
		t.Errorf("The report is\n%s\nwant it to say %q.", written, want)
	}
}

// A sequence the caller named is never minimized, and it still gets a report.
// This is the path `reconciler-fuzzer replay` takes, which is how M6's own
// fault sequence is run.
func TestReplayWritesAReportForTheSequenceItWasGiven(t *testing.T) {
	violation := run.Violation{ID: "G1", Statement: "the target falls quiet"}
	session := &fakeSession{results: []run.Result{{Violation: &violation}}}
	path := writeSequence(t, 8675309)

	code, _, stderr := invoke(t, session, "replay", "--target", toyTargetYAML, "--out", t.TempDir(), path)

	if code != exitViolation {
		t.Fatalf("reconciler-fuzzer replay exited %d: %s", code, stderr)
	}
	written, err := os.ReadFile(filepath.Join(session.dirs[0], report.MarkdownFile))
	if err != nil {
		t.Fatalf("The run directory holds no report: %v", err)
	}
	// The command replays the caller's file, which is a better thing to run
	// than a copy reconciler-fuzzer made of it.
	for _, want := range []string{violation.ID, "reconciler-fuzzer replay --target " + toyTargetYAML + " " + path} {
		if !strings.Contains(string(written), want) {
			t.Errorf("The report is\n%s\nwant it to carry %q.", written, want)
		}
	}
}

// §10 M6's acceptance says the report names the MINIMIZED sequence. Nothing
// else ties the two together: a report carrying the sequence reconciler-fuzzer
// drew would send a reader to reproduce the long way round.
func TestTheReportCarriesTheMinimizedSequence(t *testing.T) {
	violation := run.Violation{ID: "G4", Statement: "the target converges"}
	session := &fakeSession{fails: func(candidate run.Sequence, _ string) *run.Violation {
		// Only the restart is needed, so the pass cuts the rest away.
		if slices.ContainsFunc(candidate.Ops, func(op run.Op) bool { return op.Type == run.OpRestart }) {
			return &violation
		}
		return nil
	}}
	generate := countingGenerator(nil, run.OpSettle, run.OpRestart, run.OpSettle)

	code, _, stderr := invokeWith(t, session, generate,
		"run", "--target", toyTargetYAML, "--out", t.TempDir(), "--runs", "1", "--seed", "42")

	if code != exitViolation {
		t.Fatalf("reconciler-fuzzer run exited %d: %s", code, stderr)
	}
	reported := reportedSequence(t, session.dirs[0])
	if want := []run.OpType{run.OpRestart, run.OpSettle}; !slices.Equal(opTypesOf(reported), want) {
		t.Errorf("The report carries %v, want the minimized %v.", opTypesOf(reported), want)
	}
	written, err := run.ReadSequence(filepath.Join(session.dirs[0], sequenceFile))
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(opTypesOf(reported), opTypesOf(written)) {
		t.Errorf("The report carries %v and the directory holds %v; they are one sequence.",
			opTypesOf(reported), opTypesOf(written))
	}
}

// The notes printed above a violation are of the run it came from, which is
// the minimized sequence's once that reproduced.
func TestTheNotesPrintedAreOfTheRunReported(t *testing.T) {
	violation := run.Violation{ID: "G4", Statement: "the settle wait after op 0 (restart) expired"}
	drawn, minimized := "the target exited during op 1 (restart)", "the target exited during op 0 (restart)"
	session := &fakeSession{
		results: []run.Result{{Violation: &violation, Notes: []string{drawn}}},
		fails: func(candidate run.Sequence, _ string) *run.Violation {
			if slices.ContainsFunc(candidate.Ops, func(op run.Op) bool { return op.Type == run.OpRestart }) {
				return &violation
			}
			return nil
		},
		notes: []string{minimized},
	}
	generate := countingGenerator(nil, run.OpSettle, run.OpRestart, run.OpSettle)

	code, stdout, stderr := invokeWith(t, session, generate,
		"run", "--target", toyTargetYAML, "--out", t.TempDir(), "--runs", "1", "--seed", "42")

	if code != exitViolation {
		t.Fatalf("reconciler-fuzzer run exited %d: %s", code, stderr)
	}
	if strings.Contains(stdout, drawn) || !strings.Contains(stdout, "run 1: "+minimized+"\nrun 1: G4") {
		t.Errorf("reconciler-fuzzer run printed\n%s\nwant the minimized run's note above its violation, and not the drawn run's.", stdout)
	}
}

// reportedSequence is the sequence report.json embedded.
func reportedSequence(t *testing.T, dir string) run.Sequence {
	t.Helper()
	body, err := os.ReadFile(filepath.Join(dir, report.JSONFile))
	if err != nil {
		t.Fatalf("The run directory holds no report: %v", err)
	}
	var written struct {
		Sequence json.RawMessage `json:"sequence"`
	}
	if err := json.Unmarshal(body, &written); err != nil {
		t.Fatalf("The report does not parse: %v", err)
	}
	sequence, err := run.UnmarshalSequence(written.Sequence)
	if err != nil {
		t.Fatalf("The report's sequence does not parse: %v", err)
	}
	return sequence
}

// A report is the artefact a reader trusts, so it says when the recordings
// beside it are of a run that found nothing (D31, D35).
func TestTheReportSaysWhenTheMinimizedSequenceDidNotReproduce(t *testing.T) {
	violation := run.Violation{ID: "G4", Statement: "the target converges"}
	session := &fakeSession{
		results: []run.Result{{Violation: &violation}},
		// Only the first run fails, so the shrink pass minimizes and the run of
		// what it found passes.
		fails: func(candidate run.Sequence, dir string) *run.Violation {
			if strings.Contains(dir, shrinkDir) && len(candidate.Ops) < 3 {
				return &violation
			}
			return nil
		},
	}
	generate := countingGenerator(nil, run.OpSettle, run.OpSettle, run.OpSettle)

	code, _, _ := invokeWith(t, session, generate,
		"run", "--target", toyTargetYAML, "--out", t.TempDir(), "--runs", "1", "--seed", "42")

	if code != exitViolation {
		t.Fatalf("reconciler-fuzzer run exited %d, want %d.", code, exitViolation)
	}
	written, err := os.ReadFile(filepath.Join(session.dirs[0], report.MarkdownFile))
	if err != nil {
		t.Fatalf("The run directory holds no report: %v", err)
	}
	if !strings.Contains(string(written), "passed when it ran again") {
		t.Errorf("The report is\n%s\nwant it to say the recordings are of a run that found nothing.", written)
	}
	// The counts describe the sequence the directory holds a run of, or they
	// describe two sequences and one of them is a fiction (DESIGN.md §5.7).
	encoded, err := os.ReadFile(filepath.Join(session.dirs[0], report.JSONFile))
	if err != nil {
		t.Fatal(err)
	}
	var carried struct{ Applied, Ops int }
	if err := json.Unmarshal(encoded, &carried); err != nil {
		t.Fatal(err)
	}
	if carried.Applied > carried.Ops {
		t.Errorf("The report says the run applied %d of %d ops.", carried.Applied, carried.Ops)
	}
}

// The shrink pass weakens a fault as well as removing ops, so a sequence it
// changed without shortening is still one the report has to carry.
func TestASequenceWeakenedButNotShortenedCountsAsSimpler(t *testing.T) {
	failing := run.Sequence{Seed: 1, Target: "toy-widget", Ops: []run.Op{
		{Index: 0, Type: run.OpFault, Fault: &run.Fault{Action: run.Action{Error: 500}, Until: run.Trigger{Count: 8}}},
		{Index: 1, Type: run.OpSettle},
	}}
	weakened := failing
	weakened.Ops = slices.Clone(failing.Ops)
	weakened.Ops[0].Fault = &run.Fault{Action: run.Action{Error: 500}, Until: run.Trigger{Count: 4}}

	if !simpler(weakened, failing) {
		t.Error("A fault weakened from a count of 8 to 4 reads as the sequence reconciler-fuzzer drew.")
	}
	if simpler(failing, failing) {
		t.Error("The sequence reconciler-fuzzer drew reads as simpler than itself.")
	}
}

// The run directory holds the minimized sequence's own run, so a run of it
// that reproduces nothing is worth saying out loud.
func TestAMinimizedSequenceThatPassesOnItsOwnRunIsReported(t *testing.T) {
	violation := run.Violation{ID: "G4"}
	session := &fakeSession{
		results: []run.Result{{Violation: &violation}},
		fails: func(candidate run.Sequence, dir string) *run.Violation {
			if filepath.Base(dir) == shrinkDir && len(candidate.Ops) > 0 {
				return &violation
			}
			return nil
		},
	}
	generate := countingGenerator(nil, run.OpSettle, run.OpRestart)

	code, stdout, stderr := invokeWith(t, session, generate,
		"run", "--target", toyTargetYAML, "--out", t.TempDir(), "--runs", "1", "--seed", "42")

	if code != exitViolation {
		t.Fatalf("reconciler-fuzzer run exited %d, want %d: the run found a violation.", code, exitViolation)
	}
	if !strings.Contains(stderr, "passed when it ran again") {
		t.Errorf("reconciler-fuzzer run reported %q, want the minimized sequence's own run named.", stderr)
	}
	if !strings.Contains(stdout, "G4") {
		t.Errorf("reconciler-fuzzer run printed %q, want the violation it found.", stdout)
	}
}

// A run the deadline cut short says so in the flag's own terms, because Go's
// phrase for it names nothing the caller set (DESIGN.md §11).
func TestADeadlineThatEndsARunNamesTheFlag(t *testing.T) {
	for _, failure := range []struct {
		name     string
		deadline string
		err      error
		names    bool
	}{
		{"the deadline", "1ns", fmt.Errorf("op 0 (create): %w", context.DeadlineExceeded), true},
		{"a target that stopped", "1ns", errors.New("op 0 (create): the target is no longer running"), false},
		// The teardown runs on a budget of its own, which no flag names
		// (DESIGN.md §5.5).
		{"a deadline the flag did not set", "5s", fmt.Errorf("stopping: %w", context.DeadlineExceeded), false},
	} {
		t.Run(failure.name, func(t *testing.T) {
			session := &fakeSession{failures: []error{failure.err}}

			code, _, stderr := invokeWith(t, session, countingGenerator(nil),
				"run", "--target", toyTargetYAML, "--out", t.TempDir(), "--runs", "1", "--deadline", failure.deadline)

			if code != exitError {
				t.Fatalf("reconciler-fuzzer run exited %d, want %d.", code, exitError)
			}
			if named := strings.Contains(stderr, "the --deadline of "+failure.deadline); named != failure.names {
				t.Errorf("reconciler-fuzzer run printed %q, and what ended the run was %s.", stderr, failure.name)
			}
		})
	}
}

// DESIGN.md §11: the deadline is what the invocation has. An invocation it
// ended early tested less than asked, so it does not pass.
func TestADeadlineThatStopsTheInvocationBetweenRunsExitsTwo(t *testing.T) {
	session := &fakeSession{}

	code, stdout, stderr := invokeWith(t, session, countingGenerator(nil),
		"run", "--target", toyTargetYAML, "--out", t.TempDir(), "--runs", "3", "--no-baseline", "--deadline", "1ns")

	if code != exitError {
		t.Errorf("reconciler-fuzzer run exited %d, want %d.", code, exitError)
	}
	if len(session.sequences) != 1 {
		t.Errorf("The session executed %d sequences, want the first alone: the deadline had passed.", len(session.sequences))
	}
	if want := "the --deadline of 1ns stopped the invocation after 1 of 3 runs"; !strings.Contains(stderr, want) {
		t.Errorf("reconciler-fuzzer run printed %q on stderr, want %q.", stderr, want)
	}
	if strings.Contains(stdout, "every run passed") {
		t.Errorf("reconciler-fuzzer run printed %q, but not every run ran.", stdout)
	}
}

// targetWithoutTimeouts is the toy target declaring no timeouts, so that it
// takes the defaults.
func targetWithoutTimeouts(t *testing.T) string {
	t.Helper()
	toy, err := filepath.Abs("../../targets/toy-widget")
	if err != nil {
		t.Fatal(err)
	}
	declared := fmt.Sprintf(`name: toy-widget
crds:
  - %s/crds/
primary: toy.reconciler-fuzzer/v1/Widget
sample: %s/widget.yaml
manages:
  - v1/ConfigMap
launch:
  binary: bin/toy-widget
`, toy, toy)
	path := filepath.Join(t.TempDir(), "target.yaml")
	if err := os.WriteFile(path, []byte(declared), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

// growingGenerator draws a create and as many settles as the seed, so that no
// two runs take the same time.
func growingGenerator(t *target.Target) (Generator, error) {
	return drawing(func(seed int64) (run.Sequence, error) {
		sequence := run.Sequence{Seed: seed, Target: t.Name, Ops: []run.Op{{Type: run.OpCreate}}}
		for i := range int(seed) {
			sequence.Ops = append(sequence.Ops, run.Op{Index: i + 1, Type: run.OpSettle})
		}
		return sequence, nil
	}), nil
}

// Without --deadline, the runs get what they can take, and a sequence
// reconciler-fuzzer drew gets time to be minimized.
func TestWithoutADeadlineTheRunsGetWhatTheyCanTake(t *testing.T) {
	targetFile := targetWithoutTimeouts(t)
	loaded, err := target.Load(targetFile)
	if err != nil {
		t.Fatal(err)
	}
	sequence := writeSequence(t, 1)
	longer := filepath.Join(t.TempDir(), "longer.json")
	settles := []run.Op{{Type: run.OpSettle}, {Index: 1, Type: run.OpSettle}}
	if err := run.WriteSequence(longer, run.Sequence{Seed: 2, Target: "toy-widget", Ops: settles}); err != nil {
		t.Fatal(err)
	}
	named := "the deadline is %[1]s: this run can take that long at the target's timeouts. --deadline sets another.\n"
	for _, test := range []struct {
		name       string
		args       []string
		minimizing time.Duration
		says       string
	}{
		{"drawn runs and the baseline", []string{"run", "--runs", "10", "--seed", "1"}, 4 * time.Minute,
			"the deadline is %[1]s: these 11 runs can take %[2]s at the target's timeouts, and minimizing a failure gets the rest, at least 4m0s. --deadline sets another.\n"},
		{"a named sequence", []string{"run", sequence}, 0, named},
		{"named sequences", []string{"run", sequence, longer}, 0,
			"the deadline is %[1]s: these 2 runs can take that long at the target's timeouts. --deadline sets another.\n"},
		{"a replay", []string{"replay", sequence}, 0, named},
	} {
		t.Run(test.name, func(t *testing.T) {
			session := &fakeSession{}
			args := slices.Concat(test.args[:1], []string{"--target", targetFile, "--out", t.TempDir()}, test.args[1:])
			before := time.Now()

			code, stdout, stderr := invokeWith(t, session, growingGenerator, args...)

			after := time.Now()
			if code != exitOK {
				t.Fatalf("reconciler-fuzzer %v exited %d: %s", args, code, stderr)
			}
			var runs time.Duration
			for _, sequence := range session.sequences {
				runs += run.Bound(loaded, sequence)
			}
			want := runs + test.minimizing
			if deadline := session.deadlines[0]; deadline.Before(before.Add(want)) || deadline.After(after.Add(want)) {
				t.Errorf("The runs had %v, want %v.", deadline.Sub(before), want)
			}
			if says := fmt.Sprintf(test.says, want, runs); !strings.Contains(stdout, says) {
				t.Errorf("reconciler-fuzzer %v printed %q, want %q.", args, stdout, says)
			}
		})
	}
}

// Runs whose waits no duration can count get the longest deadline, not one
// that has already passed.
func TestADeadlineTooLongToCountIsTheLongest(t *testing.T) {
	generate := func(t *target.Target) (Generator, error) {
		return drawing(func(seed int64) (run.Sequence, error) {
			ops := []run.Op{{Type: run.OpCreate}}
			for range 64 {
				ops = append(ops, run.Op{Type: run.OpFault, Fault: &run.Fault{Action: run.Action{Error: 500}, Until: run.Trigger{Count: 1}}})
			}
			ops = append(ops, run.Op{Type: run.OpSettle})
			for i := range ops {
				ops[i].Index = i
			}
			return run.Sequence{Seed: seed, Target: t.Name, Ops: ops}, nil
		}), nil
	}
	session := &fakeSession{}

	code, stdout, stderr := invokeWith(t, session, generate, "run", "--target", toyTargetYAML, "--out", t.TempDir(), "--runs", "2", "--no-baseline")

	if code != exitOK || len(session.sequences) != 2 {
		t.Fatalf("reconciler-fuzzer run exited %d after %d runs: %s", code, len(session.sequences), stderr)
	}
	if longest := time.Duration(math.MaxInt64).String(); !strings.Contains(stdout, "the deadline is "+longest) {
		t.Errorf("reconciler-fuzzer run printed %q, want the deadline of %s.", stdout, longest)
	}
}

func TestAGivenDeadlineIsUsedAsGiven(t *testing.T) {
	session := &fakeSession{}
	before := time.Now()

	code, stdout, stderr := invoke(t, session, "run", "--target", toyTargetYAML, "--out", t.TempDir(), "--runs", "2", "--deadline", "1m")

	after := time.Now()
	if code != exitOK {
		t.Fatalf("reconciler-fuzzer run exited %d: %s", code, stderr)
	}
	if deadline := session.deadlines[0]; deadline.Before(before.Add(time.Minute)) || deadline.After(after.Add(time.Minute)) {
		t.Errorf("The runs had %v, want the 1m given.", deadline.Sub(before))
	}
	if strings.Contains(stdout, "the deadline is") {
		t.Errorf("reconciler-fuzzer run printed %q, and the deadline was given.", stdout)
	}
}

// No flag set a derived deadline, so what it stopped says reconciler-fuzzer
// derived it.
func TestADerivedDeadlineSaysSoWhereItStops(t *testing.T) {
	for _, test := range []struct {
		name    string
		failure error
		want    string
	}{
		{"a run", fmt.Errorf("op 0 (settle): %w", context.DeadlineExceeded), "run 1: the derived deadline of "},
		{"the invocation", nil, "the derived deadline of "},
	} {
		t.Run(test.name, func(t *testing.T) {
			ctx, cancel := context.WithDeadline(t.Context(), time.Now())
			defer cancel()
			session := &fakeSession{failures: []error{test.failure}}

			code, _, stderr := invokeCtx(t, ctx, session, countingGenerator(nil),
				"run", "--target", toyTargetYAML, "--out", t.TempDir(), "--runs", "3", "--seed", "1")

			if code != exitError {
				t.Errorf("reconciler-fuzzer run exited %d, want %d.", code, exitError)
			}
			if !strings.Contains(stderr, test.want) || strings.Contains(stderr, "--deadline") {
				t.Errorf("reconciler-fuzzer run printed %q on stderr, want %q.", stderr, test.want)
			}
		})
	}
}

func TestAnInterruptBeforeTheFirstRunStartsNone(t *testing.T) {
	ctx, cancel := context.WithCancelCause(t.Context())
	cancel(interrupt{syscall.SIGINT})
	session := &fakeSession{}

	code, _, stderr := invokeCtx(t, ctx, session, countingGenerator(nil),
		"run", "--target", toyTargetYAML, "--out", t.TempDir(), "--runs", "3", "--no-baseline")

	if code != 128+int(syscall.SIGINT) {
		t.Errorf("reconciler-fuzzer run exited %d, want %d.", code, 128+int(syscall.SIGINT))
	}
	if len(session.sequences) != 0 {
		t.Errorf("The session executed %d sequences after the interrupt.", len(session.sequences))
	}
	if !session.closed {
		t.Error("reconciler-fuzzer run left the test cluster running.")
	}
	if want := "an interrupt stopped the invocation after 0 of 3 runs"; !strings.Contains(stderr, want) {
		t.Errorf("reconciler-fuzzer run printed %q on stderr, want %q.", stderr, want)
	}
}

// A terminal's interrupt reaches the target too, which then stops. The
// interrupt is what the caller needs to hear about.
func TestAnInterruptDuringARunStopsTheInvocation(t *testing.T) {
	for _, test := range []struct {
		name string
		err  error
	}{
		{"ending a wait", fmt.Errorf("op 0 (settle): %w", context.Canceled)},
		{"stopping the target", fmt.Errorf("op 0 (settle): %w: %w: signal: interrupt", context.Canceled, run.ErrTargetStopped)},
	} {
		t.Run(test.name, func(t *testing.T) {
			ctx, cancel := context.WithCancelCause(t.Context())
			session := &fakeSession{failures: []error{test.err}}
			session.after = func() { cancel(interrupt{syscall.SIGTERM}) }

			code, stdout, stderr := invokeCtx(t, ctx, session, countingGenerator(nil),
				"run", "--target", toyTargetYAML, "--out", t.TempDir(), "--runs", "3")

			if code != 128+int(syscall.SIGTERM) {
				t.Errorf("reconciler-fuzzer run exited %d, want %d.", code, 128+int(syscall.SIGTERM))
			}
			if len(session.sequences) != 1 {
				t.Errorf("The session executed %d sequences, want the one the interrupt stopped.", len(session.sequences))
			}
			for _, want := range []string{"run 1: an interrupt stopped the run", session.dirs[0]} {
				if !strings.Contains(stderr, want) {
					t.Errorf("reconciler-fuzzer run printed %q on stderr, which does not mention %q.", stderr, want)
				}
			}
			for _, unwanted := range []string{"--deadline", "no longer running", "canceled"} {
				if strings.Contains(stderr, unwanted) {
					t.Errorf("reconciler-fuzzer run printed %q on stderr, which blames %q for the interrupt.", stderr, unwanted)
				}
			}
			if strings.Contains(stdout, "every run passed") {
				t.Errorf("reconciler-fuzzer run printed %q, but not every run ran.", stdout)
			}
		})
	}
}

// A run that failed on its own before the interrupt says why.
func TestAnInterruptKeepsARunsOwnError(t *testing.T) {
	for _, test := range []struct {
		name string
		err  error
		// then is where reconciler-fuzzer points next, given the run's directory.
		then func(dir string) string
	}{
		{"a refusal",
			&run.Refused{Op: run.Op{Index: 0, Type: run.OpCreate}, Reason: errors.New("spec.count: must be at most 10")},
			func(dir string) string { return "the op is in " + filepath.Join(dir, "sequence.json") }},
		{"a target that stopped", fmt.Errorf("op 0 (create): %w: exit status 1; it wrote %q", run.ErrTargetStopped,
			"toy-widget: flag provided but not defined: -no-such-flag"),
			func(dir string) string { return "the run's files are in " + dir }},
	} {
		t.Run(test.name, func(t *testing.T) {
			ctx, cancel := context.WithCancelCause(t.Context())
			session := &fakeSession{failures: []error{test.err}}
			session.after = func() { cancel(interrupt{syscall.SIGTERM}) }

			code, _, stderr := invokeCtx(t, ctx, session, countingGenerator(nil),
				"run", "--target", toyTargetYAML, "--out", t.TempDir(), "--runs", "3")

			if code != 128+int(syscall.SIGTERM) {
				t.Errorf("reconciler-fuzzer run exited %d, want %d.", code, 128+int(syscall.SIGTERM))
			}
			for _, want := range []string{"run 1: " + test.err.Error(), test.then(session.dirs[0])} {
				if !strings.Contains(stderr, want) {
					t.Errorf("reconciler-fuzzer run printed %q on stderr, which does not say %q.", stderr, want)
				}
			}
		})
	}
}

// A violation found before the interrupt is reported, and says what cut its
// minimization short.
func TestAnInterruptEndsMinimization(t *testing.T) {
	for _, test := range []struct {
		name string
		// replays is how many executes the interrupt waits for: the run
		// itself, then the candidates of the shrink pass.
		replays int
		want    string
	}{
		{name: "before a smaller sequence", replays: 1, want: "an interrupt ended minimization before it found a smaller sequence"},
		{name: "with a smaller sequence unrun", replays: 2, want: "an interrupt ended minimization with 2 ops"},
	} {
		t.Run(test.name, func(t *testing.T) {
			ctx, cancel := context.WithCancelCause(t.Context())
			violation := run.Violation{ID: "G4", Statement: "the target converges"}
			session := &fakeSession{fails: func(run.Sequence, string) *run.Violation { return &violation }}
			session.after = func() {
				if len(session.sequences) == test.replays {
					cancel(interrupt{syscall.SIGINT})
				}
			}
			generate := countingGenerator(nil, run.OpSettle, run.OpSettle, run.OpSettle)

			code, _, stderr := invokeCtx(t, ctx, session, generate,
				"run", "--target", toyTargetYAML, "--out", t.TempDir(), "--runs", "1", "--seed", "42")

			if code != 128+int(syscall.SIGINT) {
				t.Fatalf("reconciler-fuzzer run exited %d, want %d: %s", code, 128+int(syscall.SIGINT), stderr)
			}
			written, err := os.ReadFile(filepath.Join(session.dirs[0], report.MarkdownFile))
			if err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(string(written), test.want) || strings.Contains(string(written)+stderr, "deadline") {
				t.Errorf("reconciler-fuzzer run printed %q and the report\n%s\nwant it to say %q.", stderr, written, test.want)
			}
		})
	}
}

// The minimized sequence runs again into the run directory. A run of it that
// did not finish holds part of a run, which is not a pass.
func TestAnUnfinishedRunOfTheMinimizedSequenceIsNoPass(t *testing.T) {
	ctx, cancel := context.WithCancelCause(t.Context())
	violation := run.Violation{ID: "G4", Statement: "the target converges"}
	session := &fakeSession{failures: make([]error, 10)}
	rerun := func() bool { n := len(session.sequences); return n > 1 && session.dirs[n-1] == session.dirs[0] }
	session.after = func() {
		if rerun() {
			cancel(interrupt{syscall.SIGINT})
			session.failures[len(session.sequences)-1] = fmt.Errorf("op 1 (settle): %w", context.Canceled)
		}
	}
	session.fails = func(sequence run.Sequence, _ string) *run.Violation {
		if rerun() || len(sequence.Ops) < 2 {
			return nil
		}
		return &violation
	}
	session.applies = func(sequence run.Sequence) int {
		if rerun() {
			return 1
		}
		return len(sequence.Ops)
	}
	generate := countingGenerator(nil, run.OpSettle, run.OpSettle, run.OpSettle)

	_, _, stderr := invokeCtx(t, ctx, session, generate,
		"run", "--target", toyTargetYAML, "--out", t.TempDir(), "--runs", "1", "--seed", "42")

	written, err := os.ReadFile(filepath.Join(session.dirs[0], report.MarkdownFile))
	if err != nil {
		t.Fatal(err)
	}
	if said := string(written) + stderr; strings.Contains(said, "passed when it ran again") || !strings.Contains(said, "did not finish when it ran again") {
		t.Errorf("reconciler-fuzzer run printed %q and the report\n%s\nwant them to say the run did not finish.", stderr, written)
	}
	if want := fmt.Sprintf("reconciler-fuzzer: the minimized sequence did not finish when it ran again, so %s holds that partial run: an interrupt stopped the run",
		session.dirs[0]); !strings.Contains(stderr, want) {
		t.Errorf("reconciler-fuzzer run printed %q on stderr, want %q.", stderr, want)
	}
	encoded, err := os.ReadFile(filepath.Join(session.dirs[0], report.JSONFile))
	if err != nil {
		t.Fatal(err)
	}
	var carried struct{ Applied, Ops int }
	if err := json.Unmarshal(encoded, &carried); err != nil {
		t.Fatal(err)
	}
	if carried.Applied != 1 || carried.Ops != 2 {
		t.Errorf("The report says the run applied %d of %d ops, want the 1 of 2 the unfinished run applied.", carried.Applied, carried.Ops)
	}
}

// A run writes its recordings as it ends, so a run of the minimized sequence
// that reconciler-fuzzer is killed in would leave the drawn run's beside its
// own.
func TestTheMinimizedSequenceRunsAgainIntoAnEmptyDirectory(t *testing.T) {
	violation := run.Violation{ID: "G4", Statement: "the target converges"}
	session := &fakeSession{}
	var held []string
	reran := false
	session.fails = func(sequence run.Sequence, dir string) *run.Violation {
		switch {
		case len(session.dirs) == 1:
			if err := os.WriteFile(filepath.Join(dir, "requests.jsonl"), nil, 0o644); err != nil {
				t.Fatal(err)
			}
		case dir == session.dirs[0]:
			reran = true
			entries, _ := os.ReadDir(dir)
			for _, entry := range entries {
				held = append(held, entry.Name())
			}
		}
		if len(sequence.Ops) < 2 {
			return nil
		}
		return &violation
	}

	code, _, stderr := invokeWith(t, session, countingGenerator(nil, run.OpSettle, run.OpSettle, run.OpSettle),
		"run", "--target", toyTargetYAML, "--out", t.TempDir(), "--runs", "1", "--seed", "42")

	if code != exitViolation || !reran {
		t.Fatalf("reconciler-fuzzer run exited %d and ran the minimized sequence again: %t, want %d and true: %s", code, reran, exitViolation, stderr)
	}
	if len(held) > 0 {
		t.Errorf("As the minimized sequence ran again, its directory already held %q.", held)
	}
}

func TestAGeneratorThatFailsExitsTwo(t *testing.T) {
	broken := errors.New("the CRD declares no schema to draw from")
	for _, test := range []struct {
		name         string
		newGenerator func(*target.Target) (Generator, error)
	}{
		{
			name:         "building it",
			newGenerator: func(*target.Target) (Generator, error) { return nil, broken },
		},
		{
			name: "drawing a sequence",
			newGenerator: func(*target.Target) (Generator, error) {
				return drawing(func(int64) (run.Sequence, error) { return run.Sequence{}, broken }), nil
			},
		},
		{
			name: "building the baseline",
			newGenerator: func(t *target.Target) (Generator, error) {
				drawn, _ := countingGenerator(nil)(t)
				return failingBaseline{drawn.(drawing), broken}, nil
			},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			code, _, stderr := invokeWith(t, &fakeSession{}, test.newGenerator,
				"run", "--target", toyTargetYAML, "--out", t.TempDir())

			if code != exitError {
				t.Errorf("A generator that failed exited %d, want %d.", code, exitError)
			}
			if !strings.Contains(stderr, "no schema") {
				t.Errorf("reconciler-fuzzer run reported %q, want the generator's error.", stderr)
			}
		})
	}
}

func TestRunSaysOnceWhichPathsGenerationLeavesAlone(t *testing.T) {
	code, stdout, stderr := invokeWith(t, &fakeSession{}, rapidGenerator,
		"run", "--target", rulesTargetYAML, "--out", t.TempDir(), "--runs", "3", "--seed", "1")

	if code != exitOK {
		t.Fatalf("reconciler-fuzzer run exited %d: %s", code, stderr)
	}
	if said := strings.Count(stdout, "generation leaves spec.surge alone"); said != 1 {
		t.Errorf("reconciler-fuzzer run printed %q, which says %d times that generation leaves spec.surge alone, want once.",
			stdout, said)
	}
}

func TestReplayExecutesTheNamedSequenceAndPrintsItsSeed(t *testing.T) {
	session := &fakeSession{}
	path := writeSequence(t, 8675309)
	out := t.TempDir()

	code, stdout, stderr := invoke(t, session, "replay", "--target", toyTargetYAML, "--out", out, path)

	if code != exitOK {
		t.Fatalf("reconciler-fuzzer replay exited %d: %s", code, stderr)
	}
	if len(session.sequences) != 1 || session.sequences[0].Seed != 8675309 {
		t.Errorf("The session executed %v, want the named sequence.", session.sequences)
	}
	if !strings.Contains(stdout, "8675309") {
		t.Errorf("reconciler-fuzzer replay printed %q, want the seed printed.", stdout)
	}
	if !session.closed {
		t.Errorf("reconciler-fuzzer replay left the test cluster running.")
	}
}

// A skipped check reads as a passing one from outside, so the run prints what
// it could not judge (DESIGN.md §6).
func TestARunPrintsWhatTheChecksCouldNotJudge(t *testing.T) {
	note := "G3 is not evaluated for the deletion of widget: the run ended before its 10s deadline"
	for _, ending := range []struct {
		name      string
		violation *run.Violation
		failure   error
		code      int
	}{
		{"a pass", nil, nil, exitOK},
		{"a violation", &run.Violation{ID: "G4"}, nil, exitViolation},
		{"a harness error", nil, errors.New("the target is no longer running"), exitError},
	} {
		t.Run(ending.name, func(t *testing.T) {
			session := &fakeSession{
				results:  []run.Result{{Notes: []string{note}, Violation: ending.violation}},
				failures: []error{ending.failure},
			}

			code, stdout, stderr := invoke(t, session, "replay", "--target", toyTargetYAML, "--out", t.TempDir(), writeSequence(t, 1))

			if code != ending.code {
				t.Fatalf("reconciler-fuzzer replay exited %d, want %d: %s", code, ending.code, stderr)
			}
			if !strings.Contains(stdout, note) {
				t.Errorf("reconciler-fuzzer replay printed %q, want the note %q.", stdout, note)
			}
		})
	}
}

func TestPassingRunsAreNotPersisted(t *testing.T) {
	session := &fakeSession{}
	out := t.TempDir()

	code, _, stderr := invoke(t, session, "run", "--target", toyTargetYAML, "--out", out, writeSequence(t, 1))

	if code != exitOK {
		t.Fatalf("reconciler-fuzzer run exited %d: %s", code, stderr)
	}
	if _, err := os.Stat(session.dirs[0]); !os.IsNotExist(err) {
		t.Errorf("The passing run left %s behind: %v", session.dirs[0], err)
	}
}

func TestAViolationExitsOneAndKeepsTheRunDirectory(t *testing.T) {
	violation := run.Violation{ID: "G3", Statement: "the target deletes what it manages"}
	session := &fakeSession{results: []run.Result{{Violation: &violation}}}
	out := t.TempDir()

	code, stdout, _ := invoke(t, session, "run", "--target", toyTargetYAML, "--out", out, writeSequence(t, 42))

	if code != exitViolation {
		t.Errorf("A run that found a violation exited %d, want %d.", code, exitViolation)
	}
	if !strings.Contains(stdout, "G3") || !strings.Contains(stdout, "42") {
		t.Errorf("reconciler-fuzzer run printed %q, want the violation and the seed.", stdout)
	}
	if _, err := os.Stat(session.dirs[0]); err != nil {
		t.Errorf("The failing run's directory is missing: %v", err)
	}
	if !strings.HasSuffix(session.dirs[0], filepath.Join("run-1")) {
		t.Errorf("The failing run wrote to %s, want run-1 under the invocation's directory.", session.dirs[0])
	}
}

func TestAHarnessErrorExitsTwo(t *testing.T) {
	session := &fakeSession{failures: []error{errors.New("the control plane did not start")}}

	code, _, stderr := invoke(t, session, "run", "--target", toyTargetYAML, "--out", t.TempDir(), writeSequence(t, 1))

	if code != exitError {
		t.Errorf("A run that failed exited %d, want %d.", code, exitError)
	}
	if !strings.Contains(stderr, "the control plane did not start") {
		t.Errorf("reconciler-fuzzer run reported %q, want the harness error.", stderr)
	}
}

func TestATargetTheClusterRefusesEndsTheInvocationBeforeARun(t *testing.T) {
	refused := errors.New("these are cluster-scoped: the managed rbac.authorization.k8s.io/v1/ClusterRole")
	for _, args := range [][]string{
		{"run", "--target", toyTargetYAML, "--out", t.TempDir(), writeSequence(t, 1)},
		{"matrix", "--target", toyTargetYAML, "--sequences", bugSequences(t, 0), "--out", matrixFile(t)},
	} {
		t.Run(args[0], func(t *testing.T) {
			session := &fakeSession{refused: refused}

			code, stdout, stderr := invoke(t, session, args...)

			if code != exitError {
				t.Errorf("reconciler-fuzzer %s exited %d, want %d.", args[0], code, exitError)
			}
			if !strings.Contains(stderr, refused.Error()) {
				t.Errorf("reconciler-fuzzer %s reported %q, want %q.", args[0], stderr, refused)
			}
			if stdout != "" {
				t.Errorf("reconciler-fuzzer %s printed %q, and ran nothing.", args[0], stdout)
			}
			if len(session.sequences) != 0 {
				t.Errorf("reconciler-fuzzer %s ran %d sequences against a target the cluster refused.", args[0], len(session.sequences))
			}
			if !session.closed {
				t.Errorf("reconciler-fuzzer %s left the session open.", args[0])
			}
		})
	}
}

// The empty assets directory fails a control plane that starts first.
func TestOpeningASessionChecksTheLaunchBinaryFirst(t *testing.T) {
	t.Setenv("KUBEBUILDER_ASSETS", t.TempDir())
	unbuilt := &target.Target{Launch: target.LaunchSpec{Binary: "bin/no-such-operator"}}
	for _, opts := range []options{{}, {kubeconfig: filepath.Join(t.TempDir(), "no-such-kubeconfig")}} {
		s, err := openSession(opts, unbuilt)

		if err == nil {
			_ = s.close()
			t.Fatalf("openSession(%+v) accepted a launch.binary that does not exist.", opts)
		}
		if !strings.Contains(err.Error(), "launch.binary") {
			t.Errorf("openSession(%+v) returned %q, want the launch.binary error before anything starts.", opts, err)
		}
	}
}

func TestAHarnessErrorNamesItsRunAndDirectory(t *testing.T) {
	session := &fakeSession{failures: []error{nil, errors.New("the control plane did not start")}}

	code, _, stderr := invoke(t, session, "run", "--target", toyTargetYAML, "--out", t.TempDir(), "--runs", "3")

	if code != exitError {
		t.Errorf("A run that failed exited %d, want %d.", code, exitError)
	}
	for _, want := range []string{"run 2: the control plane did not start", session.dirs[1]} {
		if !strings.Contains(stderr, want) {
			t.Errorf("reconciler-fuzzer run reported %q, which does not mention %q.", stderr, want)
		}
	}
}

func TestAHarnessErrorBeforeTheRunWroteAnythingNamesNoDirectory(t *testing.T) {
	session := &fakeSession{failures: []error{errors.New("the sequence is for another target")}, writesNothing: true}

	code, _, stderr := invoke(t, session, "replay", "--target", toyTargetYAML, "--out", t.TempDir(), writeSequence(t, 1))

	if code != exitError {
		t.Errorf("A run that failed exited %d, want %d.", code, exitError)
	}
	if strings.Contains(stderr, session.dirs[0]) {
		t.Errorf("reconciler-fuzzer replay reported %q, which points at a directory the run never made.", stderr)
	}
}

func TestARefusedDrawSaysWhereItsSequenceIs(t *testing.T) {
	refusal := &run.Refused{Op: run.Op{Index: 0, Type: run.OpCreate}, Reason: errors.New("maxUnavailable must not exceed count")}
	session := &fakeSession{failures: []error{nil, refusal}}

	code, _, stderr := invoke(t, session, "run", "--target", toyTargetYAML, "--out", t.TempDir(), "--runs", "3", "--seed", "1")

	if code != exitError {
		t.Errorf("A run the API server refused exited %d, want %d.", code, exitError)
	}
	if len(session.sequences) != 2 {
		t.Errorf("reconciler-fuzzer ran %d sequences, want it to stop at the refused one.", len(session.sequences))
	}
	for _, want := range []string{
		"run 2: the API server refused op 0 (create)", "maxUnavailable must not exceed count",
		filepath.Join(session.dirs[1], "sequence.json"), "a CRD rule that reads status", "generate.mutate",
		"generate.overlay",
	} {
		if !strings.Contains(stderr, want) {
			t.Errorf("reconciler-fuzzer run reported %q, which does not mention %q.", stderr, want)
		}
	}
}

func TestARefusedSequenceFileIsNamed(t *testing.T) {
	refusal := &run.Refused{Op: run.Op{Index: 0, Type: run.OpCreate}, Reason: errors.New("spec.count: must be at most 10")}
	session := &fakeSession{failures: []error{refusal}}
	path := writeSequence(t, 1)

	code, _, stderr := invoke(t, session, "replay", "--target", toyTargetYAML, "--out", t.TempDir(), path)

	if code != exitError {
		t.Errorf("A sequence the API server refused exited %d, want %d.", code, exitError)
	}
	if !strings.Contains(stderr, path) {
		t.Errorf("reconciler-fuzzer replay reported %q, which does not name %s.", stderr, path)
	}
	if strings.Contains(stderr, "generate.") {
		t.Errorf("reconciler-fuzzer replay reported %q, which blames generation for a sequence reconciler-fuzzer did not draw.", stderr)
	}
}

func TestRunStopsAtTheFirstFailingSequence(t *testing.T) {
	violation := run.Violation{ID: "G1"}
	session := &fakeSession{results: []run.Result{{}, {Violation: &violation}}}
	out := t.TempDir()
	first, second, third := writeSequence(t, 1), writeSequence(t, 2), writeSequence(t, 3)

	code, _, _ := invoke(t, session, "run", "--target", toyTargetYAML, "--out", out, first, second, third)

	if code != exitViolation {
		t.Errorf("reconciler-fuzzer run exited %d, want %d.", code, exitViolation)
	}
	if len(session.sequences) != 2 {
		t.Errorf("The session executed %d sequences, want it to stop at the failing one and run it as written.",
			len(session.sequences))
	}
}

func TestLaunchArgsAppendToTheTargetsInOrder(t *testing.T) {
	session := &fakeSession{}
	declared := loadToyArgs(t)

	code, _, stderr := invoke(t, session, "replay", "--target", toyTargetYAML, "--out", t.TempDir(),
		"--launch-arg", "--bug=3", "--launch-arg", "--bug=7", writeSequence(t, 1))

	if code != exitOK {
		t.Fatalf("reconciler-fuzzer replay exited %d: %s", code, stderr)
	}
	want := append(slices.Clone(declared), "--bug=3", "--bug=7")
	if len(session.args) != 1 || !slices.Equal(session.args[0], want) {
		t.Errorf("The target was launched with %v, want %v: a later flag wins.", session.args, want)
	}
}

func TestExitCodeMapsTheOutcome(t *testing.T) {
	violation := run.Violation{ID: "G1"}
	for _, test := range []struct {
		name   string
		result run.Result
		err    error
		want   int
	}{
		{name: "the run passed", want: exitOK},
		{name: "an invariant failed", result: run.Result{Violation: &violation}, want: exitViolation},
		{name: "the harness failed", err: errors.New("the proxy died"), want: exitError},
		{name: "both", result: run.Result{Violation: &violation}, err: errors.New("the proxy died"), want: exitError},
	} {
		t.Run(test.name, func(t *testing.T) {
			if got := exitCode(test.result, test.err); got != test.want {
				t.Errorf("exitCode returned %d, want %d.", got, test.want)
			}
		})
	}
}

func TestParseReadsTheFlagsOfSection11(t *testing.T) {
	opts, _, err := parse([]string{
		"run", "--target", "t.yaml", "--runs", "3", "--seed", "42", "--out", "elsewhere",
		"--deadline", "90s", "--kubeconfig", "kubeconfig", "--launch-arg", "--bug=1",
	})

	if err != nil {
		t.Fatalf("parse failed: %v", err)
	}
	if opts.target != "t.yaml" || opts.runs != 3 || opts.seed != 42 || opts.out != "elsewhere" {
		t.Errorf("parse read %+v.", opts)
	}
	if opts.deadline.String() != "1m30s" || !opts.deadlineGiven || opts.kubeconfig != "kubeconfig" {
		t.Errorf("parse read the deadline %v and the kubeconfig %q.", opts.deadline, opts.kubeconfig)
	}
	if !slices.Equal(opts.launchArgs, []string{"--bug=1"}) {
		t.Errorf("parse read the launch args %v.", opts.launchArgs)
	}
}

func TestParseReadsTheSequenceFiles(t *testing.T) {
	_, sequences, err := parse([]string{"run", "--target", "t.yaml", "a.json", "b.json"})

	if err != nil {
		t.Fatalf("parse failed: %v", err)
	}
	if !slices.Equal(sequences, []string{"a.json", "b.json"}) {
		t.Errorf("parse read the sequences %v.", sequences)
	}
}

func TestParseReadsSequenceFilesThatReadAsFlagsAfterTwoDashes(t *testing.T) {
	_, sequences, err := parse([]string{"run", "--target", "t.yaml", "--", "-a.json", "-b.json"})

	if err != nil || !slices.Equal(sequences, []string{"-a.json", "-b.json"}) {
		t.Errorf("parse read the sequences %v and returned %v, want [-a.json -b.json].", sequences, err)
	}
}

func TestParseReadsADashAsASequenceFile(t *testing.T) {
	_, sequences, err := parse([]string{"run", "--target", "t.yaml", "a.json", "-"})

	if err != nil || !slices.Equal(sequences, []string{"a.json", "-"}) {
		t.Errorf("parse read the sequences %v and returned %v, want [a.json -].", sequences, err)
	}
}

func TestParseDefaultsTheOutputDirectoryAndLeavesTheDeadlineToDerive(t *testing.T) {
	opts, _, err := parse([]string{"replay", "--target", "t.yaml", "a.json"})

	if err != nil {
		t.Fatalf("parse failed: %v", err)
	}
	if opts.deadlineGiven || opts.deadline != 0 || opts.out != defaultOut {
		t.Errorf("parse read a deadline of %v and defaulted to the directory %q, want no deadline and %q.",
			opts.deadline, opts.out, defaultOut)
	}
}

func TestTheInvocationDirectoryTakesTheSequenceSeedUnlessOneIsGiven(t *testing.T) {
	for _, test := range []struct {
		name string
		args []string
		want string
	}{
		{name: "no seed flag", want: "-8675309"},
		{name: "a seed flag", args: []string{"--seed", "7"}, want: "-7"},
	} {
		t.Run(test.name, func(t *testing.T) {
			session := &fakeSession{}
			out := t.TempDir()
			args := append([]string{"run", "--target", toyTargetYAML, "--out", out}, test.args...)

			code, _, stderr := invoke(t, session, append(args, writeSequence(t, 8675309))...)

			if code != exitOK {
				t.Fatalf("reconciler-fuzzer run exited %d: %s", code, stderr)
			}
			if invocation := filepath.Base(filepath.Dir(session.dirs[0])); !strings.HasSuffix(invocation, test.want) {
				t.Errorf("The invocation wrote to %s, want a directory ending in %q.", invocation, test.want)
			}
		})
	}
}

// shellWords splits a command line the way sh does.
func shellWords(t *testing.T, line string) []string {
	t.Helper()
	printed, err := exec.Command("sh", "-c", `printf '%s\0' `+line).Output()
	if err != nil {
		t.Fatalf("sh could not split %q: %v", line, err)
	}
	return strings.Split(strings.TrimSuffix(string(printed), "\x00"), "\x00")
}

func TestTheReplayCommandParsesBackToWhatRan(t *testing.T) {
	for _, test := range []struct {
		name string
		ran  options
		path string
	}{
		{name: "the target alone", ran: options{target: "t.yaml"}, path: "s.json"},
		{name: "a kubeconfig", ran: options{target: "t.yaml", kubeconfig: "/home/me/.kube/config"}, path: "s.json"},
		{name: "launch args", ran: options{target: "t.yaml", launchArgs: []string{"--bug=3", "--mode=a=b", "--bug=7"}}, path: "s.json"},
		{name: "a kubeconfig and launch args", ran: options{target: "t.yaml", kubeconfig: "k", launchArgs: []string{"--x=1"}}, path: "s.json"},
		{name: "a path with a space", ran: options{target: "my target.yaml"}, path: "reconciler-fuzzer-out/run 1/sequence.json"},
		{name: "what a shell would expand", ran: options{target: "$HOME/t.yaml", launchArgs: []string{"--name=it's", "*"}}, path: "`s`.json"},
		{name: "an empty launch arg", ran: options{target: "t.yaml", launchArgs: []string{""}}, path: "s.json"},
		{name: "a path that reads as a flag", ran: options{target: "t.yaml"}, path: "-seqs/b1.json"},
	} {
		t.Run(test.name, func(t *testing.T) {
			line := test.ran.replayCommand(test.path)

			words := shellWords(t, line)
			if words[0] != "reconciler-fuzzer" {
				t.Fatalf("The replay command %q runs %q.", line, words[0])
			}
			replay, sequences, err := parse(words[1:])

			if err != nil {
				t.Fatalf("reconciler-fuzzer cannot parse the replay command %q: %v", line, err)
			}
			if replay.command != "replay" || replay.target != test.ran.target || replay.kubeconfig != test.ran.kubeconfig ||
				!slices.Equal(replay.launchArgs, test.ran.launchArgs) || len(sequences) != 1 || filepath.Clean(sequences[0]) != test.path {
				t.Errorf("The replay command %q parses to %s %q, kubeconfig %q, launch args %q, sequences %q; want replay %q, %q, %q, [%q].",
					line, replay.command, replay.target, replay.kubeconfig, replay.launchArgs, sequences,
					test.ran.target, test.ran.kubeconfig, test.ran.launchArgs, test.path)
			}
		})
	}
}

// zsh expands a word that begins with =, and sh does not.
func TestTheReplayCommandQuotesAWordZshWouldExpand(t *testing.T) {
	line := options{target: "t.yaml", launchArgs: []string{"=x"}}.replayCommand("s.json")

	if !strings.Contains(line, "'=x'") {
		t.Errorf("The replay command is %q, want =x quoted.", line)
	}
}

// literalBytes are the bytes shellQuote leaves bare. sh and zsh read each as
// itself, but for a leading =.
const literalBytes = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789_./=:@%+,-"

func TestTheReplayCommandLeavesAWordOfLiteralBytesBare(t *testing.T) {
	words := []string{literalBytes}
	for _, c := range strings.ReplaceAll(literalBytes, "=", "") {
		words = append(words, string(c))
	}
	for _, word := range words {
		if quoted := shellQuote(word); quoted != word {
			t.Errorf("shellQuote(%q) is %q, want it bare.", word, quoted)
		}
	}
}

// A shell reads some of these bytes as themselves, but quoting them costs
// nothing.
func TestTheReplayCommandQuotesEveryOtherByte(t *testing.T) {
	others := []string{"é", " ", "🤖", "\xff"}
	for c := range rune(0x80) {
		if !strings.ContainsRune(literalBytes, c) {
			others = append(others, string(c))
		}
	}
	for _, other := range others {
		for _, word := range []string{other, "x" + other} {
			if quoted := shellQuote(word); !strings.HasPrefix(quoted, "'") {
				t.Errorf("shellQuote(%q) is %q, want it single-quoted.", word, quoted)
			}
		}
	}
}

// A new flag either changes what a run executes, and the replay command
// carries it, or it does not. This test makes its author say which.
func TestEveryFlagIsReplayedOrSelectsNothing(t *testing.T) {
	replayed := []string{"target", "kubeconfig", "launch-arg"}
	inert := []string{"runs", "seed", "no-baseline", "out", "deadline", "sequences", "junit"}
	for _, command := range []string{"run", "replay", "matrix"} {
		(&options{command: command}).flags().VisitAll(func(f *flag.Flag) {
			if !slices.Contains(replayed, f.Name) && !slices.Contains(inert, f.Name) {
				t.Errorf("reconciler-fuzzer %s --%s is neither replayed nor inert: replayCommand must carry it, or this test must say it selects nothing.",
					command, f.Name)
			}
		})
	}
}

func loadToyArgs(t *testing.T) []string {
	t.Helper()
	toy, err := target.Load(toyTargetYAML)
	if err != nil {
		t.Fatalf("Loading the toy target failed: %v", err)
	}
	return toy.Launch.Args
}

func TestARunEvaluatesTheInvariants(t *testing.T) {
	session := &fakeSession{}

	code, _, stderr := invoke(t, session, "replay", "--target", toyTargetYAML, "--out", t.TempDir(), writeSequence(t, 1))

	if code != exitOK {
		t.Fatalf("reconciler-fuzzer replay exited %d: %s", code, stderr)
	}
	if len(session.checks) != 1 || session.checks[0] != (run.Engine{}) {
		t.Errorf("The run checked with %v, want the invariant engine.", session.checks)
	}
}

// The state and its count reach report.json, and not only the line the CLI
// prints (#13, #24).
func TestTheReportQuotesTheStateAtTheVerdict(t *testing.T) {
	for _, c := range []struct {
		name  string
		state []observe.Version
	}{
		{name: "the state the verdict quoted", state: []observe.Version{{Key: observe.Key{
			GVK:  schema.GroupVersionKind{Version: "v1", Kind: "ConfigMap"},
			Name: "widget-0",
		}, ResourceVersion: "12"}}},
		{name: "a target that managed nothing"},
	} {
		t.Run(c.name, func(t *testing.T) {
			managed := len(c.state)
			violation := run.Violation{ID: "G4", Statement: "the target converges",
				Managed: c.state, ManagedTotal: &managed}
			session := &fakeSession{results: []run.Result{{Violation: &violation}}}

			code, _, stderr := invokeWith(t, session, countingGenerator(nil, run.OpSettle),
				"run", "--target", toyTargetYAML, "--out", t.TempDir(), "--runs", "1", "--seed", "42")

			if code != exitViolation {
				t.Fatalf("reconciler-fuzzer run exited %d, want %d: %s", code, exitViolation, stderr)
			}
			encoded, err := os.ReadFile(filepath.Join(session.dirs[0], report.JSONFile))
			if err != nil {
				t.Fatal(err)
			}
			var carried struct {
				Managed      []observe.Version
				ManagedTotal *int
			}
			if err := json.Unmarshal(encoded, &carried); err != nil {
				t.Fatal(err)
			}
			if len(carried.Managed) != managed {
				t.Errorf("report.json is\n%s\nwant the %d objects the violation quoted.", encoded, managed)
			}
			if carried.ManagedTotal == nil || *carried.ManagedTotal != managed {
				t.Errorf("report.json is\n%s\nwant the count of the objects the target managed.", encoded)
			}
		})
	}
}

func TestTheReportQuotesTheReadyPredicate(t *testing.T) {
	violation := run.Violation{ID: "G4", Statement: "the target converges",
		Ready: &invariant.Readiness{Expr: "has(status.ready)", Error: "no such key: ready"}}
	session := &fakeSession{results: []run.Result{{Violation: &violation}}}

	code, _, stderr := invokeWith(t, session, countingGenerator(nil, run.OpSettle),
		"run", "--target", toyTargetYAML, "--out", t.TempDir(), "--runs", "1", "--seed", "42")

	if code != exitViolation {
		t.Fatalf("reconciler-fuzzer run exited %d, want %d: %s", code, exitViolation, stderr)
	}
	md, err := os.ReadFile(filepath.Join(session.dirs[0], report.MarkdownFile))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(md), "## Ready predicate") || !strings.Contains(string(md), "no such key: ready") {
		t.Errorf("report.md is\n%s\nwant the ready predicate and its error.", md)
	}
}

func TestTheReportSaysWhatARestartChanged(t *testing.T) {
	violation := run.Violation{
		ID: "G5", Statement: "the v1/ConfigMap widget-0 changed across the Restart at op 1 (restart)",
		Differences:      []invariant.Difference{{Object: "v1/ConfigMap widget-0", ResourceVersions: [2]string{"11", "21"}, Path: "data.index", Before: `"0"`, After: `"1"`}},
		DifferencesTotal: 7,
		Compared:         "the state converged after op 0 (create) and the one after op 2 (settle)",
	}
	session := &fakeSession{results: []run.Result{{Violation: &violation}}}

	code, _, stderr := invokeWith(t, session, countingGenerator(nil, run.OpSettle),
		"run", "--target", toyTargetYAML, "--out", t.TempDir(), "--runs", "1", "--seed", "42")

	if code != exitViolation {
		t.Fatalf("reconciler-fuzzer run exited %d, want %d: %s", code, exitViolation, stderr)
	}
	encoded, err := os.ReadFile(filepath.Join(session.dirs[0], report.JSONFile))
	if err != nil {
		t.Fatal(err)
	}
	var carried struct {
		Differences      []invariant.Difference
		DifferencesTotal int
		Compared         string
	}
	if err := json.Unmarshal(encoded, &carried); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(carried.Differences, violation.Differences) || carried.DifferencesTotal != 7 || carried.Compared != violation.Compared {
		t.Errorf("report.json is\n%s\nwant the differences the violation carried.", encoded)
	}
}

// The report says what the check's own bound left out, which only the check
// knows (#22).
func TestTheReportSaysWhatTheChecksBoundLeftOut(t *testing.T) {
	violation := run.Violation{
		ID: "G4", Statement: "the target converges",
		Requests:      []proxy.Request{{Verb: "get", Path: "/api/v1/widgets"}},
		RequestsTotal: 133,
		Versions:      []observe.Version{{Key: observe.Key{Name: "widget"}, ResourceVersion: "11"}},
		VersionsTotal: 41,
		VersionsOf:    "toy.reconciler-fuzzer/v1/Widget widget",
	}
	session := &fakeSession{results: []run.Result{{Violation: &violation}}}

	code, _, stderr := invokeWith(t, session, countingGenerator(nil, run.OpSettle),
		"run", "--target", toyTargetYAML, "--out", t.TempDir(), "--runs", "1", "--seed", "42")

	if code != exitViolation {
		t.Fatalf("reconciler-fuzzer run exited %d, want %d: %s", code, exitViolation, stderr)
	}
	encoded, err := os.ReadFile(filepath.Join(session.dirs[0], report.JSONFile))
	if err != nil {
		t.Fatal(err)
	}
	var carried struct {
		RequestsTotal, VersionsTotal int
		VersionsOf                   string
	}
	if err := json.Unmarshal(encoded, &carried); err != nil {
		t.Fatal(err)
	}
	if carried.VersionsTotal != 41 || carried.RequestsTotal != 133 {
		t.Errorf("report.json says it chose from %d requests and %d versions, want the 133 and 41 the check did.",
			carried.RequestsTotal, carried.VersionsTotal)
	}
	if carried.VersionsOf != violation.VersionsOf {
		t.Errorf("report.json says the timeline is of %q, want %q.", carried.VersionsOf, violation.VersionsOf)
	}
}

func TestAnEnvtestInvocationWarnsOnceOfTheWorkloadsItManages(t *testing.T) {
	for _, test := range []struct {
		name  string
		args  []string
		warns bool
	}{
		{name: "workloads on envtest", args: []string{"--target", workloadsTargetYAML}, warns: true},
		{name: "workloads on a kubeconfig cluster", args: []string{"--target", workloadsTargetYAML, "--kubeconfig", "kind.kubeconfig"}},
		{name: "no workloads", args: []string{"--target", toyTargetYAML}},
	} {
		t.Run(test.name, func(t *testing.T) {
			args := append([]string{"run", "--out", t.TempDir(), "--runs", "3"}, test.args...)

			session := &fakeSession{}
			code, _, stderr := invoke(t, session, args...)

			if code != exitOK {
				t.Fatalf("reconciler-fuzzer run exited %d: %s", code, stderr)
			}
			want := 0
			if test.warns {
				want = 1
			}
			if warned := strings.Count(stderr, "kubelet"); warned != want {
				t.Fatalf("reconciler-fuzzer run printed %q on stderr, which warns %d times of what envtest never runs, want %d.",
					stderr, warned, want)
			}
			if !test.warns {
				return
			}
			if session.stderrAtOpen != stderr {
				t.Errorf("reconciler-fuzzer run had printed %q when it opened the session, want the warning %q first.",
					session.stderrAtOpen, stderr)
			}
			// The target also manages a ConfigMap and an example.com Deployment.
			static := ": Deployment, StatefulSet, DaemonSet, ReplicaSet, Job, CronJob, ReplicationController, Pod, PersistentVolumeClaim."
			for _, want := range []string{static, "--kubeconfig", "kind cluster"} {
				if !strings.Contains(stderr, want) {
					t.Errorf("reconciler-fuzzer run warned %q, want it to name %q.", stderr, want)
				}
			}
		})
	}
}

// A kubeconfig cluster runs its own garbage collector, and reconciler-fuzzer
// none.
func TestOnlyAReportFromEnvtestNamesCollectorJSONL(t *testing.T) {
	for _, test := range []struct {
		name  string
		args  []string
		names bool
	}{
		{name: "on envtest", names: true},
		{name: "on a kubeconfig cluster", args: []string{"--kubeconfig", "kind.kubeconfig"}},
	} {
		t.Run(test.name, func(t *testing.T) {
			violation := run.Violation{ID: "G3"}
			session := &fakeSession{results: []run.Result{{Violation: &violation}}}
			args := append([]string{"replay", "--target", toyTargetYAML, "--out", t.TempDir()}, test.args...)

			code, _, stderr := invoke(t, session, append(args, writeSequence(t, 1))...)

			if code != exitViolation {
				t.Fatalf("reconciler-fuzzer replay exited %d, want %d: %s", code, exitViolation, stderr)
			}
			written, err := os.ReadFile(filepath.Join(session.dirs[0], report.MarkdownFile))
			if err != nil {
				t.Fatal(err)
			}
			if names := strings.Contains(string(written), "collector.jsonl"); names != test.names {
				t.Errorf("The report names collector.jsonl: %t, want %t.\n%s", names, test.names, written)
			}
		})
	}
}

func TestAG4ReportFromEnvtestRepeatsTheWorkloadWarning(t *testing.T) {
	const runNote = "G3 is not evaluated for the deletion of widget"
	for _, test := range []struct {
		name, check, target string
		args                []string
		repeats             bool
	}{
		{name: "G4 on envtest", check: "G4", target: workloadsTargetYAML, repeats: true},
		{name: "G4 on a kubeconfig cluster", check: "G4", target: workloadsTargetYAML, args: []string{"--kubeconfig", "kind.kubeconfig"}},
		{name: "G4 with no workloads", check: "G4", target: toyTargetYAML},
		{name: "G3 on envtest", check: "G3", target: workloadsTargetYAML},
	} {
		t.Run(test.name, func(t *testing.T) {
			violation := run.Violation{ID: test.check}
			session := &fakeSession{results: []run.Result{{Violation: &violation, Notes: []string{runNote}}}}
			args := append([]string{"replay", "--target", test.target, "--out", t.TempDir()}, test.args...)

			code, _, stderr := invoke(t, session, append(args, writeSequence(t, 1))...)

			if code != exitViolation {
				t.Fatalf("reconciler-fuzzer replay exited %d, want %d: %s", code, exitViolation, stderr)
			}
			want := []string{runNote}
			if test.repeats {
				warning, _ := strings.CutPrefix(strings.TrimSuffix(stderr, "\n"), "reconciler-fuzzer: ")
				want = append(want, warning)
			}
			encoded, err := os.ReadFile(filepath.Join(session.dirs[0], report.JSONFile))
			if err != nil {
				t.Fatal(err)
			}
			var carried struct{ Notes []string }
			if err := json.Unmarshal(encoded, &carried); err != nil {
				t.Fatal(err)
			}
			if !slices.Equal(carried.Notes, want) {
				t.Errorf("report.json carries the notes %q, want %q.", carried.Notes, want)
			}
		})
	}
}
