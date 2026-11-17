package model

import (
	"maps"
	"reflect"
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

func (e echoShape) status(h *held, w want, now time.Time) map[string]any { return e.write(h, w, now) }

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
