package main

import (
	"bytes"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"math"
	"math/rand/v2"
	"os"
	"path/filepath"
	"regexp"
	"runtime/debug"
	"slices"
	"strings"
	"time"

	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/runtime/schema"

	"github.com/rosenhouse-bot-sandbox/reconciler-fuzzer/internal/cluster"
	"github.com/rosenhouse-bot-sandbox/reconciler-fuzzer/internal/generate"
	"github.com/rosenhouse-bot-sandbox/reconciler-fuzzer/internal/report"
	"github.com/rosenhouse-bot-sandbox/reconciler-fuzzer/internal/run"
	"github.com/rosenhouse-bot-sandbox/reconciler-fuzzer/internal/target"
)

// reconciler-fuzzer exits with one of these codes.
const (
	exitOK        = 0
	exitViolation = 1
	exitError     = 2
)

const (
	defaultOut  = "reconciler-fuzzer-out"
	defaultRuns = 10
	// minimizing is what a derived deadline gives a shrink pass beyond the runs.
	minimizing = 4 * time.Minute
)

const (
	// shrinkDir holds a shrink pass's replays, which are deleted before the
	// report is written.
	shrinkDir = "shrink"
	// sequenceFile is what run.WriteRunSequence writes.
	sequenceFile = "sequence.json"
	// shrunkFile holds a minimized sequence that the deadline or an interrupt
	// left unrun, so no recording in the directory comes from it.
	shrunkFile = "sequence.shrunk.json"
)

// Generator builds the sequences of an invocation that names no sequence
// file, deterministically, for the target it was built for.
type Generator interface {
	Draw(seed int64) (run.Sequence, error)
	// Baseline is the sequence that runs after the drawn ones.
	Baseline() (run.Sequence, error)
	// LeftAlone says which spec paths generation never changes, and why.
	LeftAlone() []string
}

// rapidGenerator draws sequences from the target's CRD schema. It reads the
// CRDs once, because a draw itself does no I/O.
func rapidGenerator(t *target.Target) (Generator, error) {
	g, err := generate.New(t, generate.Options{})
	if err != nil {
		return nil, err
	}
	return g, nil
}

// cli is one invocation. Its writers, its generator, its test cluster, its
// clock and what discards a passing run are injected, so the unit tier needs
// no API server.
type cli struct {
	stdout, stderr io.Writer
	open           func(options, *target.Target) (session, error)
	// newGenerator builds the generator the invocation draws from, once, as
	// pkg/generate's does: reading the target's CRDs costs I/O, drawing does
	// not.
	newGenerator func(*target.Target) (Generator, error)
	now          func() time.Time
	discard      func(out *run.Output, n int) error
}

func newCLI(stdout, stderr io.Writer) *cli {
	return &cli{stdout: stdout, stderr: stderr, open: openSession, newGenerator: rapidGenerator,
		now: time.Now, discard: (*run.Output).Discard}
}

// session executes sequences against one test cluster. Runs share it, because
// starting a control plane costs seconds.
type session interface {
	// vet refuses a target the cluster cannot run, before any run starts.
	vet(t *target.Target) error
	// prepare creates the cluster-scoped fixtures that all runs share.
	prepare(ctx context.Context, t *target.Target) error
	execute(ctx context.Context, t *target.Target, sequence run.Sequence, dir string, check run.Checker) (run.Result, error)
	close() error
}

// options are the flags of every command.
type options struct {
	command       string
	target        string
	sequences     string
	out           string
	junit         string
	kubeconfig    string
	deadline      time.Duration
	deadlineGiven bool
	launchArgs    []string
	runs          int
	runsGiven     bool
	seed          int64
	seedGiven     bool
	noBaseline    bool
}

// main returns what reconciler-fuzzer exits with. An invocation a signal
// interrupted returns 128 plus the signal's number, as a shell reports it.
func (c *cli) main(ctx context.Context, args []string) int {
	return exitStatus(ctx, c.dispatch(ctx, args))
}

func exitStatus(ctx context.Context, code int) int {
	if stop, ok := interruption(ctx); ok {
		return 128 + int(stop.signal)
	}
	return code
}

func (c *cli) dispatch(ctx context.Context, args []string) int {
	if len(args) == 0 {
		fmt.Fprint(c.stderr, help(""))
		return exitError
	}
	opts, paths, err := parse(args)
	if errors.Is(err, flag.ErrHelp) {
		fmt.Fprint(c.stdout, help(opts.command))
		return exitOK
	}
	if err != nil {
		code := c.failUnrecorded(opts, nil, err)
		fmt.Fprint(c.stderr, usage(opts.command))
		return code
	}
	switch opts.command {
	case "version":
		fmt.Fprintf(c.stdout, "reconciler-fuzzer %s\n", version())
		return exitOK
	case "matrix":
		return c.bugMatrix(ctx, opts)
	}
	return c.exercise(ctx, opts, paths)
}

