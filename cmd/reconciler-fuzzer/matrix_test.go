package main

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/rosenhouse-bot-sandbox/reconciler-fuzzer/internal/proxy"
	"github.com/rosenhouse-bot-sandbox/reconciler-fuzzer/internal/run"
	"github.com/rosenhouse-bot-sandbox/reconciler-fuzzer/internal/target"
)

// bugSequences writes one sequence per bug id into a new directory.
func bugSequences(t *testing.T, bugs ...int) string {
	t.Helper()
	dir := t.TempDir()
	for _, bug := range bugs {
		sequence := run.Sequence{Seed: int64(bug), Target: "toy-widget", Ops: []run.Op{{Type: run.OpSettle}}}
		if err := run.WriteSequence(filepath.Join(dir, "b"+strconv.Itoa(bug)+".json"), sequence); err != nil {
			t.Fatalf("Writing the test sequence failed: %v", err)
		}
	}
	return dir
}

// recorded is a run the engine reads: its settle wait converged, or expired
// with no fault active while the target repeated a failing request, which are
// a G4 and a G6 violation.
func recorded(t *testing.T, converged bool) run.Result {
	t.Helper()
	toy, err := target.Load(toyTargetYAML)
	if err != nil {
		t.Fatalf("Loading the toy target failed: %v", err)
	}
	start := time.Now()
	result := run.Result{Recorded: run.Input{
		Target:   toy,
		Requests: []proxy.Request{{Start: start, Verb: "watch", Resource: "widgets", Watch: true, Status: 200}},
		Timeline: run.Timeline{
			Ops:         []run.AppliedOp{{Op: run.Op{Index: 0, Type: run.OpSettle}, At: start}},
			Checkpoints: []run.Checkpoint{{At: start.Add(5 * time.Second), Op: 0, Converged: converged}},
		},
	}}
	if converged {
		return result
	}
	for i := range toy.Thresholds.ErrLoop + 1 {
		result.Recorded.Requests = append(result.Recorded.Requests, proxy.Request{
			Start: start.Add(time.Duration(i) * time.Second), Verb: "get", Resource: "configmaps", Name: "widget-0", Status: 404,
		})
	}
	return result
}

// recordedWithAnUnjudgedRestart is a converged run whose restart has no
// converged snapshot after it, so G5 leaves a note instead of a verdict.
func recordedWithAnUnjudgedRestart(t *testing.T) run.Result {
	t.Helper()
	result := recorded(t, true)
	at := result.Recorded.Timeline.Checkpoints[0].At.Add(time.Second)
	result.Recorded.Timeline.Ops = append(result.Recorded.Timeline.Ops,
		run.AppliedOp{Op: run.Op{Index: 1, Type: run.OpRestart}, At: at})
	return result
}

func matrixFile(t *testing.T) string { return filepath.Join(t.TempDir(), "bug-matrix.md") }

func readMatrix(t *testing.T, path string) string {
	t.Helper()
	written, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("Reading the matrix failed: %v", err)
	}
	return string(written)
}

// Each sequence runs under its bug and then against the toy with no bug. B0's
// one run is both.
func TestMatrixRunsEachSequenceUnderItsBugAndWithout(t *testing.T) {
	session := &fakeSession{results: []run.Result{
		recorded(t, true), recorded(t, false), recorded(t, true), recorded(t, false), recorded(t, true),
	}}
	sequences := bugSequences(t, 0, 1, 2)
	out := matrixFile(t)

	code, stdout, stderr := invoke(t, session, "matrix", "--target", toyTargetYAML, "--sequences", sequences, "--out", out,
		"--launch-arg", "--x=1")

	if code != exitOK {
		t.Fatalf("reconciler-fuzzer matrix exited %d: %s%s", code, stdout, stderr)
	}
	correct := append(loadToyArgs(t), "--x=1")
	want := []struct {
		bug  int64
		args []string
	}{
		{0, correct},
		{1, append(slices.Clone(correct), "--bug=1")}, {1, correct},
		{2, append(slices.Clone(correct), "--bug=2")}, {2, correct},
	}
	if len(session.sequences) != len(want) {
		t.Fatalf("The session executed %d sequences, want %d: each under its bug and with none, and B0 once.",
			len(session.sequences), len(want))
	}
	for i, want := range want {
		if ran := session.sequences[i].Seed; ran != want.bug || !slices.Equal(session.args[i], want.args) {
			t.Errorf("Run %d executed b%d.json with %v, want b%d.json with %v.", i, ran, session.args[i], want.bug, want.args)
		}
	}
	if dirs := slices.Compact(slices.Sorted(slices.Values(session.dirs))); len(dirs) != len(want) {
		t.Errorf("The runs wrote to %v, want a directory each.", session.dirs)
	}
	for _, line := range []string{
		"B0: b0.json fired nothing", "B1: b1.json under --bug=1 fired G4, G6", "B1: b1.json without --bug=1 fired nothing",
	} {
		if !strings.Contains(stdout, line) {
			t.Errorf("reconciler-fuzzer matrix printed\n%s\nwant the line %q.", stdout, line)
		}
	}
	if !session.closed {
		t.Errorf("reconciler-fuzzer matrix left the test cluster running.")
	}
}

