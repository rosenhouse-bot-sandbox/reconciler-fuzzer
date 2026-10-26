//go:build envtest

package main_test

import (
	"bufio"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/rosenhouse-bot-sandbox/reconciler-fuzzer/internal/run"
)

// runUpgrade runs the upgrade plan under B0 into a directory it removes
// only if the test passes, and returns the directory.
func runUpgrade(t *testing.T, name string) string {
	t.Helper()
	out, err := os.MkdirTemp("", "rf-fleet-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if t.Failed() {
			t.Logf("The run is in %s.", out)
		} else {
			os.RemoveAll(out)
		}
	})
	o := run.Options{ConfigPath: upgradeConfig, PlanPath: planPath(upgradeConfig, name), Out: out, LaunchArgs: []string{"--bug=0"}}
	if _, err := run.Run(t.Context(), o); err != nil {
		t.Fatalf("%v; the run is in %s", err, out)
	}
	return out
}

// The appended restart starts version 2 again.
func TestARestartActionsArgsOutliveIt(t *testing.T) {
	t.Parallel()
	log, err := os.ReadFile(filepath.Join(runUpgrade(t, "delete"), "controller.log"))
	if err != nil {
		t.Fatal(err)
	}
	starts, v2 := strings.Count(string(log), "toy-fleet\tstarting\t"), strings.Count(string(log), `"version": 2}`)
	if starts != 3 || v2 != 2 {
		t.Errorf("The controller started %d times, %d of them as version 2; want 3 and 2. controller.log:\n%s", starts, v2, log)
	}
}

// The server defaults the new CRD's spec.tier on the Cluster that version
// 1 made.
func TestARestartActionInstallsNewCRDs(t *testing.T) {
	t.Parallel()
	out := runUpgrade(t, "crds")
	f, err := os.Open(filepath.Join(out, "trace.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	lines := bufio.NewScanner(f)
	lines.Buffer(nil, 1<<24)
	for lines.Scan() {
		var e struct {
			Version *struct {
				Ref  struct{ Resource string }
				Body struct{ Spec struct{ Tier string } }
			}
		}
		if err := json.Unmarshal(lines.Bytes(), &e); err != nil {
			t.Fatal(err)
		}
		if e.Version != nil && e.Version.Ref.Resource == "clusters" && e.Version.Body.Spec.Tier == "standard" {
			return
		}
	}
	if err := lines.Err(); err != nil {
		t.Fatal(err)
	}
	t.Error("No Cluster version has spec.tier standard.")
}