// exercise executes the sequences the caller named, or the ones
// reconciler-fuzzer draws from the seed, and stops at the first that fails.
// Once the runs are planned, the invocation's directory records how each ended.
func (c *cli) exercise(ctx context.Context, opts options, paths []string) int {
	exercised, err := target.Load(opts.target)
	if err != nil {
		return c.failUnrecorded(opts, nil, err)
	}
	exercised.Launch.Args = append(exercised.Launch.Args, opts.launchArgs...)
	if len(paths) == 0 && !opts.seedGiven {
		opts.seed = newSeed()
	}
	runs, err := c.plan(opts, exercised, paths)
	if err != nil {
		return c.failUnrecorded(opts, exercised, err)
	}
	start := c.now()
	out, err := run.OpenOutput(opts.out, opts.invocationSeed(runs[0].sequence), start)
	if err != nil {
		return c.failUnrecorded(opts, exercised, err)
	}

	record := newSummary(opts, exercised, runs, start)
	// The last save warns of a file reconciler-fuzzer cannot write.
	_ = c.save(record, opts, out.Dir())
	var code int
	s, err := c.startSession(opts, exercised)
	if err != nil {
		code = c.stop(record, err)
	} else {
		code = c.runAll(ctx, opts, s, exercised, runs, out, record)
	}
	record.finish(ctx, code, c.now())
	// A second signal kills reconciler-fuzzer at once, and stopping the cluster
	// takes seconds, so the summary comes first.
	c.warn(c.save(record, opts, out.Dir()))
	if s != nil {
		c.warn(s.close())
	}
	// An interrupt that arrived since changes what reconciler-fuzzer exits with.
	if exitStatus(ctx, code) != *record.ExitCode {
		record.finish(ctx, code, record.Finish)
		c.warn(c.save(record, opts, out.Dir()))
	}
	return code
}

// save writes the summary, and the JUnit file if one was asked for.
func (c *cli) save(record *summary, opts options, dir string) error {
	err := record.write(dir)
	if opts.junit != "" {
		err = errors.Join(err, record.writeJUnit(opts.junit, dir))
	}
	return err
}

// runAll executes the runs in order, records each, and stops at the first
// that fails.
func (c *cli) runAll(ctx context.Context, opts options, s session, t *target.Target,
	runs []planned, out *run.Output, record *summary) int {
	if err := s.vet(t); err != nil {
		return c.stop(record, err)
	}
	if err := s.prepare(ctx, t); err != nil {
		return c.stop(record, err)
	}
	sequences := make([]run.Sequence, len(runs))
	for i, planned := range runs {
		sequences[i] = planned.sequence
	}
	c.derive(&opts, t, sequences, runs[0].generated())
	record.deadline(opts)

	ctx, cancel := context.WithTimeout(ctx, opts.deadline)
	defer cancel()
	for i, planned := range runs {
		if _, ok := interruption(ctx); ok {
			return c.stop(record, fmt.Errorf("an interrupt stopped the invocation after %d of %d runs", i, len(runs)))
		}
		// The deadline is the invocation's budget: the next run does not
		// start. The first always starts, so a spent deadline is blamed on a
		// run. An invocation the deadline stopped tested less than asked, so
		// it does not pass.
		if i > 0 && ctx.Err() != nil {
			return c.stop(record, fmt.Errorf("%s stopped the invocation after %d of %d runs", opts.deadlineName(), i, len(runs)))
		}
		number := i + 1
		fmt.Fprintf(c.stdout, "run %d: %s\n", number, planned.describe())
		dir := out.RunDir(number)
		ran := &record.Runs[i]
		ran.Outcome = outcomeUnfinished
		_ = c.save(record, opts, out.Dir())
		started := c.now()
		result, err := s.execute(ctx, t, planned.sequence, dir, run.Engine{})
		ran.ran(result, c.now().Sub(started))
		code := exitCode(result, err)
		if code == exitViolation {
			// Minimizing can take minutes, so the summary keeps the find first.
			ran.found(*result.Violation, reportNotes(opts, t, result), dir)
			_ = c.save(record, opts, out.Dir())
			violation, notes := c.reportFailure(ctx, opts, s, t, planned, result, number, dir, func() {
				ran.rerunning = true
				_ = c.save(record, opts, out.Dir())
			})
			ran.found(violation, notes, dir)
			return exitViolation
		}
		c.printNotes(number, result.Notes)
		if code == exitError {
			err = opts.named(ctx, err)
			ran.stopped(err, dir)
			return c.failRun(number, planned, dir, err)
		}
		ran.Outcome = outcomePassed
		if err := c.discard(out, number); err != nil {
			return c.stop(record, err)
		}
	}
	fmt.Fprintln(c.stdout, "every run passed.")
	return exitOK
}