func TestMatrixWritesBugAgainstCheck(t *testing.T) {
	session := &fakeSession{results: []run.Result{recorded(t, true), recorded(t, false), recorded(t, true)}}
	out := matrixFile(t)

	code, _, stderr := invoke(t, session, "matrix",
		"--target", toyTargetYAML, "--sequences", bugSequences(t, 0, 1), "--out", out)

	if code != exitOK {
		t.Fatalf("reconciler-fuzzer matrix exited %d: %s", code, stderr)
	}
	written := readMatrix(t, out)
	want := []string{
		"| Bug | G1 | G2 | G3 | G4 | G5 | G6 | G7 | P1 | No bug |",
		"|---|---|---|---|---|---|---|---|---|---|",
		"| B0 |  |  |  |  |  |  |  |  |  |",
		"| B1 |  |  |  | ✓ |  | ✓ |  |  |  |",
	}
	for _, line := range want {
		if !slices.Contains(strings.Split(written, "\n"), line) {
			t.Errorf("The matrix is\n%s\nwant the line %q.", written, line)
		}
	}
	if !strings.Contains(written, "every check that fired") {
		t.Errorf("The matrix is\n%s\nwant it to say a row lists every check that fired.", written)
	}
	if !strings.Contains(written, "also runs against the toy with no bug") {
		t.Errorf("The matrix is\n%s\nwant it to say what its last column is.", written)
	}
}

// DESIGN.md §6 and D31: a check that could not judge reads like a passing one
// from outside, so the matrix says which cell is which.
func TestMatrixMarksACheckThatLeftSomethingUnjudged(t *testing.T) {
	// The control is the row D31 is about: it reads as empty either way.
	session := &fakeSession{results: []run.Result{recordedWithAnUnjudgedRestart(t), recorded(t, false), recorded(t, true)}}
	out := matrixFile(t)

	code, stdout, stderr := invoke(t, session, "matrix",
		"--target", toyTargetYAML, "--sequences", bugSequences(t, 0, 1), "--out", out)

	if code != exitOK {
		t.Fatalf("reconciler-fuzzer matrix exited %d: %s", code, stderr)
	}
	written := readMatrix(t, out)
	if !strings.Contains(written, "| B0 |  |  |  |  | ? |  |  |  | G5 ? |") {
		t.Errorf("The matrix is\n%s\nwant B0's G5 cell and its no-bug cell to say G5 judged nothing.", written)
	}
	if !strings.Contains(written, "left something unjudged") {
		t.Errorf("The matrix is\n%s\nwant it to say what the mark means.", written)
	}
	if !strings.Contains(stdout, "G5 is not evaluated") {
		t.Errorf("reconciler-fuzzer matrix printed %q, want the note G5 left.", stdout)
	}
}

func TestACheckThatFiredIsMarkedCaughtWhateverItLeftUnjudged(t *testing.T) {
	found := checked{fired: []string{"G5"}, skipped: []string{"G5"}}

	if mark := found.mark("G5"); mark != "✓" {
		t.Errorf("A check that fired and left a note is marked %q, want ✓.", mark)
	}
}

