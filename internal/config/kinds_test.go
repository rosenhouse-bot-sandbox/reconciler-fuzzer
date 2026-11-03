package config_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/rosenhouse-bot-sandbox/reconciler-fuzzer/internal/config"
)

// writeKinds writes config beside a crd.yaml whose CRDs serve toy/v1 and
// toy/v2 Cluster, and toy/v1 Database but not toy/v1beta1 Database.
func writeKinds(t *testing.T, config string) string {
	t.Helper()
	path := writeConfig(t, config)
	crds := crdOf("toy", "Cluster", "Namespaced", "v1", "v2") + "---\n" + crdOf("toy", "Database", "Namespaced", "v1beta1!", "v1")
	if err := os.WriteFile(filepath.Join(filepath.Dir(path), "crd.yaml"), []byte(crds), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

const kindsHead = "launch: {binary: bin/controller}\ncrds: [crd.yaml]\n"

func entries(kind string) string {
	return "primaries:\n  - {kind: toy/v1/Cluster, sample: sample.yaml, ready: \"true\"}\n  - {kind: " + kind + ", sample: sample.yaml, ready: \"true\"}\n"
}

func TestLoadRefusesAKindNoCRDServes(t *testing.T) {
	const unserved = ": no CRD under crds and no built-in kind serves it; "
	for name, c := range map[string]struct{ config, want string }{
		"another version":        {kindsHead + entries("toy/v3/Database"), "primaries[1].kind toy/v3/Database" + unserved + "did you mean toy/v1/Database?"},
		"an unserved version":    {kindsHead + entries("toy/v1beta1/Database"), "primaries[1].kind toy/v1beta1/Database" + unserved + "did you mean toy/v1/Database?"},
		"another group":          {kindsHead + entries("other/v1/Database"), "primaries[1].kind other/v1/Database" + unserved + "did you mean toy/v1/Database?"},
		"a plural":               {kindsHead + entries("toy/v1/databases"), "did you mean toy/v1/Database?"},
		"a missing CRD":          {kindsHead + entries("toy/v1/Gizmo"), "primaries[1].kind toy/v1/Gizmo" + unserved + "the CRDs serve toy/v1/Cluster, toy/v1/Database and toy/v2/Cluster"},
		"a built-in kind's case": {kindsHead + entries("v1/Configmap"), "did you mean v1/ConfigMap?"},
		"a built-in plural":      {kindsHead + entries("apps/v1/deployments"), "did you mean apps/v1/Deployment?"},
		"a model":                {kindsHead + entries("toy/v1/Database") + "models: [{kind: toy/v1/Gizmo, after: 1s}]\n", "models[0].kind toy/v1/Gizmo" + unserved},
		"an owned kind": {kindsHead + "primary: {kind: toy/v1/Cluster, sample: sample.yaml, ready: \"true\", owns: [{kind: v1/Secrets, name: self.metadata.name}]}\n",
			"primary.owns[0].kind v1/Secrets" + unserved + "did you mean v1/Secret?"},
		"no CRD": {"launch: {binary: bin/controller}\nprimary: {kind: toy/v1/Cluster, sample: sample.yaml, ready: \"true\"}\n", "primary.kind toy/v1/Cluster" + unserved + "crds holds no CRD"},
	} {
		if _, err := config.Load(writeKinds(t, c.config)); err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: Load = %v, want an error containing %q", name, err, c.want)
		}
	}
}

func TestLoadTakesEachServedVersion(t *testing.T) {
	for _, c := range []string{
		kindsHead + "primary: {kind: toy/v2/Cluster, sample: sample.yaml, ready: \"true\"}\n",
		kindsHead + entries("toy/v1/Database"),
		kindsHead + entries("toy/v1/Database") + "models: [{kind: autoscaling/v1/HorizontalPodAutoscaler, after: 1s}]\n",
		kindsHead + entries("toy/v1/Database") + "models: [{kind: autoscaling/v2/HorizontalPodAutoscaler, after: 1s}]\n",
	} {
		if _, err := config.Load(writeKinds(t, c)); err != nil {
			t.Errorf("Load = %v, want this config to load:\n%s", err, c)
		}
	}
}