// failUnrecorded ends an invocation that has no directory to write its summary
// in. Its JUnit file still says what stopped it.
func (c *cli) failUnrecorded(opts options, t *target.Target, err error) int {
	if opts.junit != "" {
		now := c.now().UTC()
		record := &summary{ReconcilerFuzzer: version(), Target: summaryTarget{Name: "reconciler-fuzzer"}, Start: now, Finish: now, Error: err.Error()}
		if t != nil {
			record.Target.Name = t.Name
		}
		c.warn(record.writeJUnit(opts.junit, ""))
	}
	return c.fail(err)
}

// stop ends the invocation on an error no run carries.
func (c *cli) stop(record *summary, err error) int {
	record.Error = err.Error()
	return c.fail(err)
}

// planned is one run's sequence and the file it was read from.
// reconciler-fuzzer generated the sequence of a run that names no file: it
// drew it from a seed, or it is the baseline.
type planned struct {
	sequence run.Sequence
	path     string
	baseline bool
}

func (p planned) generated() bool { return p.path == "" }

// describe is how a run's first line names its sequence.
func (p planned) describe() string {
	switch {
	case p.baseline:
		return "baseline"
	case p.generated():
		return fmt.Sprintf("seed %d, generated", p.sequence.Seed)
	}
	return fmt.Sprintf("seed %d, sequence %s", p.sequence.Seed, p.path)
}

// plan reads the sequences the caller named, or draws one per run from
// consecutive seeds, so that each run replays from its own, and then adds the
// baseline.
func (c *cli) plan(opts options, t *target.Target, paths []string) ([]planned, error) {
	if len(paths) > 0 {
		sequences, err := readSequences(paths)
		if err != nil {
			return nil, err
		}
		runs := make([]planned, len(sequences))
		for i, sequence := range sequences {
			runs[i] = planned{sequence: sequence, path: paths[i]}
		}
		return runs, nil
	}
	g, err := c.newGenerator(t)
	if err != nil {
		return nil, err
	}
	for _, note := range g.LeftAlone() {
		fmt.Fprintln(c.stdout, note)
	}
	runs := make([]planned, opts.runs)
	for i := range runs {
		sequence, err := g.Draw(opts.seed + int64(i))
		if err != nil {
			return nil, fmt.Errorf("generating run %d: %w", i+1, err)
		}
		runs[i] = planned{sequence: sequence}
	}
	if opts.noBaseline {
		return runs, nil
	}
	sequence, err := g.Baseline()
	if err != nil {
		return nil, err
	}
	return append(runs, planned{sequence: sequence, baseline: true}), nil
}

