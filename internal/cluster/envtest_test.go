//go:build envtest

package cluster_test

import (
	"os"
	"path/filepath"
	"testing"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/discovery"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	clientcmdapi "k8s.io/client-go/tools/clientcmd/api"

	"github.com/rosenhouse-bot-sandbox/reconciler-fuzzer/internal/cluster"
)

var (
	thingKind     = schema.GroupVersionKind{Group: "test.reconciler-fuzzer", Version: "v1", Kind: "Thing"}
	thingResource = schema.GroupVersionResource{Group: "test.reconciler-fuzzer", Version: "v1", Resource: "things"}
)

const thingCRD = `apiVersion: apiextensions.k8s.io/v1
kind: CustomResourceDefinition
metadata:
  name: things.test.reconciler-fuzzer
spec:
  group: test.reconciler-fuzzer
  names:
    kind: Thing
    plural: things
  scope: Namespaced
  versions:
    - name: v1
      served: true
      storage: true
      schema:
        openAPIV3Schema:
          type: object
`

func TestStartIgnoresUseExistingCluster(t *testing.T) {
	t.Setenv("USE_EXISTING_CLUSTER", "true")
	t.Setenv("KUBECONFIG", filepath.Join(t.TempDir(), "no-such-kubeconfig"))

	c, err := cluster.Start(cluster.Options{})
	if err != nil {
		t.Fatalf("Start returned an error: %v", err)
	}
	if err := c.Stop(); err != nil {
		t.Errorf("Stop returned an error: %v", err)
	}
}

func thingCRDDir(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "thing.yaml"), []byte(thingCRD), 0o600); err != nil {
		t.Fatal(err)
	}
	return dir
}

func TestStartServesAPIAndInstallsCRDs(t *testing.T) {
	c, err := cluster.Start(cluster.Options{CRDPaths: []string{thingCRDDir(t)}})
	if err != nil {
		t.Fatalf("Start returned an error: %v", err)
	}
	t.Cleanup(func() {
		if err := c.Stop(); err != nil {
			t.Errorf("Stop returned an error: %v", err)
		}
	})

	dc, err := discovery.NewDiscoveryClientForConfig(c.Config())
	if err != nil {
		t.Fatalf("Building a discovery client failed: %v", err)
	}
	version, err := dc.ServerVersion()
	if err != nil {
		t.Fatalf("Fetching the server version failed: %v", err)
	}
	if version.GitVersion == "" {
		t.Error("The server reported an empty GitVersion.")
	}

	// A mapper built after Start knows the kinds the CRDs installed.
	requireThingServed(t, c.Config())
}

