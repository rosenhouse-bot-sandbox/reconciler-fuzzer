// Command toy-bucket runs the Bucket controller of the bucket toy, with the
// seeded bug --bug names.
package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"time"

	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"

	"github.com/rosenhouse-bot-sandbox/reconciler-fuzzer/targets/toy-bucket/controller"
)

func main() {
	if err := run(os.Args[1:], os.Stdout); err != nil && !errors.Is(err, flag.ErrHelp) {
		fmt.Fprintf(os.Stderr, "toy-bucket: %v\n", err)
		os.Exit(1)
	}
}

func run(args []string, out io.Writer) error {
	flags := flag.NewFlagSet("toy-bucket", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	bugID := flags.Int("bug", 0, "seeded bug to run, 0 or 30 to 35; 0 is the correct controller")
	halt := flags.Bool("halt", false, "stop reconciling a Bucket, except its deletion, after a create of unknown outcome")
	poll := flags.Duration("poll", time.Second, "how often to read a ready bucket")
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
	if *poll <= 0 {
		return fmt.Errorf("--poll=%v: want more than 0", *poll)
	}
	endpoint := os.Getenv("CLOUD_ENDPOINT")
	if endpoint == "" {
		return errors.New("CLOUD_ENDPOINT is not set")
	}

	ctrl.SetLogger(zap.New(zap.UseDevMode(true)))
	config, err := ctrl.GetConfig()
	if err != nil {
		return fmt.Errorf("loading the cluster configuration: %w", err)
	}
	scheme, err := controller.NewScheme()
	if err != nil {
		return err
	}
	manager, err := ctrl.NewManager(config, ctrl.Options{
		Scheme:  scheme,
		Metrics: metricsserver.Options{BindAddress: "0"},
		Cache:   cacheOptions(os.Getenv("WATCH_NAMESPACE")),
	})
	if err != nil {
		return fmt.Errorf("creating the manager: %w", err)
	}
	reconciler := &controller.BucketReconciler{
		Client: manager.GetClient(), Cloud: &controller.Cloud{Endpoint: endpoint},
		Bug: bug, Halt: *halt, Poll: *poll, Nonce: controller.NewNonce(),
	}
	if err := reconciler.SetupWithManager(manager); err != nil {
		return fmt.Errorf("setting up the controller: %w", err)
	}
	ctrl.Log.WithName("toy-bucket").Info("starting", "bug", int(bug), "halt", *halt)
	return manager.Start(ctrl.SetupSignalHandler())
}

// cacheOptions confine the cache to namespace, where it is set.
func cacheOptions(namespace string) cache.Options {
	if namespace == "" {
		return cache.Options{}
	}
	return cache.Options{DefaultNamespaces: map[string]cache.Config{namespace: {}}}
}