// reportFailure minimizes a sequence reconciler-fuzzer generated and leaves it in
// the run directory with the evidence of a run of it. A sequence the caller
// wrote is reported as it was written. It calls rerunning before it runs the
// minimized sequence, and returns the violation and notes the report carries.
func (c *cli) reportFailure(ctx context.Context, opts options, s session, t *target.Target,
	failed planned, result run.Result, number int, dir string, rerunning func()) (run.Violation, []string) {
	violation := *result.Violation
	if !failed.generated() {
		// The caller's file is a better thing to replay than a copy of it.
		c.warn(c.writeReport(dir, opts, t, failed.path, failed, result))
		c.printNotes(number, result.Notes)
		c.report(number, violation, dir)
		return violation, reportNotes(opts, t, result)
	}
	if len(failed.sequence.Ops) > 1 {
		fmt.Fprintf(c.stdout, "run %d: %s failed, and minimizing its %s can take minutes.\n", number, violation.ID, ops(failed.sequence))
	}
	shrunk := run.Shrink(ctx, failed.sequence, violation, func(ctx context.Context, candidate run.Sequence) (run.Result, error) {
		return s.execute(ctx, t, candidate, filepath.Join(dir, shrinkDir), run.Engine{})
	})
	reported := shrunk
	simplified := simpler(shrunk, failed.sequence)
	// notes are the reported run's. The report adds notes of its own below.
	notes := result.Notes
	switch {
	case ctx.Err() != nil && simplified:
		// The deadline ended the pass before the smaller sequence could be
		// run into the directory, which still holds the run of the sequence
		// reconciler-fuzzer drew. The directory reports the sequence its evidence is
		// of, and keeps the smaller one beside it.
		reported = failed.sequence
		c.warn(run.WriteSequence(filepath.Join(dir, shrunkFile), shrunk))
		c.warn(fmt.Errorf("%s ended the shrink pass with %s, left unrun in %s",
			ended(ctx), ops(shrunk), filepath.Join(dir, shrunkFile)))
		result.Notes = append(result.Notes, fmt.Sprintf(
			"%s ended minimization with %s, left unrun in %s: this is the sequence reconciler-fuzzer generated",
			ended(ctx), ops(shrunk), shrunkFile))
	case ctx.Err() != nil:
		// A reader takes a report's sequence for the minimized one, and the
		// pass never got to a smaller one.
		result.Notes = append(result.Notes, ended(ctx)+
			" ended minimization before it found a smaller sequence: this is the sequence reconciler-fuzzer generated")
	case simplified:
		rerunning()
		again, err := c.rerun(ctx, opts, s, t, shrunk, dir)
		switch {
		case again.Violation != nil:
			result, violation, notes = again, *again.Violation, again.Notes
		case err != nil:
			result.Timeline = again.Timeline
			result.Notes = append(result.Notes, fmt.Sprintf(
				"the minimized sequence did not finish when it ran again, so this directory holds that partial run and not the one %s was found in",
				violation.ID))
		default:
			// The recordings are of the rerun, so the report counts its ops.
			result.Timeline = again.Timeline
			// The directory now holds a run of the minimized sequence that
			// found nothing. The finding stands, and the report says which
			// run these recordings are of rather than leaving a reader to
			// infer it from a passing log.
			result.Notes = append(result.Notes, fmt.Sprintf(
				"the minimized sequence passed when it ran again, so this directory holds that run and not the one %s was found in",
				violation.ID))
		}
	}
	// The shrink pass's replays back no report, so they are deleted.
	c.warn(os.RemoveAll(filepath.Join(dir, shrinkDir)))
	c.warn(run.WriteRunSequence(dir, reported))
	c.warn(c.writeReport(dir, opts, t, filepath.Join(dir, sequenceFile), planned{sequence: reported, baseline: failed.baseline}, result))
	// The violation is reported once the directory holds the run it belongs to.
	c.printNotes(number, notes)
	c.report(number, violation, dir)
	fmt.Fprintf(c.stdout, "  the sequence is %s, in %s\n", ops(reported), filepath.Join(dir, sequenceFile))
	return violation, reportNotes(opts, t, result)
}

// rerun executes the minimized sequence into the run directory, in place of
// what it held, so that the recordings there are of the sequence the run
// reports, and returns what that run found. A run that did not finish or
// reproduced nothing says what the directory then holds.
func (c *cli) rerun(ctx context.Context, opts options, s session, t *target.Target, shrunk run.Sequence, dir string) (run.Result, error) {
	// A run writes its recordings as it ends.
	c.warn(os.RemoveAll(dir))
	result, err := s.execute(ctx, t, shrunk, dir, run.Engine{})
	switch {
	case err != nil:
		c.warn(fmt.Errorf("the minimized sequence did not finish when it ran again, so %s holds that partial run: %w",
			dir, opts.named(ctx, err)))
	case result.Violation == nil:
		c.warn(fmt.Errorf("the minimized sequence passed when it ran again, so %s holds that run", dir))
	}
	return result, err
}

// replayCommand is the one line a report carries to reproduce the run. It
// repeats the flags that select what ran, because a command that leaves them
// out runs a different target and reproduces nothing.
func (o options) replayCommand(sequence string) string {
	command := []string{"reconciler-fuzzer", "replay", "--target", o.target}
	if o.kubeconfig != "" {
		command = append(command, "--kubeconfig", o.kubeconfig)
	}
	for _, arg := range o.launchArgs {
		command = append(command, "--launch-arg", arg)
	}
	if strings.HasPrefix(sequence, "-") {
		sequence = "./" + sequence
	}
	command = append(command, sequence)
	for i, word := range command {
		command[i] = shellQuote(word)
	}
	return strings.Join(command, " ")
}

var shellSafe = regexp.MustCompile(`^[A-Za-z0-9_./:@%+,-][A-Za-z0-9_./=:@%+,-]*$`)

// shellQuote single-quotes a word unless shellSafe shows that sh and zsh read
// it literally.
func shellQuote(word string) string {
	if shellSafe.MatchString(word) {
		return word
	}
	return "'" + strings.ReplaceAll(word, "'", `'\''`) + "'"
}

