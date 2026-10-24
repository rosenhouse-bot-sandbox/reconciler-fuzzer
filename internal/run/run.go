// Package run executes a plan: for each step it runs the fault period, ends
// the faults, waits for the settle condition and watches the confirm window.
package run

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"time"

	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/yaml"

	"github.com/rosenhouse-bot-sandbox/reconciler-fuzzer/internal/check"

	"github.com/rosenhouse-bot-sandbox/reconciler-fuzzer/internal/config"
	"github.com/rosenhouse-bot-sandbox/reconciler-fuzzer/internal/crud"
	"github.com/rosenhouse-bot-sandbox/reconciler-fuzzer/internal/fake"
	"github.com/rosenhouse-bot-sandbox/reconciler-fuzzer/internal/interpose"
	"github.com/rosenhouse-bot-sandbox/reconciler-fuzzer/internal/kube"
	"github.com/rosenhouse-bot-sandbox/reconciler-fuzzer/internal/launch"
	"github.com/rosenhouse-bot-sandbox/reconciler-fuzzer/internal/model"
	"github.com/rosenhouse-bot-sandbox/reconciler-fuzzer/internal/owner"
	"github.com/rosenhouse-bot-sandbox/reconciler-fuzzer/internal/plan"
	"github.com/rosenhouse-bot-sandbox/reconciler-fuzzer/internal/s3"
	"github.com/rosenhouse-bot-sandbox/reconciler-fuzzer/internal/system"
	"github.com/rosenhouse-bot-sandbox/reconciler-fuzzer/internal/trace"
	"github.com/rosenhouse-bot-sandbox/reconciler-fuzzer/internal/vault"
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
	Delete(ctx context.Context, ref trace.Ref, propagation plan.Propagation) error
	Gone(ctx context.Context, ref trace.Ref) (bool, error)
	WaitGone(ctx context.Context, ref trace.Ref) error
}

var (
	_ Process     = (*launch.Binary)(nil)
	_ Cluster     = (*kube.Kube)(nil)
	_ FaultTarget = (*interpose.Listener)(nil)
	_ Actor       = (*kube.Kube)(nil)
	_ Actor       = (*fake.Fake)(nil)
	_ ModelTarget = (*fake.Fake)(nil)
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
	Listeners map[string]FaultTarget
	// Models take the model faults, stuck and slowDelete.
	Models []ModelTarget
	// Actors maps a system name to what damages its objects.
	Actors       map[string]Actor
	FirstSettled FirstSettled
	// Sample is the first entry's sample, which a create without an object
	// uses.
	Sample map[string]any
	// Select resolves a damage or changeFixture target. owner.World.Select
	// implements it.
	Select Select
	// Failed returns the error of a failed system, such as a fake that
	// exited. Nil means none can fail.
	Failed func() error
}

func (r *Runner) failed() error {
	if r.Failed == nil {
		return nil
	}
	return r.Failed()
}

// Actor changes a system's objects as the harness. *kube.Kube and
// *fake.Fake implement it.
type Actor interface {
	Patch(ctx context.Context, ref trace.Ref, patch []byte) error
	Delete(ctx context.Context, ref trace.Ref, propagation plan.Propagation) error
}

// Select resolves target at t among the live instances creator made, owned
// by slot's CR when slot is set. An error that is owner.ErrNoMatch says why
// none matches.
type Select func(ix *trace.Index, creator trace.Caller, target plan.Target, slot string, t time.Duration) (trace.Ref, error)

// FaultTarget starts request faults. *interpose.Listener implements it.
type FaultTarget interface {
	Fault(f plan.Fault) (end func(), err error)
}

// ModelTarget is a stage model. *model.Stage implements it.
type ModelTarget interface {
	FaultTarget
	Name() string
	// Takes reports whether f is a model fault on the model's kind.
	Takes(f plan.Fault) bool
}

var _ ModelTarget = (*model.Stage)(nil)

// Options configure Run.
type Options struct {
	ConfigPath string
	// Plan is used when PlanPath is empty.
	PlanPath string
	Plan     *plan.Plan
	// Out receives plan.json, trace.jsonl, controller.log and report.md.
	Out string
	// LaunchArgs are appended to the config's launch args.
	LaunchArgs []string
}

