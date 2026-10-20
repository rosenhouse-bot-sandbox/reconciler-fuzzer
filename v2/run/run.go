// Package run executes a plan: for each step it runs the fault period, ends
// the faults, waits for the settle condition and watches the confirm window.
package run

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"time"

	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/yaml"

	"github.com/rosenhouse-bot-sandbox/reconciler-fuzzer/v2/check"

	"github.com/rosenhouse-bot-sandbox/reconciler-fuzzer/v2/config"
	"github.com/rosenhouse-bot-sandbox/reconciler-fuzzer/v2/interpose"
	"github.com/rosenhouse-bot-sandbox/reconciler-fuzzer/v2/kube"
	"github.com/rosenhouse-bot-sandbox/reconciler-fuzzer/v2/launch"
	"github.com/rosenhouse-bot-sandbox/reconciler-fuzzer/v2/plan"
	"github.com/rosenhouse-bot-sandbox/reconciler-fuzzer/v2/system"
	"github.com/rosenhouse-bot-sandbox/reconciler-fuzzer/v2/trace"
)

// PollInterval is how often the settle wait tests the settle condition.
const PollInterval = 50 * time.Millisecond

// Process is the controller. *launch.Binary implements it.
type Process interface {
	Start(ctx context.Context) error
	Stop(ctx context.Context) error
	Kill(ctx context.Context) error
	Supervise(on bool) error
}

// Cluster applies CR actions as the harness. *kube.Kube implements it.
type Cluster interface {
	Create(ctx context.Context, obj map[string]any) (trace.Ref, error)
	Patch(ctx context.Context, ref trace.Ref, patch []byte) error
	Delete(ctx context.Context, ref trace.Ref) error
	WaitGone(ctx context.Context, ref trace.Ref) error
}

var (
	_ Process     = (*launch.Binary)(nil)
	_ Cluster     = (*kube.Kube)(nil)
	_ FaultTarget = (*interpose.Listener)(nil)
)

// FirstSettled is the earliest time in [from, to] at which the settle
// condition holds. check.FirstSettled implements it.
type FirstSettled func(ix *trace.Index, from, to time.Duration) (time.Duration, bool)

// Runner executes one plan against started systems and a controller.
type Runner struct {
	Config   *config.Config
	Recorder *trace.Recorder
	Cluster  Cluster
	Process  Process
	// Listeners maps a system name to its controller listener, where
	// request faults go.
	Listeners    map[string]FaultTarget
	FirstSettled FirstSettled
	// Sample is the primary sample's body, which a create without an
	// object uses.
	Sample map[string]any
}

// FaultTarget starts request faults. *interpose.Listener implements it.
type FaultTarget interface {
	Fault(f plan.Fault) (end func(), err error)
}

// Options configure Run.
type Options struct {
	ConfigPath string
	PlanPath   string
	// Out receives plan.json, trace.jsonl, controller.log and report.md.
	Out string
	// LaunchArgs are appended to the config's launch args.
	LaunchArgs []string
}

// Run loads the config and plan, starts the Kubernetes system and the
// controller, executes the plan, judges the trace and writes the outputs.
func Run(ctx context.Context, o Options) (findings int, err error) {
	cfg, err := config.Load(o.ConfigPath)
	if err != nil {
		return 0, err
	}
	p, err := plan.Load(o.PlanPath)
	if err != nil {
		return 0, err
	}
	sample, err := loadSample(cfg.Primary.Sample)
	if err != nil {
		return 0, err
	}
	if err := os.MkdirAll(o.Out, 0o755); err != nil {
		return 0, err
	}
	if err := plan.Save(filepath.Join(o.Out, "plan.json"), p); err != nil {
		return 0, err
	}
	rec, err := trace.NewRecorder(filepath.Join(o.Out, "trace.jsonl"))
	if err != nil {
		return 0, err
	}
	err = execute(ctx, o, cfg, p, sample, rec)
	if closeErr := rec.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		return 0, fmt.Errorf("%w; the trace is in %s", err, o.Out)
	}
	ix := trace.NewIndex(rec.Events())
	spec, err := check.SpecFor(cfg, ix)
	if err != nil {
		return 0, err
	}
	report := check.Judge(ix, spec)
	if err := os.WriteFile(filepath.Join(o.Out, "report.md"), []byte(check.Markdown(ix, report)), 0o644); err != nil {
		return 0, err
	}
	return len(report.Findings), nil
}