// writeReport leaves the report of the reported sequence beside the
// recordings it describes. replay names the sequence file a reader should run
// to see this again.
func (c *cli) writeReport(dir string, opts options, t *target.Target,
	replay string, reported planned, result run.Result) error {
	if result.Violation == nil {
		return nil
	}
	sequence := reported.sequence
	encoded, err := sequence.Marshal()
	if err != nil {
		return err
	}
	violation := *result.Violation
	return report.Write(dir, report.Report{
		Check:            report.Check{ID: violation.ID, Statement: violation.Statement, At: violation.At, Evidence: violation.Evidence},
		Target:           report.Target{Name: t.Name, Version: t.Version},
		ReconcilerFuzzer: version(),
		Seed:             sequence.Seed,
		Baseline:         reported.baseline,
		Notes:            reportNotes(opts, t, result),
		Replay:           opts.replayCommand(replay),
		Sequence:         encoded,
		Applied:          len(result.Timeline.Ops),
		Ops:              len(sequence.Ops),
		Collector:        opts.kubeconfig == "",
		Requests:         violation.Requests,
		RequestsTotal:    violation.RequestsTotal,
		Versions:         violation.Versions,
		VersionsTotal:    violation.VersionsTotal,
		VersionsOf:       violation.VersionsOf,
		Managed:          violation.Managed,
		ManagedTotal:     violation.ManagedTotal,
		Ready:            violation.Ready,
		Differences:      violation.Differences,
		DifferencesTotal: violation.DifferencesTotal,
		Compared:         violation.Compared,
	})
}

// reportNotes are the notes of a failing run's report.
func reportNotes(opts options, t *target.Target, result run.Result) []string {
	if limit := envtestLimit(opts, t); limit != "" && result.Violation.ID == "G4" {
		return slices.Concat(result.Notes, []string{limit})
	}
	return result.Notes
}

// warn reports what went wrong beside a finding, which stands whether or not
// the run directory could be tidied.
func (c *cli) warn(err error) {
	if joined, ok := err.(interface{ Unwrap() []error }); ok {
		for _, err := range joined.Unwrap() {
			c.warn(err)
		}
		return
	}
	if err != nil {
		fmt.Fprintln(c.stderr, "reconciler-fuzzer:", err)
	}
}

func ops(s run.Sequence) string {
	if len(s.Ops) == 1 {
		return "1 op"
	}
	return fmt.Sprintf("%d ops", len(s.Ops))
}

// newSeed is the seed of a run the caller gave none for. Every run prints its
// seed, so that the failure is reproducible from it.
func newSeed() int64 { return rand.Int64() }

func (c *cli) printNotes(number int, notes []string) {
	for _, note := range notes {
		fmt.Fprintf(c.stdout, "run %d: %s\n", number, note)
	}
}

func (c *cli) report(number int, violation run.Violation, dir string) {
	fmt.Fprintf(c.stdout, "run %d: %s %s\n", number, violation.ID, violation.Statement)
	if said := quotes(violation); said != "" {
		fmt.Fprintf(c.stdout, "  %s\n", said)
	}
	fmt.Fprintf(c.stdout, "  the evidence is in %s\n", dir)
}

// quotes is what the CLI prints under a violation: when the check judged, and
// what it quoted.
func quotes(violation run.Violation) string {
	var said []string
	if !violation.At.IsZero() {
		said = append(said, "at "+violation.At.Format(time.RFC3339Nano))
	}
	if violation.Evidence != "" {
		said = append(said, violation.Evidence)
	}
	return strings.Join(said, "; ")
}

func (c *cli) fail(err error) int {
	fmt.Fprintln(c.stderr, "reconciler-fuzzer:", err)
	return exitError
}

// failRun reports a run that could not finish and says where to look.
func (c *cli) failRun(number int, failed planned, dir string, err error) int {
	fmt.Fprintf(c.stderr, "reconciler-fuzzer: run %d: %v\n", number, err)
	var refused *run.Refused
	switch {
	case !errors.As(err, &refused):
		c.showRunFiles(dir)
	case failed.generated():
		fmt.Fprintf(c.stderr, "  the op is in %s\n", filepath.Join(dir, sequenceFile))
		fmt.Fprintln(c.stderr, "  reconciler-fuzzer drew it to pass the CRD's schema and rules without the status the controller"+
			" writes. So a CRD rule that reads status refused it, or a rule reconciler-fuzzer cannot see, such as an admission"+
			" webhook's. Keep drawn values inside that rule with generate.mutate or generate.overlay.")
	default:
		fmt.Fprintf(c.stderr, "  the op is in %s\n", failed.path)
	}
	return exitError
}

// showRunFiles prints the directory a failed run left, if it exists.
func (c *cli) showRunFiles(dir string) {
	if _, err := os.Stat(dir); err == nil {
		fmt.Fprintf(c.stderr, "  the run's files are in %s\n", dir)
	}
}

// exitCode maps a run's outcome to reconciler-fuzzer's exit codes.
func exitCode(result run.Result, err error) int {
	switch {
	case err != nil:
		return exitError
	case result.Violation != nil:
		return exitViolation
	default:
		return exitOK
	}
}

var errRunInterrupted = errors.New("an interrupt stopped the run")

