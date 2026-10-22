package generate

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"flag"
	"os"
	"testing"
)

var updateGolden = flag.Bool("update", false, "rewrite testdata/golden.json")

// goldenConfigs draw no model faults. Their plans change only with the draws.
var goldenConfigs = []string{
	toyConfig,
	"../../examples/cert-manager/config.yaml",
	"../../examples/external-secrets/config.yaml",
}

const goldenFile = "testdata/golden.json"

func TestPlansWithoutModelsAreUnchanged(t *testing.T) {
	got := map[string][]string{}
	for _, path := range goldenConfigs {
		g := newGenerator(t, loadConfig(t, path))
		for seed := uint64(0); seed < 20; seed++ {
			p, err := g.Plan(seed)
			if err != nil {
				t.Fatal(err)
			}
			data, err := json.Marshal(p)
			if err != nil {
				t.Fatal(err)
			}
			sum := sha256.Sum256(data)
			got[path] = append(got[path], hex.EncodeToString(sum[:8]))
		}
	}
	if *updateGolden {
		data, _ := json.MarshalIndent(got, "", "  ")
		if err := os.WriteFile(goldenFile, append(data, '\n'), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	data, err := os.ReadFile(goldenFile)
	if err != nil {
		t.Fatal(err)
	}
	var want map[string][]string
	if err := json.Unmarshal(data, &want); err != nil {
		t.Fatal(err)
	}
	for _, path := range goldenConfigs {
		for seed, sum := range want[path] {
			if got[path][seed] != sum {
				t.Errorf("%s seed %d draws a different plan.", path, seed)
			}
		}
		if len(want[path]) != 20 {
			t.Errorf("%s has %d golden plans, want 20.", path, len(want[path]))
		}
	}
}
