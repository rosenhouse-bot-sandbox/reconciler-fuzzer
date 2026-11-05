// Package hunt runs generated plans over many seeds, replays each finding,
// shrinks it and removes duplicates.
package hunt

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"iter"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"

	"sigs.k8s.io/yaml"

	"github.com/rosenhouse-bot-sandbox/reconciler-fuzzer/internal/check"
	"github.com/rosenhouse-bot-sandbox/reconciler-fuzzer/internal/config"
	"github.com/rosenhouse-bot-sandbox/reconciler-fuzzer/internal/generate"
	"github.com/rosenhouse-bot-sandbox/reconciler-fuzzer/internal/plan"
)

// DefaultReplays is how many times the CLI replays a plan with new findings
// unless told otherwise, and DefaultCandidates how many candidates it tries
// in the shrink of one finding, so that one slow shrink leaves time for the
// other seeds.
const (
	DefaultReplays    = 5
	DefaultCandidates = 16
)

// errCandidateLimit ends a shrink once it has tried its candidates.
var errCandidateLimit = errors.New("the shrink tried its candidates")

// RunPlan runs one plan into out and returns its report. run.Run implements
// it in production.
type RunPlan func(ctx context.Context, p *plan.Plan, out string) (check.Report, error)

// Options configure a hunt.
type Options struct {
	ConfigPath string
	// Seeds are the seeds to draw plans from.
	Seeds iter.Seq[uint64]
	Out   string
	// Replays is how many times to replay a plan with new findings, and
	// Candidates how many shrink candidates to try for each finding. Zero
	// candidates skip the shrink.
	Replays, Candidates int
	Run                 RunPlan
	// Log hears the hunt's progress, one line at a time. It may be nil.
	Log io.Writer
}

// Found is one distinct finding of a hunt.
type Found struct {
	Finding check.Finding
	Seed    uint64
	// Reproduced counts the replays, of Replays, that found the signature
	// again.
	Reproduced, Replays int
	// Shrunk is the smallest plan that still fails. It is saved at
	// ShrunkPath. Both are unset when the finding was not shrunk, or when an
	// error kept its shrink from being saved.
	Shrunk     *plan.Plan
	ShrunkPath string
	// CutShort says an error, such as an interrupt, left the finding's
	// replays or shrink unfinished. Shrunk is then the smallest failing plan
	// so far, if any.
	CutShort bool
	// Known is the config's known entry that the finding matches, if any.
	// The hunt neither replays nor shrinks a known finding.
	Known *config.Known
	// replaying and shrinking say that the hunt has yet to finish the
	// finding's replays or its shrink.
	replaying, shrinking bool
}

// Hunt runs each seed's plan into <Out>/seed-<n>. For each finding whose
// signature no earlier seed found, it replays the plan Replays times and
// shrinks it. It removes the run of a seed that found nothing new, unless a
// failed action ended its plan early. A seed whose run fails is noted, and
// the hunt continues. A run that finds a program missing ends the hunt, since
// the setup is at fault, not the seed.
// It removes any summary.md under Out first, then writes it after each seed,
// and as the replays and shrinks of a seed's findings finish. It returns the
// distinct findings, and an error when a seed failed or the context ended.
func Hunt(ctx context.Context, o Options) ([]Found, error) {
	if err := os.Remove(filepath.Join(o.Out, "summary.md")); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return nil, err
	}
	cfg, err := config.Load(o.ConfigPath)
	if err != nil {
		return nil, err
	}
	g, err := generate.New(cfg)
	if err != nil {
		return nil, err
	}
	samples, err := loadSamples(cfg)
	if err != nil {
		return nil, err
	}
	h := &hunter{run: o.Run, samples: samples, log: o.Log, replays: o.Replays, candidates: o.Candidates, seen: map[string]bool{}, known: cfg.Known}
	var found []Found
	var failed []string
	seeds := 0
	h.progress = func(more []Found) {
		// The write after the seed reports an error.
		_ = writeSummary(o.Out, fmt.Sprintf("%d seeds", seeds+1), slices.Concat(found, more), failed, h.endedPlan)
	}
	for seed := range o.Seeds {
		p, err := g.Plan(seed)
		if err != nil {
			return found, err
		}
		dir := filepath.Join(o.Out, fmt.Sprintf("seed-%d", seed))
		more, ended, err := h.investigate(ctx, p, dir, fmt.Sprintf("seed %d", seed))
		found = append(found, more...)
		if err == nil && len(more) == 0 && !ended {
			if err := os.RemoveAll(dir); err != nil {
				return found, err
			}
		}
		if ctx.Err() != nil {
			// The seed counts unless the cancel ended the run of its plan.
			if err == nil || len(more) > 0 {
				seeds++
			}
			if !errors.Is(err, ctx.Err()) {
				err = errors.Join(ctx.Err(), err)
			}
			return found, errors.Join(err, writeSummary(o.Out, fmt.Sprintf("%d seeds", seeds), found, failed, h.endedPlan))
		}
		seeds++
		missing := missingProgram(err)
		if err != nil {
			err = fmt.Errorf("seed %d: %w", seed, err)
			failed = append(failed, err.Error())
			if !missing {
				h.logf("%v", err)
			}
		}
		if err := writeSummary(o.Out, fmt.Sprintf("%d seeds", seeds), found, failed, h.endedPlan); err != nil {
			return found, err
		}
		if missing {
			return found, err
		}
	}
	err = writeSummary(o.Out, fmt.Sprintf("%d seeds", seeds), found, failed, h.endedPlan)
	if len(failed) > 0 {
		err = errors.Join(err, fmt.Errorf("%d of %d seeds failed", len(failed), seeds))
	}
	return found, err
}

