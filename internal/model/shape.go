package model

import (
	"cmp"
	"fmt"
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
	conditions := m.Conditions
	if len(conditions) == 0 {
		conditions = []config.Condition{{Type: "Ready"}}
	}
	return conditionsShape{readyFields: compiled(replacing(m.Status, stuckFields), m), stuckFields: compiled(replacing(stuckFields, m.Status), m), conditions: conditions}
}

// compiled renders fields over an object. config.Load refuses fields that
// do not compile.
func compiled(fields map[string]any, m config.Model) func(self map[string]any) map[string]any {
	render, err := config.CompileStatus(fields, m.Schema)
	if err != nil {
		panic(fmt.Sprintf("model %s: %v", m.Kind, err))
	}
	return render
}

// conditionsShape writes its conditions beside fields that it renders over
// the object.
type conditionsShape struct {
	readyFields, stuckFields func(self map[string]any) map[string]any
	conditions               []config.Condition
}

func (r conditionsShape) status(h *held, w want, now time.Time) map[string]any {
	fields := r.readyFields
	if w.state == wantStuck && w.failure == "" {
		fields = r.stuckFields
	}
	var conditions []any
	for _, declared := range r.conditions {
		conditions = append(conditions, r.condition(declared, h, w, now))
	}
	patch := fields(h.object)
	patch["conditions"] = conditions
	return patch
}

func (r conditionsShape) condition(declared config.Condition, h *held, w want, now time.Time) metav1.Condition {
	c := metav1.Condition{
		Type: declared.Type, Status: metav1.ConditionTrue, Reason: cmp.Or(declared.Reason, "Modelled"), Message: cmp.Or(w.message, declared.Message, "modelled ready"),
		ObservedGeneration: h.generation, LastTransitionTime: metav1.NewTime(now),
	}
	switch {
	case holds(declared, w):
		c.Status, c.Reason, c.Message = metav1.ConditionFalse, stuckReason(declared), "a stuck fault holds it"
	case w.state == wantProgressing:
		c.Status, c.Reason, c.Message = metav1.ConditionFalse, "Progressing", "a new generation"
	}
	return c
}

// holds says whether w holds c False: a stuck fault holds the condition its
// failure names, or each condition without one.
func holds(c config.Condition, w want) bool {
	return w.state == wantStuck && (w.failure == "" || w.failure == c.Type)
}

func stuckReason(c config.Condition) string { return cmp.Or(c.StuckReason, trace.StuckReason) }

// shows reads each condition. A generation needs a progressing write only
// when it inherits a True condition without observedGeneration, since the
// ready check cannot tell that it is stale.
func (r conditionsShape) shows(h *held, w want) bool {
	for _, declared := range r.conditions {
		c := h.condition(declared.Type)
		var shown bool
		switch {
		case holds(declared, w):
			shown = c != nil && c.Status == "False" && c.Reason == stuckReason(declared) && h.current(c)
		case w.state != wantProgressing:
			shown = c != nil && c.Status == "True" && h.current(c)
		default:
			shown = c == nil || c.Status != "True" || c.ObservedGeneration != nil
		}
		if !shown {
			return false
		}
	}
	return true
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
