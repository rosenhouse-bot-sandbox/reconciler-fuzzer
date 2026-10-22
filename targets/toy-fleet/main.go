// Command toy-fleet runs the fleet's Cluster, Database and User controllers,
// with the seeded bug --bug names.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"time"

	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"

	"github.com/rosenhouse-bot-sandbox/reconciler-fuzzer/targets/toy-fleet/controller"
)

func main() {
	if err := run(os.Args[1:], os.Stdout); err != nil && !errors.Is(err, flag.ErrHelp) {
		fmt.Fprintf(os.Stderr, "toy-fleet: %v\n", err)
		os.Exit(1)
	}
}

func run(args []string, out io.Writer) error {
	flags := flag.NewFlagSet("toy-fleet", flag.ContinueOnError)
	flags.SetOutput(io.Discard) // main reports what Parse rejects; --help prints below.
	kubeconfig := flags.String("kubeconfig", "", "path to a kubeconfig file; defaults to $KUBECONFIG, then to the in-cluster configuration")
	bugID := flags.Int("bug", 0, "seeded bug to run; 0 is the correct controller")
	metricsAddress := flags.String("metrics-bind-address", "0", "address the metrics server binds to; 0 disables it")
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

	ctrl.SetLogger(zap.New(zap.UseDevMode(true)))
	config, err := restConfig(*kubeconfig)
	if err != nil {
		return fmt.Errorf("loading the cluster configuration: %w", err)
	}
	scheme, err := controller.NewScheme()
	if err != nil {
		return err
	}
	manager, err := ctrl.NewManager(config, ctrl.Options{
		Scheme:  scheme,
		Metrics: metricsserver.Options{BindAddress: *metricsAddress},
		Cache:   cacheOptions(os.Getenv("WATCH_NAMESPACE")),
	})
	if err != nil {
		return fmt.Errorf("creating the manager: %w", err)
	}
	if err := controller.Setup(manager, bug); err != nil {
		return fmt.Errorf("setting up the controllers: %w", err)
	}
	ctrl.Log.WithName("toy-fleet").Info("starting", "bug", int(bug))
	if err := manager.Start(ctrl.SetupSignalHandler()); err != nil {
		return err
	}
	c, err := client.New(config, client.Options{Scheme: scheme})
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	return controller.Shutdown(ctx, c, os.Getenv("WATCH_NAMESPACE"), bug)
}

// cacheOptions confine the cache to namespace, where it is set.
func cacheOptions(namespace string) cache.Options {
	if namespace == "" {
		return cache.Options{}
	}
	return cache.Options{DefaultNamespaces: map[string]cache.Config{namespace: {}}}
}

// restConfig prefers the --kubeconfig path, then $KUBECONFIG, then the
// in-cluster configuration.
func restConfig(kubeconfig string) (*rest.Config, error) {
	if kubeconfig != "" {
		return clientcmd.BuildConfigFromFlags("", kubeconfig)
	}
	return ctrl.GetConfig()
}
