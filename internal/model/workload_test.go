package model

import (
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/rosenhouse-bot-sandbox/reconciler-fuzzer/internal/config"
	"github.com/rosenhouse-bot-sandbox/reconciler-fuzzer/internal/plan"
	"github.com/rosenhouse-bot-sandbox/reconciler-fuzzer/internal/trace"
)

// asJSON rounds status through JSON, as the API server does.
func asJSON(t *testing.T, status map[string]any) map[string]any {
	t.Helper()
	raw, err := json.Marshal(status)
	if err != nil {
		t.Fatal(err)
	}
	var out map[string]any
	json.Unmarshal(raw, &out)
	return out
}

func conditionsOf(status map[string]any) map[string]string {
	out := map[string]string{}
	for _, c := range status["conditions"].([]any) {
		c := c.(map[string]any)
		out[c["type"].(string)] = fmt.Sprint(c["status"], "/", c["reason"])
	}
	return out
}

func counts(status map[string]any) [4]any {
	return [4]any{status["replicas"], status["updatedReplicas"], status["availableReplicas"], status["unavailableReplicas"]}
}

func deploymentHeld(spec, status map[string]any) *held {
	return &held{ref: trace.Ref{Name: "web"}, generation: 3, spec: spec, status: status}
}

func TestDeploymentStatusRollsOut(t *testing.T) {
	ready, err := config.CompileReady((config.Model{Stage: config.DeploymentStage}).Ready(), nil)
	if err != nil {
		t.Fatal(err)
	}
	three := map[string]any{"replicas": 3.0}
	for name, c := range map[string]struct {
		h          *held
		w          want
		counts     [4]any
		conditions map[string]string
	}{
		"ready": {deploymentHeld(three, nil), want{state: wantReady},
			[4]any{3.0, 3.0, 3.0, nil}, map[string]string{"Available": "True/MinimumReplicasAvailable", "Progressing": "True/NewReplicaSetAvailable"}},
		"first rollout": {deploymentHeld(three, nil), want{state: wantProgressing},
			[4]any{1.0, 1.0, 0.0, 1.0}, map[string]string{"Available": "False/MinimumReplicasUnavailable", "Progressing": "True/ReplicaSetUpdated"}},
		"next rollout keeps the old replicas": {deploymentHeld(three, map[string]any{"availableReplicas": 3.0}), want{state: wantProgressing},
			[4]any{4.0, 1.0, 3.0, 1.0}, map[string]string{"Available": "True/MinimumReplicasAvailable", "Progressing": "True/ReplicaSetUpdated"}},
		"scale down": {deploymentHeld(map[string]any{"replicas": 1.0}, map[string]any{"availableReplicas": 3.0}), want{state: wantStuck},
			[4]any{2.0, 1.0, 1.0, 1.0}, map[string]string{"Available": "True/MinimumReplicasAvailable", "Progressing": "True/ReplicaSetUpdated"}},
		"too few available": {deploymentHeld(map[string]any{"replicas": 4.0}, map[string]any{"availableReplicas": 2.0}), want{state: wantStuck},
			[4]any{3.0, 1.0, 2.0, 1.0}, map[string]string{"Available": "False/MinimumReplicasUnavailable", "Progressing": "True/ReplicaSetUpdated"}},
		"maxUnavailable": {deploymentHeld(map[string]any{"replicas": 4.0, "strategy": map[string]any{"rollingUpdate": map[string]any{"maxUnavailable": 2.0}}},
			map[string]any{"availableReplicas": 2.0}), want{state: wantStuck},
			[4]any{3.0, 1.0, 2.0, 1.0}, map[string]string{"Available": "True/MinimumReplicasAvailable", "Progressing": "True/ReplicaSetUpdated"}},
		"recreate": {deploymentHeld(map[string]any{"replicas": 3.0, "strategy": map[string]any{"type": "Recreate"}}, map[string]any{"availableReplicas": 3.0}), want{state: wantStuck},
			[4]any{1.0, 1.0, 0.0, 1.0}, map[string]string{"Available": "False/MinimumReplicasUnavailable", "Progressing": "True/ReplicaSetUpdated"}},
		"deadline exceeded": {deploymentHeld(three, map[string]any{"availableReplicas": 3.0}), want{state: wantStuck, failure: "progressDeadlineExceeded"},
			[4]any{4.0, 1.0, 3.0, 1.0}, map[string]string{"Available": "True/MinimumReplicasAvailable", "Progressing": "False/ProgressDeadlineExceeded"}},
		"replica failure": {deploymentHeld(three, map[string]any{"availableReplicas": 3.0}), want{state: wantStuck, failure: "replicaFailure"},
			[4]any{3.0, 0.0, 3.0, 0.0}, map[string]string{"Available": "True/MinimumReplicasAvailable", "Progressing": "True/ReplicaSetUpdated", "ReplicaFailure": "True/FailedCreate"}},
	} {
		status := asJSON(t, deploymentStatus(c.h, c.w, time.Unix(0, 0)))
		if got := counts(status); got != c.counts {
			t.Errorf("%s: replicas, updated, available, unavailable = %v, want %v", name, got, c.counts)
		}
		if got := conditionsOf(status); !reflect.DeepEqual(got, c.conditions) {
			t.Errorf("%s: conditions = %v, want %v", name, got, c.conditions)
		}
		if status["observedGeneration"] != 3.0 {
			t.Errorf("%s: observedGeneration = %v", name, status["observedGeneration"])
		}
		obj := map[string]any{"metadata": map[string]any{"generation": 3}, "spec": c.h.spec, "status": status}
		if got, err := ready(obj); err != nil || got != (c.w.state == wantReady) {
			t.Errorf("%s: ready = %v, %v", name, got, err)
		}
	}
}