// A run note says why a row reads as it does, such as why G3 fired.
func TestMatrixPrintsTheRunsNotes(t *testing.T) {
	control := recorded(t, true)
	note := "reconciler-fuzzer's garbage collector never deletes v1/ConfigMap widget-0, because it does not watch v1/Secret, the kind of its owner tls"
	control.Notes = []string{note}
	session := &fakeSession{results: []run.Result{control, recorded(t, false), recorded(t, true)}}

	code, stdout, stderr := invoke(t, session, "matrix",
		"--target", toyTargetYAML, "--sequences", bugSequences(t, 0, 1), "--out", matrixFile(t))

	if code != exitOK {
		t.Fatalf("reconciler-fuzzer matrix exited %d: %s", code, stderr)
	}
	if want := "B0: b0.json fired nothing\n  " + note + "\n"; !strings.Contains(stdout, want) {
		t.Errorf("reconciler-fuzzer matrix printed %q, want %q.", stdout, want)
	}
}

func TestMatrixListsTheBugsInCatalogOrder(t *testing.T) {
	session := &fakeSession{results: []run.Result{
		recorded(t, true), recorded(t, false), recorded(t, true), recorded(t, false), recorded(t, true),
	}}
	out := matrixFile(t)

	code, _, stderr := invoke(t, session, "matrix",
		"--target", toyTargetYAML, "--sequences", bugSequences(t, 0, 2, 10), "--out", out)

	if code != exitOK {
		t.Fatalf("reconciler-fuzzer matrix exited %d: %s", code, stderr)
	}
	var listed []string
	for _, row := range regexp.MustCompile(`(?m)^\| (B\d+) \|`).FindAllStringSubmatch(readMatrix(t, out), -1) {
		listed = append(listed, row[1])
	}
	if want := []string{"B0", "B2", "B10"}; !slices.Equal(listed, want) {
		t.Errorf("The matrix lists %v, want %v: the bugs come in catalog order, not in the order the files are named.",
			listed, want)
	}
}

// The acceptance: some check catches each bug, and none fires against the toy
// with no bug.
func TestMatrixFailsOnARowThatBreaksTheAcceptance(t *testing.T) {
	for _, test := range []struct {
		name    string
		results []run.Result
		want    []string
	}{
		{
			name:    "a bug nothing caught",
			results: []run.Result{recorded(t, true), recorded(t, true), recorded(t, true)},
			want:    []string{"B1: b1.json under --bug=1 fired nothing; reproduce it with\n  reconciler-fuzzer replay"},
		},
		{
			name:    "a control something caught",
			results: []run.Result{recorded(t, false), recorded(t, false), recorded(t, true)},
			want:    []string{"B0: b0.json fired G4, G6; reproduce it with\n  reconciler-fuzzer replay"},
		},
		{
			name:    "a sequence something caught against the toy with no bug",
			results: []run.Result{recorded(t, true), recorded(t, false), recorded(t, false)},
			want:    []string{"B1: b1.json without --bug=1 fired G4, G6; reproduce it with\n  reconciler-fuzzer replay"},
		},
		{
			name:    "both runs of one sequence",
			results: []run.Result{recorded(t, true), recorded(t, true), recorded(t, false)},
			want:    []string{"B1: b1.json under --bug=1 fired nothing; reproduce it with\n  reconciler-fuzzer replay", "B1: b1.json without --bug=1 fired G4, G6; reproduce it with\n  reconciler-fuzzer replay"},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			session := &fakeSession{results: test.results}
			out := matrixFile(t)

			code, _, stderr := invoke(t, session, "matrix",
				"--target", toyTargetYAML, "--sequences", bugSequences(t, 0, 1), "--out", out)

			if code != exitViolation {
				t.Errorf("reconciler-fuzzer matrix exited %d, want %d.", code, exitViolation)
			}
			for _, want := range test.want {
				if !strings.Contains(stderr, want) {
					t.Errorf("reconciler-fuzzer matrix reported %q, want %q.", stderr, want)
				}
			}
			if readMatrix(t, out) == "" {
				t.Errorf("reconciler-fuzzer matrix wrote no matrix, want the one it judged.")
			}
		})
	}
}

// replay is a replay command the matrix printed.
type replay struct {
	options
	sequences []string
}

// replayed parses each replay command the matrix printed.
func replayed(t *testing.T, stderr string) []replay {
	t.Helper()
	var commands []replay
	for _, line := range strings.Split(stderr, "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "reconciler-fuzzer replay ") {
			continue
		}
		command, sequences, err := parse(shellWords(t, line)[1:])
		if err != nil {
			t.Fatalf("reconciler-fuzzer cannot parse the replay command %q: %v", line, err)
		}
		commands = append(commands, replay{command, sequences})
	}
	return commands
}

