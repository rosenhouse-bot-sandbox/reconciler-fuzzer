// Command toy-widget runs the Widget, Tenant or Grant controller of the toy
// target, with the seeded bug --bug names.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"slices"
	"strconv"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"

	toyv1 "github.com/rosenhouse-bot-sandbox/reconciler-fuzzer/targets/toy-widget/api/v1"
	"github.com/rosenhouse-bot-sandbox/reconciler-fuzzer/targets/toy-widget/controller"
)

// b1Hold is how long B1 holds its premature status. It is longer than the
// toy's timeouts.stable and shorter than twice that, which lands B1's children
// after the checkpoint and inside the quiet window that follows.
const b1Hold = 3 * time.Second

func main() {
	if err := run(os.Args[1:], os.Stdout); err != nil && !errors.Is(err, flag.ErrHelp) {
		fmt.Fprintf(os.Stderr, "toy-widget: %v\n", err)
		os.Exit(1)
	}
}

func run(args []string, out io.Writer) error {
	flags := flag.NewFlagSet("toy-widget", flag.ContinueOnError)
	flags.SetOutput(io.Discard) // main reports what Parse rejects; --help prints below.
	kubeconfig := flags.String("kubeconfig", "", "path to a kubeconfig file; defaults to $KUBECONFIG, then to the in-cluster configuration")
	bugID := flags.Int("bug", 0, fmt.Sprintf("seeded bug to run, 0 to %d; 0 is the correct controller", controller.MaxBug))
	metricsAddress := flags.String("metrics-bind-address", "0", "address the metrics server binds to; 0 disables it")
	resync := flags.Duration("resync", 0, "requeue every Widget this often and write its status each time; 0 disables it")
	cleanupDelay := flags.Duration("cleanup-delay", 0, "how long a deleted Widget keeps its finalizer before the controller cleans up")
	labelFrom := flags.String("label-from", "", "a ConfigMap in the Widget's namespace whose data.label each child copies")
	lease := flags.Duration("lease", 0, "elect a leader through a Lease in $WATCH_NAMESPACE that lasts this long, and exit on losing it; 0 elects none")
	gadget := flags.Bool("gadget", false, "give each Widget a Gadget, and report whether it is ready in status.gadgetReady")
	index := flags.Bool("index", false, "index Widgets by spec.count, which starts their informer before the manager leads")
	tenant := flags.Bool("tenant", false, "reconcile Tenants instead of Widgets, watching every namespace")
	grant := flags.Bool("grant", false, "reconcile Grants instead of Widgets")
	mirrorNamespace := flags.String("mirror-namespace", "rf-shared", "with --grant, the namespace of each Grant's ConfigMap and of grant-config")
	if err := flags.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			flags.SetOutput(out)
			flags.Usage()
		}
		return err
	}
	bug, err := controller.ParseBug(*bugID)
	if err != nil {
		return err
	}
	if *resync < 0 {
		return fmt.Errorf("--resync=%v: want 0 or more", *resync)
	}
	if *cleanupDelay < 0 {
		return fmt.Errorf("--cleanup-delay=%v: want 0 or more", *cleanupDelay)
	}
	if *lease < 0 {
		return fmt.Errorf("--lease=%v: want 0 or more", *lease)
	}
	mode, err := modeOf(*gadget, *tenant, *grant)
	if err != nil {
		return err
	}

	ctrl.SetLogger(zap.New(zap.UseDevMode(true)))

	config, err := restConfig(*kubeconfig)
	if err != nil {
		return fmt.Errorf("loading the cluster configuration: %w", err)
	}
	scheme, err := controller.NewScheme()
	if err != nil {
		return err
	}
	manager, err := ctrl.NewManager(config, managerOptions(scheme, *metricsAddress, *lease, cacheOptions(mode, *mirrorNamespace)))
	if err != nil {
		return fmt.Errorf("creating the manager: %w", err)
	}

	if *index {
		if err := indexWidgets(manager); err != nil {
			return fmt.Errorf("indexing Widgets: %w", err)
		}
	}

	var reconciler interface{ SetupWithManager(ctrl.Manager) error }
	switch mode {
	case tenantMode:
		reconciler = &controller.TenantReconciler{Client: manager.GetClient(), Scheme: manager.GetScheme(), Bug: bug}
	case grantMode:
		reconciler = &controller.GrantReconciler{
			Client: manager.GetClient(), APIReader: manager.GetAPIReader(), Scheme: manager.GetScheme(), Bug: bug,
			MirrorNamespace: *mirrorNamespace,
		}
	default:
		reconciler = &controller.Reconciler{
			Client:       manager.GetClient(),
			APIReader:    manager.GetAPIReader(),
			Scheme:       manager.GetScheme(),
			Bug:          bug,
			B1Hold:       b1Hold,
			Resync:       *resync,
			CleanupDelay: *cleanupDelay,
			LabelFrom:    *labelFrom,
			Gadget:       *gadget,
		}
	}
	if err := reconciler.SetupWithManager(manager); err != nil {
		return fmt.Errorf("setting up the controller: %w", err)
	}

	ctrl.Log.WithName("toy-widget").Info("starting", "bug", int(bug))
	return manager.Start(ctrl.SetupSignalHandler())
}

