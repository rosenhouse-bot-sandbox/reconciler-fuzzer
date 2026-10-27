// Package hunt runs generated plans over many seeds, replays each finding,
// shrinks it and removes duplicates.
package hunt

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"io"
	"iter"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"sigs.k8s.io/yaml"

	"github.com/rosenhouse-bot-sandbox/reconciler-fuzzer/internal/check"
	"github.com/rosenhouse-bot-sandbox/reconciler-fuzzer/internal/config"
	"github.com/rosenhouse-bot-sandbox/reconciler-fuzzer/internal/generate"
	"github.com/rosenhouse-bot-sandbox/reconciler-fuzzer/internal/plan"
)

// DefaultReplays is how many times the CLI replays each new finding's plan
// unless told otherwise, and DefaultCandidates how many candidates it tries
// in the shrink of one finding, so that one slow shrink leaves time for the
// other seeds.
const (
	DefaultReplays    = 5
	DefaultCandidates = 16
)

// errShrinkCut ends a shrink after its candidates.
var errShrinkCut = errors.New("the shrink tried its candidates")

// RunPlan runs one plan into out and returns its report. run.Run implements
// it in production.
type RunPlan func(ctx context.Context, p *plan.Plan, out string) (check.Report, error)

// Options configure a hunt.
type Options struct {
	ConfigPath string
	// Seeds are the seeds to draw plans from.
	Seeds iter.Seq[uint64]
	Out   string
	// Replays is how many times to replay the plan of each new finding, and
	// Candidates how many shrink candidates to try for each finding. Zero
	// candidates skip the shrink.
	Replays, Candidates int
	Run                 RunPlan
	// Log hears one line for each run, and the name of each new finding. It
	// may be nil.
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
	// ShrunkPath. It is nil when the finding was not shrunk.
	Shrunk     *plan.Plan
	ShrunkPath string
	// replaying and shrinking say that the hunt has yet to finish the
	// finding's replays or its shrink.
	replaying, shrinking bool
}

// Hunt runs each seed's plan into <Out>/seed-<n>. For each finding whose
// signature no earlier seed found, it replays the plan Replays times and
// shrinks it. It removes the run of a seed that found nothing new. A seed
// whose run fails is noted, and the hunt goes on. It writes summary.md under
// Out after each seed, and as the replays and shrinks of a seed's findings
// finish. It returns the distinct findings, and an error when a seed failed
// or the context ended.
func Hunt(ctx context.Context, o Options) ([]Found, error) {
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
	h := &hunter{run: o.Run, samples: samples, log: o.Log, replays: o.Replays, candidates: o.Candidates, seen: map[string]bool{}}
	var found []Found
	var failed []string
	seeds := 0
	h.progress = func(more []Found) {
		// The write after the seed reports an error.
		_ = writeSummary(o.Out, fmt.Sprintf("%d seeds", seeds+1), slices.Concat(found, more), failed)
	}
	for seed := range o.Seeds {
		p, err := g.Plan(seed)
		if err != nil {
			return found, err
		}
		dir := filepath.Join(o.Out, fmt.Sprintf("seed-%d", seed))
		more, err := h.investigate(ctx, p, dir, fmt.Sprintf("seed %d", seed))
		found = append(found, more...)
		if ctx.Err() != nil {
			return found, errors.Join(ctx.Err(), writeSummary(o.Out, fmt.Sprintf("%d seeds", seeds), found, failed))
		}
		seeds++
		if err != nil {
			failed = append(failed, fmt.Sprintf("seed %d: %v", seed, err))
		} else if len(more) == 0 {
			if err := os.RemoveAll(dir); err != nil {
				return found, err
			}
		}
		if err := writeSummary(o.Out, fmt.Sprintf("%d seeds", seeds), found, failed); err != nil {
			return found, err
		}
	}
	err = writeSummary(o.Out, fmt.Sprintf("%d seeds", seeds), found, failed)
	if len(failed) > 0 {
		err = errors.Join(err, fmt.Errorf("%d of %d seeds failed", len(failed), seeds))
	}
	return found, err
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
// findings as Hunt does. It writes summary.md under Out as they finish.
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
		_ = writeSummary(o.Out, "1 plan", found, nil)
	}
	found, err := h.investigate(ctx, p, o.Out, "plan")
	if err != nil {
		return nil, err
	}
	return found, writeSummary(o.Out, "1 plan", found, nil)
}

// hunter investigates plans and remembers the signatures they found.
type hunter struct {
	run                 RunPlan
	samples             []map[string]any
	log                 io.Writer
	replays, candidates int
	seen                map[string]bool
	// progress hears the findings of the plan under investigation as their
	// replays and shrinks finish.
	progress func([]Found)
}

func (h *hunter) logf(format string, args ...any) {
	if h.log != nil {
		fmt.Fprintf(h.log, format+"\n", args...)
	}
}

