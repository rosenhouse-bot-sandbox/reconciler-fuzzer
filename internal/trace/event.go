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
	// GC and Namespaces are the models of kube-controller-manager's garbage
	// collector and namespace controller.
	GC         Caller = "gc"
	Namespaces Caller = "namespace"
)

// ModelCaller is the caller, and system name, of the stage model of a kind:
// "model:" + resource, then "." + group unless the group is core.
func ModelCaller(group, resource string) Caller {
	if group == "" {
		return Caller("model:" + resource)
	}
	return Caller("model:" + resource + "." + group)
}

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
	// WatchEnd records that a watch Call's stream closed.
	WatchEnd *WatchEnd `json:"watchEnd,omitempty"`
	// Work records that a model started or stopped having work pending.
	Work *Work `json:"work,omitempty"`
	// Seen records that a response showed a caller one object version.
	Seen *Seen `json:"seen,omitempty"`
}

// Run describes the run. Fixture events precede it.
type Run struct {
	Namespace string `json:"namespace"`
	// Primaries are the config's primaries entries, in order. They name
	// the CR kinds.
	Primaries []Primary `json:"primaries,omitempty"`
	// PlanSteps counts the plan's steps. The runner's delete-all step is
	// step PlanSteps and its restart step PlanSteps+1.
	PlanSteps int `json:"planSteps"`
	// Stable, Reads and Writes are the config values the checks read, so
	// that judge needs no config.
	Stable time.Duration `json:"stable,omitempty"`
	Reads  int           `json:"reads,omitempty"`
	Writes int           `json:"writes,omitempty"`
	// Models lists the config's stage models.
	Models []Model `json:"models,omitempty"`
	// StatusKinds lists the installed CRDs with a status subresource.
	StatusKinds []Kind `json:"statusKinds,omitempty"`
	// Kinds names each kube kind the observer watches from the start, such
	// as an installed CRD or one the RBAC names, by a ref with no namespace
	// or name.
	Kinds []Ref `json:"kinds,omitempty"`
	// Owners maps a system's name to its owner CEL, owning-CR rule 2.
	Owners map[string]string `json:"owners,omitempty"`
	// Systems lists the fake services.
	Systems []System `json:"systems,omitempty"`
	// Properties are the config's properties.
	Properties []Property `json:"properties,omitempty"`
	// References are the config's references. The controller's listeners
	// record Seen events of each To kind.
	References []Reference `json:"references,omitempty"`
}

// Reference is one config reference as the checks need it. A fake's To
// holds only its collection as Resource, and System names the fake. Name
// and Namespace are field paths in the From CR, and Ready is CEL over the
// to object.
type Reference struct {
	From       Kind          `json:"from"`
	System     string        `json:"system,omitempty"`
	To         Kind          `json:"to"`
	Namespaced bool          `json:"namespaced,omitempty"`
	Name       string        `json:"name"`
	Namespace  string        `json:"namespace,omitempty"`
	Ready      string        `json:"ready"`
	Required   bool          `json:"required,omitempty"`
	Track      time.Duration `json:"track,omitempty"`
}

// Primary is one primaries entry as the checks need it. Ready, Retain and
// Halted are CEL over self, a CR of Kind; Retain and Halted are empty when
// unset. Owns are its owning-CR rule 3, and Periodic is nil when unset.
type Primary struct {
	Kind     Kind      `json:"kind"`
	Ready    string    `json:"ready"`
	Retain   string    `json:"retain,omitempty"`
	Halted   string    `json:"halted,omitempty"`
	Owns     []Owns    `json:"owns,omitempty"`
	Periodic *Periodic `json:"periodic,omitempty"`
}

// Property is one config property. When is "step" or "always".
type Property struct {
	Name string `json:"name"`
	Expr string `json:"expr"`
	When string `json:"when"`
}

// Periodic declares the CR paths a controller writes on a timer.
type Periodic struct {
	Every time.Duration `json:"every"`
	Paths []string      `json:"paths"`
}

// System is one fake service as the checks need it. Role, Ready and Gone
// are CEL over one of its resources, or empty.
type System struct {
	Name  string `json:"name"`
	API   string `json:"api"`
	Role  string `json:"role,omitempty"`
	Ready string `json:"ready,omitempty"`
	Gone  string `json:"gone,omitempty"`
	// Ignore lists the top-level body fields Restore does not compare.
	Ignore     []string      `json:"ignore,omitempty"`
	Poll       time.Duration `json:"poll,omitempty"`
	Dependency *Dependency   `json:"dependency,omitempty"`
}

