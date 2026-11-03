// Command reconciler-fuzzer runs plans against a controller, judges a stored
// trace, hunts over seeds and shrinks a failing plan.
//
// judge reads ready, stable and budgets from the trace unless --config is
// given.
//
// Each command exits 1 with findings and 2 on a usage or harness error. A
// hunt or shrink with findings exits 1 even when a seed failed or an
// interrupt cut it short.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"

	"github.com/go-logr/logr"
	ctrllog "sigs.k8s.io/controller-runtime/pkg/log"

	"github.com/rosenhouse-bot-sandbox/reconciler-fuzzer/internal/check"
	"github.com/rosenhouse-bot-sandbox/reconciler-fuzzer/internal/config"
	"github.com/rosenhouse-bot-sandbox/reconciler-fuzzer/internal/generate"
	"github.com/rosenhouse-bot-sandbox/reconciler-fuzzer/internal/hunt"
	"github.com/rosenhouse-bot-sandbox/reconciler-fuzzer/internal/plan"
	"github.com/rosenhouse-bot-sandbox/reconciler-fuzzer/internal/run"
	"github.com/rosenhouse-bot-sandbox/reconciler-fuzzer/internal/trace"
)

const usage = `usage:
  reconciler-fuzzer run    --config c.yaml (--plan plan.json | --seed n) --out dir [--launch-arg a]...
  reconciler-fuzzer judge  --trace dir[/trace.jsonl] [--config c.yaml]
  reconciler-fuzzer hunt   --config c.yaml --seeds n [--from s] --out dir [--launch-arg a]...
  reconciler-fuzzer shrink --config c.yaml --plan plan.json --out dir [--launch-arg a]...
`

// runPlan is run.Run, which a test replaces.
var runPlan = run.Run

func main() {
	// envtest logs through controller-runtime, which otherwise prints a
	// warning with a stack trace.
	ctrllog.SetLogger(logr.Discard())
	os.Exit(cli(os.Args[1:], os.Stdout, os.Stderr))
}