// Run loads the config and plan, starts the fake services, the Kubernetes
// system, the models and the controller, executes the plan, judges the trace and writes the outputs.
func Run(ctx context.Context, o Options) (check.Report, error) {
	cfg, err := config.Load(o.ConfigPath)
	if err != nil {
		return check.Report{}, err
	}
	p := o.Plan
	if o.PlanPath != "" {
		if p, err = plan.Load(o.PlanPath); err != nil {
			return check.Report{}, err
		}
	} else if err := p.Validate(); err != nil {
		return check.Report{}, err
	}
	if err := p.ValidateKinds(cfg.ListedKinds()); err != nil {
		return check.Report{}, err
	}
	samples, err := loadSamples(cfg.Primaries)
	if err != nil {
		return check.Report{}, err
	}
	if err := os.MkdirAll(o.Out, 0o755); err != nil {
		return check.Report{}, err
	}
	if err := plan.Save(filepath.Join(o.Out, "plan.json"), p); err != nil {
		return check.Report{}, err
	}
	rec, err := trace.NewRecorder(filepath.Join(o.Out, "trace.jsonl"))
	if err != nil {
		return check.Report{}, err
	}
	err = execute(ctx, o, cfg, p, samples, rec)
	if closeErr := rec.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		return check.Report{}, fmt.Errorf("%w; the trace is in %s", err, o.Out)
	}
	ix := trace.NewIndex(rec.Events())
	spec, err := check.SpecFor(cfg, ix)
	if err != nil {
		return check.Report{}, err
	}
	report := check.Judge(ix, spec)
	if err := os.WriteFile(filepath.Join(o.Out, "report.md"), []byte(check.Markdown(ix, report)), 0o644); err != nil {
		return check.Report{}, err
	}
	return report, nil
}

// execute starts the fake services, the Kubernetes system, the models and
// the controller, runs the plan and stops them. samples are the entries'
// samples, in config order.
func execute(ctx context.Context, o Options, cfg *config.Config, p *plan.Plan, samples []map[string]any, rec *trace.Recorder) (err error) {
	env := &system.Env{Record: rec.Record, Dir: o.Out, Seed: p.Seed}
	vars := map[string]map[string]string{}
	controllerListeners := map[string]*interpose.Listener{}
	actors := map[string]Actor{}
	var targets []ModelTarget
	var fakes []*fake.Fake
	for _, s := range cfg.Fakes() {
		fo, err := fakeOptions(s)
		if err != nil {
			return err
		}
		f := fake.New(fo)
		defer func() { err = errors.Join(err, f.Stop(context.WithoutCancel(ctx))) }()
		fakeEndpoints, err := f.Start(ctx, env)
		if err != nil {
			return fmt.Errorf("starting fake %s: %w", s.Name, err)
		}
		vars[s.Name], controllerListeners[s.Name], actors[s.Name] = fakeEndpoints.Vars, fakeEndpoints.Controller, f
		targets, fakes = append(targets, f), append(fakes, f)
	}
	watch, seen := modelKinds(cfg.Models)
	k := kube.New(kube.Options{
		CRDs: cfg.CRDs, RBAC: cfg.Launch.RBAC, Fixtures: cfg.Fixtures, Vars: flatten(vars), Warm: samples,
		Watch: slices.Concat(primaryKinds(cfg.Primaries), watch), Seen: seen,
	})
	endpoints, err := k.Start(ctx, env)
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, k.Stop(context.WithoutCancel(ctx))) }()
	vars[kube.Name], controllerListeners[kube.Name], actors[kube.Name] = endpoints.Vars, endpoints.Controller, k
	systems := model.New(k)
	for _, m := range cfg.Models {
		stage, err := model.NewStage(k, m)
		if err != nil {
			return err
		}
		systems, targets = append(systems, stage), append(targets, stage)
	}
	for _, m := range systems {
		if stopper, ok := m.(system.Stopper); ok {
			defer func() { err = errors.Join(err, stopper.Stop(context.WithoutCancel(ctx))) }()
		}
		if _, err := m.Start(ctx, env); err != nil {
			return fmt.Errorf("starting model %s: %w", m.Name(), err)
		}
	}
	run, err := runEvent(cfg, k, len(p.Steps))
	if err != nil {
		return err
	}
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
	launchOptions, err := expandLaunch(cfg.Launch, o.LaunchArgs, vars)
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
	listeners := map[string]FaultTarget{}
	for name, l := range controllerListeners {
		l.SetKill(controller.Kill)
		listeners[name] = l
	}
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
		Listeners: listeners,
		Models:    targets,
		Actors:    actors,
		FirstSettled: func(ix *trace.Index, from, to time.Duration) (time.Duration, bool) {
			return check.FirstSettled(ix, from, to, spec)
		},
		Sample: samples[0],
		Failed: func() error {
			var errs []error
			for _, f := range fakes {
				errs = append(errs, f.Err())
			}
			return errors.Join(errs...)
		},
		Select: func(ix *trace.Index, creator trace.Caller, target plan.Target, slot string, t time.Duration) (trace.Ref, error) {
			return owner.New(ix, spec.Owners).Select(creator, target, slot, t)
		},
	}
	if err := runner.Quiet(ctx); err != nil {
		return err
	}
	return runner.Execute(ctx, p)
}

// fakeOptions configure the fake of s, with its fixtures and token.
func fakeOptions(s config.System) (fake.Options, error) {
	api, err := apiMap(s)
	if err != nil {
		return fake.Options{}, err
	}
	o := fake.Options{Name: s.Name, Command: s.Process.Command, Dir: s.Process.Dir, API: api, SeenAll: s.Dependency != nil, ModelFaults: s.Map().ModelFaults}
	if s.Token != "" {
		o.Vars = map[string]string{"token": s.Token}
	}
	for _, file := range s.Fixtures {
		fixtures, err := config.ReadSystemFixtures(file)
		if err != nil {
			return fake.Options{}, fmt.Errorf("system %s: %w", s.Name, err)
		}
		for _, f := range fixtures {
			body, err := json.Marshal(f.Body)
			if err != nil {
				return fake.Options{}, err
			}
			o.Fixtures = append(o.Fixtures, fake.Fixture{Resource: f.Resource, Name: f.Name, Body: body})
		}
	}
	return o, nil
}

