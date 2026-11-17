package model

import (
	"maps"
	"reflect"
	"slices"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/rosenhouse-bot-sandbox/reconciler-fuzzer/internal/config"
	"github.com/rosenhouse-bot-sandbox/reconciler-fuzzer/internal/trace"
)

type state int

const (
	wantReady state = iota
	wantStuck
	// progressing is the status of a generation that is not ready yet.
	wantProgressing
)

// want is a status the stage writes. A stuck failure names a failure of
// the stage, or is empty for its default. message is a fault end's.
type want struct {
	state   state
	failure string
	message string
}

// shape writes the status of one stage.
type shape interface {
	// status is the merge patch of h's status for w.
	status(h *held, w want, now time.Time) map[string]any
	// shows says whether h's heard status is w for h's generation.
	shows(h *held, w want) bool
}

func shapeOf(m config.Model) shape {
	switch m.Stage {
	case config.DeploymentStage:
		return echoShape{deploymentStatus}
	case config.PodStage:
		return echoShape{podStatus}
	}
	stuckFields := m.StuckStatus
	if stuckFields == nil {
		stuckFields = m.Status
	}
	return readyConditionShape{readyFields: replacing(m.Status, stuckFields), stuckFields: replacing(stuckFields, m.Status)}
}

// readyConditionShape writes one Ready condition beside literal fields.
type readyConditionShape struct {
	readyFields, stuckFields map[string]any
}

func (r readyConditionShape) status(h *held, w want, now time.Time) map[string]any {
	fields := r.readyFields
	c := metav1.Condition{Type: "Ready", Status: metav1.ConditionTrue, Reason: "Modelled", Message: "modelled ready", ObservedGeneration: h.generation, LastTransitionTime: metav1.NewTime(now)}
	switch {
	case w.state == wantStuck:
		fields, c.Status, c.Reason, c.Message = r.stuckFields, metav1.ConditionFalse, trace.StuckReason, "a stuck fault holds it"
	case w.state == wantProgressing:
		c.Status, c.Reason, c.Message = metav1.ConditionFalse, "Progressing", "a new generation"
	case w.message != "":
		c.Message = w.message
	}
	patch := maps.Clone(fields)
	patch["conditions"] = []any{c}
	return patch
}

// shows reads the Ready condition. A generation needs a progressing write
// only when it inherits a Ready True without observedGeneration, since the
// ready check cannot tell that it is stale.
func (r readyConditionShape) shows(h *held, w want) bool {
	c := h.ready
	switch w.state {
	case wantStuck:
		return c != nil && c.Status == "False" && c.Reason == trace.StuckReason && h.current()
	case wantReady:
		return c != nil && c.Status == "True" && h.current()
	}
	return c == nil || c.Status != "True" || c.ObservedGeneration != nil
}

// echoShape writes a whole status, and takes the API server's answer to its
// last write as what that write shows, since the server drops fields that
// its version of the kind lacks and omits zeros.
type echoShape struct {
	write func(h *held, w want, now time.Time) map[string]any
}

// status keeps the times of what has not changed since the heard status: a
// condition's transition while its status and reason hold, the Pod's start,
// and a container's start while it stays in one state.
func (e echoShape) status(h *held, w want, now time.Time) map[string]any {
	status := e.write(h, w, now)
	if start, kept := h.status["startTime"]; kept && status["startTime"] != nil {
		status["startTime"] = start
	}
	keepTimes(h.status, status, "conditions", "type", func(old, new map[string]any) bool {
		return old["status"] == new["status"] && old["reason"] == new["reason"]
	}, "lastTransitionTime", "lastUpdateTime")
	for _, key := range []string{"initContainerStatuses", "containerStatuses"} {
		keepTimes(h.status, status, key, "name", func(old, new map[string]any) bool {
			return reflect.DeepEqual(stateKind(old["state"]), stateKind(new["state"]))
		}, "state")
	}
	return status
}

// keepTimes copies fields from each item of the heard list key to the item
// of the written list with the same id, where same says they match.
func keepTimes(heard, written map[string]any, key, id string, same func(old, new map[string]any) bool, fields ...string) {
	olds, _ := heard[key].([]any)
	news, _ := written[key].([]any)
	for _, n := range news {
		n, _ := n.(map[string]any)
		for _, o := range olds {
			o, _ := o.(map[string]any)
			if n == nil || o == nil || o[id] != n[id] || !same(o, n) {
				continue
			}
			for _, field := range fields {
				if value, set := o[field]; set {
					n[field] = value
				}
			}
		}
	}
}

// stateKind names a container state, such as running, with its waiting
// reason.
func stateKind(state any) []string {
	var kinds []string
	m, _ := state.(map[string]any)
	for kind, detail := range m {
		reason, _ := detail.(map[string]any)["reason"].(string)
		kinds = append(kinds, kind+"/"+reason)
	}
	slices.Sort(kinds)
	return kinds
}

func (e echoShape) shows(h *held, w want) bool {
	return h.shown != nil && h.shown.state == w.state && h.shown.failure == w.failure &&
		h.wrote == h.generation && reflect.DeepEqual(untimed(h.status), untimed(h.echo))
}

// untimed drops the timestamps of v, which the API server rounds.
func untimed(v any) any {
	switch v := v.(type) {
	case map[string]any:
		out := map[string]any{}
		for key, field := range v {
			if !timeKeys[key] {
				out[key] = untimed(field)
			}
		}
		return out
	case []any:
		out := make([]any, len(v))
		for i, item := range v {
			out[i] = untimed(item)
		}
		return out
	}
	return v
}

var timeKeys = map[string]bool{
	"lastTransitionTime": true, "lastUpdateTime": true, "lastProbeTime": true,
	"startedAt": true, "finishedAt": true, "startTime": true,
}