// A sequence that fails the toy with no bug proves nothing about its bug.
func TestMatrixFailsWhereASequenceFiresAgainstTheToyWithNoBug(t *testing.T) {
	session := &fakeSession{results: []run.Result{recorded(t, true), recorded(t, false), recorded(t, false)}}
	out := matrixFile(t)

	code, _, stderr := invoke(t, session, "matrix",
		"--target", toyTargetYAML, "--sequences", bugSequences(t, 0, 1), "--out", out,
		"--kubeconfig", "kind.kubeconfig", "--launch-arg", "--x=1")

	if code != exitViolation {
		t.Errorf("reconciler-fuzzer matrix exited %d, want %d.", code, exitViolation)
	}
	if !strings.Contains(stderr, "B1: b1.json without --bug=1 fired G4, G6") {
		t.Errorf("reconciler-fuzzer matrix reported %q, want it to name B1's run without its bug.", stderr)
	}
	commands := replayed(t, stderr)
	if len(commands) != 1 || commands[0].kubeconfig != "kind.kubeconfig" || !slices.Equal(commands[0].launchArgs, []string{"--x=1"}) {
		t.Errorf("reconciler-fuzzer matrix printed the replay commands %+v, want one with the kubeconfig and --x=1 and no --bug.", commands)
	}
	if written := readMatrix(t, out); !strings.Contains(written, "| B1 |  |  |  | ✓ |  | ✓ |  |  | G4 ✓, G6 ✓ |") {
		t.Errorf("The matrix is\n%s\nwant B1's no-bug cell to name the checks that fired.", written)
	}
}

// A bug nothing caught replays under its bug, with every flag the matrix ran.
func TestMatrixReplaysABugNothingCaughtUnderItsBug(t *testing.T) {
	session := &fakeSession{results: []run.Result{recorded(t, true), recorded(t, true), recorded(t, true)}}

	code, _, stderr := invoke(t, session, "matrix",
		"--target", toyTargetYAML, "--sequences", bugSequences(t, 0, 1), "--out", matrixFile(t),
		"--kubeconfig", "kind.kubeconfig", "--launch-arg", "--x=1")

	if code != exitViolation {
		t.Errorf("reconciler-fuzzer matrix exited %d, want %d.", code, exitViolation)
	}
	commands := replayed(t, stderr)
	if len(commands) != 1 || commands[0].kubeconfig != "kind.kubeconfig" ||
		!slices.Equal(commands[0].launchArgs, []string{"--x=1", "--bug=1"}) {
		t.Errorf("reconciler-fuzzer matrix printed the replay commands %+v, want one with the kubeconfig, --x=1 and --bug=1.", commands)
	}
}

// recordedWithABrokenProperty is a run whose property cannot be evaluated.
func recordedWithABrokenProperty(t *testing.T) run.Result {
	t.Helper()
	result := recorded(t, true)
	broken := *result.Recorded.Target
	broken.Properties = []target.Property{{ID: "P1", Eval: func(*unstructured.Unstructured, []*unstructured.Unstructured) (bool, error) {
		return false, errors.New("the predicate broke")
	}}}
	result.Recorded.Target = &broken
	return result
}