// named blames an interrupt, or the deadline, for a run its context cut short.
// reconciler-fuzzer exits 2 at the deadline, as it does for a broken target, so
// the error has to say which. The teardown's cleanup runs on a budget of its
// own, so named reads ctx.Err() as well as err, and blames only the deadline
// reconciler-fuzzer set.
func (o options) named(ctx context.Context, err error) error {
	if _, ok := interruption(ctx); ok && errors.Is(err, context.Canceled) {
		return errRunInterrupted
	}
	if !errors.Is(err, context.DeadlineExceeded) || !errors.Is(ctx.Err(), context.DeadlineExceeded) {
		return err
	}
	return fmt.Errorf("%s ended the run: %w", o.deadlineName(), err)
}

// deadlineName names the deadline, and says whether reconciler-fuzzer derived
// it.
func (o options) deadlineName() string {
	if o.deadlineGiven {
		return "the --deadline of " + o.deadline.String()
	}
	return "the derived deadline of " + o.deadline.String()
}

// derive gives an invocation that set no deadline what its runs can take at
// the target's timeouts, and time to minimize a failure where reconciler-fuzzer
// drew the sequences.
func (c *cli) derive(opts *options, t *target.Target, sequences []run.Sequence, minimizes bool) {
	if opts.deadlineGiven {
		return
	}
	runs := run.Bound(t, sequences...)
	opts.deadline = runs
	these := "this run"
	if len(sequences) > 1 {
		these = fmt.Sprintf("these %d runs", len(sequences))
	}
	if !minimizes {
		fmt.Fprintf(c.stdout, "the deadline is %s: %s can take that long at the target's timeouts. --deadline sets another.\n",
			opts.deadline, these)
		return
	}
	opts.deadline = min(runs, math.MaxInt64-minimizing) + minimizing
	fmt.Fprintf(c.stdout, "the deadline is %s: %s can take %s at the target's timeouts, and minimizing a failure gets the rest, at least %s. --deadline sets another.\n",
		opts.deadline, these, runs, minimizing)
}

// ended names what ended ctx: an interrupt, or else the deadline.
func ended(ctx context.Context) string {
	if _, ok := interruption(ctx); ok {
		return "an interrupt"
	}
	return "the deadline"
}

// simpler reports whether the shrink pass changed the sequence at all. It
// removes ops and weakens faults, and a sequence the report carries has to be
// the one the directory holds a run of, however the pass simplified it.
func simpler(shrunk, failing run.Sequence) bool {
	was, err := failing.Marshal()
	if err != nil {
		return false
	}
	now, err := shrunk.Marshal()
	if err != nil {
		return false
	}
	return !bytes.Equal(was, now)
}

// invocationSeed names the output directory: the seed the caller gave, or the
// first sequence's, so that the directory is reproducible from it.
func (o options) invocationSeed(first run.Sequence) int64 {
	if o.seedGiven {
		return o.seed
	}
	return first.Seed
}

// parse reads an invocation of at least a command. Its errors are usage
// errors, and opts.command names the command whose usage they need, if any.
func parse(args []string) (options, []string, error) {
	if asksForHelp(args[0]) {
		return parseHelp(args[1:])
	}
	opts := options{command: args[0]}
	c, found := lookup(opts.command)
	if !found {
		return opts, nil, fmt.Errorf("%q is not a reconciler-fuzzer command", opts.command)
	}

	flags := opts.flags()
	if err := flags.Parse(args[1:]); err != nil {
		return opts, nil, twoDashes(err)
	}
	flags.Visit(func(f *flag.Flag) {
		opts.seedGiven = opts.seedGiven || f.Name == "seed"
		opts.runsGiven = opts.runsGiven || f.Name == "runs"
		opts.deadlineGiven = opts.deadlineGiven || f.Name == "deadline"
	})

	sequences := flags.Args()
	if misplaced, found := misplacedFlag(args[1:], sequences); found {
		switch {
		case asksForHelp(misplaced):
			return opts, nil, flag.ErrHelp
		case misplaced == "--":
			return opts, nil, fmt.Errorf("-- follows %s, and it goes before the first sequence file", sequences[0])
		}
		return opts, nil, fmt.Errorf("%s follows %s, and flags go first", misplaced, sequences[0])
	}
	for _, name := range c.required {
		if flags.Lookup(name).Value.String() == "" {
			return opts, nil, fmt.Errorf("the --%s flag is required", name)
		}
	}
	if opts.command == "version" && len(sequences) > 0 {
		return opts, nil, fmt.Errorf("version takes no argument: %s", strings.Join(sequences, " "))
	}
	if opts.deadlineGiven && opts.deadline <= 0 {
		return opts, nil, fmt.Errorf("--deadline is %s, and an invocation needs time to run: leave the flag out, and reconciler-fuzzer derives one", opts.deadline)
	}
	if opts.command == "run" {
		if err := opts.validateRuns(len(sequences)); err != nil {
			return opts, nil, err
		}
	}
	if opts.command == "replay" && len(sequences) != 1 {
		return opts, nil, fmt.Errorf("replay takes one sequence file, and %d were given", len(sequences))
	}
	if opts.command == "matrix" && len(sequences) > 0 {
		return opts, nil, fmt.Errorf("matrix takes no sequence file, since it runs the --sequences directory: %s", strings.Join(sequences, " "))
	}
	return opts, sequences, nil
}