func container(name string, sidecar bool) map[string]any {
	c := map[string]any{"name": name, "image": "example.com/" + name}
	if sidecar {
		c["restartPolicy"] = "Always"
	}
	return c
}

// podSpec has a sidecar, an init container and two containers.
var podSpec = map[string]any{
	"initContainers": []any{container("proxy", true), container("migrate", false)},
	"containers":     []any{container("app", false), container("metrics", false)},
}

// stateOf names each container's state and readiness, as "app=running+".
func stateOf(status map[string]any) string {
	var states []string
	for _, key := range []string{"initContainerStatuses", "containerStatuses"} {
		list, _ := status[key].([]any)
		for _, c := range list {
			c := c.(map[string]any)
			state := c["state"].(map[string]any)
			for kind, detail := range state {
				s := kind
				if reason, _ := detail.(map[string]any)["reason"].(string); reason != "" && kind == "waiting" {
					s = reason
				}
				if c["ready"] == true {
					s += "+"
				}
				states = append(states, c["name"].(string)+"="+s)
			}
		}
	}
	return strings.Join(states, " ")
}

func TestPodStatusShowsEachFailure(t *testing.T) {
	ready, err := config.CompileReady((config.Model{Stage: config.PodStage}).Ready(), nil)
	if err != nil {
		t.Fatal(err)
	}
	bare := map[string]any{"containers": []any{container("app", false)}}
	for name, c := range map[string]struct {
		spec       map[string]any
		w          want
		phase      string
		containers string
		conditions map[string]string
	}{
		"ready": {podSpec, want{state: wantReady}, "Running", "proxy=running+ migrate=terminated+ app=running+ metrics=running+",
			map[string]string{"PodReadyToStartContainers": "True/<nil>", "Initialized": "True/<nil>", "Ready": "True/<nil>", "ContainersReady": "True/<nil>", "PodScheduled": "True/<nil>"}},
		"creating": {podSpec, want{state: wantProgressing}, "Pending", "proxy=PodInitializing migrate=PodInitializing app=PodInitializing metrics=PodInitializing",
			map[string]string{"PodReadyToStartContainers": "False/<nil>", "Initialized": "False/ContainersNotInitialized", "Ready": "False/ContainersNotReady", "ContainersReady": "False/ContainersNotReady", "PodScheduled": "True/<nil>"}},
		"stuck": {bare, want{state: wantStuck}, "Pending", "app=ContainerCreating",
			map[string]string{"PodReadyToStartContainers": "False/<nil>", "Initialized": "True/<nil>", "Ready": "False/ContainersNotReady", "ContainersReady": "False/ContainersNotReady", "PodScheduled": "True/<nil>"}},
		"unschedulable": {podSpec, want{state: wantStuck, failure: "unschedulable"}, "Pending", "",
			map[string]string{"PodScheduled": "False/Unschedulable"}},
		"imagePullBackOff": {podSpec, want{state: wantStuck, failure: "imagePullBackOff"}, "Pending", "proxy=running+ migrate=terminated+ app=ImagePullBackOff metrics=running+",
			map[string]string{"PodReadyToStartContainers": "True/<nil>", "Initialized": "True/<nil>", "Ready": "False/ContainersNotReady", "ContainersReady": "False/ContainersNotReady", "PodScheduled": "True/<nil>"}},
		"crashLoopBackOff": {podSpec, want{state: wantStuck, failure: "crashLoopBackOff"}, "Running", "proxy=running+ migrate=terminated+ app=CrashLoopBackOff metrics=running+",
			map[string]string{"PodReadyToStartContainers": "True/<nil>", "Initialized": "True/<nil>", "Ready": "False/ContainersNotReady", "ContainersReady": "False/ContainersNotReady", "PodScheduled": "True/<nil>"}},
		"initContainerFailed": {podSpec, want{state: wantStuck, failure: "initContainerFailed"}, "Pending", "proxy=running+ migrate=CrashLoopBackOff app=PodInitializing metrics=PodInitializing",
			map[string]string{"PodReadyToStartContainers": "True/<nil>", "Initialized": "False/ContainersNotInitialized", "Ready": "False/ContainersNotReady", "ContainersReady": "False/ContainersNotReady", "PodScheduled": "True/<nil>"}},
		"sidecarFailed": {podSpec, want{state: wantStuck, failure: "sidecarFailed"}, "Running", "proxy=CrashLoopBackOff migrate=terminated+ app=running+ metrics=running+",
			map[string]string{"PodReadyToStartContainers": "True/<nil>", "Initialized": "True/<nil>", "Ready": "False/ContainersNotReady", "ContainersReady": "False/ContainersNotReady", "PodScheduled": "True/<nil>"}},
		"no init container": {bare, want{state: wantStuck, failure: "initContainerFailed"}, "Pending", "app=ContainerCreating",
			map[string]string{"PodReadyToStartContainers": "False/<nil>", "Initialized": "True/<nil>", "Ready": "False/ContainersNotReady", "ContainersReady": "False/ContainersNotReady", "PodScheduled": "True/<nil>"}},
		"no sidecar": {bare, want{state: wantStuck, failure: "sidecarFailed"}, "Pending", "app=ContainerCreating",
			map[string]string{"PodReadyToStartContainers": "False/<nil>", "Initialized": "True/<nil>", "Ready": "False/ContainersNotReady", "ContainersReady": "False/ContainersNotReady", "PodScheduled": "True/<nil>"}},
	} {
		h := &held{ref: trace.Ref{Name: "p"}, generation: 1, spec: c.spec}
		status := asJSON(t, podStatus(h, c.w, time.Unix(0, 0)))
		if status["phase"] != c.phase {
			t.Errorf("%s: phase = %v, want %s", name, status["phase"], c.phase)
		}
		if got := stateOf(status); got != c.containers {
			t.Errorf("%s: containers = %q, want %q", name, got, c.containers)
		}
		if got := conditionsOf(status); !reflect.DeepEqual(got, c.conditions) {
			t.Errorf("%s: conditions = %v, want %v", name, got, c.conditions)
		}
		if scheduled := status["podIP"] != nil; scheduled != (c.w.failure != "unschedulable") {
			t.Errorf("%s: podIP = %v", name, status["podIP"])
		}
		obj := map[string]any{"metadata": map[string]any{"generation": 1}, "status": status}
		if got, err := ready(obj); err != nil || got != (c.w.state == wantReady) {
			t.Errorf("%s: ready = %v, %v", name, got, err)
		}
	}
}

