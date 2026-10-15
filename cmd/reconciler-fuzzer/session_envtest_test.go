//go:build envtest

package main

import (
	"bytes"
	"context"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/watch"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	clientcmdapi "k8s.io/client-go/tools/clientcmd/api"

	"github.com/rosenhouse-bot-sandbox/reconciler-fuzzer/internal/cluster"
	"github.com/rosenhouse-bot-sandbox/reconciler-fuzzer/internal/target"
)

func TestTheSessionRefusesAKindTheClusterServesAtClusterScope(t *testing.T) {
	toy, err := target.Load(toyTargetYAML)
	if err != nil {
		t.Fatal(err)
	}
	toy.Launch.Binary = "sh"
	s, err := openSession(options{}, toy)
	if err != nil {
		t.Fatalf("Opening the session failed: %v", err)
	}
	defer func() { _ = s.close() }()
	clusterRoles := *toy
	clusterRoles.Manages = append(slices.Clone(toy.Manages),
		schema.GroupVersionKind{Group: "rbac.authorization.k8s.io", Version: "v1", Kind: "ClusterRole"})

	if err := s.vet(toy); err != nil {
		t.Errorf("The session refused the toy: %v", err)
	}
	if err := s.vet(&clusterRoles); err == nil || !strings.Contains(err.Error(), "ClusterRole") {
		t.Errorf("The session answered %v for a target that manages ClusterRoles.", err)
	}
}

func TestClosingAnEnvtestSessionStopsItsControlPlane(t *testing.T) {
	s, err := openSession(options{}, &target.Target{Launch: target.LaunchSpec{Binary: "sh"}})
	if err != nil {
		t.Fatalf("Opening a session failed: %v", err)
	}
	client, err := kubernetes.NewForConfig(s.(*clusterSession).Config())
	if err != nil {
		t.Fatal(err)
	}

	if err := s.close(); err != nil {
		t.Fatalf("Closing the session failed: %v", err)
	}

	if _, err := client.Discovery().ServerVersion(); err == nil {
		t.Error("The API server still answers after the session closed.")
	}
}

// A bare control plane stands in for kind: it has none of the target's CRDs,
// and a stand-in adds what kube-controller-manager adds to every namespace.
func TestKubeconfigModeJudgesOnlyWhatTheTargetDid(t *testing.T) {
	toyTarget, err := filepath.Abs(toyTargetYAML)
	if err != nil {
		t.Fatal(err)
	}
	sequences := filepath.Join(filepath.Dir(toyTarget), "sequences")
	// launch.binary is relative to the working directory.
	t.Chdir(t.TempDir())
	buildToyInto(t, filepath.Dir(toyTarget), filepath.Join("bin", "toy-widget"))

	bare, err := cluster.Start(cluster.Options{})
	if err != nil {
		t.Fatalf("Starting a bare control plane failed: %v", err)
	}
	t.Cleanup(func() {
		if err := bare.Stop(); err != nil {
			t.Errorf("Stopping the control plane failed: %v", err)
		}
	})
	kubeconfig := writeKubeconfig(t, bare.Config())
	standInForTheControllerManager(t, bare.Config())

	for _, test := range []struct {
		name     string
		args     []string
		sequence string
		code     int
		want     string
	}{
		{name: "the toy with no bug passes", sequence: "b0.json", code: exitOK, want: "every run passed."},
		{
			name: "a deleteManaged op takes the toy's own ConfigMap", args: []string{"--launch-arg", "--bug=8"},
			sequence: "b8.json", code: exitViolation, want: "G7 the v1/ConfigMap widget-0 that op 1 (deleteManaged) deleted never came back",
		},
		{
			name: "G3 names the toy's orphan", args: []string{"--launch-arg", "--bug=3"},
			sequence: "b3.json", code: exitViolation, want: "G3 the v1/ConfigMap widget-0 was still there",
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			args := append([]string{"run", "--target", toyTarget, "--kubeconfig", kubeconfig, "--out", t.TempDir()}, test.args...)
			args = append(args, filepath.Join(sequences, test.sequence))
			var stdout, stderr bytes.Buffer
			c := newCLI(&stdout, &stderr)

			code := c.main(t.Context(), args)

			if code != test.code || !strings.Contains(stdout.String(), test.want) {
				t.Errorf("reconciler-fuzzer exited %d, want %d, and printed\n%s%s\nwant %q.", code, test.code, stdout.String(), stderr.String(), test.want)
			}
			if strings.Contains(stdout.String(), "kube-root-ca.crt") {
				t.Errorf("reconciler-fuzzer blamed the target for the cluster's ConfigMap:\n%s", stdout.String())
			}
		})
	}
}

func buildToyInto(t *testing.T, toyDir, binary string) {
	t.Helper()
	if _, err := exec.LookPath("go"); err != nil {
		t.Skipf("The toy target cannot be built without the go tool: %v", err)
	}
	binary, err := filepath.Abs(binary)
	if err != nil {
		t.Fatal(err)
	}
	build := exec.Command("go", "build", "-o", binary, ".")
	build.Dir = toyDir
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("Building the toy target failed: %v\n%s", err, out)
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

// standInForTheControllerManager adds the default ServiceAccount and
// kube-root-ca.crt to each run namespace a second after it appears, after the
// run's Observer has synced.
func standInForTheControllerManager(t *testing.T, config *rest.Config) {
	t.Helper()
	client, err := kubernetes.NewForConfig(config)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	namespaces, err := client.CoreV1().Namespaces().Watch(ctx, metav1.ListOptions{})
	if err != nil {
		t.Fatal(err)
	}
	var added sync.WaitGroup
	t.Cleanup(func() {
		cancel()
		added.Wait()
	})
	populate := func(namespace string) {
		defer added.Done()
		select {
		case <-ctx.Done():
			return
		case <-time.After(time.Second):
		}
		account := &corev1.ServiceAccount{ObjectMeta: metav1.ObjectMeta{Name: "default"}}
		if _, err := client.CoreV1().ServiceAccounts(namespace).Create(ctx, account, metav1.CreateOptions{}); err != nil && ctx.Err() == nil {
			t.Errorf("The stand-in could not create the default ServiceAccount: %v", err)
		}
		rootCA := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: "kube-root-ca.crt"}, Data: map[string]string{"ca.crt": "stand-in"}}
		if _, err := client.CoreV1().ConfigMaps(namespace).Create(ctx, rootCA, metav1.CreateOptions{}); err != nil && ctx.Err() == nil {
			t.Errorf("The stand-in could not create kube-root-ca.crt: %v", err)
		}
	}
	added.Add(1)
	go func() {
		defer added.Done()
		for event := range namespaces.ResultChan() {
			namespace, ok := event.Object.(*corev1.Namespace)
			if ok && event.Type == watch.Added && strings.HasPrefix(namespace.Name, "reconciler-fuzzer-run-") {
				added.Add(1)
				go populate(namespace.Name)
			}
		}
	}()
}