// mode is the kind the toy reconciles.
type mode int

const (
	widgetMode mode = iota
	tenantMode
	grantMode
)

// modeOf chooses the mode from at most one of --gadget, --tenant and --grant.
func modeOf(gadget, tenant, grant bool) (mode, error) {
	var chosen []string
	for flag, set := range map[string]bool{"--gadget": gadget, "--tenant": tenant, "--grant": grant} {
		if set {
			chosen = append(chosen, flag)
		}
	}
	if len(chosen) > 1 {
		slices.Sort(chosen)
		return 0, fmt.Errorf("%s: want at most one", strings.Join(chosen, " and "))
	}
	switch {
	case tenant:
		return tenantMode, nil
	case grant:
		return grantMode, nil
	}
	return widgetMode, nil
}

// cacheOptions confine the cache to WATCH_NAMESPACE, where it is set, as an
// operator-sdk operator does, except in Tenant mode. Grant mode caches
// ConfigMaps only in the mirror namespace.
func cacheOptions(m mode, mirrorNamespace string) cache.Options {
	namespace := os.Getenv("WATCH_NAMESPACE")
	if m == tenantMode || namespace == "" {
		return cache.Options{}
	}
	options := cache.Options{DefaultNamespaces: map[string]cache.Config{namespace: {}}}
	if m == grantMode {
		options.ByObject = map[client.Object]cache.ByObject{
			&corev1.ConfigMap{}: {Namespaces: map[string]cache.Config{mirrorNamespace: {}}},
		}
	}
	return options
}

// managerOptions use cacheOptions. A lease elects a leader in
// WATCH_NAMESPACE, and the manager stops once it loses the lease.
func managerOptions(scheme *runtime.Scheme, metricsAddress string, lease time.Duration, c cache.Options) ctrl.Options {
	namespace := os.Getenv("WATCH_NAMESPACE")
	options := ctrl.Options{
		Scheme:         scheme,
		Metrics:        metricsserver.Options{BindAddress: metricsAddress},
		LeaderElection: lease > 0,
		Cache:          c,
	}
	if lease > 0 {
		renew, retry := lease*2/3, lease/5
		options.LeaderElectionID, options.LeaderElectionNamespace = "toy-widget", namespace
		options.LeaseDuration, options.RenewDeadline, options.RetryPeriod = &lease, &renew, &retry
	}
	return options
}

// indexWidgets indexes Widgets by spec.count, as a controller indexes a field
// it lists by. The manager starts the informer an index needs before it leads.
func indexWidgets(manager ctrl.Manager) error {
	return manager.GetFieldIndexer().IndexField(context.Background(), &toyv1.Widget{}, "spec.count", func(object client.Object) []string {
		return []string{strconv.Itoa(int(object.(*toyv1.Widget).Spec.Count))}
	})
}

// restConfig prefers the --kubeconfig path, then $KUBECONFIG, then the
// in-cluster configuration.
func restConfig(kubeconfig string) (*rest.Config, error) {
	if kubeconfig != "" {
		return clientcmd.BuildConfigFromFlags("", kubeconfig)
	}
	return ctrl.GetConfig()
}
