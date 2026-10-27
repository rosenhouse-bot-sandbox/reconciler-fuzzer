//go:build envtest

package run

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

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

// TestAPropertyOverAnInstalledCRDWithNoObjectHolds runs a property over the
// toy's grants CRD, which the run event in trace.jsonl lists in kinds. The
// controller only lists widgets, so no Grant exists.
func TestAPropertyOverAnInstalledCRDWithNoObjectHolds(t *testing.T) {
	toyDir, err := filepath.Abs(toy)
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	config := fmt.Sprintf(`launch:
  binary: /bin/sh
  args: [-c, 'while true; do curl -sf ${systems.kube.url}/apis/toy.reconciler-fuzzer/v1/widgets >/dev/null; sleep 1; done']
  rbac: [%[1]s/rbac/role.yaml]
crds: [%[1]s/crds/]
primary:
  kind: toy.reconciler-fuzzer/v1/Widget
  sample: %[1]s/widget.yaml
  ready: "true"
timeouts: {settle: 10s, stable: 1s}
properties:
  - name: no-stuck-grants
    expr: "world.kube['grants.toy.reconciler-fuzzer'].all(g, !has(g.metadata.deletionTimestamp))"
`, toyDir)
	if err := os.WriteFile(filepath.Join(dir, "config.yaml"), []byte(config), 0o644); err != nil {
		t.Fatal(err)
	}
	p := &plan.Plan{Steps: []plan.Step{{Actions: []plan.Action{{Do: plan.Create, Slot: "a"}}}}}
	out := filepath.Join(dir, "out")
	report, err := Run(context.Background(), Options{ConfigPath: filepath.Join(dir, "config.yaml"), Plan: p, Out: out})
	if err != nil {
		t.Fatal(err)
	}
	if len(report.Findings) > 0 {
		t.Errorf("findings %+v, want none", report.Findings)
	}
	events, err := trace.Load(filepath.Join(out, "trace.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	grants := trace.Ref{System: "kube", Group: "toy.reconciler-fuzzer", Resource: "grants"}
	if i := slices.IndexFunc(events, func(e trace.Event) bool { return e.Run != nil }); i < 0 || !slices.Contains(events[i].Run.Kinds, grants) {
		t.Errorf("the trace's run event does not list grants in kinds")
	}
}

// TestClientGoLogsToHarnessLog deletes the grants CRD, a fixture, after the
// controller's request for grants made the harness watch them. client-go's
// failed watch of grants goes to harness.log, not to stderr.
func TestClientGoLogsToHarnessLog(t *testing.T) {
	toyDir, err := filepath.Abs(toy)
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	config := fmt.Sprintf(`launch:
  binary: /bin/sh
  args: [-c, 'curl -s ${systems.kube.url}/apis/toy.reconciler-fuzzer/v1/grants >/dev/null; while true; do curl -sf ${systems.kube.url}/apis/toy.reconciler-fuzzer/v1/widgets >/dev/null; sleep 1; done']
  rbac: [%[1]s/rbac/role.yaml]
crds: [%[1]s/crds/toy.reconciler-fuzzer_widgets.yaml]
fixtures: [%[1]s/crds/toy.reconciler-fuzzer_grants.yaml]
primary:
  kind: toy.reconciler-fuzzer/v1/Widget
  sample: %[1]s/widget.yaml
  ready: "true"
timeouts: {settle: 10s, stable: 1s}
`, toyDir)
	if err := os.WriteFile(filepath.Join(dir, "config.yaml"), []byte(config), 0o644); err != nil {
		t.Fatal(err)
	}
	grantsCRD := &plan.Target{Group: "apiextensions.k8s.io", Resource: "customresourcedefinitions", Name: "grants.toy.reconciler-fuzzer"}
	p := &plan.Plan{Steps: []plan.Step{
		{Actions: []plan.Action{{Do: plan.Create, Slot: "a"}}},
		{Actions: []plan.Action{{Do: plan.ChangeFixture, Target: grantsCRD}}},
		{Period: plan.Duration(2 * time.Second)},
	}}
	stderr, err := os.Create(filepath.Join(dir, "stderr"))
	if err != nil {
		t.Fatal(err)
	}
	defer stderr.Close()
	saved := os.Stderr
	os.Stderr = stderr
	out := filepath.Join(dir, "out")
	_, err = Run(context.Background(), Options{ConfigPath: filepath.Join(dir, "config.yaml"), Plan: p, Out: out})
	os.Stderr = saved
	if err != nil {
		t.Fatal(err)
	}
	if printed, _ := os.ReadFile(stderr.Name()); strings.Contains(string(printed), "Failed to watch") {
		t.Errorf("stderr holds client-go's log:\n%s", printed)
	}
	harnessLog, err := os.ReadFile(filepath.Join(out, "harness.log"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(harnessLog), "Failed to watch") || !strings.Contains(string(harnessLog), "grants") {
		t.Errorf("harness.log lacks the failed watch of grants:\n%s", harnessLog)
	}
}

// reader is a controller that reads the Vault secret a fixture names.
const reader = `while true; do curl -sf -H "X-Vault-Token: $VAULT_TOKEN" "$VAULT_URL/v1/secret/data/app/db" >/dev/null; sleep 1; done`

func TestRunStartsVaultBeforeKubeAndCreatesBothFixtures(t *testing.T) {
	vault, err := filepath.Abs("../../bin/vault")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(vault); err != nil {
		t.Fatalf("%v; run make setup", err)
	}
	toyDir, err := filepath.Abs(toy)
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	files := map[string]string{
		"reader.sh":    reader,
		"secrets.yaml": "- {resource: secret, name: app/db, body: {password: p1}}\n",
		"store.yaml":   "apiVersion: v1\nkind: ConfigMap\nmetadata: {name: store}\ndata: {url: '${systems.vault.url}'}\n",
		"config.yaml": fmt.Sprintf(`launch:
  binary: /bin/sh
  args: [%[2]s/reader.sh]
  env: {VAULT_URL: "${systems.vault.url}", VAULT_TOKEN: "${systems.vault.token}"}
crds: [%[1]s/crds/]
fixtures: [store.yaml]
primary:
  kind: toy.reconciler-fuzzer/v1/Widget
  sample: %[1]s/widget.yaml
  ready: "true"
systems:
  - name: vault
    process: {command: [%[3]s, server, -dev, -dev-root-token-id=root, '-dev-listen-address=${addr}']}
    api: vault
    token: root
    fixtures: [secrets.yaml]
timeouts: {settle: 10s, stable: 1s}
`, toyDir, dir, vault),
	}
	for name, content := range files {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	out := filepath.Join(dir, "out")
	p := &plan.Plan{Steps: []plan.Step{{Actions: []plan.Action{{Do: plan.Create, Slot: "a"}}}}}
	report, err := Run(context.Background(), Options{ConfigPath: filepath.Join(dir, "config.yaml"), Plan: p, Out: out})
	if err != nil {
		t.Fatal(err)
	}
	if len(report.Findings) > 0 {
		t.Errorf("findings %+v, want none", report.Findings)
	}
	events, err := trace.Load(filepath.Join(out, "trace.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	var secret, store, reads, expanded bool
	for _, e := range events {
		if v := e.Version; v != nil && v.Ref.Name == "store" && strings.Contains(string(v.Body), `"url":"http://127.0.0.1:`) {
			expanded = true
		}
		switch c := e.Call; {
		case c == nil:
		case c.Caller == trace.Harness && c.Verb == "create" && c.ResultUID == "vault/secret/app/db/1":
			secret = !store
		case c.Caller == trace.Harness && c.Verb == "create" && c.Ref.Resource == "configmaps":
			store = true
		case c.Caller == trace.Controller && c.Ref.System == "vault" && c.Verb == "get" && c.Status == 200:
			reads = true
		}
	}
	if !secret || !store || !expanded || !reads {
		t.Errorf("the vault fixture first %v, the kube fixture %v expanded %v, a controller read %v; want all", secret, store, expanded, reads)
	}
}

// A launch.env without KUBECONFIG must not leave the controller on the
// shell's cluster.
func TestTheControllerGetsTheHarnessKubeconfigByDefault(t *testing.T) {
	t.Setenv("KUBECONFIG", "/home/me/.kube/production")
	toyDir, err := filepath.Abs(toy)
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	got := filepath.Join(dir, "got")
	config := fmt.Sprintf("launch: {binary: /bin/sh, args: [-c, 'echo \"$KUBECONFIG\" > %[2]s; exec sleep 60']}\ncrds: [%[1]s/crds/]\nprimary: {kind: toy.reconciler-fuzzer/v1/Widget, sample: %[1]s/widget.yaml, ready: \"true\"}\ntimeouts: {settle: 3s, stable: 1s}\n",
		toyDir, got)
	if err := os.WriteFile(filepath.Join(dir, "config.yaml"), []byte(config), 0o644); err != nil {
		t.Fatal(err)
	}
	out := filepath.Join(dir, "out")
	p := &plan.Plan{Steps: []plan.Step{{Actions: []plan.Action{{Do: plan.Create, Slot: "a"}}}}}
	if _, err := Run(context.Background(), Options{ConfigPath: filepath.Join(dir, "config.yaml"), Plan: p, Out: out}); err != nil {
		t.Fatal(err)
	}
	text, err := os.ReadFile(got)
	if err != nil {
		t.Fatal(err)
	}
	if want := filepath.Join(out, "kubeconfig") + "\n"; string(text) != want {
		t.Errorf("The controller got KUBECONFIG %q, want %q.", text, want)
	}
}