// singleDash is where an error of the flag package spells a flag with one dash.
var singleDash = regexp.MustCompile(`((?:defined|argument): |for flag )-`)

// twoDashes spells the flag in a parse error with two dashes, as the help does.
func twoDashes(err error) error {
	if errors.Is(err, flag.ErrHelp) {
		return err
	}
	return errors.New(singleDash.ReplaceAllString(err.Error(), "$1--"))
}

// misplacedFlag is a flag after the first operand, where parsing stopped.
// Operands after a "--" are files, whatever they look like.
func misplacedFlag(args, operands []string) (string, bool) {
	if len(operands) < len(args) && args[len(args)-len(operands)-1] == "--" {
		return "", false
	}
	for _, operand := range operands {
		if len(operand) > 1 && strings.HasPrefix(operand, "-") {
			return operand, true
		}
	}
	return "", false
}

func asksForHelp(arg string) bool {
	return slices.Contains([]string{"help", "-h", "--h", "-help", "--help"}, arg)
}

// parseHelp reads the operands of reconciler-fuzzer help.
func parseHelp(operands []string) (options, []string, error) {
	switch {
	case len(operands) == 0 || len(operands) == 1 && asksForHelp(operands[0]):
		return options{}, nil, flag.ErrHelp
	case len(operands) > 1:
		return options{}, nil, fmt.Errorf("help takes one command, and %d were given", len(operands))
	}
	if _, found := lookup(operands[0]); !found {
		return options{}, nil, fmt.Errorf("%q is not a reconciler-fuzzer command", operands[0])
	}
	return options{command: operands[0]}, nil, flag.ErrHelp
}

// validateRuns holds --runs and --no-baseline to the sequences
// reconciler-fuzzer generates itself: named sequence files are what they are.
func (o options) validateRuns(named int) error {
	switch {
	case o.runs < 1:
		return fmt.Errorf("--runs is %d, and an invocation runs at least one sequence", o.runs)
	case named > 0 && o.runsGiven:
		return errors.New("--runs draws sequences, and naming sequence files runs those: give one or the other")
	case named > 0 && o.noBaseline:
		return errors.New("--no-baseline leaves the baseline out after drawn sequences, and naming sequence files runs those alone")
	}
	return nil
}

func (o *options) flags() *flag.FlagSet {
	flags := flag.NewFlagSet("reconciler-fuzzer "+o.command, flag.ContinueOnError)
	flags.SetOutput(io.Discard) // The caller prints what Parse returns.
	if o.command == "version" {
		return flags
	}
	flags.StringVar(&o.target, "target", "", "Exercise the controller that this target.yaml `file` declares.")
	flags.StringVar(&o.kubeconfig, "kubeconfig", "",
		"Run against the cluster this kubeconfig `file` names, rather than an API server reconciler-fuzzer starts.")
	flags.DurationVar(&o.deadline, "deadline", 0,
		"Stop the invocation after `duration`, such as 10m. Without it, reconciler-fuzzer derives one from the target's timeouts and prints it.")
	flags.Var((*stringList)(&o.launchArgs), "launch-arg",
		"Append `arg` to the target's launch.args. Repeat the flag to append more, and the target sees them in order.")
	if o.command == "matrix" {
		flags.StringVar(&o.out, "out", defaultMatrix, "Write the matrix to `file`.")
		flags.StringVar(&o.sequences, "sequences", "", "Run the b<id>.json sequence of each seeded bug in `dir`.")
	} else {
		flags.StringVar(&o.out, "out", defaultOut,
			"Write the invocation's summary, and each failing run's report and recordings, under `dir`.")
		flags.StringVar(&o.junit, "junit", "", "Also write each run's outcome to `file` as JUnit XML.")
	}
	if o.command == "run" {
		flags.IntVar(&o.runs, "runs", defaultRuns, "Draw and run `n` sequences.")
		flags.Int64Var(&o.seed, "seed", 0,
			"Draw the first sequence from seed `n`, and each later one from the next seed. Without it, reconciler-fuzzer picks a seed and prints it.")
		flags.BoolVar(&o.noBaseline, "no-baseline", false,
			"Leave out the baseline, which runs after the drawn sequences.")
	}
	return flags
}

