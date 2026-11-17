package model

import (
	"encoding/json"
	"fmt"
	"hash/fnv"
	"strings"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
)

// deploymentStatus is the status Kubernetes' Deployment controller writes.
// A rollout that is not done keeps the replicas the last status counted
// available, as an old ReplicaSet's Pods, and adds one unavailable Pod of
// the new template.
func deploymentStatus(h *held, w want, now time.Time) map[string]any {
	var spec appsv1.DeploymentSpec
	decodeInto(h.spec, &spec)
	replicas := int64(1)
	if spec.Replicas != nil {
		replicas = int64(*spec.Replicas)
	}
	rs := fmt.Sprintf("ReplicaSet %q", h.ref.Name+"-"+templateHash(h.spec["template"]))
	condition := func(kind, status, reason, message string) map[string]any {
		return map[string]any{"type": kind, "status": status, "reason": reason, "message": message,
			"lastUpdateTime": now.UTC().Format(time.RFC3339), "lastTransitionTime": now.UTC().Format(time.RFC3339)}
	}
	if w.state == wantReady {
		return map[string]any{
			"observedGeneration": h.generation, "replicas": replicas, "updatedReplicas": replicas,
			"readyReplicas": replicas, "availableReplicas": replicas, "unavailableReplicas": nil,
			"conditions": []any{
				condition("Available", "True", "MinimumReplicasAvailable", "Deployment has minimum availability."),
				condition("Progressing", "True", "NewReplicaSetAvailable", rs+" has successfully progressed."),
			},
		}
	}
	available := min(numberAt(h.status, "availableReplicas"), replicas)
	if spec.Strategy.Type == appsv1.RecreateDeploymentStrategyType {
		available = 0
	}
	updated := min(replicas, 1)
	progressingCondition := condition("Progressing", "True", "ReplicaSetUpdated", rs+" is progressing.")
	var failure map[string]any
	switch w.failure {
	case "progressDeadlineExceeded":
		progressingCondition = condition("Progressing", "False", "ProgressDeadlineExceeded", rs+" has timed out progressing.")
	case "replicaFailure":
		updated = 0
		failure = condition("ReplicaFailure", "True", "FailedCreate", fmt.Sprintf(`pods "%s-" is forbidden: exceeded quota: compute-resources`, h.ref.Name))
	}
	availableCondition := condition("Available", "True", "MinimumReplicasAvailable", "Deployment has minimum availability.")
	if available < replicas-maxUnavailable(spec, replicas) || replicas > 0 && available == 0 {
		availableCondition = condition("Available", "False", "MinimumReplicasUnavailable", "Deployment does not have minimum availability.")
	}
	conditions := []any{availableCondition, progressingCondition}
	if failure != nil {
		conditions = append(conditions, failure)
	}
	return map[string]any{
		"observedGeneration": h.generation, "replicas": available + updated, "updatedReplicas": updated,
		"readyReplicas": available, "availableReplicas": available, "unavailableReplicas": updated,
		"conditions": conditions,
	}
}

// maxUnavailable is how many of replicas a rolling update may take down:
// rollingUpdate.maxUnavailable, by default 25% rounded down.
func maxUnavailable(spec appsv1.DeploymentSpec, replicas int64) int64 {
	value := intstr.FromString("25%")
	if u := spec.Strategy.RollingUpdate; u != nil && u.MaxUnavailable != nil {
		value = *u.MaxUnavailable
	}
	n, err := intstr.GetScaledValueFromIntOrPercent(&value, int(replicas), false)
	if err != nil {
		return 0
	}
	return int64(n)
}

