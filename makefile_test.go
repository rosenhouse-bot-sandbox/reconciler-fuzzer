package reconcilerfuzzer_test

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// TestMakeTargetsCoverEveryToy dry-runs the Makefile on a targets/ that holds
// two made-up toys and a file.
func TestMakeTargetsCoverEveryToy(t *testing.T) {
	dir := t.TempDir()
	toys := []string{"toy-a", "toy-b"}
	for _, toy := range toys {
		writeFile(t, dir, "targets/"+toy+"/main.go")
	}
	writeFile(t, dir, "targets/README.md")
	for target, perToy := range map[string][]string{
		"build":          {"./targets/%s"},
		"generate":       {"paths=./targets/%s/...", "output:crd:artifacts:config=targets/%s/crds"},
		"test-generated": {"./targets/%s"},
		"bug-matrix":     {"./targets/%s"},
	} {
		out, err := runMake(dir, "-n", "-o", "setup", target)
		if err != nil {
			t.Fatalf("make -n %s: %v\n%s", target, err, out)
		}
		fields := strings.Fields(out)
		for _, toy := range toys {
			for _, format := range perToy {
				if want := fmt.Sprintf(format, toy); !slices.Contains(fields, want) {
					t.Errorf("make %s does not run %s:\n%s", target, want, out)
				}
			}
		}
		if strings.Contains(out, "README") {
			t.Errorf("make %s takes targets/README.md for a toy:\n%s", target, out)
		}
	}
}

// TestVerifyGenerateChecksEveryToysGeneratedFiles runs verify-generate, without
// generate, in a repository where each toy has one uncommitted file.
func TestVerifyGenerateChecksEveryToysGeneratedFiles(t *testing.T) {
	dir := t.TempDir()
	if out, err := exec.Command("git", "-C", dir, "init", "-q").CombinedOutput(); err != nil {
		t.Fatalf("git init: %v\n%s", err, out)
	}
	writeFile(t, dir, "targets/toy-a/main.go")
	if out, err := runMake(dir, "-o", "generate", "verify-generate"); err != nil {
		t.Fatalf("verify-generate fails on an uncommitted main.go: %v\n%s", err, out)
	}
	for _, generated := range []string{
		"targets/toy-a/crds/toy_widgets.yaml",
		"targets/toy-b/thirdparty/storage/v1/zz_generated.deepcopy.go",
	} {
		writeFile(t, dir, generated)
		if out, err := runMake(dir, "-o", "generate", "verify-generate"); err == nil || !strings.Contains(out, generated) {
			t.Errorf("verify-generate gave %v and does not name the uncommitted %s:\n%s", err, generated, out)
		}
		if err := os.Remove(filepath.Join(dir, generated)); err != nil {
			t.Fatal(err)
		}
	}
}

// runMake runs this repository's Makefile in dir.
func runMake(dir string, args ...string) (string, error) {
	makefile, err := filepath.Abs("Makefile")
	if err != nil {
		return "", err
	}
	out, err := exec.Command("make", append([]string{"--no-print-directory", "-C", dir, "-f", makefile}, args...)...).CombinedOutput()
	return string(out), err
}

func writeFile(t *testing.T, dir, path string) {
	t.Helper()
	path = filepath.Join(dir, path)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, nil, 0o644); err != nil {
		t.Fatal(err)
	}
}
