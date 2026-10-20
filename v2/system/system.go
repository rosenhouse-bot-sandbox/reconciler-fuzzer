// Package system defines what every party outside the controller implements.
package system

import (
	"context"
	"encoding/json"

	"github.com/rosenhouse-bot-sandbox/reconciler-fuzzer/v2/interpose"
	"github.com/rosenhouse-bot-sandbox/reconciler-fuzzer/v2/plan"
	"github.com/rosenhouse-bot-sandbox/reconciler-fuzzer/v2/trace"
)

// System is anything outside the controller that it talks to or depends on.
// No check names a system.
type System interface {
	Name() string
	// Start the system with two listeners: one for the controller, one for
	// the harness.
	Start(ctx context.Context, env *Env) (Endpoints, error)
	// List every resource directly, bypassing the listeners. This is the
	// ground truth.
	Snapshot(ctx context.Context) ([]Resource, error)
	// Say whether the system has work scheduled, such as a delayed status
	// change.
	Pending() bool
	// Start a fault inside the system. Calling end() ends it. Request faults
	// go to the controller listener instead.
	Fault(f plan.Fault) (end func(), err error)
}

// Env is what the harness gives a system to start.
type Env struct {
	// Record appends to the run's trace.
	Record func(trace.Event)
	// Dir is a scratch directory the system owns, such as for a kubeconfig.
	Dir  string
	Seed uint64
}

// Endpoints are what a started system offers the controller and harness.
type Endpoints struct {
	Controller *interpose.Listener
	Harness    *interpose.Listener
	// Vars expand as ${systems.<name>.<key>} in launch args and env.
	Vars map[string]string
}

// Resource is one object in a snapshot.
type Resource struct {
	Ref  trace.Ref
	UID  string
	Body json.RawMessage
}

// Stopper is a system that holds processes or servers to stop after a run.
type Stopper interface {
	Stop(ctx context.Context) error
}
