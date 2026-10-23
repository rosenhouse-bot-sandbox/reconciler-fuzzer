package reconcilerfuzzer_test

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"
)

// TestMakeTargetsCoverEveryToy dry-runs the Makefile on a targets/ that holds
// two made-up toys and a directory with no main.go.
func TestMakeTargetsCoverEveryToy(t *testing.T) {
	dir := t.TempDir()
	toys := []string{"toy-a", "toy-b"}
	for _, toy := range toys {
		writeFile(t, dir, "targets/"+toy+"/main.go", "package main\n")
	}
	writeFile(t, dir, "targets/leftover/testdata/rapid/TestX/a.fail", "")
	for target, want := range map[string]struct{ words, perToy []string }{
		"build":          {nil, []string{"./targets/%s"}},
		"generate":       {[]string{"object", "crd"}, []string{"paths=./targets/%s/...", "output:crd:artifacts:config=targets/%s/crds"}},
		"test-generated": {nil, []string{"./targets/%s"}},
		"bug-matrix":     {[]string{"-p 1", "-update"}, []string{"./targets/%s"}},
	} {
		out, err := runMake(dir, "-n", "-o", "setup", target)
		if err != nil {
			t.Fatalf("make -n %s: %v\n%s", target, err, out)
		}
		words := slices.Clone(want.words)
		for _, toy := range toys {
			for _, format := range want.perToy {
				words = append(words, fmt.Sprintf(format, toy))
			}
		}
		command := " " + strings.Join(strings.Fields(out), " ") + " "
		for _, word := range words {
			if !strings.Contains(command, " "+word+" ") {
				t.Errorf("make %s does not run %s:\n%s", target, word, out)
			}
		}
		if strings.Contains(out, "leftover") {
			t.Errorf("make %s takes targets/leftover, which has no main.go, for a toy:\n%s", target, out)
		}
	}
}

// TestVerifyGenerateChecksEveryToysGeneratedFiles runs verify-generate, without
// generate, in a new repository with a committed deepcopy file and an
// uncommitted main.go.
func TestVerifyGenerateChecksEveryToysGeneratedFiles(t *testing.T) {
	const committed = "targets/toy-a/api/v1/zz_generated.deepcopy.go"
	repository := func() string {
		dir := t.TempDir()
		writeFile(t, dir, committed, "package v1\n")
		git(t, dir, "init", "-q")
		git(t, dir, "add", ".")
		git(t, dir, "commit", "-qm", "Generate")
		writeFile(t, dir, "targets/toy-a/main.go", "package main\n")
		return dir
	}
	if out, err := runMake(repository(), "-o", "generate", "verify-generate"); err != nil {
		t.Fatalf("verify-generate fails on an uncommitted main.go: %v\n%s", err, out)
	}
	for _, stale := range []string{
		committed,
		"targets/toy-a/crds/example.com_things.yaml",
		"targets/toy-b/thirdparty/storage/v1/zz_generated.deepcopy.go",
	} {
		dir := repository()
		writeFile(t, dir, stale, "// changed\n")
		if out, err := runMake(dir, "-o", "generate", "verify-generate"); err == nil || !strings.Contains(out, stale) {
			t.Errorf("verify-generate gave %v and does not name the stale %s:\n%s", err, stale, out)
		}
	}
}

// TestEveryToyHasABugMatrixTest checks for the test and flag that make
// bug-matrix runs in each toy.
func TestEveryToyHasABugMatrixTest(t *testing.T) {
	mains, err := filepath.Glob("targets/*/main.go")
	if err != nil || len(mains) < 2 {
		t.Fatalf("found %v, %v; want the toys", mains, err)
	}
	for _, main := range mains {
		toy := filepath.Dir(main)
		source, _ := os.ReadFile(filepath.Join(toy, "bugmatrix_test.go"))
		if !isBugMatrixTest(string(source)) {
			t.Errorf("%s has no bugmatrix_test.go with TestBugMatrixIsCurrent and an update flag. make bug-matrix runs that test with -update.", toy)
		}
	}
}

func TestIsBugMatrixTestCatchesMistakes(t *testing.T) {
	const good = "var update = flag.Bool(\"update\", false, \"write it\")\n\nfunc TestBugMatrixIsCurrent(t *testing.T) {}\n"
	if !isBugMatrixTest(good) {
		t.Fatalf("a good bugmatrix_test.go fails: %q", good)
	}
	for name, mutated := range map[string]string{
		"no file": "",
		"no test": strings.Replace(good, "TestBugMatrixIsCurrent", "TestBugMatrix", 1),
		"no flag": strings.Replace(good, `"update"`, `"write"`, 1),
	} {
		if isBugMatrixTest(mutated) {
			t.Errorf("%s: it passes", name)
		}
	}
}

var (
	bugMatrixTest = regexp.MustCompile(`(?m)^func TestBugMatrixIsCurrent\(`)
	updateFlag    = regexp.MustCompile(`\bflag\.Bool\("update",`)
)

func isBugMatrixTest(source string) bool {
	return bugMatrixTest.MatchString(source) && updateFlag.MatchString(source)
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

// git runs git in dir as a made-up user who signs nothing.
func git(t *testing.T, dir string, args ...string) {
	t.Helper()
	config := []string{"-C", dir, "-c", "user.name=t", "-c", "user.email=t@example.com", "-c", "commit.gpgsign=false"}
	if out, err := exec.Command("git", append(config, args...)...).CombinedOutput(); err != nil {
		t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
	}
}

func writeFile(t *testing.T, dir, path, content string) {
	t.Helper()
	path = filepath.Join(dir, path)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}