// A run that errored judged nothing, so the matrix neither passes it nor
// writes it. It keeps only that run's files, and says how to run it again.
func TestMatrixExitsTwoWhereARunErrors(t *testing.T) {
	stopped := errors.New("the target stopped")
	for _, test := range []struct {
		name     string
		flags    []string
		results  []run.Result
		failures []error
		want     string
		replay   string
		bugArgs  []string
	}{
		{
			name:     "under the bug",
			results:  []run.Result{recorded(t, true), recorded(t, false), recorded(t, true)},
			failures: []error{nil, stopped},
			want:     "b1.json under --bug=1: the target stopped",
			replay:   "b1.json",
			bugArgs:  []string{"--bug=1"},
		},
		{
			name:     "without the bug",
			results:  []run.Result{recorded(t, true), recorded(t, false), recorded(t, true)},
			failures: []error{nil, nil, stopped},
			want:     "b1.json without --bug=1: the target stopped",
			replay:   "b1.json",
		},
		{
			name:     "in the control",
			results:  []run.Result{recorded(t, true)},
			failures: []error{stopped},
			want:     "b0.json: the target stopped",
			replay:   "b0.json",
		},
		{
			name:    "in the checks under the bug",
			results: []run.Result{recorded(t, true), recordedWithABrokenProperty(t)},
			want:    "b1.json under --bug=1: evaluating property P1: the predicate broke",
			replay:  "b1.json",
			bugArgs: []string{"--bug=1"},
		},
		{
			name:    "in the checks without the bug",
			results: []run.Result{recorded(t, true), recorded(t, false), recordedWithABrokenProperty(t)},
			want:    "b1.json without --bug=1: evaluating property P1: the predicate broke",
			replay:  "b1.json",
		},
		{
			name:     "at the deadline",
			flags:    []string{"--deadline", "1ns"},
			failures: []error{fmt.Errorf("op 0 (create): %w", context.DeadlineExceeded)},
			want:     "b0.json: the --deadline of 1ns ended the run: op 0 (create): context deadline exceeded",
			replay:   "b0.json",
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			tmp := t.TempDir()
			t.Setenv("TMPDIR", tmp)
			session := &fakeSession{results: test.results, failures: test.failures}
			session.after = func() {
				if err := os.WriteFile(filepath.Join(session.dirs[len(session.dirs)-1], "target.log"), nil, 0o644); err != nil {
					t.Fatal(err)
				}
			}
			sequences, out := bugSequences(t, 0, 1), matrixFile(t)

			code, _, stderr := invoke(t, session, append([]string{"matrix",
				"--target", toyTargetYAML, "--sequences", sequences, "--out", out, "--launch-arg", "--x=1"}, test.flags...)...)

			if code != exitError {
				t.Errorf("reconciler-fuzzer matrix exited %d, want %d.", code, exitError)
			}
			if _, err := os.Stat(out); !errors.Is(err, fs.ErrNotExist) {
				t.Errorf("reconciler-fuzzer matrix wrote %s, want no matrix of a run that errored.", out)
			}
			kept, finished := session.dirs[len(session.dirs)-1], session.dirs[:len(session.dirs)-1]
			if want := "reconciler-fuzzer: " + test.want + "\n  the run's files are in " + kept + "\n  run it again with\n"; !strings.Contains(stderr, want) {
				t.Errorf("reconciler-fuzzer matrix reported\n%s\nwant\n%s", stderr, want)
			}
			commands := replayed(t, stderr)
			if len(commands) != 1 || !slices.Equal(commands[0].launchArgs, append([]string{"--x=1"}, test.bugArgs...)) ||
				!slices.Equal(commands[0].sequences, []string{filepath.Join(sequences, test.replay)}) {
				t.Errorf("reconciler-fuzzer matrix printed the replay commands %+v, want one of %s with --x=1 and %v.", commands, test.replay, test.bugArgs)
			}
			if !strings.HasPrefix(kept, tmp+string(filepath.Separator)) {
				t.Errorf("reconciler-fuzzer matrix kept %s, want it under %s.", kept, tmp)
			}
			if _, err := os.Stat(filepath.Join(kept, "target.log")); err != nil {
				t.Errorf("reconciler-fuzzer matrix removed the target.log of the run that erred: %v", err)
			}
			if parent, err := os.Stat(filepath.Dir(kept)); err != nil || parent.Mode().Perm()&0o077 != 0 {
				t.Errorf("reconciler-fuzzer matrix kept %s where other users can read it (%v).", kept, err)
			}
			for _, dir := range finished {
				if _, err := os.Stat(dir); !errors.Is(err, fs.ErrNotExist) {
					t.Errorf("reconciler-fuzzer matrix kept %s, the files of a run that finished.", dir)
				}
			}
		})
	}
}

