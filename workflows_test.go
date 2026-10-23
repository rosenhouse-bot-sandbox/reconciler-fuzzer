package reconcilerfuzzer_test

import (
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"
)

func TestWorkflowsCallOnlyMakeTargetsThatExist(t *testing.T) {
	files, err := filepath.Glob(".github/workflows/*.yml")
	if err != nil || len(files) == 0 {
		t.Fatalf("found no workflows: %v", err)
	}
	for _, file := range files {
		workflow, err := os.ReadFile(file)
		if err != nil {
			t.Fatal(err)
		}
		for _, problem := range workflowProblems(string(workflow), makeTargets(t)) {
			t.Errorf("%s: %s", file, problem)
		}
		for _, example := range matrixExamples(string(workflow)) {
			if _, err := os.Stat(filepath.Join("examples", example, "config.yaml")); err != nil {
				t.Errorf("%s: the matrix names %s: %v", file, example, err)
			}
		}
	}
}

func TestWorkflowProblemsCatchesMistakes(t *testing.T) {
	targets := []string{"test", "hunt-a", "hunt-b", "a-version", "b-version"}
	const calls = "# make exits 2 on failure.\n- run: make test\n- run: make hunt-${{ matrix.example }}\n" +
		"  run: echo \"$(make --no-print-directory ${{ matrix.example }}-version)\"\n" +
		"  body: `make hunt-$EXAMPLE` exited\n"
	const listMatrix = "      matrix:\n        example: [a, b]\n"
	const includeMatrix = "      matrix:\n        include:\n          - example: a\n            binary: x\n          - example: b\n"
	for _, matrix := range []string{listMatrix, includeMatrix} {
		if problems := workflowProblems(matrix+calls, targets); len(problems) > 0 {
			t.Fatalf("a good workflow gave %q", problems)
		}
	}
	for name, c := range map[string]struct {
		workflow string
		targets  []string
	}{
		"missing target":      {listMatrix + calls, slices.Delete(slices.Clone(targets), 0, 1)},
		"list matrix typo":    {strings.Replace(listMatrix, "b]", "c]", 1) + calls, targets},
		"include matrix typo": {strings.Replace(includeMatrix, "example: b", "example: c", 1) + calls, targets},
		"no matrix to expand": {calls, targets},
		"no make call":        {"- run: echo", targets},
		"missing target after flag": {
			listMatrix + calls,
			slices.DeleteFunc(slices.Clone(targets), func(s string) bool { return s == "b-version" }),
		},
	} {
		if problems := workflowProblems(c.workflow, c.targets); len(problems) == 0 {
			t.Errorf("%s: no problem found", name)
		}
	}
	if got := matrixExamples(includeMatrix); !slices.Equal(got, []string{"a", "b"}) {
		t.Errorf("matrixExamples = %q, want a and b.", got)
	}
}

var (
	exampleVar   = `(?:\$\{\{ matrix\.example \}\}|\$EXAMPLE)`
	workflowMake = regexp.MustCompile(`\bmake(?:\s+-\S+)*\s+((?:[a-z]|` + exampleVar + `)(?:[\w-]|` + exampleVar + `)*)`)
	exampleRef   = regexp.MustCompile(exampleVar)
	yamlComment  = regexp.MustCompile(`(?m)^\s*#.*$`)
	exampleList  = regexp.MustCompile(`(?m)^\s*example: \[(.*)\]$`)
	exampleItem  = regexp.MustCompile(`(?m)^\s*- example: (\S+)$`)
)

// matrixExamples lists the values of the workflow's matrix.example.
func matrixExamples(workflow string) []string {
	var names []string
	for _, m := range exampleList.FindAllStringSubmatch(workflow, -1) {
		for _, name := range strings.Split(m[1], ",") {
			names = append(names, strings.TrimSpace(name))
		}
	}
	for _, m := range exampleItem.FindAllStringSubmatch(workflow, -1) {
		names = append(names, m[1])
	}
	return names
}

// workflowProblems lists each make target the workflow calls that is not in
// targets, expanding matrix.example and $EXAMPLE to each value of the
// workflow's matrix.
func workflowProblems(workflow string, targets []string) []string {
	var problems []string
	calls := workflowMake.FindAllStringSubmatch(yamlComment.ReplaceAllString(workflow, ""), -1)
	if len(calls) == 0 {
		return []string{"it calls no make target"}
	}
	examples := matrixExamples(workflow)
	for _, call := range calls {
		names := []string{call[1]}
		if exampleRef.MatchString(call[1]) {
			if len(examples) == 0 {
				problems = append(problems, "it expands matrix.example in "+call[1]+" without a matrix")
			}
			names = nil
			for _, example := range examples {
				names = append(names, exampleRef.ReplaceAllLiteralString(call[1], example))
			}
		}
		for _, name := range names {
			if !slices.Contains(targets, name) {
				problems = append(problems, "the Makefile has no target "+name)
			}
		}
	}
	return problems
}

// TestTheGeneratedTierRunsEveryGeneratedTest checks that GENERATED selects
// each generated test. TestMakeTargetsCoverEveryToy checks that
// test-generated runs each toy.
func TestTheGeneratedTierRunsEveryGeneratedTest(t *testing.T) {
	makefile, err := os.ReadFile("Makefile")
	if err != nil {
		t.Fatal(err)
	}
	paths, err := filepath.Glob("targets/*/generated_envtest_test.go")
	if err != nil || len(paths) < 2 {
		t.Fatalf("found %v, %v; want the toys' generated tests", paths, err)
	}
	tests := map[string]string{}
	for _, path := range paths {
		source, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		tests[filepath.Dir(path)] = string(source)
	}
	for _, problem := range generatedTierProblems(string(makefile), tests) {
		t.Error(problem)
	}
}

func TestGeneratedTierProblemsCatchesMistakes(t *testing.T) {
	const makefile = "GENERATED = TestA|TestB\n"
	tests := map[string]string{"targets/x": "func TestA(t *testing.T) {}\n", "targets/y": "func TestB(t *testing.T) {}\nfunc helper() {}\n"}
	if problems := generatedTierProblems(makefile, tests); len(problems) > 0 {
		t.Fatalf("a good Makefile gave %q", problems)
	}
	for name, mutated := range map[string]string{
		"test left out of GENERATED": strings.Replace(makefile, "|TestB", "", 1),
		"no GENERATED":               strings.Replace(makefile, "GENERATED =", "OTHER =", 1),
	} {
		if problems := generatedTierProblems(mutated, tests); len(problems) == 0 {
			t.Errorf("%s: no problem found", name)
		}
	}
}

var (
	generatedVar = regexp.MustCompile(`(?m)^GENERATED = (.+)$`)
	testFunc     = regexp.MustCompile(`(?m)^func (Test\w+)\(`)
)

// generatedTierProblems lists each test of tests, keyed by package
// directory, that the Makefile's GENERATED pattern does not select.
func generatedTierProblems(makefile string, tests map[string]string) []string {
	variable := generatedVar.FindStringSubmatch(makefile)
	if variable == nil {
		return []string{"the Makefile has no GENERATED variable"}
	}
	selected, err := regexp.Compile(variable[1])
	if err != nil {
		return []string{err.Error()}
	}
	var problems []string
	for dir, source := range tests {
		for _, test := range testFunc.FindAllStringSubmatch(source, -1) {
			if !selected.MatchString(test[1]) {
				problems = append(problems, "GENERATED does not select "+dir+"."+test[1])
			}
		}
	}
	return problems
}
