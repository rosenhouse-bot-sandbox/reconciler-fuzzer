// Command crudfake serves internal/crudfake on --listen, or else on
// $RECONCILER_FUZZER_ADDR.
package main

import (
	"flag"
	"fmt"
	"net/http"
	"os"

	"github.com/rosenhouse-bot-sandbox/reconciler-fuzzer/internal/crudfake"
)

func main() {
	config := flag.String("config", "", "the YAML file of collections")
	listen := flag.String("listen", os.Getenv("RECONCILER_FUZZER_ADDR"), "the address to serve on")
	flag.Parse()
	cfg, err := crudfake.Load(*config)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(2)
	}
	server := crudfake.New(cfg)
	defer server.Close()
	if err := http.ListenAndServe(*listen, server); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(2)
	}
}