// cli runs one command and returns the exit code.
func cli(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		fmt.Fprint(stderr, usage)
		return 2
	}
	flags := flag.NewFlagSet(args[0], flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	configPath := flags.String("config", "", "")
	var out, planPath *string
	var launchArgs repeated
	if args[0] != "judge" {
		out = flags.String("out", "", "")
		flags.Var(&launchArgs, "launch-arg", "")
	}
	if args[0] == "run" || args[0] == "shrink" {
		planPath = flags.String("plan", "", "")
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	// runInto runs one plan, as hunt and shrink do.
	runInto := func(ctx context.Context, p *plan.Plan, out string) (check.Report, error) {
		return runPlan(ctx, run.Options{ConfigPath: *configPath, Plan: p, Out: out, LaunchArgs: launchArgs})
	}
	var command func() (int, error)
	switch args[0] {
	case "run":
		seed := flags.String("seed", "", "")
		command = func() (int, error) {
			if err := required(flags, "config", "out"); err != nil {
				return 0, err
			}
			o := run.Options{ConfigPath: *configPath, PlanPath: *planPath, Out: *out, LaunchArgs: launchArgs}
			switch {
			case (*planPath == "") == (*seed == ""):
				return 0, errors.New("give exactly one of --plan and --seed")
			case *seed != "":
				n, err := strconv.ParseUint(*seed, 10, 64)
				if err != nil {
					return 0, fmt.Errorf("--seed %s: want a non-negative integer", *seed)
				}
				if o.Plan, err = drawPlan(*configPath, n); err != nil {
					return 0, harnessError{err}
				}
			}
			result, err := runPlan(ctx, o)
			if err != nil {
				return 0, harnessError{err}
			}
			report, err := os.ReadFile(filepath.Join(*out, "report.md"))
			if err != nil {
				return 0, harnessError{err}
			}
			stdout.Write(report)
			return len(result.Findings), nil
		}
	case "hunt":
		seeds := flags.Uint64("seeds", 0, "")
		from := flags.Uint64("from", 1, "")
		command = func() (int, error) {
			if err := required(flags, "config", "out"); err != nil {
				return 0, err
			}
			if *seeds == 0 {
				return 0, errors.New("the flag --seeds needs a positive count")
			}
			if *from+*seeds-1 < *from {
				return 0, errors.New("--from plus --seeds passes the largest seed")
			}
			o := hunt.Options{ConfigPath: *configPath, Out: *out, Run: runInto, Log: stderr, Seeds: func(yield func(uint64) bool) {
				for n := range *seeds {
					if !yield(*from + n) {
						return
					}
				}
			}}
			found, err := hunt.Hunt(ctx, o)
			printSummary(stdout, *out)
			// A find outranks a failed seed or a cut-short hunt.
			if err != nil && len(found) > 0 {
				fmt.Fprintln(stderr, err)
				return len(found), nil
			}
			if err != nil {
				return 0, harnessError{err}
			}
			return len(found), nil
		}
	case "shrink":
		command = func() (int, error) {
			if err := required(flags, "config", "plan", "out"); err != nil {
				return 0, err
			}
			found, err := hunt.ShrinkPlan(ctx, hunt.ShrinkOptions{ConfigPath: *configPath, PlanPath: *planPath, Out: *out, Run: runInto, Log: stderr})
			if err != nil && len(found) == 0 {
				// ShrinkPlan wrote no summary.md, and --out may hold a stale one.
				return 0, harnessError{err}
			}
			printSummary(stdout, *out)
			// A find outranks a cut-short shrink.
			if err != nil {
				fmt.Fprintln(stderr, err)
			}
			return len(found), nil
		}
	case "judge":
		tracePath := flags.String("trace", "", "")
		command = func() (int, error) {
			if err := required(flags, "trace"); err != nil {
				return 0, err
			}
			report, findings, err := judge(*configPath, *tracePath)
			if err != nil {
				return 0, harnessError{err}
			}
			fmt.Fprint(stdout, report)
			return findings, nil
		}
	default:
		fmt.Fprintf(stderr, "unknown command %q\n%s", args[0], usage)
		return 2
	}
	if err := flags.Parse(args[1:]); err != nil {
		fmt.Fprintf(stderr, "%v\n%s", err, usage)
		return 2
	}
	if flags.NArg() > 0 {
		fmt.Fprintf(stderr, "unexpected arguments %q\n%s", flags.Args(), usage)
		return 2
	}
	findings, err := command()
	var harness harnessError
	switch {
	case errors.As(err, &harness):
		fmt.Fprintln(stderr, err)
		return 2
	case err != nil:
		fmt.Fprintf(stderr, "%v\n%s", err, usage)
		return 2
	case findings > 0:
		return 1
	}
	return 0
}

type harnessError struct{ error }

// printSummary prints the summary.md that hunt and shrink write, if any.
func printSummary(stdout io.Writer, out string) {
	if summary, err := os.ReadFile(filepath.Join(out, "summary.md")); err == nil {
		stdout.Write(summary)
	}
}

func required(flags *flag.FlagSet, names ...string) error {
	for _, name := range names {
		if flags.Lookup(name).Value.String() == "" {
			return fmt.Errorf("the flag --%s is required", name)
		}
	}
	return nil
}

func drawPlan(configPath string, seed uint64) (*plan.Plan, error) {
	cfg, err := config.Load(configPath)
	if err != nil {
		return nil, err
	}
	g, err := generate.New(cfg)
	if err != nil {
		return nil, err
	}
	return g.Plan(seed)
}

// judge returns report.md and the number of findings.
func judge(configPath, tracePath string) (string, int, error) {
	var cfg *config.Config
	if configPath != "" {
		var err error
		if cfg, err = config.Load(configPath); err != nil {
			return "", 0, err
		}
	}
	if info, err := os.Stat(tracePath); err == nil && info.IsDir() {
		tracePath = filepath.Join(tracePath, "trace.jsonl")
	}
	events, err := trace.Load(tracePath)
	if err != nil {
		return "", 0, err
	}
	ix := trace.NewIndex(events)
	spec, err := check.SpecFor(cfg, ix)
	if err != nil {
		return "", 0, err
	}
	report := check.Judge(ix, spec)
	return check.Markdown(ix, report), len(report.Findings), nil
}

type repeated []string

func (r *repeated) String() string { return strings.Join(*r, " ") }

func (r *repeated) Set(value string) error {
	*r = append(*r, value)
	return nil
}