// flatten names each system's vars as systems.<name>.<key>.
func flatten(systemVars map[string]map[string]string) map[string]string {
	vars := map[string]string{}
	for system, values := range systemVars {
		for key, value := range values {
			vars["systems."+system+"."+key] = value
		}
	}
	return vars
}

// apiMap returns the map s.API names.
func apiMap(s config.System) (fake.API, error) {
	switch s.API {
	case crud.Name:
		return crud.New(s.Name), nil
	case vault.Name:
		return vault.New(s.Name, s.Token), nil
	case s3.Name:
		return s3.New(s.Name), nil
	}
	return nil, fmt.Errorf("system %s: unknown api %q", s.Name, s.API)
}

// runEvent describes the run for the checks.
func runEvent(cfg *config.Config, k *kube.Kube, planSteps int) (trace.Event, error) {
	models, err := traceModels(cfg.Models, k.Ref)
	if err != nil {
		return trace.Event{}, err
	}
	owners, primaries, err := owner.Sources(cfg, func(gvk schema.GroupVersionKind) (string, bool, error) {
		ref, err := k.Ref(gvk, "")
		return ref.Resource, ref.Namespace != "", err
	})
	if err != nil {
		return trace.Event{}, err
	}
	return trace.Event{Run: &trace.Run{
		Namespace: k.Namespace(), Primaries: primaries, PlanSteps: planSteps,
		Stable: cfg.Timeouts.Stable, Reads: cfg.Budgets.Reads, Writes: cfg.Budgets.Writes,
		Models: models, StatusKinds: k.StatusKinds(), Kinds: k.Kinds(),
		Owners: owners, Systems: owner.Systems(cfg), Properties: owner.Properties(cfg),
	}}, nil
}

// modelKinds returns the kinds the observer watches for the models, and the
// dependency kinds the controller listener records Seen events for.
func modelKinds(models []config.Model) (watch, seen []schema.GroupVersionKind) {
	for _, m := range models {
		watch = append(watch, m.Kind)
		if m.Dependency != nil {
			seen = append(seen, m.Kind)
		}
	}
	return watch, seen
}

// traceModels describes the models for the run event. ref maps a kind to
// its resource.
func traceModels(models []config.Model, ref func(schema.GroupVersionKind, string) (trace.Ref, error)) ([]trace.Model, error) {
	var described []trace.Model
	for _, m := range models {
		r, err := ref(m.Kind, "")
		if err != nil {
			return nil, err
		}
		d := trace.Model{
			Kind:  trace.Kind{Group: m.Kind.Group, Version: m.Kind.Version, Kind: m.Kind.Kind, Resource: r.Resource},
			Ready: config.ReadyConditionCEL,
		}
		if m.Dependency != nil {
			d.Dependency = &trace.Dependency{Required: m.Dependency.Required, Track: m.Dependency.Track}
		}
		described = append(described, d)
	}
	return described, nil
}

// primaryKinds returns each entry's kind and the kube kinds of its owns.
func primaryKinds(primaries []config.Primary) []schema.GroupVersionKind {
	var kinds []schema.GroupVersionKind
	for _, p := range primaries {
		kinds = append(kinds, p.Kind)
		for _, o := range p.Owns {
			if o.System == "" {
				kinds = append(kinds, o.Kind)
			}
		}
	}
	return kinds
}

// expandLaunch expands ${systems.<name>.<key>} in the launch args, with
// extra appended, and in the env. systemVars maps a system to its Vars.
func expandLaunch(l config.Launch, extra []string, systemVars map[string]map[string]string) (launch.Options, error) {
	vars := flatten(systemVars)
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

// loadSamples reads each entry's sample, in config order. A sample of
// another kind than its entry's is an error.
func loadSamples(primaries []config.Primary) ([]map[string]any, error) {
	var samples []map[string]any
	for _, p := range primaries {
		data, err := os.ReadFile(p.Sample)
		if err != nil {
			return nil, err
		}
		var sample map[string]any
		if err := yaml.Unmarshal(data, &sample); err != nil {
			return nil, fmt.Errorf("loading sample %s: %w", p.Sample, err)
		}
		apiVersion, _ := sample["apiVersion"].(string)
		kind, _ := sample["kind"].(string)
		if got := schema.FromAPIVersionAndKind(apiVersion, kind).GroupKind(); got != p.Kind.GroupKind() {
			return nil, fmt.Errorf("loading sample %s: it holds a %s, not a %s", p.Sample, got, p.Kind.GroupKind())
		}
		samples = append(samples, sample)
	}
	return samples, nil
}