// podStatus is the status the scheduler and kubelet write for a Pod that
// keeps running. Each stuck failure strikes the first container it can: an
// init container that is not a sidecar for initContainerFailed, a sidecar
// for sidecarFailed, and a container for the rest. A Pod without such a
// container gets the default, which keeps every container creating.
func podStatus(h *held, w want, now time.Time) map[string]any {
	var spec corev1.PodSpec
	decodeInto(h.spec, &spec)
	failure := w.failure
	if w.state == wantProgressing || w.state == wantStuck && failure == "initContainerFailed" && firstInit(spec, false) < 0 ||
		w.state == wantStuck && failure == "sidecarFailed" && firstInit(spec, true) < 0 {
		failure = "containerCreating"
	} else if w.state == wantStuck && failure == "" {
		failure = "containerCreating"
	}
	p := pod{now: now.UTC().Format(time.RFC3339), name: h.ref.Name, generation: h.generation}
	switch failure {
	case "unschedulable":
		return p.status("Pending", nil, nil, map[string]podCondition{
			"PodScheduled": {"False", "Unschedulable", "0/1 nodes are available: 1 Insufficient cpu. preemption: 0/1 nodes are available: 1 No preemption victims found for incoming pod."},
		}, false)
	case "containerCreating":
		var inits, containers []any
		for _, c := range spec.InitContainers {
			inits = append(inits, p.waiting(c, "PodInitializing", ""))
		}
		reason := "ContainerCreating"
		if len(spec.InitContainers) > 0 {
			reason = "PodInitializing"
		}
		for _, c := range spec.Containers {
			containers = append(containers, p.waiting(c, reason, ""))
		}
		initialized := podCondition{"True", "", ""}
		if len(spec.InitContainers) > 0 {
			initialized = podCondition{"False", "ContainersNotInitialized", "containers with incomplete status: " + names(spec.InitContainers)}
		}
		return p.status("Pending", inits, containers, p.notReady(spec.Containers, map[string]podCondition{
			"PodReadyToStartContainers": {"False", "", ""}, "Initialized": initialized,
		}), true)
	case "initContainerFailed":
		failed := firstInit(spec, false)
		var inits, containers []any
		for i, c := range spec.InitContainers {
			switch {
			case i == failed:
				inits = append(inits, p.crashLooping(c))
			case i < failed:
				inits = append(inits, p.started(c, true))
			default:
				inits = append(inits, p.waiting(c, "PodInitializing", ""))
			}
		}
		for _, c := range spec.Containers {
			containers = append(containers, p.waiting(c, "PodInitializing", ""))
		}
		return p.status("Pending", inits, containers, p.notReady(spec.Containers, map[string]podCondition{
			"Initialized": {"False", "ContainersNotInitialized", "containers with incomplete status: [" + spec.InitContainers[failed].Name + "]"},
		}), true)
	}
	var inits, containers, unready []any
	failedSidecar, failedContainer := -1, -1
	switch failure {
	case "sidecarFailed":
		failedSidecar = firstInit(spec, true)
	case "imagePullBackOff", "crashLoopBackOff":
		failedContainer = 0
	}
	for i, c := range spec.InitContainers {
		if i == failedSidecar {
			inits, unready = append(inits, p.crashLooping(c)), append(unready, c.Name)
		} else {
			inits = append(inits, p.started(c, true))
		}
	}
	phase := "Running"
	for i, c := range spec.Containers {
		switch {
		case i == failedContainer && failure == "imagePullBackOff":
			phase = "Pending"
			containers = append(containers, p.waiting(c, "ImagePullBackOff", fmt.Sprintf("Back-off pulling image %q", c.Image)))
		case i == failedContainer:
			containers = append(containers, p.crashLooping(c))
		default:
			containers = append(containers, p.started(c, false))
			continue
		}
		unready = append(unready, c.Name)
	}
	conditions := map[string]podCondition{}
	if len(unready) > 0 {
		message := "containers with unready status: " + fmt.Sprint(unready)
		conditions["Ready"] = podCondition{"False", "ContainersNotReady", message}
		conditions["ContainersReady"] = podCondition{"False", "ContainersNotReady", message}
	}
	return p.status(phase, inits, containers, conditions, true)
}

// podCondition is a Pod condition's status, reason and message.
type podCondition struct{ status, reason, message string }

// pod builds the parts of one Pod's status at now.
type pod struct {
	now, name  string
	generation int64
}

// podConditions are the conditions a scheduled Pod carries, in kubelet's
// order.
var podConditions = []string{"PodReadyToStartContainers", "Initialized", "Ready", "ContainersReady", "PodScheduled"}