// missingProgram reports whether err says that a program is missing.
// exec.LookPath returns an *exec.Error when it finds no executable file.
// os.StartProcess returns an *fs.PathError with Op "fork/exec" and ENOENT
// when the file is missing, as when envtest finds no etcd. Other fork/exec
// errors, such as EAGAIN, can be transient.
func missingProgram(err error) bool {
	var start *fs.PathError
	return errors.As(err, new(*exec.Error)) ||
		errors.As(err, &start) && start.Op == "fork/exec" && errors.Is(start.Err, fs.ErrNotExist)
}

// ShrinkOptions configure the shrink of one plan file, as Options do a
// hunt.
type ShrinkOptions struct {
	ConfigPath          string
	PlanPath            string
	Out                 string
	Replays, Candidates int
	Run                 RunPlan
	Log                 io.Writer
}

// ShrinkPlan runs the plan into Out, then replays and shrinks each of its
// findings as Hunt does. It writes summary.md under Out as the replays and
// shrinks finish. An error that leaves no findings writes none.
func ShrinkPlan(ctx context.Context, o ShrinkOptions) ([]Found, error) {
	cfg, err := config.Load(o.ConfigPath)
	if err != nil {
		return nil, err
	}
	samples, err := loadSamples(cfg)
	if err != nil {
		return nil, err
	}
	p, err := plan.Load(o.PlanPath)
	if err != nil {
		return nil, err
	}
	h := &hunter{run: o.Run, samples: samples, log: o.Log, replays: o.Replays, candidates: o.Candidates, seen: map[string]bool{}}
	h.progress = func(found []Found) {
		// The write below reports an error.
		_ = writeSummary(o.Out, "1 plan", found, nil, h.endedPlan)
	}
	found, _, err := h.investigate(ctx, p, o.Out, "plan")
	if err != nil && len(found) == 0 {
		return nil, err
	}
	return found, errors.Join(err, writeSummary(o.Out, "1 plan", found, nil, h.endedPlan))
}

// hunter investigates plans and remembers the signatures they found.
type hunter struct {
	run                 RunPlan
	samples             []map[string]any
	log                 io.Writer
	replays, candidates int
	seen                map[string]bool
	known               []config.Known
	// endedPlan holds the note of each failed action that ended a plan
	// investigated early, prefixed with the plan's name.
	endedPlan []string
	// progress hears the findings of the plan under investigation as their
	// replays and shrinks finish.
	progress func([]Found)
}

func (h *hunter) logf(format string, args ...any) {
	if h.log != nil {
		fmt.Fprintf(h.log, format+"\n", args...)
	}
}

