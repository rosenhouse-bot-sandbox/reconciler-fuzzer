// Package trace is the single log of a run: every request, object version,
// fault, action and process start and exit. Checks read only the trace.
package trace

import (
	"encoding/json"
	"time"
)

// Caller is who made a request. The listener a request arrives on decides it.
type Caller string

const (
	Controller Caller = "controller"
	Harness    Caller = "harness"
	// Cluster stands for an unknown writer: a version no recorded response
	// produced.
	Cluster Caller = "cluster"
)

// Ref names one object. Namespace is empty for a cluster-scoped object.
type Ref struct {
	System    string `json:"system"`
	Group     string `json:"group,omitempty"`
	Resource  string `json:"resource"`
	Namespace string `json:"namespace,omitempty"`
	Name      string `json:"name,omitempty"`
}

// Event is one entry of trace.jsonl. Exactly one pointer field is set.
type Event struct {
	// Seq is the total order the recorder assigns.
	Seq uint64 `json:"seq"`
	// At is the time since the run started, by the recorder's monotonic clock.
	At time.Duration `json:"at"`

	Run     *Run     `json:"run,omitempty"`
	Phase   *Phase   `json:"phase,omitempty"`
	Action  *Action  `json:"action,omitempty"`
	Fault   *Fault   `json:"fault,omitempty"`
	Call    *Call    `json:"call,omitempty"`
	Version *Version `json:"version,omitempty"`
	Proc    *Proc    `json:"proc,omitempty"`
}

// Run describes the run. Fixture events precede it.
type Run struct {
	Namespace string `json:"namespace"`
	// Primary names the CR kind; its Name is empty.
	Primary Ref `json:"primary"`
	// PlanSteps counts the plan's steps. The runner's delete-all step is
	// step PlanSteps and its restart step PlanSteps+1.
	PlanSteps int `json:"planSteps"`
	// Ready, Stable, Reads and Writes are the config values the checks
	// read, so that judge needs no config.
	Ready  string        `json:"ready,omitempty"`
	Stable time.Duration `json:"stable,omitempty"`
	Reads  int           `json:"reads,omitempty"`
	Writes int           `json:"writes,omitempty"`
}

// PhaseName marks a boundary inside a step.
type PhaseName string

const (
	FaultPeriod PhaseName = "faultPeriod"
	EndFaults   PhaseName = "endFaults"
	Settled     PhaseName = "settled"
	TimedOut    PhaseName = "timedOut"
	Confirmed   PhaseName = "confirmed"
)

// Phase records that step Step entered phase Name. An EndFaults phase
// carries the settle deadline, as time since the run started.
type Phase struct {
	Step     int           `json:"step"`
	Name     PhaseName     `json:"name"`
	Deadline time.Duration `json:"deadline,omitempty"`
	// BackoffBound says that backoffCap, not the fault period, set Deadline.
	BackoffBound bool `json:"backoffBound,omitempty"`
}

// Action records one plan action as the harness applied it.
type Action struct {
	Step  int    `json:"step"`
	Index int    `json:"index"`
	Do    string `json:"do"`
	Slot  string `json:"slot,omitempty"`
	Ref   *Ref   `json:"ref,omitempty"`
	Error string `json:"error,omitempty"`
}

// Fault records that a fault started or ended.
type Fault struct {
	Step  int    `json:"step"`
	Index int    `json:"index"`
	Ended bool   `json:"ended,omitempty"`
	What  string `json:"what"`
}

// Call is one request on one listener, recorded when its response ends. A
// watch is recorded when its stream opens.
type Call struct {
	Caller Caller `json:"caller"`
	Method string `json:"method"`
	Path   string `json:"path"`
	// Verb is the Kubernetes verb, or the lowercased method for a
	// non-resource URL.
	Verb string `json:"verb"`
	// APIVersion is the version segment of the path, such as "v1".
	APIVersion  string        `json:"apiVersion,omitempty"`
	Ref         Ref           `json:"ref"`
	Subresource string        `json:"subresource,omitempty"`
	Watch       bool          `json:"watch,omitempty"`
	Status      int           `json:"status"`
	Latency     time.Duration `json:"latency,omitempty"`
	// Injected names the fault that altered the request.
	Injected string `json:"injected,omitempty"`
	// ResultUID and ResultRV come from the response body of a mutating
	// request. They are empty when the body named no object.
	ResultUID string `json:"resultUID,omitempty"`
	ResultRV  string `json:"resultRV,omitempty"`
	// ResultName is the name the server gave an object created with
	// generateName.
	ResultName string `json:"resultName,omitempty"`
}

// Version is one state of one object, as the observer saw it.
type Version struct {
	Ref     Ref             `json:"ref"`
	UID     string          `json:"uid"`
	RV      string          `json:"rv"`
	Deleted bool            `json:"deleted,omitempty"`
	Body    json.RawMessage `json:"body"`
}

// ProcEvent is "start" or "exit".
type ProcEvent string

const (
	Start ProcEvent = "start"
	Exit  ProcEvent = "exit"
)

// Proc records a controller process start or exit.
type Proc struct {
	Event ProcEvent `json:"event"`
	PID   int       `json:"pid,omitempty"`
	// Exit is the exit status or signal, such as "exit status 2".
	Exit string `json:"exit,omitempty"`
	// HarnessCaused says the harness stopped or killed the process.
	HarnessCaused bool   `json:"harnessCaused,omitempty"`
	StderrTail    string `json:"stderrTail,omitempty"`
}