// Nothing on envtest would remove a finalizer or create a ServiceAccount.
func TestStartAdmitsWhatOnlyAControllerManagerWouldFinish(t *testing.T) {
	c, err := cluster.Start(cluster.Options{})
	if err != nil {
		t.Fatalf("Start returned an error: %v", err)
	}
	t.Cleanup(func() {
		if err := c.Stop(); err != nil {
			t.Errorf("Stop returned an error: %v", err)
		}
	})
	client, err := kubernetes.NewForConfig(c.Config())
	if err != nil {
		t.Fatalf("Building a client failed: %v", err)
	}
	namespace := newNamespace(t, client)
	ctx := t.Context()

	t.Run("a claim carries no finalizer and deletes at once", func(t *testing.T) {
		claim := &corev1.PersistentVolumeClaim{
			ObjectMeta: metav1.ObjectMeta{Name: "data"},
			Spec: corev1.PersistentVolumeClaimSpec{
				AccessModes: []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce},
				Resources: corev1.VolumeResourceRequirements{
					Requests: corev1.ResourceList{corev1.ResourceStorage: resource.MustParse("1Gi")},
				},
			},
		}
		claims := client.CoreV1().PersistentVolumeClaims(namespace)
		created, err := claims.Create(ctx, claim, metav1.CreateOptions{})
		if err != nil {
			t.Fatalf("Creating the claim failed: %v", err)
		}
		if len(created.Finalizers) > 0 {
			t.Errorf("The claim carries the finalizers %v, which nothing on envtest removes.", created.Finalizers)
		}
		if err := claims.Delete(ctx, created.Name, metav1.DeleteOptions{}); err != nil {
			t.Fatalf("Deleting the claim failed: %v", err)
		}
		if _, err := claims.Get(ctx, created.Name, metav1.GetOptions{}); !apierrors.IsNotFound(err) {
			t.Errorf("Reading the deleted claim returned %v, want NotFound.", err)
		}
	})

	t.Run("a Job, whose default is to orphan, deletes at once", func(t *testing.T) {
		job := &batchv1.Job{
			ObjectMeta: metav1.ObjectMeta{Name: "init"},
			Spec: batchv1.JobSpec{Template: corev1.PodTemplateSpec{Spec: corev1.PodSpec{
				RestartPolicy: corev1.RestartPolicyNever,
				Containers:    []corev1.Container{{Name: "main", Image: "example.invalid/main"}},
			}}},
		}
		jobs := client.BatchV1().Jobs(namespace)
		created, err := jobs.Create(ctx, job, metav1.CreateOptions{})
		if err != nil {
			t.Fatalf("Creating the Job failed: %v", err)
		}
		if err := jobs.Delete(ctx, created.Name, metav1.DeleteOptions{}); err != nil {
			t.Fatalf("Deleting the Job failed: %v", err)
		}
		if _, err := jobs.Get(ctx, created.Name, metav1.GetOptions{}); !apierrors.IsNotFound(err) {
			t.Errorf("Reading the deleted Job returned %v, want NotFound.", err)
		}
	})

	t.Run("a foreground delete removes the object at once", func(t *testing.T) {
		configMaps := client.CoreV1().ConfigMaps(namespace)
		created, err := configMaps.Create(ctx, &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: "config"}}, metav1.CreateOptions{})
		if err != nil {
			t.Fatalf("Creating the ConfigMap failed: %v", err)
		}
		foreground := metav1.DeletePropagationForeground
		if err := configMaps.Delete(ctx, created.Name, metav1.DeleteOptions{PropagationPolicy: &foreground}); err != nil {
			t.Fatalf("Deleting the ConfigMap failed: %v", err)
		}
		if _, err := configMaps.Get(ctx, created.Name, metav1.GetOptions{}); !apierrors.IsNotFound(err) {
			t.Errorf("Reading the deleted ConfigMap returned %v, want NotFound.", err)
		}
	})

	t.Run("a pod needs no ServiceAccount", func(t *testing.T) {
		pod := &corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{Name: "workload"},
			Spec:       corev1.PodSpec{Containers: []corev1.Container{{Name: "main", Image: "example.invalid/main"}}},
		}
		if _, err := client.CoreV1().Pods(namespace).Create(ctx, pod, metav1.CreateOptions{}); err != nil {
			t.Errorf("Creating a pod in a namespace with no ServiceAccount failed: %v", err)
		}
	})
}

func TestConnectInstallsCRDsAndLeavesThem(t *testing.T) {
	bare, err := cluster.Start(cluster.Options{})
	if err != nil {
		t.Fatalf("Start returned an error: %v", err)
	}
	t.Cleanup(func() {
		if err := bare.Stop(); err != nil {
			t.Errorf("Stop returned an error: %v", err)
		}
	})
	kubeconfig := writeKubeconfig(t, bare.Config())
	crds := cluster.Options{CRDPaths: []string{thingCRDDir(t)}}

	// The second Connect finds the CRD installed and replaces it.
	for range 2 {
		c, err := cluster.Connect(kubeconfig, crds)
		if err != nil {
			t.Fatalf("Connect returned an error: %v", err)
		}
		if err := c.Stop(); err != nil {
			t.Fatalf("Stop returned an error: %v", err)
		}
	}

	requireThingServed(t, bare.Config())
}