// investigate runs p into dir. It adds each failed action of that run that
// ended p early to h.endedPlan, and returns whether there was one. For each
// finding whose signature is not yet
// seen, it replays p into dir/replay-<i>, shrinks it with candidate runs
// into dir/shrink-<f>-<c>-<r>, and saves the result in dir. f numbers the
// finding, c the candidate and r the run. It removes each candidate run that
// did not fail, and the replay or candidate run that a cancel cut. A replay
// or candidate whose run fails counts as not failing. A finding that no
// replay reproduced is not shrunk, unless it had no replays. Only the run of
// p itself returns its error. When a cancel, or a failed cleanup or save,
// ends it after that run, it returns the findings with the error and marks
// each one left unfinished. A shrink cut short saves the smallest failing
// plan so far.
func (h *hunter) investigate(ctx context.Context, p *plan.Plan, dir, name string) (_ []Found, ended bool, _ error) {
	report, err := h.run(ctx, p, dir)
	if err != nil {
		return nil, false, err
	}
	var found []Found
	for _, f := range report.Findings {
		// A known finding does not hide an unknown one of its signature.
		k := knownAs(h.known, f)
		seen := f.Signature()
		if k != nil {
			seen += "|known " + k.See
		}
		if !h.seen[seen] {
			h.seen[seen] = true
			if k != nil {
				found = append(found, Found{Finding: f, Seed: p.Seed, Known: k})
			} else {
				found = append(found, Found{Finding: f, Seed: p.Seed, Replays: h.replays, replaying: true, shrinking: h.candidates > 0})
			}
		}
	}
	for _, note := range report.EndedPlan {
		h.endedPlan = append(h.endedPlan, name+": "+note)
	}
	ended = len(report.EndedPlan) > 0
	line := fmt.Sprintf("%s: %d new %s", name, len(found), plural(len(found), "finding"))
	if ended {
		line += ", and a failed action ended its plan early"
	}
	h.logf("%s", line)
	if len(found) == 0 {
		return nil, ended, nil
	}
	for j, f := range found {
		line := fmt.Sprintf("%s, finding %d: %s: %s", name, j+1, f.Finding.Check, f.Finding.Summary)
		if f.Known != nil {
			line += ", known, see " + f.Known.See
		}
		h.logf("%s", line)
	}
	if Unknown(found) == 0 {
		h.progress(found)
		return found, ended, nil
	}
	defer finish(found)
	h.progress(found)
	for i := 1; i <= h.replays; i++ {
		out := filepath.Join(dir, fmt.Sprintf("replay-%d", i))
		replay, err := h.run(ctx, p, out)
		if ctx.Err() != nil {
			return found, ended, errors.Join(ctx.Err(), os.RemoveAll(out))
		}
		var reproduced []int
		for j := range found {
			if found[j].Known == nil && err == nil && gives(replay, found[j].Finding) {
				found[j].Reproduced++
				reproduced = append(reproduced, j+1)
			}
		}
		if err != nil {
			h.logf("%s, replay %d: reproduced no finding, since its run failed: %v", name, i, err)
		} else {
			h.logf("%s, replay %d: reproduced %s", name, i, listFindings(reproduced))
		}
	}
	for j := range found {
		f := &found[j]
		f.replaying = false
		switch {
		case f.Known != nil:
		case h.candidates == 0:
			h.logf("%s, finding %d: not shrunk, since the candidate limit is 0", name, j+1)
		case h.replays > 0 && f.Reproduced == 0:
			h.logf("%s, finding %d: not shrunk, since no replay reproduced it", name, j+1)
			f.shrinking = false
		}
	}
	h.progress(found)
	for j := range found {
		f := &found[j]
		if !f.shrinking {
			continue
		}
		finding := fmt.Sprintf("%s, finding %d", name, j+1)
		k := 2
		if 2*f.Reproduced < h.replays {
			k = 4
		}
		tried := 0
		fails := func(ctx context.Context, c *plan.Plan) (bool, error) {
			if tried++; tried > h.candidates {
				return false, errCandidateLimit
			}
			for r := 1; r <= k; r++ {
				out := filepath.Join(dir, fmt.Sprintf("shrink-%d-%d-%d", j+1, tried, r))
				report, err := h.run(ctx, c, out)
				if ctx.Err() != nil {
					return false, errors.Join(ctx.Err(), os.RemoveAll(out))
				}
				if err == nil && gives(report, f.Finding) {
					h.logf("%s, candidate %d: kept, since it still gives the finding", finding, tried)
					return true, nil
				}
				if err := os.RemoveAll(out); err != nil {
					return false, err
				}
			}
			h.logf("%s, candidate %d: rejected, since none of its %d runs gave the finding", finding, tried, k)
			return false, nil
		}
		shrunk, shrinkErr := Shrink(ctx, p, h.samples, fails)
		if errors.Is(shrinkErr, errCandidateLimit) {
			h.logf("%s: shrink stopped after %d %s", finding, h.candidates, plural(h.candidates, "candidate"))
			shrinkErr = nil
		}
		path := filepath.Join(dir, "shrunk.json")
		if j > 0 {
			path = filepath.Join(dir, fmt.Sprintf("shrunk-%d.json", j+1))
		}
		saveErr := plan.Save(path, shrunk)
		if saveErr == nil {
			f.Shrunk, f.ShrunkPath = shrunk, path
		}
		if err := errors.Join(shrinkErr, saveErr); err != nil {
			h.logf("%s: shrink cut short, %v", finding, err)
			return found, ended, err
		}
		if shrunk == p {
			h.logf("%s: saved unchanged to %s, since the shrink kept no candidate", finding, path)
		} else {
			h.logf("%s: shrunk to %s", finding, path)
		}
		f.shrinking = false
		h.progress(found)
	}
	return found, ended, nil
}

