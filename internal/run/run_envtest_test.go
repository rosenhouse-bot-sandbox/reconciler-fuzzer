//go:build envtest

package run

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/rosenhouse-bot-sandbox/reconciler-fuzzer/internal/plan"
	"github.com/rosenhouse-bot-sandbox/reconciler-fuzzer/internal/trace"
)

const toy = "../../targets/toy-widget/"

// lister is a controller that only lists the fake's buckets.
const lister = `while true; do curl -sf "$CLOUD_URL/buckets" >/dev/null; sleep 1; done`

func TestRunStartsAFakeAndFaultsIt(t *testing.T) {
	dir := t.TempDir()
	crudfake := filepath.Join(dir, "crudfake")
	if out, err := exec.Command("go", "build", "-o", crudfake, "../../cmd/crudfake").CombinedOutput(); err != nil {
		t.Fatalf("building crudfake: %v\n%s", err, out)
	}
	toyDir, err := filepath.Abs(toy)
	if err != nil {
		t.Fatal(err)
	}
	files := map[string]string{
		"cloud.yaml": "collections:\n  buckets: {pending: CREATING, ready: READY, deleting: DELETING, createDelay: 100ms, deleteDelay: 100ms}\n",
		"lister.sh":  lister,
		"config.yaml": fmt.Sprintf(`launch:
  binary: /bin/sh
  args: [%[3]s/lister.sh]
  env: {CLOUD_URL: "${systems.cloud.url}"}
crds: [%[1]s/crds/]
primary:
  kind: toy.reconciler-fuzzer/v1/Widget
  sample: %[1]s/widget.yaml
  ready: "true"
systems:
  - {name: cloud, process: {command: [%[2]s, --config, cloud.yaml]}, api: crud, poll: 500ms}
timeouts: {settle: 10s, stable: 1s}
`, toyDir, crudfake, dir),
	}
	for name, content := range files {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	p := &plan.Plan{Steps: []plan.Step{
		{Actions: []plan.Action{{Do: plan.Create, Slot: "a"}}},
		{
			Faults: []plan.Fault{{System: "cloud", Match: plan.Match{Verb: "list"}, Nth: 1, Error: &plan.Error{Code: 503}}},
			Period: plan.Duration(1500e6),
		},
		{Faults: []plan.Fault{{System: "cloud", Stuck: true}}, Period: plan.Duration(500e6)},
	}}
	out := filepath.Join(dir, "out")
	report, err := Run(context.Background(), Options{ConfigPath: filepath.Join(dir, "config.yaml"), Plan: p, Out: out})
	if err != nil {
		t.Fatal(err)
	}
	if len(report.Findings) > 0 || len(report.Notes) > 0 {
		t.Errorf("findings %+v, notes %q, want none", report.Findings, report.Notes)
	}
	rec, err := trace.Load(filepath.Join(out, "trace.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	var systems, injected, stuck int
	for _, e := range rec {
		switch {
		case e.Run != nil:
			if len(e.Run.Systems) == 1 && e.Run.Systems[0].Name == "cloud" {
				systems++
			}
		case e.Call != nil && e.Call.Ref.System == "cloud" && e.Call.Injected == "error(503)":
			injected++
		case e.Fault != nil && e.Fault.Model == "cloud":
			stuck++
		}
	}
	if systems != 1 || injected != 1 || stuck != 2 {
		t.Errorf("the trace holds %d runs naming cloud, %d injected 503s and %d stuck events, want 1, 1 and 2", systems, injected, stuck)
	}
}
