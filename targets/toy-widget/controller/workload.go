package controller

import (
	"context"
	"fmt"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	toyv1 "github.com/rosenhouse-bot-sandbox/reconciler-fuzzer/targets/toy-widget/api/v1"
)

// Workloads that --workload gives each Widget.
const (
	DeploymentWorkload = "deployment"
	PodWorkload        = "pod"
)

// syncWorkload creates or updates the Widget's Deployment or Pod and returns
// workloadReady, which is nil without --workload. The Garbage collector
// deletes the workload with its Widget.
func (r *Reconciler) syncWorkload(ctx context.Context, widget *toyv1.Widget) (*bool, error) {
	switch r.Workload {
	case DeploymentWorkload:
		return r.syncDeployment(ctx, widget)
	case PodWorkload:
		return r.syncPod(ctx, widget)
	}
	return nil, nil
}

// syncDeployment runs spec.count replicas.
func (r *Reconciler) syncDeployment(ctx context.Context, widget *toyv1.Widget) (*bool, error) {
	labels := map[string]string{"widget": widget.Name}
	deployment := &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Namespace: widget.Namespace, Name: widget.Name}}
	_, err := controllerutil.CreateOrUpdate(ctx, r.Client, deployment, func() error {
		deployment.Spec.Replicas = ptr.To(widget.Spec.Count)
		deployment.Spec.Selector = &metav1.LabelSelector{MatchLabels: labels}
		deployment.Spec.Template.Labels = labels
		if len(deployment.Spec.Template.Spec.Containers) == 0 {
			deployment.Spec.Template.Spec.Containers = []corev1.Container{{Name: "app", Image: "example.com/widget"}}
		}
		deployment.Spec.Template.Spec.Containers[0].Args = []string{fmt.Sprintf("--count=%d", widget.Spec.Count)}
		return controllerutil.SetControllerReference(widget, deployment, r.Scheme)
	})
	if err != nil {
		return nil, fmt.Errorf("ensuring Deployment %s: %w", deployment.Name, err)
	}
	if !deployment.DeletionTimestamp.IsZero() {
		return ptr.To(false), nil
	}
	if r.Bug == B36 {
		// B36: an Available Deployment counts as rolled out, though its old
		// Pods may still serve.
		return ptr.To(isTrue(deploymentCondition(deployment, appsv1.DeploymentAvailable))), nil
	}
	return ptr.To(rolledOut(deployment)), nil
}

// rolledOut says whether every replica runs the current template and is
// available, as kubectl rollout status does.
func rolledOut(d *appsv1.Deployment) bool {
	replicas := ptr.Deref(d.Spec.Replicas, 1)
	progressing := deploymentCondition(d, appsv1.DeploymentProgressing)
	return d.Status.ObservedGeneration >= d.Generation && d.Status.UpdatedReplicas == replicas &&
		d.Status.Replicas == replicas && d.Status.AvailableReplicas == replicas &&
		(progressing == nil || progressing.Reason != "ProgressDeadlineExceeded")
}

func deploymentCondition(d *appsv1.Deployment, kind appsv1.DeploymentConditionType) *appsv1.DeploymentCondition {
	for i := range d.Status.Conditions {
		if d.Status.Conditions[i].Type == kind {
			return &d.Status.Conditions[i]
		}
	}
	return nil
}

func isTrue(c *appsv1.DeploymentCondition) bool { return c != nil && c.Status == corev1.ConditionTrue }

// syncPod creates the Widget's Pod, which runs a proxy sidecar and a
// migration before its app.
func (r *Reconciler) syncPod(ctx context.Context, widget *toyv1.Widget) (*bool, error) {
	pod := &corev1.Pod{}
	err := r.Get(ctx, client.ObjectKeyFromObject(widget), pod)
	if apierrors.IsNotFound(err) {
		pod = &corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{Namespace: widget.Namespace, Name: widget.Name},
			Spec: corev1.PodSpec{
				InitContainers: []corev1.Container{
					{Name: "proxy", Image: "example.com/proxy", RestartPolicy: ptr.To(corev1.ContainerRestartPolicyAlways)},
					{Name: "migrate", Image: "example.com/migrate"},
				},
				Containers: []corev1.Container{{Name: "app", Image: "example.com/widget"}},
			},
		}
		if err := controllerutil.SetControllerReference(widget, pod, r.Scheme); err != nil {
			return nil, err
		}
		err = r.Create(ctx, pod)
	}
	if err != nil {
		return nil, fmt.Errorf("ensuring Pod %s: %w", widget.Name, err)
	}
	if !metav1.IsControlledBy(pod, widget) {
		return nil, fmt.Errorf("Pod %s belongs to another owner; waiting for it to go", widget.Name)
	}
	if !pod.DeletionTimestamp.IsZero() {
		return ptr.To(false), nil
	}
	if r.Bug == B37 {
		return ptr.To(pod.Status.Phase == corev1.PodRunning), nil // B37: a Running Pod counts as ready.
	}
	ready := false
	for _, c := range pod.Status.Conditions {
		ready = ready || c.Type == corev1.PodReady && c.Status == corev1.ConditionTrue
	}
	return &ready, nil
}

// workloadObject is the type --workload watches, or nil.
func workloadObject(workload string) client.Object {
	switch workload {
	case DeploymentWorkload:
		return &appsv1.Deployment{}
	case PodWorkload:
		return &corev1.Pod{}
	}
	return nil
}
