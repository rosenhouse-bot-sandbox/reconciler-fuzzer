//go:build envtest

package reconcilerfuzzer_test

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"
)

// docs/ci.md's recipe pins reconciler-fuzzer in a module of its own. This runs
// it in a fresh operator module, on the oldest go that the README says fetches
// a newer one, and on the go before it.
func TestTheToolsModuleRecipe(t *testing.T) {
	install := section(t, readFile(t, "README.md"), "## Install")
	oldest := regexp.MustCompile(`from Go (1\.\d+) on`).FindStringSubmatch(install)
	remedy := regexp.MustCompile("run the commands with\\s+`GOTOOLCHAIN=(\\w+)`").FindStringSubmatch(install)
	if oldest == nil || remedy == nil {
		t.Fatalf("README.md's Install section names no oldest go, or no GOTOOLCHAIN to run the commands with:\n%s", install)
	}
	keep := section(t, readFile(t, ciPage), "## Keep reconciler-fuzzer out of your go.mod")
	stopped := regexp.MustCompile("Under\\s+`GOTOOLCHAIN=local`, a\\s+`go`\\s+before\\s+[\\d.]+\\s+stops\\s+this\\s+recipe\\s+with\\s+`([^`]+)`").FindStringSubmatch(keep)
	if stopped == nil {
		t.Fatalf("%s's tools module section names no error that stops the recipe under GOTOOLCHAIN=local:\n%s", ciPage, keep)
	}
	checkout, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	script := readFile(t, toolsRecipe)
	toolsDir, recipe := toolsModuleDir(t, script), replacingReconcilerFuzzer(t, script, checkout)
	oldGo := oldest[1] + ".0"
	goroot := gorootOf(t, "go"+oldGo)
	operatorGoMod := "module example.com/operator\n\ngo " + oldGo + "\n"

	newOperator := func(t *testing.T) string {
		operator := t.TempDir()
		writeFile(t, filepath.Join(operator, "go.mod"), operatorGoMod)
		writeFile(t, filepath.Join(operator, "go.sum"), "")
		return operator
	}
	run := func(operator, goroot, gotoolchain string) (env []string, output string, err error) {
		cmd := exec.Command("sh", "-e", "-c", recipe)
		cmd.Dir = operator
		cmd.Env = goEnv(goroot, gotoolchain)
		out, err := cmd.CombinedOutput()
		return cmd.Env, string(out), err
	}

	t.Run("builds bin/reconciler-fuzzer and leaves the operator's module and workspace alone", func(t *testing.T) {
		operator := newOperator(t)
		// The operator pins its own tools in tools/tools.go, a common place for them.
		writeFile(t, filepath.Join(operator, "tools", "tools.go"), "//go:build tools\n\npackage tools\n")
		goWork := "go " + oldGo + "\n\nuse .\n"
		writeFile(t, filepath.Join(operator, "go.work"), goWork)
		env, out, err := run(operator, goroot, remedy[1])
		if err != nil {
			t.Fatalf("The recipe returned %v:\n%s", err, out)
		}
		for name, before := range map[string]string{"go.mod": operatorGoMod, "go.sum": "", "go.work": goWork} {
			if after := readFile(t, filepath.Join(operator, name)); after != before {
				t.Errorf("The recipe changed the operator's %s to:\n%s", name, after)
			}
		}
		list := exec.Command("go", "list", "-tags", "tools", "./tools")
		list.Dir, list.Env = operator, env
		if out, err := list.CombinedOutput(); err != nil || string(out) != "example.com/operator/tools\n" {
			t.Errorf("The recipe took the operator's tools package out of its module: %v\n%s", err, out)
		}
		version, err := exec.Command(filepath.Join(operator, "bin", "reconciler-fuzzer"), "version").CombinedOutput()
		if err != nil {
			t.Errorf("The recipe built no bin/reconciler-fuzzer: %v\n%s", err, version)
		} else if !slices.Contains(strings.Split(out, "\n"), strings.TrimSuffix(string(version), "\n")) {
			t.Errorf("The recipe does not run bin/reconciler-fuzzer, which prints %q:\n%s", version, out)
		}
		tool := exec.Command("go", "-C", toolsDir, "tool", "reconciler-fuzzer", "version")
		tool.Dir, tool.Env = operator, append(env, "GOWORK=off")
		if out, err := tool.CombinedOutput(); err != nil {
			t.Errorf("%s/go.mod does not pin reconciler-fuzzer as a tool: %v\n%s", toolsDir, err, out)
		}
	})

	t.Run("fails under GOTOOLCHAIN=local as docs/ci.md says", func(t *testing.T) {
		_, out, err := run(newOperator(t), goroot, "local")
		if err == nil || !strings.Contains(out, stopped[1]) {
			t.Errorf("The recipe returned %v, and must fail with %q:\n%s", err, stopped[1], out)
		}
	})

	t.Run("fails on the go before the oldest that README.md names", func(t *testing.T) {
		minor, err := strconv.Atoi(strings.TrimPrefix(oldest[1], "1."))
		if err != nil {
			t.Fatal(err)
		}
		before := fmt.Sprintf("go1.%d", minor-1)
		if minor-1 >= 21 {
			before += ".0" // From Go 1.21 on, a minor's first release ends in .0.
		}
		_, out, err := run(newOperator(t), gorootOf(t, before), remedy[1])
		if err == nil {
			t.Errorf("The recipe ran on %s, which README.md must then name as the oldest go:\n%s", before, out)
		}
	})
}

// gorootOf finds a toolchain, such as go1.21.0, which go downloads.
func gorootOf(t *testing.T, toolchain string) string {
	t.Helper()
	lookup := exec.Command("go", "env", "GOROOT")
	lookup.Env = goEnv("", toolchain)
	goroot, err := lookup.Output()
	if err != nil {
		t.Fatalf("Finding %s failed: %v", toolchain, err)
	}
	return strings.TrimSpace(string(goroot))
}

// replacingReconcilerFuzzer has the tools module build this checkout's
// reconciler-fuzzer.
func replacingReconcilerFuzzer(t *testing.T, recipe, checkout string) string {
	t.Helper()
	modInit := regexp.MustCompile(`(?m)^go mod init .*\n`)
	if n := len(modInit.FindAllString(recipe, -1)); n != 1 {
		t.Fatalf("%s runs go mod init %d times, not once.", toolsRecipe, n)
	}
	return modInit.ReplaceAllStringFunc(recipe, func(line string) string {
		return line + "go mod edit -replace=github.com/rosenhouse-bot-sandbox/reconciler-fuzzer=" + strconv.Quote(checkout) + "\n"
	})
}

// goEnv puts goroot's go first on PATH, when goroot is set, and sets GOTOOLCHAIN.
func goEnv(goroot, gotoolchain string) []string {
	env := slices.DeleteFunc(os.Environ(), func(variable string) bool {
		return strings.HasPrefix(variable, "GOROOT=") || strings.HasPrefix(variable, "GOTOOLCHAIN=")
	})
	if goroot != "" {
		env = append(env, "PATH="+filepath.Join(goroot, "bin")+string(os.PathListSeparator)+os.Getenv("PATH"))
	}
	return append(env, "GOTOOLCHAIN="+gotoolchain)
}
