package main

import (
	"errors"
	"flag"
	"io"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"k8s.io/apimachinery/pkg/runtime"

	"github.com/rosenhouse-bot-sandbox/reconciler-fuzzer/internal/config"
	"github.com/rosenhouse-bot-sandbox/reconciler-fuzzer/targets/toy-widget/controller"
)

func writeKubeconfig(t *testing.T, server string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "kubeconfig")
	contents := `apiVersion: v1
kind: Config
current-context: test
clusters:
  - name: test
    cluster: {server: ` + server + `}
contexts:
  - name: test
    context: {cluster: test, user: test}
users:
  - name: test
    user: {}
`
	if err := os.WriteFile(path, []byte(contents), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestRestConfigPrefersTheFlagOverTheEnvironment(t *testing.T) {
	t.Setenv("KUBECONFIG", writeKubeconfig(t, "https://environment.example"))

	config, err := restConfig(writeKubeconfig(t, "https://flag.example"))
	if err != nil {
		t.Fatalf("restConfig returned an error: %v", err)
	}
	if config.Host != "https://flag.example" {
		t.Errorf("restConfig used the server %q, want the one from --kubeconfig.", config.Host)
	}
}

func TestRestConfigFallsBackToTheEnvironment(t *testing.T) {
	t.Setenv("KUBECONFIG", writeKubeconfig(t, "https://environment.example"))

	config, err := restConfig("")
	if err != nil {
		t.Fatalf("restConfig returned an error: %v", err)
	}
	if config.Host != "https://environment.example" {
		t.Errorf("restConfig used the server %q, want the one from $KUBECONFIG.", config.Host)
	}
}

func TestManagerOptionsWatchOnlyWatchNamespace(t *testing.T) {
	t.Setenv("WATCH_NAMESPACE", "reconciler-fuzzer-run-x")

	watched := managerOptions(nil, "0", 0).Cache.DefaultNamespaces

	if namespaces := slices.Collect(maps.Keys(watched)); !slices.Equal(namespaces, []string{"reconciler-fuzzer-run-x"}) {
		t.Errorf("The manager watches %v, want only reconciler-fuzzer-run-x.", namespaces)
	}
}

func TestManagerOptionsKeepTheSchemeAndMetricsAddress(t *testing.T) {
	scheme := runtime.NewScheme()

	options := managerOptions(scheme, "127.0.0.1:0", 0)

	if options.Scheme != scheme {
		t.Error("The manager does not use the scheme it was given.")
	}
	if options.Metrics.BindAddress != "127.0.0.1:0" {
		t.Errorf("The manager binds metrics at %q, want 127.0.0.1:0.", options.Metrics.BindAddress)
	}
}

func TestManagerOptionsElectNoLeader(t *testing.T) {
	if managerOptions(nil, "0", 0).LeaderElection {
		t.Error("The manager runs leader election, which a single replica does not need.")
	}
}

// A lease lets a fault on leases make the toy lose it and exit, as a replica
// of a real controller does.
func TestManagerOptionsElectALeaderInWatchNamespaceWithALease(t *testing.T) {
	t.Setenv("WATCH_NAMESPACE", "reconciler-fuzzer-run-x")

	options := managerOptions(nil, "0", 3*time.Second)

	if !options.LeaderElection || options.LeaderElectionID == "" || options.LeaderElectionNamespace != "reconciler-fuzzer-run-x" {
		t.Errorf("The manager elects a leader %t, by the lease %q in %q; want a lease in reconciler-fuzzer-run-x.",
			options.LeaderElection, options.LeaderElectionID, options.LeaderElectionNamespace)
	}
	for _, lease := range []struct {
		name string
		got  *time.Duration
		want time.Duration
	}{
		{"lasts", options.LeaseDuration, 3 * time.Second},
		{"is renewed within", options.RenewDeadline, 2 * time.Second},
		{"is tried every", options.RetryPeriod, 600 * time.Millisecond},
	} {
		if lease.got == nil || *lease.got != lease.want {
			t.Errorf("The lease %s %s, want %v.", lease.name, durationOrNil(lease.got), lease.want)
		}
	}
}

func durationOrNil(d *time.Duration) string {
	if d == nil {
		return "nil"
	}
	return d.String()
}

func TestManagerOptionsWatchEveryNamespaceWithoutWatchNamespace(t *testing.T) {
	t.Setenv("WATCH_NAMESPACE", "")

	if watched := managerOptions(nil, "0", 0).Cache.DefaultNamespaces; watched != nil {
		t.Errorf("The manager watches %v, want every namespace.", watched)
	}
}

// B1 holds its premature status past one stable period, so Converge sees
// it settle, and lands its children within the confirm window.
func TestTheB1HoldLandsTheChildrenInTheConfirmWindow(t *testing.T) {
	c, err := config.Load("config.yaml")
	if err != nil {
		t.Fatal(err)
	}
	if stable := c.Timeouts.Stable; b1Hold <= stable || b1Hold >= 2*stable {
		t.Errorf("B1 holds for %v against a stable of %v, want stable < hold < 2 × stable.", b1Hold, stable)
	}
}

func TestRunRejectsABugOutsideTheCatalog(t *testing.T) {
	// The unreadable kubeconfig keeps a run that fails to reject the bug from
	// reaching an API server.
	t.Setenv("KUBECONFIG", filepath.Join(t.TempDir(), "no-such-kubeconfig"))
	outside := strconv.Itoa(controller.MaxBug + 1)

	err := run([]string{"--bug=" + outside}, io.Discard)

	if err == nil {
		t.Fatalf("run accepted --bug=%s.", outside)
	}
	if !strings.Contains(err.Error(), outside) {
		t.Errorf("run returned %q, which does not name the rejected value.", err)
	}
}

func TestRunRejectsOnlyANegativeDuration(t *testing.T) {
	t.Setenv("KUBECONFIG", filepath.Join(t.TempDir(), "no-such-kubeconfig"))

	for _, flag := range []string{"--resync", "--cleanup-delay", "--lease"} {
		for value, rejected := range map[string]bool{"-1s": true, "0s": false} {
			arg := flag + "=" + value

			err := run([]string{arg}, io.Discard)

			if got := err != nil && strings.Contains(err.Error(), arg); got != rejected {
				t.Errorf("run returned %v for %s, want it rejected: %v.", err, arg, rejected)
			}
		}
	}
}

func TestRunPrintsTheUsageForHelp(t *testing.T) {
	printed := &strings.Builder{}

	err := run([]string{"--help"}, printed)

	if !errors.Is(err, flag.ErrHelp) {
		t.Errorf("run returned %v for --help, want flag.ErrHelp so that main exits 0.", err)
	}
	for _, flag := range []string{"-bug", "-gadget"} {
		if !strings.Contains(printed.String(), flag) {
			t.Errorf("run printed %q for --help, want the usage of %s.", printed.String(), flag)
		}
	}
}
