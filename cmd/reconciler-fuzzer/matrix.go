package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"

	"github.com/rosenhouse-bot-sandbox/reconciler-fuzzer/internal/run"
	"github.com/rosenhouse-bot-sandbox/reconciler-fuzzer/internal/target"
)

// defaultMatrix is the bug matrix the repository checks in.
const defaultMatrix = "docs/bug-matrix.md"

// control is B0, the row that seeds no bug. It proves the matrix is not vacuous.
const control = 0

// bugSequence names the sequence of one seeded bug.
var bugSequence = regexp.MustCompile(`^b(\d+)\.json$`)

// bugRow is one bug, the sequence that exposes it, and what the checks found
// over that sequence under the bug and without it. B0 has one run, which is
// both.
type bugRow struct {
	bug             int
	file            string
	sequence        run.Sequence
	bugged, correct checked
}

// checked is what the checks found over one run.
type checked struct {
	fired []string
	// skipped names the checks that left a note: they judged less than the row
	// shows, which a blank cell would read as a pass.
	skipped []string
}

// bugMatrix runs each seeded bug's sequence under the bug and without it, and
// writes which checks fired.
func (c *cli) bugMatrix(ctx context.Context, opts options) int {
	exercised, err := target.Load(opts.target)
	if err != nil {
		return c.fail(err)
	}
	exercised.Launch.Args = append(exercised.Launch.Args, opts.launchArgs...)
	rows, err := readBugSequences(opts.sequences)
	if err != nil {
		return c.fail(err)
	}
	checks, err := checkIDs(exercised)
	if err != nil {
		return c.fail(err)
	}

	s, err := c.startSession(opts, exercised)
	if err != nil {
		return c.fail(err)
	}
	defer func() { c.warn(s.close()) }()
	if err := s.vet(exercised); err != nil {
		return c.fail(err)
	}
	if err := s.prepare(ctx, exercised); err != nil {
		return c.fail(err)
	}
	dir, err := os.MkdirTemp("", "reconciler-fuzzer-matrix-")
	if err != nil {
		return c.fail(fmt.Errorf("creating the matrix's run directory: %w", err))
	}
	// Remove leaves the directory while it holds a run that erred.
	defer os.Remove(dir)
	var runs []run.Sequence
	for _, row := range rows {
		runs = append(runs, row.sequence)
		if row.bug != control {
			runs = append(runs, row.sequence)
		}
	}
	c.derive(&opts, exercised, runs, false)

	ctx, cancel := context.WithTimeout(ctx, opts.deadline)
	defer cancel()
	for i := range rows {
		if failed := c.exerciseRow(ctx, opts, s, exercised, &rows[i], dir); failed != nil {
			c.fail(failed.err)
			c.showRunFiles(failed.dir)
			fmt.Fprintf(c.stderr, "  run it again with\n    %s\n", opts.replayRun(rows[i], failed.bugArgs))
			return exitError
		}
	}

	if err := os.WriteFile(opts.out, []byte(matrix(opts.sequences, checks, rows)), 0o644); err != nil {
		return c.fail(fmt.Errorf("writing the matrix: %w", err))
	}
	fmt.Fprintf(c.stdout, "wrote %s\n", opts.out)
	return c.judge(opts, rows)
}

// erred is a matrix run that could not finish.
type erred struct {
	bugArgs []string
	dir     string
	err     error
}

// exerciseRow runs the row's sequence under its bug, and then without it.
func (c *cli) exerciseRow(ctx context.Context, opts options, s session, t *target.Target, row *bugRow, dir string) *erred {
	var failed *erred
	if row.bug != control {
		if row.bugged, failed = c.exerciseUnder(ctx, opts, s, t, row, row.bugArgs(), filepath.Join(dir, row.name())); failed != nil {
			return failed
		}
	}
	row.correct, failed = c.exerciseUnder(ctx, opts, s, t, row, nil, filepath.Join(dir, row.name()+"-no-bug"))
	if row.bug == control {
		row.bugged = row.correct
	}
	return failed
}

// exerciseUnder runs the row's sequence with bugArgs appended to the target's
// launch args, and records what the checks found over the whole run. It
// removes the run's files unless the run erred.
func (c *cli) exerciseUnder(ctx context.Context, opts options, s session, t *target.Target, row *bugRow, bugArgs []string, dir string) (checked, *erred) {
	exercised := *t
	exercised.Launch.Args = slices.Concat(t.Launch.Args, bugArgs)
	ran := row.describe(bugArgs)
	result, err := s.execute(ctx, &exercised, row.sequence, dir, observing{})
	if err != nil {
		return checked{}, &erred{bugArgs, dir, fmt.Errorf("%s: %w", ran, opts.named(ctx, err))}
	}
	results, err := run.Evaluate(result.Recorded)
	if err != nil {
		return checked{}, &erred{bugArgs, dir, fmt.Errorf("%s: %w", ran, err)}
	}
	os.RemoveAll(dir)
	var found checked
	notes := slices.Clone(result.Notes)
	for _, check := range results {
		if len(check.Violations) > 0 {
			found.fired = append(found.fired, check.ID)
		}
		if len(check.Notes) > 0 {
			found.skipped = append(found.skipped, check.ID)
		}
		notes = append(notes, check.Notes...)
	}
	fmt.Fprintf(c.stdout, "%s: %s fired %s\n", row.name(), ran, found.summary())
	for _, note := range notes {
		fmt.Fprintf(c.stdout, "  %s\n", note)
	}
	return found, nil
}