// finish marks each finding's replays and shrink finished, and cuts short
// each finding that an error left unfinished.
func finish(found []Found) {
	for j := range found {
		f := &found[j]
		f.CutShort = f.replaying || f.shrinking
		f.replaying, f.shrinking = false, false
	}
}

// Unknown counts the findings that match no known entry.
func Unknown(found []Found) int {
	n := 0
	for _, f := range found {
		if f.Known == nil {
			n++
		}
	}
	return n
}

// knownAs returns the first known entry that f matches, or nil.
func knownAs(known []config.Known, f check.Finding) *config.Known {
	for i, k := range known {
		if k.Check == f.Check && k.Summary == f.Summary && (k.Details == nil || len(f.Details) > 0 && !slices.ContainsFunc(f.Details, func(line string) bool { return !k.Details.MatchString(line) })) {
			return &known[i]
		}
	}
	return nil
}

func gives(r check.Report, f check.Finding) bool {
	return slices.ContainsFunc(r.Findings, func(g check.Finding) bool { return g.Signature() == f.Signature() })
}

// loadSamples reads each entry's sample, in config order.
func loadSamples(cfg *config.Config) ([]map[string]any, error) {
	var samples []map[string]any
	for _, p := range cfg.Primaries {
		data, err := os.ReadFile(p.Sample)
		if err != nil {
			return nil, err
		}
		var sample map[string]any
		if err := yaml.Unmarshal(data, &sample); err != nil {
			return nil, fmt.Errorf("%s: %w", p.Sample, err)
		}
		samples = append(samples, sample)
	}
	return samples, nil
}

// Summary is summary.md: one row for each finding, then the first line of
// each failed run's error, then of each note of an action that ended a plan
// early. A row says "pending" for replays or a shrink yet to finish.
func Summary(over string, found []Found, failed, endedPlan []string) string {
	var b strings.Builder
	if len(found) == 0 {
		fmt.Fprintf(&b, "No findings over %s.\n", over)
	} else {
		known := ""
		if n := len(found) - Unknown(found); n > 0 {
			known = fmt.Sprintf(", %d known,", n)
		}
		fmt.Fprintf(&b, "%d distinct findings%s over %s.\n\n", len(found), known, over)
		b.WriteString("| Seed | Check | Finding | Reproduced | Shrunk plan |\n| --- | --- | --- | --- | --- |\n")
		for _, f := range found {
			reproduced := fmt.Sprintf("%d/%d", f.Reproduced, f.Replays)
			if f.replaying {
				reproduced = "pending"
			}
			shrunk := cmp.Or(f.ShrunkPath, "not shrunk")
			if f.shrinking {
				shrunk = "pending"
			}
			if f.CutShort {
				shrunk += " (cut short)"
			}
			if f.Known != nil {
				reproduced, shrunk = "known", "see "+f.Known.See
			}
			// A plan's seed 0 means it has none, as in plan.json.
			seed := "-"
			if f.Seed != 0 {
				seed = fmt.Sprint(f.Seed)
			}
			fmt.Fprintf(&b, "| %s | %s | %s | %s | %s |\n", seed, f.Finding.Check,
				strings.ReplaceAll(f.Finding.Summary, "|", `\|`), reproduced, shrunk)
		}
	}
	firstLines(&b, "Runs that failed", failed)
	firstLines(&b, "Plans that a failed action ended early", endedPlan)
	return b.String()
}

// firstLines lists the first line of each entry under heading, if any.
func firstLines(b *strings.Builder, heading string, entries []string) {
	if len(entries) == 0 {
		return
	}
	fmt.Fprintf(b, "\n%s:\n\n", heading)
	for _, e := range entries {
		first, _, _ := strings.Cut(e, "\n")
		fmt.Fprintf(b, "- %s\n", first)
	}
}

func writeSummary(out, over string, found []Found, failed, endedPlan []string) error {
	if err := os.MkdirAll(out, 0o755); err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(out, "summary.md"), []byte(Summary(over, found, failed, endedPlan)), 0o644)
}

// listFindings names findings by number, as in "findings 1, 2 and 4".
func listFindings(numbers []int) string {
	switch len(numbers) {
	case 0:
		return "no finding"
	case 1:
		return fmt.Sprintf("finding %d", numbers[0])
	}
	words := make([]string, len(numbers))
	for i, n := range numbers {
		words[i] = strconv.Itoa(n)
	}
	last := len(words) - 1
	return fmt.Sprintf("findings %s and %s", strings.Join(words[:last], ", "), words[last])
}

func plural(n int, noun string) string {
	if n == 1 {
		return noun
	}
	return noun + "s"
}
