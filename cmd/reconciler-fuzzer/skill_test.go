package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/rosenhouse-bot-sandbox/reconciler-fuzzer/internal/generate"
	"github.com/rosenhouse-bot-sandbox/reconciler-fuzzer/internal/reference"
	"github.com/rosenhouse-bot-sandbox/reconciler-fuzzer/internal/target"
)

const skillPage = "../../skills/adopt-reconciler-fuzzer/SKILL.md"

var yamlBlock = regexp.MustCompile("(?s)```yaml\n(.*?)```")

var skillCommand = regexp.MustCompile(`(?m)reconciler-fuzzer (run|replay)( [^\n` + "`" + `]*)?$`)

// The adoption skill runs only flags reconciler-fuzzer takes.
func TestTheAdoptionSkillPassesOnlyFlagsReconcilerFuzzerTakes(t *testing.T) {
	commands := skillCommand.FindAllStringSubmatch(readFile(t, skillPage), -1)
	if len(commands) == 0 {
		t.Fatal("The skill runs no reconciler-fuzzer command, so this test checks nothing.")
	}
	for _, m := range commands {
		o := &options{command: m[1]}
		if err := o.flags().Parse(strings.Fields(m[2])); err != nil {
			t.Errorf("The skill runs %q: %v", m[0], err)
		}
	}
}

// The skill's target.yaml loads beside a kubebuilder project's files, and
// generation draws from it.
func TestTheAdoptionSkillsTargetLoads(t *testing.T) {
	var declared strings.Builder
	for _, block := range yamlBlock.FindAllStringSubmatch(readFile(t, skillPage), -1) {
		declared.WriteString(block[1])
	}
	project := t.TempDir()
	if err := os.CopyFS(project, os.DirFS("testdata/kubebuilder")); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(project, "test/reconciler-fuzzer/target.yaml")
	if err := os.WriteFile(path, []byte(declared.String()), 0o644); err != nil {
		t.Fatal(err)
	}
	loaded, err := target.Load(path)
	if err != nil {
		t.Fatalf("The skill's target.yaml does not load: %v\n%s", err, declared.String())
	}
	if _, err := generate.New(loaded, generate.Options{}); err != nil {
		t.Errorf("Generation refuses the skill's target.yaml: %v", err)
	}
}

var dottedKey = regexp.MustCompile("`([a-z]+[A-Za-z]*\\.[A-Za-z.]+)`")

// Each target.yaml key the skill's prose names is one reconciler-fuzzer takes.
func TestTheAdoptionSkillNamesOnlyKeysTheReferenceLists(t *testing.T) {
	documented := reference.Listed(t, "## target.yaml")
	named := 0
	for _, m := range dottedKey.FindAllStringSubmatch(readFile(t, skillPage), -1) {
		key := m[1]
		top, _, _ := strings.Cut(key, ".")
		if !slices.ContainsFunc(documented, func(d string) bool { return strings.HasPrefix(d, top+".") }) {
			continue
		}
		named++
		if !slices.Contains(documented, key) {
			t.Errorf("The skill names %s, which docs/reference.md does not list.", key)
		}
	}
	if named == 0 {
		t.Fatal("The skill names no dotted key, so this test checks nothing.")
	}
}

var versionSubstitution = regexp.MustCompile(`\$\(reconciler-fuzzer version[^)]*\)`)

// The skill and the README name the installed module's directory by the
// version that reconciler-fuzzer version prints.
func TestTheSkillAndTheReadmeReadTheVersionReconcilerFuzzerPrints(t *testing.T) {
	_, printed, _ := invoke(t, &fakeSession{}, "version")
	for _, page := range []string{skillPage, "../../README.md"} {
		substitutions := versionSubstitution.FindAllString(readFile(t, page), -1)
		if len(substitutions) == 0 {
			t.Errorf("%s reads no version, so this test checks nothing there.", page)
		}
		for _, substitution := range substitutions {
			bash := exec.Command("bash", "-c", `reconciler-fuzzer() { printf %s "$PRINTED"; }; printf %s "`+substitution+`"`)
			bash.Env = append(os.Environ(), "PRINTED="+printed)
			got, err := bash.Output()
			if err != nil {
				t.Fatalf("bash: %v", err)
			}
			if string(got) != version() {
				t.Errorf("%s reads the version as %s, which gives %q, want %q.", page, substitution, got, version())
			}
		}
	}
}