// Owns is one owns entry with its kind's resource. Name and Namespace are
// CEL over the CR. System is empty for kube; a fake's Kind holds only its
// collection as Resource.
type Owns struct {
	System    string `json:"system,omitempty"`
	Kind      Kind   `json:"kind"`
	Name      string `json:"name"`
	Namespace string `json:"namespace,omitempty"`
}

// Kind names a kind both ways: by group, version and Kind, and by resource.
type Kind struct {
	Group    string `json:"group,omitempty"`
	Version  string `json:"version"`
	Kind     string `json:"kind"`
	Resource string `json:"resource"`
}

// OfKind reports whether r names a kube object of kind k.
func (r Ref) OfKind(k Kind) bool {
	return r.System == "kube" && r.Group == k.Group && r.Resource == k.Resource
}

// Model is one stage model as the checks need it.
type Model struct {
	Kind Kind `json:"kind"`
	// Ready is CEL over self, an object of the kind.
	Ready string `json:"ready"`
	// Dependency is set when the kind is a readiness dependency of a CR.
	Dependency *Dependency `json:"dependency,omitempty"`
}

// Dependency says how Honest judges a dependency kind.
type Dependency struct {
	Required bool          `json:"required,omitempty"`
	Track    time.Duration `json:"track,omitempty"`
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
	// BackoffBound says that backoffCap, not the time from the step's start
	// until its faults ended, set Deadline.
	BackoffBound bool `json:"backoffBound,omitempty"`
}

// Action records one plan action as the harness applied it. Ref is the CR
// of a CR action, and the object a damage or changeFixture action changed.
type Action struct {
	Step  int    `json:"step"`
	Index int    `json:"index"`
	Do    string `json:"do"`
	Slot  string `json:"slot,omitempty"`
	Ref   *Ref   `json:"ref,omitempty"`
	Error string `json:"error,omitempty"`
}

// SlowDeleteFinalizer is the finalizer a slowDelete fault holds objects
// with, and StuckReason the Ready condition's reason a stuck fault writes.
const (
	SlowDeleteFinalizer = "reconciler-fuzzer.dev/slow-delete"
	StuckReason         = "Stuck"
)

// Fault records that a fault started or ended.
type Fault struct {
	Step  int    `json:"step"`
	Index int    `json:"index"`
	Ended bool   `json:"ended,omitempty"`
	What  string `json:"what"`
	// System is the fault's system. Empty is kube.
	System string `json:"system,omitempty"`
	// Model names the model that took a stuck or slowDelete fault.
	Model string `json:"model,omitempty"`
	// Hold names the kind whose watch events a holdWatch fault holds.
	Hold *Ref `json:"hold,omitempty"`
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
	// RequestName is a create's requested name, or else its generateName.
	RequestName string `json:"requestName,omitempty"`
	// Stream numbers a watch within its listener, so its WatchEnd can name
	// it.
	Stream uint64 `json:"stream,omitempty"`
	// Unmapped says the system's API map could not place the request.
	Unmapped bool `json:"unmapped,omitempty"`
	// Partial says a kube list or watch may leave objects out: it has a
	// label or field selector, or a continue token in its request or in
	// the list it answered with.
	Partial bool `json:"partial,omitempty"`
}

// Unknown outcome effects: the server committed the request, and the caller
// never saw the response.
const (
	DropResponse = "dropResponse"
	KillAfter    = "killAfter"
)

// Unknown says the caller cannot tell whether the request happened.
func (c Call) Unknown() bool { return c.Injected == DropResponse || c.Injected == KillAfter }

// WatchEnd says the watch Call with Stream on Caller's listener for System
// closed.
type WatchEnd struct {
	Caller Caller `json:"caller"`
	System string `json:"system"`
	Stream uint64 `json:"stream"`
}

// Work says whether System, a model, has work pending from this event on.
type Work struct {
	System  string `json:"system"`
	Pending bool   `json:"pending"`
}

// Seen says a response on Caller's listener showed it an object version:
// a get, one item of a list, one watch event, or a write's result.
// Listeners record Seen only for dependency and reference To kinds. Its At
// is when the listener forwarded it.
type Seen struct {
	Caller Caller `json:"caller"`
	Ref    Ref    `json:"ref"`
	UID    string `json:"uid"`
	RV     string `json:"rv"`
	// Deleted marks a watch DELETED event.
	Deleted bool `json:"deleted,omitempty"`
	// Stream is the watch's stream, or zero otherwise.
	Stream uint64 `json:"stream,omitempty"`
	// Opaque marks a response the listener could not decode, which may
	// have shown any object Ref covers. Its UID and RV are empty.
	Opaque bool `json:"opaque,omitempty"`
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
