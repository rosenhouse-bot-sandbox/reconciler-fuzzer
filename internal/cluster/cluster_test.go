package cluster_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"k8s.io/client-go/tools/clientcmd"
	clientcmdapi "k8s.io/client-go/tools/clientcmd/api"

	"github.com/rosenhouse-bot-sandbox/reconciler-fuzzer/internal/cluster"
)

func TestValidateRejectsMissingCRDPath(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "no-such-dir")

	err := cluster.Options{CRDPaths: []string{missing}}.Validate()
	if err == nil {
		t.Fatal("Validate accepted a CRD path that does not exist.")
	}
	if !strings.Contains(err.Error(), missing) {
		t.Errorf("Validate returned %q, which does not name the missing path %q.", err, missing)
	}
}

func TestValidateAcceptsExistingCRDPath(t *testing.T) {
	installControlPlane(t, "etcd", "kube-apiserver")

	if err := (cluster.Options{CRDPaths: []string{t.TempDir()}}).Validate(); err != nil {
		t.Errorf("Validate rejected an existing directory: %v", err)
	}
}

func executables(t *testing.T, names ...string) string {
	t.Helper()
	dir := t.TempDir()
	for _, name := range names {
		if err := os.WriteFile(filepath.Join(dir, name), nil, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

// installControlPlane points envtest at a directory holding executables of
// these names, and at nothing else.
func installControlPlane(t *testing.T, names ...string) string {
	t.Helper()
	dir := executables(t, names...)
	unsetenv(t, "TEST_ASSET_ETCD")
	unsetenv(t, "TEST_ASSET_KUBE_APISERVER")
	t.Setenv("KUBEBUILDER_ASSETS", dir)
	return dir
}

// unsetenv removes a variable for the test, since envtest reads one set to
// nothing as a path.
func unsetenv(t *testing.T, name string) {
	t.Helper()
	t.Setenv(name, "")
	if err := os.Unsetenv(name); err != nil {
		t.Fatal(err)
	}
}

func TestValidateNamesTheControlPlaneBinaryItCannotRun(t *testing.T) {
	for _, test := range []struct {
		name  string
		setup func(t *testing.T) (want []string)
	}{
		{"KUBEBUILDER_ASSETS names an empty directory", func(t *testing.T) []string {
			dir := installControlPlane(t)
			return []string{"KUBEBUILDER_ASSETS", filepath.Join(dir, "etcd"), "setup-envtest"}
		}},
		{"KUBEBUILDER_ASSETS is not set", func(t *testing.T) []string {
			installControlPlane(t)
			unsetenv(t, "KUBEBUILDER_ASSETS")
			if _, err := os.Stat("/usr/local/kubebuilder/bin/etcd"); err == nil {
				t.Skip("etcd is installed where envtest looks by default.")
			}
			return []string{"KUBEBUILDER_ASSETS is not set", "/usr/local/kubebuilder/bin/etcd", "setup-envtest"}
		}},
		{"KUBEBUILDER_ASSETS is empty, and only the working directory holds the control plane", func(t *testing.T) []string {
			installControlPlane(t)
			t.Setenv("KUBEBUILDER_ASSETS", "")
			t.Setenv("PATH", t.TempDir())
			t.Chdir(executables(t, "etcd", "kube-apiserver"))
			return []string{"KUBEBUILDER_ASSETS is empty, and envtest found no etcd on PATH; install", "setup-envtest"}
		}},
		{"TEST_ASSET_ETCD is empty", func(t *testing.T) []string {
			installControlPlane(t, "etcd", "kube-apiserver")
			t.Setenv("TEST_ASSET_ETCD", "")
			return []string{"TEST_ASSET_ETCD is empty; fix or unset TEST_ASSET_ETCD"}
		}},
		{"TEST_ASSET_ETCD names a program PATH does not hold", func(t *testing.T) []string {
			installControlPlane(t, "kube-apiserver")
			t.Setenv("TEST_ASSET_ETCD", "my-etcd")
			t.Setenv("PATH", t.TempDir())
			return []string{"TEST_ASSET_ETCD is my-etcd, and envtest found no my-etcd on PATH"}
		}},
		{"TEST_ASSET_ETCD is the root directory", func(t *testing.T) []string {
			installControlPlane(t, "etcd", "kube-apiserver")
			t.Setenv("TEST_ASSET_ETCD", "/")
			return []string{"TEST_ASSET_ETCD is /, and / is not executable"}
		}},
		{"the directory holds etcd alone", func(t *testing.T) []string {
			dir := installControlPlane(t, "etcd")
			return []string{filepath.Join(dir, "kube-apiserver")}
		}},
		{"etcd is not executable", func(t *testing.T) []string {
			dir := installControlPlane(t, "kube-apiserver")
			if err := os.WriteFile(filepath.Join(dir, "etcd"), nil, 0o644); err != nil {
				t.Fatal(err)
			}
			return []string{filepath.Join(dir, "etcd"), "not executable"}
		}},
		{"etcd is a directory", func(t *testing.T) []string {
			dir := installControlPlane(t, "kube-apiserver")
			if err := os.Mkdir(filepath.Join(dir, "etcd"), 0o755); err != nil {
				t.Fatal(err)
			}
			return []string{filepath.Join(dir, "etcd"), "not executable"}
		}},
		{"TEST_ASSET_ETCD names a missing file", func(t *testing.T) []string {
			installControlPlane(t, "etcd", "kube-apiserver")
			missing := filepath.Join(t.TempDir(), "etcd")
			t.Setenv("TEST_ASSET_ETCD", missing)
			return []string{"TEST_ASSET_ETCD", missing}
		}},
		{"TEST_ASSET_KUBE_APISERVER names a missing file", func(t *testing.T) []string {
			installControlPlane(t, "etcd", "kube-apiserver")
			missing := filepath.Join(t.TempDir(), "kube-apiserver")
			t.Setenv("TEST_ASSET_KUBE_APISERVER", missing)
			return []string{"TEST_ASSET_KUBE_APISERVER", missing}
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			want := test.setup(t)

			err := cluster.Options{}.Validate()

			if err == nil {
				t.Fatal("Validate accepted a control plane envtest cannot run.")
			}
			for _, said := range want {
				if !strings.Contains(err.Error(), said) {
					t.Errorf("Validate returned %q, which does not say %q.", err, said)
				}
			}
		})
	}
}

// envtest, like os/exec, looks a path with no slash up on PATH.
func TestValidateLooksABareNameUpOnPATH(t *testing.T) {
	for name, set := range map[string]struct{ variable, value, etcd string }{
		"KUBEBUILDER_ASSETS is empty": {"KUBEBUILDER_ASSETS", "", "etcd"},
		"KUBEBUILDER_ASSETS is .":     {"KUBEBUILDER_ASSETS", ".", "etcd"},
		"TEST_ASSET_ETCD is a name":   {"TEST_ASSET_ETCD", "my-etcd", "my-etcd"},
	} {
		t.Run(name, func(t *testing.T) {
			installControlPlane(t, "kube-apiserver")
			t.Setenv("PATH", executables(t, set.etcd, "kube-apiserver"))
			t.Setenv(set.variable, set.value)

			if err := (cluster.Options{}).Validate(); err != nil {
				t.Errorf("Validate refused a control plane on PATH: %v", err)
			}
		})
	}
}

// A TEST_ASSET_ variable wins over KUBEBUILDER_ASSETS.
func TestValidateSaysToFixAnOverrideItCannotRun(t *testing.T) {
	for _, value := range []string{"", "/no/such/etcd"} {
		installControlPlane(t, "etcd", "kube-apiserver")
		t.Setenv("TEST_ASSET_ETCD", value)

		err := cluster.Options{}.Validate()

		if err == nil || !strings.Contains(err.Error(), "fix or unset TEST_ASSET_ETCD") || strings.Contains(err.Error(), "KUBEBUILDER_ASSETS") {
			t.Errorf("With TEST_ASSET_ETCD=%q, Validate returned %v, want it to say to fix or unset TEST_ASSET_ETCD.", value, err)
		}
	}
}

// pointAssetsNowhere makes sure a regression cannot start a control plane in
// the unit tier, which runs no API server (DESIGN.md §11).
func pointAssetsNowhere(t *testing.T) {
	t.Setenv("KUBEBUILDER_ASSETS", filepath.Join(t.TempDir(), "no-such-assets"))
}

func TestConnectValidatesBeforeReadingTheKubeconfig(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "no-such-dir")

	c, err := cluster.Connect(filepath.Join(t.TempDir(), "no-such-kubeconfig"), cluster.Options{CRDPaths: []string{missing}})
	if err == nil {
		_ = c.Stop()
		t.Fatal("Connect accepted a CRD path that does not exist.")
	}
	if !strings.Contains(err.Error(), missing) {
		t.Errorf("Connect returned %q, which does not name the missing path %q.", err, missing)
	}
}

func TestConnectNamesAKubeconfigItCannotRead(t *testing.T) {
	kubeconfig := filepath.Join(t.TempDir(), "no-such-kubeconfig")

	_, err := cluster.Connect(kubeconfig, cluster.Options{})
	if want := "reading the kubeconfig " + kubeconfig; err == nil || !strings.Contains(err.Error(), want) {
		t.Errorf("Connect returned %v, want an error saying %q.", err, want)
	}
}

// A kubeconfig cluster runs none of envtest's binaries, so Connect needs none.
func TestConnectNeedsNoControlPlaneBinaries(t *testing.T) {
	pointAssetsNowhere(t)
	kubeconfig := filepath.Join(t.TempDir(), "no-such-kubeconfig")

	_, err := cluster.Connect(kubeconfig, cluster.Options{CRDPaths: []string{t.TempDir()}})
	if want := "reading the kubeconfig " + kubeconfig; err == nil || !strings.Contains(err.Error(), want) {
		t.Errorf("Connect returned %v, want it to read the kubeconfig without envtest's binaries.", err)
	}
}

func TestConnectReportsCRDsItCouldNotInstall(t *testing.T) {
	kubeconfig := filepath.Join(t.TempDir(), "kubeconfig")
	unreachable := clientcmdapi.Config{
		Clusters:       map[string]*clientcmdapi.Cluster{"nowhere": {Server: "http://127.0.0.1:1"}},
		Contexts:       map[string]*clientcmdapi.Context{"nowhere": {Cluster: "nowhere"}},
		CurrentContext: "nowhere",
	}
	if err := clientcmd.WriteToFile(unreachable, kubeconfig); err != nil {
		t.Fatal(err)
	}

	_, err := cluster.Connect(kubeconfig, cluster.Options{CRDPaths: []string{"../../targets/toy-widget/crds"}})
	if err == nil || !strings.Contains(err.Error(), "installing the CRDs") {
		t.Errorf("Connect returned %v, want an error saying it could not install the CRDs.", err)
	}
}

func TestStartValidatesBeforeStartingEnvtest(t *testing.T) {
	pointAssetsNowhere(t)
	missing := filepath.Join(t.TempDir(), "no-such-dir")

	c, err := cluster.Start(cluster.Options{CRDPaths: []string{missing}})
	if err == nil {
		_ = c.Stop()
		t.Fatal("Start accepted a CRD path that does not exist.")
	}
	if c != nil {
		t.Error("Start returned a non-nil Cluster together with an error.")
	}
	if !strings.Contains(err.Error(), missing) {
		t.Errorf("Start returned %q, which does not name the missing path %q.", err, missing)
	}
}