// execute starts the Kubernetes system and the controller, runs the plan and
// stops both.
func execute(ctx context.Context, o Options, cfg *config.Config, p *plan.Plan, sample map[string]any, rec *trace.Recorder) (err error) {
	k := kube.New(kube.Options{CRDs: cfg.CRDs, RBAC: cfg.Launch.RBAC, Fixtures: cfg.Fixtures, Watch: []schema.GroupVersionKind{cfg.Primary.Kind}, Warm: []map[string]any{sample}})
	endpoints, err := k.Start(ctx, &system.Env{Record: rec.Record, Dir: o.Out, Seed: p.Seed})
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, k.Stop(context.WithoutCancel(ctx))) }()
	primary, err := k.Ref(cfg.Primary.Kind, "")
	if err != nil {
		return err
	}
	run := trace.Event{Run: &trace.Run{
		Namespace: k.Namespace(), Primary: primary, PlanSteps: len(p.Steps),
		Ready: cfg.Primary.Ready, Stable: cfg.Timeouts.Stable, Reads: cfg.Budgets.Reads, Writes: cfg.Budgets.Writes,
	}}
	rec.Record(run)
	spec, err := check.SpecFor(cfg, trace.NewIndex([]trace.Event{run}))
	if err != nil {
		return err
	}

	log, err := os.Create(filepath.Join(o.Out, "controller.log"))
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, log.Close()) }()
	launchOptions, err := expandLaunch(cfg.Launch, o.LaunchArgs, endpoints.Vars)
	if err != nil {
		return err
	}
	launchOptions.Log = log
	launchOptions.OnStart = func(pid int) {
		rec.Record(trace.Event{Proc: &trace.Proc{Event: trace.Start, PID: pid}})
	}
	launchOptions.OnExit = func(e launch.Exit) {
		rec.Record(trace.Event{Proc: &trace.Proc{Event: trace.Exit, PID: e.PID, Exit: e.Status, HarnessCaused: e.HarnessCaused, StderrTail: e.Tail}})
	}
	controller := launch.New(launchOptions)
	defer func() {
		err = errors.Join(err, controller.Supervise(false), controller.Stop(context.WithoutCancel(ctx)))
	}()
	if err := controller.Supervise(true); err != nil {
		return err
	}
	runner := &Runner{
		Config:    cfg,
		Recorder:  rec,
		Cluster:   k,
		Process:   controller,
		Listeners: map[string]FaultTarget{kube.Name: endpoints.Controller},
		FirstSettled: func(ix *trace.Index, from, to time.Duration) (time.Duration, bool) {
			return check.FirstSettled(ix, from, to, spec)
		},
		Sample: sample,
	}
	return runner.Execute(ctx, p)
}

// expandLaunch expands ${systems.<name>.<key>} in the launch args, with
// extra appended, and in the env.
func expandLaunch(l config.Launch, extra []string, kubeVars map[string]string) (launch.Options, error) {
	vars := map[string]string{}
	for key, value := range kubeVars {
		vars["systems."+kube.Name+"."+key] = value
	}
	o := launch.Options{Path: l.Binary, Env: map[string]string{}}
	for _, arg := range append(slices.Clone(l.Args), extra...) {
		expanded, err := config.Expand(arg, vars)
		if err != nil {
			return launch.Options{}, fmt.Errorf("launch args: %w", err)
		}
		o.Args = append(o.Args, expanded)
	}
	for name, value := range l.Env {
		expanded, err := config.Expand(value, vars)
		if err != nil {
			return launch.Options{}, fmt.Errorf("launch env %s: %w", name, err)
		}
		o.Env[name] = expanded
	}
	return o, nil
}

func loadSample(path string) (map[string]any, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var sample map[string]any
	if err := yaml.Unmarshal(data, &sample); err != nil {
		return nil, fmt.Errorf("loading sample %s: %w", path, err)
	}
	return sample, nil
}
