// Command reconciler-fuzzer runs plans against a controller, prints the plan
// of a seed, judges a stored trace, hunts over seeds and shrinks a failing
// plan. "reconciler-fuzzer help <command>" describes a command's flags.
//
// judge reads ready, stable and budgets from the trace unless --config is
// given.
//
// Each command exits 1 with findings and 2 on a usage or harness error. A
// hunt with findings exits 1 even when a seed failed or an interrupt cut it
// short.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"math"
	"os"
	"os/signal"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"text/tabwriter"

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
  reconciler-fuzzer plan   --config c.yaml --seed n
  reconciler-fuzzer judge  --trace dir[/trace.jsonl] [--config c.yaml]
  reconciler-fuzzer hunt   --config c.yaml --seeds n [--from s] --out dir [--replays r] [--candidates c] [--launch-arg a]...
  reconciler-fuzzer shrink --config c.yaml --plan plan.json --out dir [--replays r] [--candidates c] [--launch-arg a]...
  reconciler-fuzzer help   [command]
`

// purposes say what each command does, for its help.
var purposes = map[string]string{
	"run":   "Run runs one plan against the controller and prints the report of its trace.",
	"plan":  "Plan prints the plan that run --seed n runs, without running it.",
	"judge": "Judge judges a stored trace and prints its report.",
	"hunt": `Hunt runs the plans of n seeds from s on and prints summary.md. It replays the
plan of a seed with new findings r times. For each finding, it then tries up to
c smaller plans, called shrink candidates, and runs each at most 4 times.`,
	"shrink": `Shrink runs a plan and prints summary.md. It replays a plan with findings r
times. For each finding, it then tries up to c smaller plans, called shrink
candidates, and runs each at most 4 times.`,
}

// helpWords ask for help in place of a command.
var helpWords = []string{"help", "-h", "-help", "--help"}

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
	if slices.Contains(helpWords, args[0]) {
		return help(args[1:], stdout, stderr)
	}
	flags := flag.NewFlagSet(args[0], flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	takes := func(commands ...string) bool { return slices.Contains(commands, args[0]) }
	configUsage := "the target's config file"
	if takes("judge") {
		configUsage = "a config whose ready, stable and budgets replace the trace's"
	}
	configPath := flags.String("config", "", configUsage)
	var out, planPath, seed *string
	var launchArgs repeated
	replays, candidates := count(hunt.DefaultReplays), count(hunt.DefaultCandidates)
	if takes("run", "hunt", "shrink") {
		out = flags.String("out", "", "the directory to write into")
		flags.Var(&launchArgs, "launch-arg", "an argument to append to launch.args; repeat it for more")
	}
	if takes("run", "shrink") {
		planPath = flags.String("plan", "", "the plan file to "+args[0])
	}
	if takes("run", "plan") {
		seed = flags.String("seed", "", "the seed to draw the plan from")
	}
	if takes("hunt", "shrink") {
		flags.Var(&replays, "replays", "how many times to replay a plan with new findings")
		flags.Var(&candidates, "candidates", "how many shrink candidates to try for each finding")
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
		command = func() (int, error) {
			if err := required(flags, "config", "out"); err != nil {
				return 0, err
			}
			o := run.Options{ConfigPath: *configPath, PlanPath: *planPath, Out: *out, LaunchArgs: launchArgs}
			switch {
			case (*planPath == "") == (*seed == ""):
				return 0, errors.New("give exactly one of --plan and --seed")
			case *seed != "":
				var err error
				if o.Plan, err = seedPlan(*configPath, *seed); err != nil {
					return 0, err
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
	case "plan":
		command = func() (int, error) {
			if err := required(flags, "config", "seed"); err != nil {
				return 0, err
			}
			p, err := seedPlan(*configPath, *seed)
			if err != nil {
				return 0, err
			}
			data, err := plan.Marshal(p)
			if err != nil {
				return 0, harnessError{err}
			}
			stdout.Write(data)
			return 0, nil
		}
	case "hunt":
		seeds := flags.Uint64("seeds", 0, "how many seeds to run")
		from := flags.Uint64("from", 1, "the first seed")
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
			o := hunt.Options{ConfigPath: *configPath, Out: *out, Replays: int(replays), Candidates: int(candidates), Run: runInto, Log: stderr,
				Seeds: func(yield func(uint64) bool) {
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
			found, err := hunt.ShrinkPlan(ctx, hunt.ShrinkOptions{ConfigPath: *configPath, PlanPath: *planPath, Out: *out,
				Replays: int(replays), Candidates: int(candidates), Run: runInto, Log: stderr})
			if err != nil {
				return 0, harnessError{err}
			}
			printSummary(stdout, *out)
			return len(found), nil
		}
	case "judge":
		tracePath := flags.String("trace", "", "the trace file, or the run directory that holds trace.jsonl")
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
	err := flags.Parse(args[1:])
	switch {
	case errors.Is(err, flag.ErrHelp):
		printHelp(stdout, args[0], flags)
		return 0
	case err != nil:
		fmt.Fprintf(stderr, "%v\n%s", err, usage)
		return 2
	case flags.NArg() > 0:
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

// help prints the usage, or the help of the command that args name.
func help(args []string, stdout, stderr io.Writer) int {
	switch {
	case len(args) > 1:
		fmt.Fprintf(stderr, "unexpected arguments %q\n%s", args[1:], usage)
		return 2
	case len(args) == 0 || slices.Contains(helpWords, args[0]):
		fmt.Fprint(stdout, usage)
		return 0
	}
	return cli([]string{args[0], "--help"}, stdout, stderr)
}

// printHelp prints the command's usage line, what it does and its flags.
func printHelp(w io.Writer, command string, flags *flag.FlagSet) {
	for line := range strings.Lines(usage) {
		if fields := strings.Fields(line); len(fields) > 1 && fields[1] == command {
			fmt.Fprintf(w, "usage: %s\n\n%s\n\n", strings.Join(fields, " "), purposes[command])
		}
	}
	table := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	flags.VisitAll(func(f *flag.Flag) {
		fmt.Fprintf(table, "  --%s\t%s", f.Name, f.Usage)
		if f.DefValue != "" && f.DefValue != "0" {
			fmt.Fprintf(table, " (default %s)", f.DefValue)
		}
		fmt.Fprintln(table)
	})
	table.Flush()
}

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

// seedPlan draws the plan of seed, the value of --seed.
func seedPlan(configPath, seed string) (*plan.Plan, error) {
	n, err := strconv.ParseUint(seed, 10, 64)
	if err != nil {
		return nil, fmt.Errorf("--seed %s: want a non-negative integer", seed)
	}
	p, err := drawPlan(configPath, n)
	if err != nil {
		return nil, harnessError{err}
	}
	return p, nil
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

// count is a flag that takes a count from 0 to math.MaxInt.
type count int

func (c *count) String() string { return strconv.Itoa(int(*c)) }

func (c *count) Set(value string) error {
	n, err := strconv.ParseUint(value, 10, strconv.IntSize-1)
	if err != nil {
		return fmt.Errorf("want a count from 0 to %d", math.MaxInt)
	}
	*c = count(n)
	return nil
}

type repeated []string

func (r *repeated) String() string { return strings.Join(*r, " ") }

func (r *repeated) Set(value string) error {
	*r = append(*r, value)
	return nil
}