// investigate runs p into dir. For each finding whose signature is not yet
// seen, it replays p into dir/replay-<i>, shrinks it with candidate runs
// into dir/shrink-<f>-<c>-<r>, and saves the result in dir. f numbers the
// finding, c the candidate and r the run. It removes each candidate run that
// did not fail. A replay or candidate whose run fails counts as not failing.
// A finding that replays all missed is not shrunk. Only the run of p itself
// returns its error. When the context ends, it returns the findings so far,
// unshrunk.
func (h *hunter) investigate(ctx context.Context, p *plan.Plan, dir, name string) ([]Found, error) {
	report, err := h.run(ctx, p, dir)
	if err != nil {
		h.logf("%s: %v", name, err)
		return nil, err
	}
	var found []Found
	for _, f := range report.Findings {
		if !h.seen[f.Signature()] {
			h.seen[f.Signature()] = true
			found = append(found, Found{Finding: f, Seed: p.Seed, Replays: h.replays, replaying: h.replays > 0, shrinking: h.candidates > 0})
		}
	}
	h.logf("%s: %d new %s", name, len(found), plural(len(found), "finding"))
	if len(found) == 0 {
		return nil, nil
	}
	for j, f := range found {
		h.logf("%s, finding %d: %s: %s", name, j+1, f.Finding.Check, f.Finding.Summary)
	}
	// A cancel or error leaves nothing pending.
	defer finish(found)
	h.progress(found)
	for i := 1; i <= h.replays; i++ {
		replay, err := h.run(ctx, p, filepath.Join(dir, fmt.Sprintf("replay-%d", i)))
		if ctx.Err() != nil {
			return found, ctx.Err()
		}
		reproduced := 0
		for j := range found {
			if err == nil && gives(replay, found[j].Finding) {
				found[j].Reproduced++
				reproduced++
			}
		}
		if err != nil {
			h.logf("%s, replay %d: reproduced %d of %d, %v", name, i, reproduced, len(found), err)
		} else {
			h.logf("%s, replay %d: reproduced %d of %d", name, i, reproduced, len(found))
		}
	}
	for j := range found {
		f := &found[j]
		f.replaying = false
		if f.shrinking && h.replays > 0 && f.Reproduced == 0 {
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
				return false, errShrinkCut
			}
			for r := 1; r <= k; r++ {
				out := filepath.Join(dir, fmt.Sprintf("shrink-%d-%d-%d", j+1, tried, r))
				report, err := h.run(ctx, c, out)
				if ctx.Err() != nil {
					return false, ctx.Err()
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
		if f.Shrunk, err = Shrink(ctx, p, h.samples, fails); errors.Is(err, errShrinkCut) {
			h.logf("%s: shrink stopped after %d candidates", finding, h.candidates)
			err = nil
		}
		if err != nil {
			f.Shrunk = nil
			return found, err
		}
		f.ShrunkPath = filepath.Join(dir, "shrunk.json")
		if j > 0 {
			f.ShrunkPath = filepath.Join(dir, fmt.Sprintf("shrunk-%d.json", j+1))
		}
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return nil, err
		}
		if err := plan.Save(f.ShrunkPath, f.Shrunk); err != nil {
			return nil, err
		}
		f.shrinking = false
		h.progress(found)
	}
	return found, nil
}

// finish marks each finding's replays and shrink finished.
func finish(found []Found) {
	for j := range found {
		found[j].replaying, found[j].shrinking = false, false
	}
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

// Summary is summary.md: one row for each finding, then the seeds whose
// run failed. A row says "pending" for replays or a shrink yet to finish.
func Summary(over string, found []Found, failed []string) string {
	var b strings.Builder
	if len(found) == 0 {
		fmt.Fprintf(&b, "No findings over %s.\n", over)
	} else {
		fmt.Fprintf(&b, "%d distinct findings over %s.\n\n", len(found), over)
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
			// A plan's seed 0 means it has none, as in plan.json.
			seed := "-"
			if f.Seed != 0 {
				seed = fmt.Sprint(f.Seed)
			}
			fmt.Fprintf(&b, "| %s | %s | %s | %s | %s |\n", seed, f.Finding.Check,
				strings.ReplaceAll(f.Finding.Summary, "|", `\|`), reproduced, shrunk)
		}
	}
	if len(failed) > 0 {
		b.WriteString("\nRuns that failed:\n\n")
		for _, line := range failed {
			fmt.Fprintf(&b, "- %s\n", line)
		}
	}
	return b.String()
}

func writeSummary(out, over string, found []Found, failed []string) error {
	if err := os.MkdirAll(out, 0o755); err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(out, "summary.md"), []byte(Summary(over, found, failed)), 0o644)
}

func plural(n int, noun string) string {
	if n == 1 {
		return noun
	}
	return noun + "s"
}