const convertedCRD = `apiVersion: apiextensions.k8s.io/v1
kind: CustomResourceDefinition
metadata:
  name: converted.test.reconciler-fuzzer
spec:
  group: test.reconciler-fuzzer
  conversion:
    strategy: Webhook
    webhook:
      conversionReviewVersions: ["v1"]
      clientConfig:
        service: {name: converter, namespace: operator-system, path: /convert}
  names:
    kind: Converted
    plural: converted
  scope: Namespaced
  versions:
    - name: v1alpha1
      served: true
      storage: false
      schema:
        openAPIV3Schema:
          type: object
    - name: v1
      served: true
      storage: true
      schema:
        openAPIV3Schema:
          type: object
`

// envtest runs no webhook, so Start drops a CRD's conversion webhook. A
// kubeconfig cluster may run one, so Connect keeps it.
func TestOnlyStartDropsACRDsConversionWebhook(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "converted.yaml"), []byte(convertedCRD), 0o600); err != nil {
		t.Fatal(err)
	}
	crds := cluster.Options{CRDPaths: []string{dir}}
	bare, err := cluster.Start(crds)
	if err != nil {
		t.Fatalf("Start returned an error: %v", err)
	}
	t.Cleanup(func() {
		if err := bare.Stop(); err != nil {
			t.Errorf("Stop returned an error: %v", err)
		}
	})
	if got := conversionStrategy(t, bare.Config()); got != "None" {
		t.Errorf("Start installed the CRD with the conversion strategy %q, want None.", got)
	}

	if _, err := cluster.Connect(writeKubeconfig(t, bare.Config()), crds); err != nil {
		t.Fatalf("Connect returned an error: %v", err)
	}
	if got := conversionStrategy(t, bare.Config()); got != "Webhook" {
		t.Errorf("Connect installed the CRD with the conversion strategy %q, want Webhook.", got)
	}
}

func conversionStrategy(t *testing.T, config *rest.Config) string {
	t.Helper()
	crds := dynamic.NewForConfigOrDie(config).Resource(schema.GroupVersionResource{
		Group: "apiextensions.k8s.io", Version: "v1", Resource: "customresourcedefinitions"})
	crd, err := crds.Get(t.Context(), "converted.test.reconciler-fuzzer", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("Reading the CRD failed: %v", err)
	}
	strategy, _, _ := unstructured.NestedString(crd.Object, "spec", "conversion", "strategy")
	return strategy
}

func requireThingServed(t *testing.T, config *rest.Config) {
	t.Helper()
	mapper, err := cluster.NewRESTMapper(config)
	if err != nil {
		t.Fatalf("Building the RESTMapper failed: %v", err)
	}
	mapping, err := mapper.RESTMapping(thingKind.GroupKind(), thingKind.Version)
	if err != nil {
		t.Fatalf("The mapper does not resolve the installed %s: %v", thingKind.Kind, err)
	}
	if mapping.Resource != thingResource {
		t.Errorf("The mapper resolves %s to %v, want %v.", thingKind.Kind, mapping.Resource, thingResource)
	}
}

func writeKubeconfig(t *testing.T, config *rest.Config) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "kubeconfig")
	kubeconfig := clientcmdapi.Config{
		Clusters: map[string]*clientcmdapi.Cluster{"envtest": {
			Server:                   config.Host,
			CertificateAuthorityData: config.CAData,
		}},
		AuthInfos: map[string]*clientcmdapi.AuthInfo{"admin": {
			ClientCertificateData: config.CertData,
			ClientKeyData:         config.KeyData,
		}},
		Contexts:       map[string]*clientcmdapi.Context{"envtest": {Cluster: "envtest", AuthInfo: "admin"}},
		CurrentContext: "envtest",
	}
	if err := clientcmd.WriteToFile(kubeconfig, path); err != nil {
		t.Fatal(err)
	}
	return path
}
