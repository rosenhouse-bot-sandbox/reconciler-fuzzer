package controller

import (
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	toyv1 "github.com/rosenhouse-bot-sandbox/reconciler-fuzzer/targets/toy-widget/api/v1"
)

func workloadFixture(t *testing.T, bug Bug, workload string, widget *toyv1.Widget) *Reconciler {
	t.Helper()
	r := fixture(t, bug, interceptor.Funcs{}, widget)
	r.Workload = workload
	return r
}

func workloadReadyOf(t *testing.T, r *Reconciler, widget *toyv1.Widget) *bool {
	t.Helper()
	mustReconcile(t, r, widget)
	return readWidget(t, r.Client, widget).Status.WorkloadReady
}

func readDeployment(t *testing.T, r *Reconciler, widget *toyv1.Widget) *appsv1.Deployment {
	t.Helper()
	d := &appsv1.Deployment{}
	if err := r.Get(t.Context(), client.ObjectKeyFromObject(widget), d); err != nil {
		t.Fatal(err)
	}
	return d
}

func setDeploymentStatus(t *testing.T, r *Reconciler, widget *toyv1.Widget, status appsv1.DeploymentStatus) {
	t.Helper()
	d := readDeployment(t, r, widget)
	d.Status = status
	if err := r.Status().Update(t.Context(), d); err != nil {
		t.Fatal(err)
	}
}

func TestADeploymentRunsTheWidgetsCount(t *testing.T) {
	widget := newWidget(3)
	r := workloadFixture(t, 0, DeploymentWorkload, widget)
	if ready := workloadReadyOf(t, r, widget); ready == nil || *ready {
		t.Fatalf("workloadReady = %v, want false", ready)
	}
	d := readDeployment(t, r, widget)
	if ptr.Deref(d.Spec.Replicas, 0) != 3 || len(d.OwnerReferences) != 1 || d.OwnerReferences[0].UID != widget.UID {
		t.Errorf("deployment = %+v", d)
	}
}

func TestADeploymentIsReadyOnlyOnceRolledOut(t *testing.T) {
	available := []appsv1.DeploymentCondition{{Type: appsv1.DeploymentAvailable, Status: corev1.ConditionTrue}}
	for name, c := range map[string]struct {
		bug    Bug
		status appsv1.DeploymentStatus
		want   bool
	}{
		"rolled out":         {0, appsv1.DeploymentStatus{ObservedGeneration: 1, Replicas: 3, UpdatedReplicas: 3, AvailableReplicas: 3, Conditions: available}, true},
		"old pods available": {0, appsv1.DeploymentStatus{ObservedGeneration: 1, Replicas: 4, UpdatedReplicas: 1, AvailableReplicas: 3, Conditions: available}, false},
		"B36 old pods":       {B36, appsv1.DeploymentStatus{ObservedGeneration: 1, Replicas: 4, UpdatedReplicas: 1, AvailableReplicas: 3, Conditions: available}, true},
		"deadline exceeded": {0, appsv1.DeploymentStatus{ObservedGeneration: 1, Replicas: 3, UpdatedReplicas: 3, AvailableReplicas: 3,
			Conditions: []appsv1.DeploymentCondition{{Type: appsv1.DeploymentProgressing, Status: corev1.ConditionFalse, Reason: "ProgressDeadlineExceeded"}}}, false},
	} {
		widget := newWidget(3)
		r := workloadFixture(t, c.bug, DeploymentWorkload, widget)
		mustReconcile(t, r, widget)
		setDeploymentStatus(t, r, widget, c.status)
		if ready := workloadReadyOf(t, r, widget); ready == nil || *ready != c.want {
			t.Errorf("%s: workloadReady = %v, want %v", name, ready, c.want)
		}
	}
}

func TestAPodIsReadyOnlyWithItsReadyCondition(t *testing.T) {
	notReady := corev1.PodStatus{Phase: corev1.PodRunning, Conditions: []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionFalse}}}
	ready := corev1.PodStatus{Phase: corev1.PodRunning, Conditions: []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionTrue}}}
	for name, c := range map[string]struct {
		bug    Bug
		status corev1.PodStatus
		want   bool
	}{
		"ready":                  {0, ready, true},
		"running, not ready":     {0, notReady, false},
		"B37 running, not ready": {B37, notReady, true},
	} {
		widget := newWidget(1)
		r := workloadFixture(t, c.bug, PodWorkload, widget)
		mustReconcile(t, r, widget)
		pod := &corev1.Pod{}
		if err := r.Get(t.Context(), client.ObjectKeyFromObject(widget), pod); err != nil {
			t.Fatal(err)
		}
		if len(pod.Spec.InitContainers) != 2 || !isSidecar(pod.Spec.InitContainers[0]) || len(pod.OwnerReferences) != 1 {
			t.Fatalf("pod = %+v", pod)
		}
		pod.Status = c.status
		if err := r.Status().Update(t.Context(), pod); err != nil {
			t.Fatal(err)
		}
		if got := workloadReadyOf(t, r, widget); got == nil || *got != c.want {
			t.Errorf("%s: workloadReady = %v, want %v", name, got, c.want)
		}
	}
}

func isSidecar(c corev1.Container) bool {
	return ptr.Deref(c.RestartPolicy, "") == corev1.ContainerRestartPolicyAlways
}

func TestWithoutAWorkloadNoWorkloadReady(t *testing.T) {
	widget := newWidget(1)
	r := workloadFixture(t, 0, "", widget)
	if ready := workloadReadyOf(t, r, widget); ready != nil {
		t.Errorf("workloadReady = %v, want nil", *ready)
	}
}