// status lists every Pod condition, True unless conditions says otherwise,
// or only PodScheduled when the Pod is not scheduled. It sets each field it
// leaves out to null, so a write replaces the last write's fields.
func (p pod) status(phase string, inits, containers []any, conditions map[string]podCondition, scheduled bool) map[string]any {
	status := map[string]any{"observedGeneration": p.generation, "phase": phase, "initContainerStatuses": nilIfEmpty(inits), "containerStatuses": nilIfEmpty(containers),
		"hostIP": nil, "hostIPs": nil, "podIP": nil, "podIPs": nil, "startTime": nil}
	var list []any
	for _, kind := range podConditions {
		c, set := conditions[kind]
		if !set {
			if !scheduled {
				continue
			}
			c = podCondition{"True", "", ""}
		}
		entry := map[string]any{"type": kind, "status": c.status, "lastProbeTime": nil, "lastTransitionTime": p.now}
		if c.reason != "" {
			entry["reason"], entry["message"] = c.reason, c.message
		}
		list = append(list, entry)
	}
	status["conditions"] = list
	if scheduled {
		ip := podIP(p.name)
		status["hostIP"], status["hostIPs"] = "10.0.0.1", []any{map[string]any{"ip": "10.0.0.1"}}
		status["podIP"], status["podIPs"] = ip, []any{map[string]any{"ip": ip}}
		status["startTime"] = p.now
	}
	return status
}

// notReady adds the Ready and ContainersReady conditions of containers that
// are not ready to conditions.
func (p pod) notReady(containers []corev1.Container, conditions map[string]podCondition) map[string]podCondition {
	c := podCondition{"False", "ContainersNotReady", "containers with unready status: " + names(containers)}
	conditions["Ready"], conditions["ContainersReady"] = c, c
	return conditions
}

func (p pod) waiting(c corev1.Container, reason, message string) map[string]any {
	waiting := map[string]any{"reason": reason}
	if message != "" {
		waiting["message"] = message
	}
	return p.containerStatus(c, false, false, 0, map[string]any{"waiting": waiting}, nil)
}

// started is the status of a container or sidecar that runs and is ready,
// or of another init container that has completed.
func (p pod) started(c corev1.Container, init bool) map[string]any {
	if !init || isSidecar(c) {
		return p.containerStatus(c, true, true, 0, map[string]any{"running": map[string]any{"startedAt": p.now}}, nil)
	}
	return p.containerStatus(c, true, false, 0, map[string]any{"terminated": map[string]any{
		"exitCode": 0, "reason": "Completed", "startedAt": p.now, "finishedAt": p.now}}, nil)
}

func (p pod) crashLooping(c corev1.Container) map[string]any {
	return p.containerStatus(c, false, false, 3,
		map[string]any{"waiting": map[string]any{"reason": "CrashLoopBackOff", "message": fmt.Sprintf("back-off 40s restarting failed container=%s pod=%s", c.Name, p.name)}},
		map[string]any{"terminated": map[string]any{"exitCode": 1, "reason": "Error", "startedAt": p.now, "finishedAt": p.now}})
}

func (p pod) containerStatus(c corev1.Container, ready, started bool, restarts int, state, lastState map[string]any) map[string]any {
	status := map[string]any{"name": c.Name, "image": c.Image, "imageID": "", "ready": ready, "started": started, "restartCount": restarts, "state": state}
	if lastState != nil {
		status["lastState"] = lastState
	}
	return status
}

func isSidecar(c corev1.Container) bool {
	return c.RestartPolicy != nil && *c.RestartPolicy == corev1.ContainerRestartPolicyAlways
}

// firstInit is the index of the first init container that is a sidecar,
// or that is not, or -1.
func firstInit(spec corev1.PodSpec, sidecar bool) int {
	for i, c := range spec.InitContainers {
		if isSidecar(c) == sidecar {
			return i
		}
	}
	return -1
}

func names(containers []corev1.Container) string {
	var list []string
	for _, c := range containers {
		list = append(list, c.Name)
	}
	return "[" + strings.Join(list, " ") + "]"
}

func nilIfEmpty(list []any) any {
	if len(list) == 0 {
		return nil
	}
	return list
}

// podIP is a stable address in 10.244.0.0/16 for the Pod named name.
func podIP(name string) string {
	h := fnv.New32a()
	h.Write([]byte(name))
	sum := h.Sum32()
	return fmt.Sprintf("10.244.%d.%d", sum>>8&0xff, sum&0xfe+1)
}

// templateHash names a ReplicaSet of template, as the Deployment
// controller's pod-template-hash does.
func templateHash(template any) string {
	raw, _ := json.Marshal(template)
	h := fnv.New32a()
	h.Write(raw)
	return fmt.Sprintf("%x", h.Sum32())
}

func decodeInto(fields map[string]any, out any) {
	raw, _ := json.Marshal(fields)
	json.Unmarshal(raw, out)
}

func numberAt(fields map[string]any, key string) int64 {
	n, _ := fields[key].(float64)
	return int64(n)
}