// echo answers the stage's writes with versions of the merged status, as an
// API server that keeps every field would.
func TestAWorkloadStageWritesProgressingAtOnceThenReadyAfterTheDelay(t *testing.T) {
	m := config.Model{Kind: gadgetGVR.GroupVersion().WithKind("Pod"), Stage: config.PodStage, After: after}
	s, w, _ := testStageOf(t, m)
	p := podObject{rv: "1", generation: 1}
	s.hear(p.version())
	writes := w.await(t, 1)
	if got := statusOf(writes[0])["phase"]; got != "Pending" {
		t.Fatalf("first write's phase = %v, want Pending", got)
	}
	p.rv, p.status = writes[0].rv, statusOf(writes[0])
	s.hear(p.version())
	writes = w.await(t, 2)
	if got := writes[1].at.Sub(writes[0].at); got < after/2 {
		t.Errorf("ready came %v after progressing, want about %v", got, after)
	}
	if got := statusOf(writes[1])["phase"]; got != "Running" {
		t.Fatalf("second write's phase = %v, want Running", got)
	}
	p.rv, p.status = writes[1].rv, statusOf(writes[1])
	s.hear(p.version())
	w.still(t, 2, 3*after)

	// A status that someone else changed is written again.
	p.rv, p.status = "200", map[string]any{"phase": "Failed"}
	s.hear(p.version())
	if got := statusOf(w.await(t, 3)[2])["phase"]; got != "Running" {
		t.Errorf("rewrite's phase = %v, want Running", got)
	}

	end, err := s.Fault(plan.Fault{Match: plan.Match{Group: gadgetGVR.Group, Resource: gadgetGVR.Resource}, Stuck: true, Failure: "crashLoopBackOff"})
	if err != nil {
		t.Fatal(err)
	}
	defer end()
	writes = w.await(t, 4)
	if got := stateOf(statusOf(writes[3])); got != "c=CrashLoopBackOff" {
		t.Errorf("stuck containers = %q", got)
	}
}

func statusOf(w write) map[string]any {
	status, _ := w.patch["status"].(map[string]any)
	return status
}

type podObject struct {
	rv         string
	generation int64
	status     map[string]any
}

func (p podObject) version() trace.Version {
	body := map[string]any{"apiVersion": "v1", "kind": "Pod",
		"metadata": map[string]any{"name": "p", "namespace": ns, "uid": "u", "resourceVersion": p.rv, "generation": p.generation},
		"spec":     map[string]any{"containers": []any{map[string]any{"name": "c", "image": "i"}}}}
	if p.status != nil {
		body["status"] = p.status
	}
	raw, _ := json.Marshal(body)
	ref := trace.Ref{System: "kube", Group: gadgetGVR.Group, Resource: gadgetGVR.Resource, Namespace: ns, Name: "p"}
	return trace.Version{Ref: ref, UID: "u", RV: p.rv, Body: raw}
}