// stringList collects a repeatable flag in the order it was given, so that a
// later --launch-arg wins where the target parses its flags.
type stringList []string

func (l *stringList) String() string { return strings.Join(*l, " ") }

func (l *stringList) Set(value string) error {
	*l = append(*l, value)
	return nil
}

func readSequences(paths []string) ([]run.Sequence, error) {
	sequences := make([]run.Sequence, 0, len(paths))
	for _, path := range paths {
		sequence, err := run.ReadSequence(path)
		if err != nil {
			return nil, err
		}
		sequences = append(sequences, sequence)
	}
	return sequences, nil
}

// clusterSession runs against one test cluster: an envtest control plane
// reconciler-fuzzer starts, or the cluster a kubeconfig names.
type clusterSession struct {
	*cluster.Cluster
	// controllerManager says the cluster runs kube-controller-manager, which
	// envtest does not.
	controllerManager bool
	mapper            meta.RESTMapper
	invocation        *run.Invocation
}

// envtestStatic are the kinds that run Pods, and the claims Pods mount. Only
// kube-controller-manager and a kubelet move their status.
var envtestStatic = []schema.GroupKind{
	{Group: "apps", Kind: "Deployment"},
	{Group: "apps", Kind: "StatefulSet"},
	{Group: "apps", Kind: "DaemonSet"},
	{Group: "apps", Kind: "ReplicaSet"},
	{Group: "batch", Kind: "Job"},
	{Group: "batch", Kind: "CronJob"},
	{Kind: "ReplicationController"},
	{Kind: "Pod"},
	{Kind: "PersistentVolumeClaim"},
}

// envtestLimit names the managed kinds envtest never moves, or is empty.
func envtestLimit(opts options, t *target.Target) string {
	if opts.kubeconfig != "" {
		return ""
	}
	var static []string
	for _, gvk := range t.Manages {
		if slices.Contains(envtestStatic, gvk.GroupKind()) {
			static = append(static, gvk.Kind)
		}
	}
	if len(static) == 0 {
		return ""
	}
	return "envtest runs no controller manager and no kubelet, so no Pod runs, and the status of these kinds never changes: " +
		strings.Join(static, ", ") + ". A ready predicate that waits on that status never holds, and a controller that" +
		" requeues while it waits can hide a missed watch. Run this target with --kubeconfig against a kind cluster."
}

// startSession warns of what envtest never runs, then opens the session.
func (c *cli) startSession(opts options, t *target.Target) (session, error) {
	if limit := envtestLimit(opts, t); limit != "" {
		fmt.Fprintln(c.stderr, "reconciler-fuzzer:", limit)
	}
	return c.open(opts, t)
}

func openSession(opts options, t *target.Target) (session, error) {
	if err := t.Launch.Check(); err != nil {
		return nil, err
	}
	crds := cluster.Options{CRDPaths: t.CRDs}
	if opts.kubeconfig != "" {
		connected, err := cluster.Connect(opts.kubeconfig, crds)
		if err != nil {
			return nil, err
		}
		return &clusterSession{Cluster: connected, controllerManager: true}, nil
	}
	started, err := cluster.Start(crds)
	if err != nil {
		return nil, err
	}
	return &clusterSession{Cluster: started}, nil
}

// vet judges the scopes that loading the target could not, such as that of a
// kind whose CRD the target does not list.
func (s *clusterSession) vet(t *target.Target) error {
	mapper, err := cluster.NewRESTMapper(s.Config())
	if err != nil {
		return err
	}
	s.mapper = mapper
	return t.CheckScopes(mapper)
}

func (s *clusterSession) prepare(ctx context.Context, t *target.Target) error {
	inv, err := run.NewInvocation(s.Config(), t)
	if err != nil {
		return err
	}
	s.invocation = inv // set before Prepare so close() cleans up partial creates
	return inv.Prepare(ctx, t, s.mapper)
}

func (s *clusterSession) execute(ctx context.Context, t *target.Target, sequence run.Sequence, dir string, check run.Checker) (run.Result, error) {
	return run.Run(ctx, t, sequence, run.Options{
		Dir:               dir,
		Config:            s.Config(),
		ControllerManager: s.controllerManager,
		Check:             check,
	})
}

func (s *clusterSession) close() error {
	if s.invocation != nil {
		// Use a fresh context; the run's may have been cancelled.
		if err := s.invocation.Close(context.Background()); err != nil {
			return errors.Join(err, s.Stop())
		}
	}
	return s.Stop()
}

func version() string {
	if info, ok := debug.ReadBuildInfo(); ok && info.Main.Version != "" {
		return info.Main.Version
	}
	return "dev"
}
