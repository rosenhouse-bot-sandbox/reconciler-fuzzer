// Command reconciler-fuzzer runs a plan against a controller, or judges a
// stored trace.
//
//	reconciler-fuzzer run --config c.yaml --plan plan.json --out dir [--launch-arg a]...
//	reconciler-fuzzer judge --trace dir[/trace.jsonl] [--config c.yaml]
//
// judge reads ready, stable and budgets from the trace unless --config is
// given.
//
// Both exit 1 when a check fails and 2 on a usage or harness error.
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
	"strings"
	"syscall"

	"github.com/rosenhouse-bot-sandbox/reconciler-fuzzer/v2/check"
	"github.com/rosenhouse-bot-sandbox/reconciler-fuzzer/v2/config"
	"github.com/rosenhouse-bot-sandbox/reconciler-fuzzer/v2/run"
	"github.com/rosenhouse-bot-sandbox/reconciler-fuzzer/v2/trace"
)

const usage = `usage:
  reconciler-fuzzer run --config c.yaml --plan plan.json --out dir [--launch-arg a]...
  reconciler-fuzzer judge --trace dir[/trace.jsonl] [--config c.yaml]
`

// runPlan is run.Run, which a test replaces.
var runPlan = run.Run

func main() { os.Exit(cli(os.Args[1:], os.Stdout, os.Stderr)) }

// cli runs one command and returns the exit code.
func cli(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		fmt.Fprint(stderr, usage)
		return 2
	}
	flags := flag.NewFlagSet(args[0], flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	configPath := flags.String("config", "", "")
	var command func() (int, error)
	switch args[0] {
	case "run":
		planPath := flags.String("plan", "", "")
		out := flags.String("out", "", "")
		var launchArgs repeated
		flags.Var(&launchArgs, "launch-arg", "")
		command = func() (int, error) {
			if err := required(flags, "config", "plan", "out"); err != nil {
				return 0, err
			}
			ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
			defer stop()
			findings, err := runPlan(ctx, run.Options{ConfigPath: *configPath, PlanPath: *planPath, Out: *out, LaunchArgs: launchArgs})
			if err != nil {
				return 0, harnessError{err}
			}
			report, err := os.ReadFile(filepath.Join(*out, "report.md"))
			if err != nil {
				return 0, harnessError{err}
			}
			stdout.Write(report)
			return findings, nil
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

func required(flags *flag.FlagSet, names ...string) error {
	for _, name := range names {
		if flags.Lookup(name).Value.String() == "" {
			return fmt.Errorf("the flag --%s is required", name)
		}
	}
	return nil
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