// judge applies the matrix's acceptance: some check catches every bug, and no
// check fires against the toy with no bug.
func (c *cli) judge(opts options, rows []bugRow) int {
	code := exitOK
	for _, row := range rows {
		if row.bug != control && len(row.bugged.fired) == 0 {
			code = exitViolation
			c.unexpected(opts, row, row.bugged, row.bugArgs())
		}
		if len(row.correct.fired) > 0 {
			code = exitViolation
			c.unexpected(opts, row, row.correct, nil)
		}
	}
	return code
}

// unexpected reports a run that broke the acceptance, and how to run it again.
func (c *cli) unexpected(opts options, row bugRow, found checked, bugArgs []string) {
	fmt.Fprintf(c.stderr, "reconciler-fuzzer: %s: %s fired %s; reproduce it with\n  %s\n",
		row.name(), row.describe(bugArgs), found.summary(), opts.replayRun(row, bugArgs))
}

// replayRun is the command that runs the row's sequence again with bugArgs.
func (o options) replayRun(row bugRow, bugArgs []string) string {
	replay := o
	replay.launchArgs = slices.Concat(o.launchArgs, bugArgs)
	return replay.replayCommand(filepath.Join(o.sequences, row.file))
}

func (r bugRow) name() string { return "B" + strconv.Itoa(r.bug) }

func (r bugRow) bugArgs() []string { return []string{fmt.Sprintf("--bug=%d", r.bug)} }

// describe names the row's sequence and the bug the matrix added to its launch
// args or withheld from them.
func (r bugRow) describe(bugArgs []string) string {
	switch {
	case len(bugArgs) > 0:
		return r.file + " under " + strings.Join(bugArgs, " ")
	case r.bug == control:
		return r.file
	}
	return r.file + " without " + strings.Join(r.bugArgs(), " ")
}

func (c checked) summary() string {
	if len(c.fired) == 0 {
		return "nothing"
	}
	return strings.Join(c.fired, ", ")
}

// observing evaluates nothing, so that a matrix run executes its whole
// sequence instead of ending at the first violation. The checks then read the
// whole recording.
type observing struct{}

func (observing) Check(run.Input) (run.Findings, error) { return run.Findings{}, nil }

// readBugSequences reads one b<id>.json per seeded bug, in bug order.
func readBugSequences(dir string) ([]bugRow, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, fmt.Errorf("reading the sequences: %w", err)
	}
	var rows []bugRow
	for _, entry := range entries {
		id := bugSequence.FindStringSubmatch(entry.Name())
		if id == nil {
			continue
		}
		bug, err := strconv.Atoi(id[1])
		if err != nil {
			return nil, fmt.Errorf("reading the sequences: %s: %w", entry.Name(), err)
		}
		sequence, err := run.ReadSequence(filepath.Join(dir, entry.Name()))
		if err != nil {
			return nil, err
		}
		rows = append(rows, bugRow{bug: bug, file: entry.Name(), sequence: sequence})
	}
	if len(rows) == 0 {
		return nil, fmt.Errorf("%s holds no b<id>.json sequence", dir)
	}
	slices.SortFunc(rows, func(a, b bugRow) int { return a.bug - b.bug })
	if rows[0].bug != control {
		return nil, fmt.Errorf("%s holds no b0.json: the control row proves the matrix is not vacuous", dir)
	}
	return rows, nil
}

// checkIDs names every check the engine runs, in its order: the generic
// invariants and the target's properties. A run of nothing trips nothing, so
// the results name the checks and hold no violation.
func checkIDs(t *target.Target) ([]string, error) {
	results, err := run.Evaluate(run.Input{Target: t})
	if err != nil {
		return nil, err
	}
	ids := make([]string, len(results))
	for i, result := range results {
		ids[i] = result.ID
	}
	return ids, nil
}

// matrix renders the toy's seeded bugs against the checks.
func matrix(sequences string, checks []string, rows []bugRow) string {
	var out strings.Builder
	fmt.Fprintf(&out, `# Bug matrix

Which [check](checks.md) catches each seeded bug of DESIGN.md §9.1. Generated by
`+"`make bug-matrix`"+` from %s; CI fails if it is stale.

A run ends at its first violation, and the matrix evaluates every check over
the whole recording, so a row lists every check that fired rather than one per
bug: several bugs trip more than one. B0 is the control, the toy with no bug,
and its row is empty. A `+"`✓`"+` caught the bug; a `+"`?`"+` left something unjudged,
which is not the same as a pass.

Each sequence also runs against the toy with no bug. The last column names each
check that fired there or left something unjudged, with the same marks. CI fails
if a check fires there.

`, sequences)
	fmt.Fprintf(&out, "| Bug | %s | No bug |\n", strings.Join(checks, " | "))
	out.WriteString("|---" + strings.Repeat("|---", len(checks)+1) + "|\n")
	for _, row := range rows {
		fmt.Fprintf(&out, "| %s |", row.name())
		var correct []string
		for _, check := range checks {
			fmt.Fprintf(&out, " %s |", row.bugged.mark(check))
			if mark := row.correct.mark(check); mark != "" {
				correct = append(correct, check+" "+mark)
			}
		}
		fmt.Fprintf(&out, " %s |\n", strings.Join(correct, ", "))
	}
	return out.String()
}

func (c checked) mark(check string) string {
	switch {
	case slices.Contains(c.fired, check):
		return "✓"
	case slices.Contains(c.skipped, check):
		return "?"
	}
	return ""
}