func TestMatrixCleansUpItsTemporaryDirectory(t *testing.T) {
	for _, test := range []struct {
		name    string
		session *fakeSession
		want    int
	}{
		{
			name:    "a matrix that passes",
			session: &fakeSession{results: []run.Result{recorded(t, true), recorded(t, false), recorded(t, true)}},
			want:    exitOK,
		},
		{
			name:    "a matrix that fails",
			session: &fakeSession{results: []run.Result{recorded(t, true), recorded(t, true), recorded(t, true)}},
			want:    exitViolation,
		},
		{
			name: "a run that erred before it wrote anything",
			session: &fakeSession{
				failures:      []error{errors.New("the sequence is for another target")},
				writesNothing: true,
			},
			want: exitError,
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			tmp := t.TempDir()
			t.Setenv("TMPDIR", tmp)

			code, _, stderr := invoke(t, test.session, "matrix",
				"--target", toyTargetYAML, "--sequences", bugSequences(t, 0, 1), "--out", matrixFile(t))

			if code != test.want {
				t.Errorf("reconciler-fuzzer matrix exited %d, want %d: %s", code, test.want, stderr)
			}
			for _, dir := range test.session.dirs {
				if !strings.HasPrefix(dir, tmp+string(filepath.Separator)) {
					t.Errorf("reconciler-fuzzer matrix ran in %s, want a directory under %s.", dir, tmp)
				}
			}
			if left, err := os.ReadDir(tmp); err != nil || len(left) > 0 {
				t.Errorf("reconciler-fuzzer matrix left %v in the temporary directory (%v), want nothing.", left, err)
			}
			if strings.Contains(stderr, "files are in") {
				t.Errorf("reconciler-fuzzer matrix reported %q, which names files it did not keep.", stderr)
			}
		})
	}
}

func TestMatrixNamesTheRunAnInterruptStopped(t *testing.T) {
	ctx, cancel := context.WithCancelCause(t.Context())
	session := &fakeSession{
		results:  []run.Result{recorded(t, true), recorded(t, false)},
		failures: []error{nil, fmt.Errorf("op 0 (settle): %w", context.Canceled)},
	}
	session.after = func() {
		if len(session.sequences) == 2 {
			cancel(interrupt{syscall.SIGINT})
		}
	}

	code, _, stderr := invokeCtx(t, ctx, session, countingGenerator(nil), "matrix",
		"--target", toyTargetYAML, "--sequences", bugSequences(t, 0, 1), "--out", matrixFile(t))

	if code != 128+int(syscall.SIGINT) {
		t.Errorf("reconciler-fuzzer matrix exited %d, want %d.", code, 128+int(syscall.SIGINT))
	}
	if want := "b1.json under --bug=1: an interrupt stopped the run"; !strings.Contains(stderr, want) {
		t.Errorf("reconciler-fuzzer matrix reported %q, want %q.", stderr, want)
	}
}

// A matrix minimizes nothing, so its runs get what they can take.
func TestMatrixDerivesItsDeadlineFromItsRuns(t *testing.T) {
	toy, err := target.Load(toyTargetYAML)
	if err != nil {
		t.Fatal(err)
	}
	sequences := bugSequences(t, 0)
	longer := run.Sequence{Seed: 1, Target: "toy-widget", Ops: []run.Op{{Type: run.OpSettle}, {Index: 1, Type: run.OpSettle}}}
	if err := run.WriteSequence(filepath.Join(sequences, "b1.json"), longer); err != nil {
		t.Fatal(err)
	}
	session := &fakeSession{results: []run.Result{recorded(t, true), recorded(t, false), recorded(t, true)}}
	before := time.Now()

	code, stdout, stderr := invoke(t, session, "matrix",
		"--target", toyTargetYAML, "--sequences", sequences, "--out", matrixFile(t))

	after := time.Now()
	if code != exitOK {
		t.Fatalf("reconciler-fuzzer matrix exited %d: %s", code, stderr)
	}
	var want time.Duration
	for _, sequence := range session.sequences {
		want += run.Bound(toy, sequence)
	}
	if deadline := session.deadlines[0]; deadline.Before(before.Add(want)) || deadline.After(after.Add(want)) {
		t.Errorf("The runs had %v, want %v.", deadline.Sub(before), want)
	}
	says := fmt.Sprintf("the deadline is %s: these 3 runs can take that long at the target's timeouts. --deadline sets another.\n", want)
	if !strings.Contains(stdout, says) {
		t.Errorf("reconciler-fuzzer matrix printed %q, want %q.", stdout, says)
	}
}

func TestMatrixConfigurationErrorsExitTwo(t *testing.T) {
	for _, test := range []struct {
		name      string
		sequences string
		extra     []string
		want      string
	}{
		{name: "no --sequences", want: "--sequences"},
		{name: "a sequence file as well", sequences: bugSequences(t, 0, 1), extra: []string{"b1.json"}, want: "no sequence file"},
		{name: "a directory that does not exist", sequences: "absent", want: "absent"},
		{name: "a directory holding no sequence", sequences: t.TempDir(), want: "b<id>.json"},
		{name: "no control sequence", sequences: bugSequences(t, 1, 2), want: "b0.json"},
	} {
		t.Run(test.name, func(t *testing.T) {
			args := []string{"matrix", "--target", toyTargetYAML, "--out", matrixFile(t)}
			if test.sequences != "" {
				args = append(args, "--sequences", test.sequences)
			}
			args = append(args, test.extra...)

			code, _, stderr := invoke(t, &fakeSession{}, args...)

			if code != exitError {
				t.Errorf("reconciler-fuzzer matrix exited %d, want %d.", code, exitError)
			}
			if !strings.Contains(stderr, test.want) {
				t.Errorf("reconciler-fuzzer matrix reported %q, want it to name %q.", stderr, test.want)
			}
		})
	}
}

// The sequences the matrix runs: the shortest that exposes each bug, in the
// canonical form of DESIGN.md §7, and needing no fault (§10, M3).
func TestTheSeededBugSequencesAreShortAndFaultless(t *testing.T) {
	const dir = "../../targets/toy-widget/sequences"
	rows, err := readBugSequences(dir)
	if err != nil {
		t.Fatalf("Reading the toy's sequences failed: %v", err)
	}
	if len(rows) < 2 {
		t.Fatalf("The toy holds %d sequences, want one per seeded bug and a control.", len(rows))
	}
	for _, row := range rows {
		t.Run(row.name(), func(t *testing.T) {
			if row.sequence.Target != "toy-widget" {
				t.Errorf("%s runs the target %q.", row.file, row.sequence.Target)
			}
			if len(row.sequence.Ops) > 6 {
				t.Errorf("%s holds %d ops, want the shortest sequence that exposes the bug.", row.file, len(row.sequence.Ops))
			}
			for _, op := range row.sequence.Ops {
				if op.Type == run.OpFault {
					t.Errorf("%s injects a fault at op %d, and faults arrive in M6.", row.file, op.Index)
				}
			}
			canonical, err := row.sequence.Marshal()
			if err != nil {
				t.Fatalf("Marshalling %s failed: %v", row.file, err)
			}
			written, err := os.ReadFile(filepath.Join(dir, row.file))
			if err != nil {
				t.Fatalf("Reading %s failed: %v", row.file, err)
			}
			if string(written) != string(canonical) {
				t.Errorf("%s is not in the canonical form of §7:\n%s\nwant\n%s", row.file, written, canonical)
			}
		})
	}
}

// A matrix run ends at no violation, so that the whole sequence executes and
// every check the run trips is recorded.
func TestMatrixChecksWithoutEndingTheRun(t *testing.T) {
	session := &fakeSession{results: []run.Result{recorded(t, true), recorded(t, false), recorded(t, true)}}

	code, _, stderr := invoke(t, session, "matrix",
		"--target", toyTargetYAML, "--sequences", bugSequences(t, 0, 1), "--out", matrixFile(t))

	if code != exitOK {
		t.Fatalf("reconciler-fuzzer matrix exited %d: %s", code, stderr)
	}
	if len(session.checks) == 0 {
		t.Fatalf("The matrix ran no sequence.")
	}
	for _, check := range session.checks {
		found, err := check.Check(run.Input{})
		if len(found.Violations) > 0 || err != nil {
			t.Errorf("The matrix checked with %v, which reported %v, %v: a matrix run ends at no violation.",
				check, found.Violations, err)
		}
	}
}

func TestMatrixWarnsOfTheWorkloadsItManages(t *testing.T) {
	session := &fakeSession{results: []run.Result{recorded(t, true)}}

	code, _, stderr := invoke(t, session, "matrix",
		"--target", workloadsTargetYAML, "--sequences", bugSequences(t, 0), "--out", matrixFile(t))

	if code != exitOK {
		t.Fatalf("reconciler-fuzzer matrix exited %d: %s", code, stderr)
	}
	if !strings.Contains(stderr, "kubelet") {
		t.Errorf("reconciler-fuzzer matrix printed %q on stderr, want the warning of what envtest never runs.", stderr)
	}
}
