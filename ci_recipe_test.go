package reconcilerfuzzer_test

import (
	"fmt"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	yaml "go.yaml.in/yaml/v3"
)

const ciRecipe = "examples/ci/github-actions.yml"

// workflow holds what these tests read of a GitHub Actions workflow. yaml.v3
// reads the on key as a string, as Actions does. A YAML 1.1 parser reads it as
// true.
type workflow struct {
	On          any               `yaml:"on"`
	Env         map[string]string `yaml:"env"`
	Permissions any               `yaml:"permissions"`
	Defaults    any               `yaml:"defaults"`
	Jobs        map[string]job    `yaml:"jobs"`
}

type job struct {
	If              string            `yaml:"if"`
	RunsOn          any               `yaml:"runs-on"`
	Env             map[string]string `yaml:"env"`
	Defaults        any               `yaml:"defaults"`
	ContinueOnError any               `yaml:"continue-on-error"`
	TimeoutMinutes  any               `yaml:"timeout-minutes"`
	Strategy        struct {
		Matrix map[string]any `yaml:"matrix"`
	} `yaml:"strategy"`
	Steps []step `yaml:"steps"`
}

// expandMatrix returns run once for each combination of the job's matrix
// values, with each ${{ matrix.<key> }} replaced.
func (j job) expandMatrix(run string) []string {
	runs := []string{run}
	for key, values := range j.Strategy.Matrix {
		list, ok := values.([]any)
		if !ok {
			continue
		}
		var expanded []string
		for _, r := range runs {
			for _, value := range list {
				expanded = append(expanded, strings.ReplaceAll(r, "${{ matrix."+key+" }}", fmt.Sprint(value)))
			}
		}
		runs = expanded
	}
	return runs
}

type step struct {
	ID              string            `yaml:"id"`
	If              string            `yaml:"if"`
	ContinueOnError any               `yaml:"continue-on-error"`
	TimeoutMinutes  any               `yaml:"timeout-minutes"`
	Uses            string            `yaml:"uses"`
	With            map[string]string `yaml:"with"`
	Env             map[string]string `yaml:"env"`
	Shell           string            `yaml:"shell"`
	WorkingDir      string            `yaml:"working-directory"`
	Run             string            `yaml:"run"`
}

func readWorkflow(t *testing.T, path string) workflow {
	t.Helper()
	w, err := parseWorkflow(readFile(t, path))
	if err != nil {
		t.Fatalf("parsing %s: %v", path, err)
	}
	return w
}

func parseWorkflow(content string) (workflow, error) {
	var w workflow
	err := yaml.Unmarshal([]byte(content), &w)
	return w, err
}

func TestParseWorkflowReadsEachFormOfOnPermissionsAndRunsOn(t *testing.T) {
	for _, content := range []string{
		"on: push\npermissions: read-all\njobs: {a: {runs-on: ubuntu-latest}}\n",
		"on: [push]\njobs: {a: {runs-on: [self-hosted, linux]}}\n",
		"on: {push: {}}\npermissions: {contents: read}\njobs: {a: {runs-on: {group: large}}}\n",
	} {
		if _, err := parseWorkflow(content); err != nil {
			t.Errorf("parsing %q: %v", content, err)
		}
	}
}

func recipeJob(t *testing.T) job {
	t.Helper()
	w := readWorkflow(t, ciRecipe)
	if len(w.Jobs) != 1 {
		t.Fatalf("%s has %d jobs, and an adopter copies one", ciRecipe, len(w.Jobs))
	}
	for _, j := range w.Jobs {
		return j
	}
	return job{}
}

func recipeSteps(t *testing.T) []step {
	t.Helper()
	return recipeJob(t).Steps
}

// stepUsing returns the one step that uses the action, at any version.
func stepUsing(t *testing.T, steps []step, action string) step {
	t.Helper()
	var found []step
	for _, s := range steps {
		if strings.HasPrefix(s.Uses, action+"@") {
			found = append(found, s)
		}
	}
	if len(found) != 1 {
		t.Fatalf("%d steps use %s, not one", len(found), action)
	}
	return found[0]
}

func readFile(t *testing.T, path string) string {
	t.Helper()
	content, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(content)
}

func TestTheCIRecipeAndTheREADMEInstallTheMakefilesPins(t *testing.T) {
	pins := makefilePins(t)
	for _, drift := range pinDrift(runText(recipeSteps(t)), readWorkflow(t, ciRecipe).Env, pins) {
		t.Errorf("%s: %s", ciRecipe, drift)
	}

	install := section(t, readFile(t, "README.md"), "## Install")
	assignments := map[string]string{}
	for _, m := range regexp.MustCompile(`(?m)^(\w+)=(\S+)$`).FindAllStringSubmatch(install, -1) {
		assignments[m[1]] = m[2]
	}
	for _, drift := range pinDrift(install, assignments, pins) {
		t.Errorf("README.md, Install: %s", drift)
	}
}

// pinDrift says where commands name a pin other than the Makefile's, reading a
// $name or ${name} through vars. It also says which pins they never name.
func pinDrift(commands string, vars, pins map[string]string) []string {
	var drift []string
	for _, pin := range []struct{ name, pattern string }{
		{"SETUP_ENVTEST_VERSION", `setup-envtest@(\S+)`},
		{"ENVTEST_K8S_VERSION", `setup-envtest use (\S+)`},
		{"ENVTEST_INDEX_URL", `--index (\S+)`},
	} {
		matches := regexp.MustCompile(pin.pattern).FindAllStringSubmatch(commands, -1)
		if len(matches) == 0 {
			drift = append(drift, "no command names "+pin.name)
		}
		for _, m := range matches {
			value := os.Expand(strings.Trim(m[1], `"`), func(name string) string { return vars[name] })
			if value != pins[pin.name] {
				drift = append(drift, fmt.Sprintf("%q names %q, and the Makefile's %s is %q", m[0], value, pin.name, pins[pin.name]))
			}
		}
	}
	return drift
}

func TestPinDriftReadsBracedAndQuotedVariables(t *testing.T) {
	pins := map[string]string{"SETUP_ENVTEST_VERSION": "v1", "ENVTEST_K8S_VERSION": "1.0.0", "ENVTEST_INDEX_URL": "https://index"}
	commands := `go install "example.com/setup-envtest@${SETUP_ENVTEST_VERSION}"
setup-envtest use "${ENVTEST_K8S_VERSION}" --index "$ENVTEST_INDEX_URL"`
	if drift := pinDrift(commands, pins, pins); drift != nil {
		t.Errorf("pinDrift found %q", drift)
	}
}

// makefilePins are the Makefile's variables, each $(NAME) in them expanded.
func makefilePins(t *testing.T) map[string]string {
	t.Helper()
	pins := map[string]string{}
	for _, m := range regexp.MustCompile(`(?m)^(\w+) \?= (.*)$`).FindAllStringSubmatch(readFile(t, "Makefile"), -1) {
		pins[m[1]] = regexp.MustCompile(`\$\((\w+)\)`).ReplaceAllStringFunc(m[2], func(ref string) string {
			return pins[ref[2:len(ref)-1]]
		})
	}
	return pins
}

// section is the text under a Markdown heading, up to the next heading.
func section(t *testing.T, doc, heading string) string {
	t.Helper()
	_, after, found := strings.Cut(doc, "\n"+heading+"\n")
	if !found {
		t.Fatalf("no %q heading", heading)
	}
	var text strings.Builder
	fenced := false
	for _, line := range strings.SplitAfter(after, "\n") {
		if strings.HasPrefix(line, "```") {
			fenced = !fenced
		}
		if !fenced && strings.HasPrefix(line, "#") {
			break
		}
		text.WriteString(line)
	}
	return text.String()
}

func TestSectionEndsAtTheNextHeadingOutsideAFence(t *testing.T) {
	doc := "# Doc\n\n## Install\n\n```sh\n# a comment\ncommand\n```\n\n### Against kind\n\ntext\n"
	if got, want := section(t, doc, "## Install"), "\n```sh\n# a comment\ncommand\n```\n\n"; got != want {
		t.Errorf("section returned %q, not %q", got, want)
	}
}

func TestTheCIRecipeCachesWhatItInstalls(t *testing.T) {
	steps := recipeSteps(t)
	restore := stepUsing(t, steps, "actions/cache/restore")
	save := stepUsing(t, steps, "actions/cache/save")
	commands := runText(steps)

	binDir := regexp.MustCompile(`setup-envtest use .*--bin-dir (\S+)`).FindStringSubmatch(commands)
	if binDir == nil {
		t.Fatal("setup-envtest installs the control plane with no --bin-dir, into a directory outside the workspace")
	}
	cached := strings.Fields(restore.With["path"])
	for _, dir := range []string{"~/go/bin", binDir[1]} {
		if !slices.Contains(cached, dir) {
			t.Errorf("the cache holds %q and leaves out %s, where the recipe installs", cached, dir)
		}
	}
	if save.With["path"] != restore.With["path"] {
		t.Errorf("the cache saves %q and restores %q", save.With["path"], restore.With["path"])
	}
	if prefixes, ok := restore.With["restore-keys"]; ok {
		t.Errorf("the cache also restores keys %q, which carry old control planes into each new cache", prefixes)
	}

	want := []string{"runner.arch", "runner.os"}
	for _, m := range regexp.MustCompile(`\$\{?(\w+_VERSION)\b`).FindAllStringSubmatch(commands, -1) {
		want = append(want, "env."+m[1])
	}
	var keyed []string
	for _, m := range regexp.MustCompile(`\$\{\{\s*(.*?)\s*\}\}`).FindAllStringSubmatch(restore.With["key"], -1) {
		keyed = append(keyed, m[1])
	}
	slices.Sort(want)
	slices.Sort(keyed)
	if want = slices.Compact(want); !slices.Equal(keyed, want) {
		t.Errorf("the cache key %q reads %q, not %q. A pin it leaves out restores a stale cache, and anything else can miss on every run", restore.With["key"], keyed, want)
	}

	missed := "steps." + restore.ID + ".outputs.cache-hit != 'true'"
	if save.If != missed {
		t.Errorf("the cache saves if %q, not only on a miss (%s)", save.If, missed)
	}
	if want := "${{ steps." + restore.ID + ".outputs.cache-primary-key }}"; save.With["key"] != want {
		t.Errorf("the cache saves under %q, not %q", save.With["key"], want)
	}
	saved := slices.IndexFunc(steps, func(s step) bool { return s.Uses == save.Uses })
	for i, s := range steps {
		switch {
		case strings.Contains(s.Run, "go install") && s.If != missed:
			t.Errorf("step %d builds with go install on a cache hit too: %q", i, s.Run)
		case strings.Contains(s.Run, "reconciler-fuzzer run") && i < saved:
			t.Errorf("step %d runs reconciler-fuzzer before the cache is saved, so a find would leave it unsaved: %q", i, s.Run)
		}
	}
}

// The walk takes every step to pass, and so runs no step if: failure().
func TestTheCIRecipeGivesEachStepWhatItNeedsOnACacheHitAndAMiss(t *testing.T) {
	cached := []string{"reconciler-fuzzer", "setup-envtest", "the control plane"}
	effects := []struct {
		usesOrRuns   *regexp.Regexp
		needs, gives []string
	}{
		{regexp.MustCompile(`^actions/setup-go@`), nil, []string{"go"}},
		{regexp.MustCompile(`(?m)^\s*go `), []string{"go"}, nil},
		{regexp.MustCompile(`/reconciler-fuzzer@`), nil, []string{"reconciler-fuzzer"}},
		{regexp.MustCompile(`/setup-envtest@`), nil, []string{"setup-envtest"}},
		{regexp.MustCompile(`setup-envtest use`), []string{"setup-envtest"}, []string{"the control plane"}},
		{regexp.MustCompile(`setup-envtest use.*\s(-i|--installed-only)\b`), []string{"the control plane"}, nil},
		{regexp.MustCompile(`KUBEBUILDER_ASSETS=`), nil, []string{"KUBEBUILDER_ASSETS"}},
		{regexp.MustCompile(`^actions/cache/save@`), cached, nil},
		{regexp.MustCompile(`go build -o`), nil, []string{"the controller"}},
		{regexp.MustCompile(`reconciler-fuzzer run`), []string{"reconciler-fuzzer", "KUBEBUILDER_ASSETS", "the controller"}, []string{"a reconciler-fuzzer run"}},
	}
	recipe := recipeJob(t)
	restore := stepUsing(t, recipe.Steps, "actions/cache/restore")
	for _, event := range slices.Sorted(maps.Keys(recipeEvents(t))) {
		for _, hit := range []bool{true, false} {
			t.Run(fmt.Sprintf("%s with cache-hit %t", event, hit), func(t *testing.T) {
				// A step's outputs are empty until it runs.
				restored := false
				runs := func(condition string) bool {
					if on := regexp.MustCompile(`^github\.event_name == '(\w+)'$`).FindStringSubmatch(condition); on != nil {
						return on[1] == event
					}
					switch condition {
					case "", "always()":
						return true
					case "failure()":
						return false
					case "steps." + restore.ID + ".outputs.cache-hit != 'true'":
						return !hit || !restored
					}
					t.Fatalf("the walk cannot evaluate if: %s", condition)
					return false
				}
				has := map[string]bool{}
				for i, s := range recipe.Steps {
					if !runs(recipe.If) || !runs(s.If) {
						continue
					}
					var gives []string
					if s.Uses == restore.Uses {
						restored = true
						if hit {
							gives = append(gives, cached...)
						}
					}
					if hit && strings.Contains(s.Run, "go install") {
						t.Errorf("step %d builds with go install on a cache hit", i)
					}
					for _, effect := range effects {
						if !effect.usesOrRuns.MatchString(s.Uses + "\n" + s.Run) {
							continue
						}
						for _, need := range effect.needs {
							if !has[need] {
								t.Errorf("step %d needs %s, and no step before it gives it", i, need)
							}
						}
						gives = append(gives, effect.gives...)
					}
					for _, g := range gives {
						has[g] = true
					}
				}
				if !has["a reconciler-fuzzer run"] {
					t.Error("no step runs reconciler-fuzzer")
				}
			})
		}
	}
}

func TestTheCIRecipeInstallsEachToolItRunsAtAPin(t *testing.T) {
	module := regexp.MustCompile(`(?m)^module (\S+)$`).FindStringSubmatch(readFile(t, "go.mod"))
	setupEnvtest := regexp.MustCompile(`go install (\S+/setup-envtest)@\$\(SETUP_ENVTEST_VERSION\)`).FindStringSubmatch(readFile(t, "Makefile"))
	if module == nil || setupEnvtest == nil {
		t.Fatal("go.mod names no module, or the Makefile does not go install setup-envtest")
	}
	version := readWorkflow(t, ciRecipe).Env["RECONCILER_FUZZER_VERSION"]
	if !regexp.MustCompile(`^([0-9a-f]{7,40}|<commit>)$`).MatchString(version) {
		t.Errorf("RECONCILER_FUZZER_VERSION is %q, not a commit, so a cache hit restores whichever reconciler-fuzzer the first miss built", version)
	}
	want := []string{
		"install " + module[1] + "/cmd/reconciler-fuzzer@" + version,
		"install " + setupEnvtest[1] + "@" + makefilePins(t)["SETUP_ENVTEST_VERSION"],
	}
	var got []string
	for _, s := range recipeSteps(t) {
		if !strings.Contains(s.Run, "go install") {
			continue
		}
		ran := runStep(t, s, t.TempDir(), map[string]string{"go": `echo "$*${GOBIN:+ into $GOBIN}${GOPATH:+ under $GOPATH}"`})
		if ran.err != nil {
			t.Fatalf("%q: %v\n%s", s.Run, ran.err, ran.output)
		}
		got = append(got, strings.Split(strings.TrimSpace(string(ran.output)), "\n")...)
	}
	slices.Sort(got)
	slices.Sort(want)
	if !slices.Equal(got, want) {
		t.Errorf("the recipe runs go %q, not %q, which puts each tool in ~/go/bin at its pin", got, want)
	}
}

func TestTheCIRecipeExportsTheControlPlaneOrFails(t *testing.T) {
	steps := recipeSteps(t)
	i := slices.IndexFunc(steps, func(s step) bool { return strings.Contains(s.Run, "KUBEBUILDER_ASSETS=") })
	if i < 0 {
		t.Fatal("no step exports KUBEBUILDER_ASSETS")
	}
	for _, test := range []struct {
		name, setupEnvtest, wantEnv string
		wantErr                     bool
	}{
		{
			name:         "setup-envtest prints the path",
			setupEnvtest: `case " $* " in *" -p path "*) echo /assets ;; *) echo "Path: /assets" ;; esac`,
			wantEnv:      "KUBEBUILDER_ASSETS=/assets\n",
		},
		{name: "setup-envtest fails", setupEnvtest: "exit 1", wantErr: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			ran := runStep(t, steps[i], t.TempDir(), map[string]string{"setup-envtest": test.setupEnvtest})
			if (ran.err != nil) != test.wantErr {
				t.Fatalf("the step returned %v, and wanted an error: %t\n%s", ran.err, test.wantErr, ran.output)
			}
			if ran.env != test.wantEnv {
				t.Errorf("the step exported %q, not %q", ran.env, test.wantEnv)
			}
		})
	}
}

// ranStep is what a recipe step's script did.
type ranStep struct {
	// env and summary are what it wrote to GITHUB_ENV and GITHUB_STEP_SUMMARY.
	env, summary string
	output       []byte
	err          error
}

// runStep runs a recipe step's script in workspace under the recipe's env
// alone, with a stub for each command stubs names.
func runStep(t *testing.T, s step, workspace string, stubs map[string]string) ranStep {
	t.Helper()
	dir := t.TempDir()
	for command, stub := range stubs {
		if err := os.WriteFile(filepath.Join(dir, command), []byte("#!/bin/sh\n"+stub+"\n"), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	script := filepath.Join(dir, "step.sh")
	if err := os.WriteFile(script, []byte(s.Run), 0o644); err != nil {
		t.Fatal(err)
	}
	githubEnv, stepSummary := filepath.Join(dir, "github-env"), filepath.Join(dir, "step-summary")
	// Actions runs a step with no shell key as bash -e on a file that holds it.
	cmd := exec.Command("bash", "-e", script)
	cmd.Dir = workspace
	cmd.Env = []string{
		"PATH=" + dir + string(os.PathListSeparator) + os.Getenv("PATH"),
		"GITHUB_ENV=" + githubEnv,
		"GITHUB_STEP_SUMMARY=" + stepSummary,
	}
	for _, env := range []map[string]string{readWorkflow(t, ciRecipe).Env, recipeJob(t).Env, s.Env} {
		for name, value := range env {
			cmd.Env = append(cmd.Env, name+"="+value)
		}
	}
	var ran ranStep
	ran.output, ran.err = cmd.CombinedOutput()
	env, _ := os.ReadFile(githubEnv)
	summary, _ := os.ReadFile(stepSummary)
	ran.env, ran.summary = string(env), string(summary)
	return ran
}

func TestTheCIRecipeFailsWhenReconcilerFuzzerFinds(t *testing.T) {
	recipe := recipeJob(t)
	if recipe.ContinueOnError != nil || slices.ContainsFunc(recipe.Steps, func(s step) bool { return s.ContinueOnError != nil }) {
		t.Error("the recipe sets continue-on-error, which can pass a find and skip the upload")
	}
	for _, s := range reconcilerFuzzerRuns(t, recipe.Steps) {
		if ran := runStep(t, s, t.TempDir(), map[string]string{"reconciler-fuzzer": "exit 1"}); ran.err == nil {
			t.Errorf("%q passes when reconciler-fuzzer fails\n%s", s.Run, ran.output)
		}
	}
}

// Actions cancels a job by signalling the step's shell, which passes no signal
// on.
func TestTheCIRecipeRunsReconcilerFuzzerInPlaceOfTheStepsShell(t *testing.T) {
	for _, s := range reconcilerFuzzerRuns(t, recipeSteps(t)) {
		ran := runStep(t, s, t.TempDir(), map[string]string{"reconciler-fuzzer": `echo "$PPID"`})
		if parent := strings.TrimSpace(string(ran.output)); ran.err != nil || parent != strconv.Itoa(os.Getpid()) {
			t.Errorf("%q runs reconciler-fuzzer as a child of the step's shell, so a cancel never reaches it\n%s", s.Run, ran.output)
		}
	}
}

func TestTheCIRecipeRunsEachStepInBashFromTheWorkspaceRoot(t *testing.T) {
	recipe := recipeJob(t)
	if readWorkflow(t, ciRecipe).Defaults != nil || recipe.Defaults != nil {
		t.Error("the recipe sets defaults, which can change the shell or the directory of every step")
	}
	for i, s := range recipe.Steps {
		if s.Shell != "" {
			t.Errorf("step %d runs under shell %q, and these tests run it as bash -e, as Actions runs a step with no shell key", i, s.Shell)
		}
		if s.WorkingDir != "" {
			t.Errorf("step %d runs in %s, and the upload and the replay command in report.md read paths from the workspace root", i, s.WorkingDir)
		}
	}
}

// reconcilerFuzzerRuns are the steps that run reconciler-fuzzer.
func reconcilerFuzzerRuns(t *testing.T, steps []step) []step {
	t.Helper()
	var runs []step
	for _, s := range steps {
		if strings.Contains(s.Run, "reconciler-fuzzer run") {
			runs = append(runs, s)
		}
	}
	if len(runs) == 0 {
		t.Fatal("no step runs reconciler-fuzzer")
	}
	return runs
}

func TestTheCIRecipeKeepsAFailingRunsEvidence(t *testing.T) {
	recipe := recipeJob(t)
	steps := recipe.Steps
	upload := stepUsing(t, steps, "actions/upload-artifact")
	if upload.If != "always()" {
		t.Errorf("the job uploads its output if %q, not however reconciler-fuzzer ends, and only the summary keeps the sequences a passing nightly drew", upload.If)
	}
	if upload.With["if-no-files-found"] == "error" {
		t.Error("the upload adds an error to a job that failed before reconciler-fuzzer wrote anything")
	}
	uploaded := strings.TrimSuffix(upload.With["path"], "/")
	if recipe.TimeoutMinutes != nil {
		t.Errorf("the job's timeout-minutes %v also counts the steps before reconciler-fuzzer, so it can stop reconciler-fuzzer before its --deadline makes it write a report", recipe.TimeoutMinutes)
	}
	for _, s := range reconcilerFuzzerRuns(t, steps) {
		if out := regexp.MustCompile(`--out (\S+)`).FindStringSubmatch(s.Run); out == nil || out[1] != uploaded {
			t.Errorf("%q writes elsewhere than %s, which the job uploads", s.Run, uploaded)
		}
		if s.TimeoutMinutes == nil {
			continue
		}
		_, deadline := reconcilerFuzzerBudget(t, s)
		minutes, err := strconv.ParseFloat(fmt.Sprint(s.TimeoutMinutes), 64)
		if err != nil || time.Duration(minutes*float64(time.Minute)) <= deadline {
			t.Errorf("timeout-minutes %v can stop %q before its --deadline makes it write a report", s.TimeoutMinutes, s.Run)
		}
	}
	last := 0
	for i, s := range steps {
		if strings.Contains(s.Run, "reconciler-fuzzer run") {
			last = i
		}
	}
	for i, s := range steps {
		reads := strings.Contains(s.Run, uploaded+"/") || strings.TrimSuffix(s.With["path"], "/") == uploaded
		if reads && i < last {
			t.Errorf("step %d reads %s before step %d runs reconciler-fuzzer", i, uploaded, last)
		}
	}

	hint := downloadHint.FindStringSubmatch(readFile(t, ciRecipe))
	if hint == nil {
		t.Fatal("the recipe does not say how to download the evidence")
	}
	if hint[2] != upload.With["name"] {
		t.Errorf("%q names an artifact other than %s", hint[0], upload.With["name"])
	}
	if hint[3] != uploaded {
		t.Errorf("%q puts the evidence elsewhere than %s, where the replay command in report.md reads it", hint[0], uploaded)
	}
}

func TestTheCIRecipeShowsTheSummaryOnTheJobsPage(t *testing.T) {
	steps := recipeSteps(t)
	i := slices.IndexFunc(steps, func(s step) bool { return strings.Contains(s.Run, "GITHUB_STEP_SUMMARY") })
	if i < 0 {
		t.Fatal("no step writes the job's summary")
	}
	shows := steps[i]
	if shows.If != "always()" {
		t.Errorf("the job shows the summary if %q, not however reconciler-fuzzer ends", shows.If)
	}

	workspace := t.TempDir()
	invocation := filepath.Join(workspace, stepUsing(t, steps, "actions/upload-artifact").With["path"], "20260101T000000Z-1")
	writeFile(t, filepath.Join(invocation, "summary.md"), "# reconciler-fuzzer\n")
	writeFile(t, filepath.Join(invocation, "summary.json"), "{}\n")
	if ran := runStep(t, shows, workspace, nil); ran.err != nil || ran.summary != "# reconciler-fuzzer\n" {
		t.Errorf("the job's summary is %q, not summary.md: %v\n%s", ran.summary, ran.err, ran.output)
	}
	if ran := runStep(t, shows, t.TempDir(), nil); ran.err != nil {
		t.Errorf("the step fails when reconciler-fuzzer wrote no summary: %v\n%s", ran.err, ran.output)
	}
}

// recipeEvents are the events the recipe runs on, each with its settings.
func recipeEvents(t *testing.T) map[string]any {
	t.Helper()
	on := readWorkflow(t, ciRecipe).On
	events, ok := on.(map[string]any)
	if !ok {
		t.Fatalf("the recipe's on key is %v, not a map of events", on)
	}
	return events
}

func TestTheCIRecipeFixesSeedsOnPullRequestsAndDrawsThemNightly(t *testing.T) {
	on := recipeEvents(t)
	if _, ok := on["pull_request"]; !ok {
		t.Error("the recipe does not run on pull_request")
	}
	schedule, _ := on["schedule"].([]any)
	if len(schedule) == 0 || slices.ContainsFunc(schedule, func(entry any) bool {
		timing, _ := entry.(map[string]any)
		cron, _ := timing["cron"].(string)
		return len(strings.Fields(cron)) != 5
	}) {
		t.Errorf("the recipe's schedule is %v, not a list of five-field crons, and Actions rejects such a workflow", on["schedule"])
	}
	seeded := map[string]bool{}
	for _, s := range reconcilerFuzzerRuns(t, recipeSteps(t)) {
		event := regexp.MustCompile(`^github\.event_name == '(\w+)'$`).FindStringSubmatch(s.If)
		if event == nil {
			t.Errorf("%q runs if %q, not on one event", s.Run, s.If)
			continue
		}
		seeded[event[1]] = strings.Contains(s.Run, "--seed ")
	}
	if want := map[string]bool{"pull_request": true, "schedule": false}; !maps.Equal(seeded, want) {
		t.Errorf("reconciler-fuzzer run fixes a seed by event as %v, and should as %v", seeded, want)
	}
}

func TestTheCIRecipeSizesEachReconcilerFuzzerRun(t *testing.T) {
	pins := makefilePins(t)
	perRun := func(tier string) time.Duration {
		runs, _ := strconv.Atoi(pins[tier+"_RUNS"])
		deadline, err := time.ParseDuration(pins[tier+"_DEADLINE"])
		if err != nil || runs == 0 {
			t.Fatalf("the Makefile's %s_RUNS is %q and its %s_DEADLINE is %q", tier, pins[tier+"_RUNS"], tier, pins[tier+"_DEADLINE"])
		}
		return deadline / time.Duration(runs)
	}
	floor := min(perRun("EXAMPLE"), perRun("NIGHTLY"))
	for _, s := range reconcilerFuzzerRuns(t, recipeSteps(t)) {
		switch runs, deadline := reconcilerFuzzerBudget(t, s); {
		case runs == 0 || deadline == 0:
			t.Errorf("%q leaves --runs or --deadline to reconciler-fuzzer's defaults, and an adopter sizes the two together", s.Run)
		case runs < 2:
			t.Errorf("%q runs %d sequences, and a tier should run several", s.Run, runs)
		case deadline/time.Duration(runs) < floor:
			t.Errorf("%q gives each run %s, less than the %s that this repository's example tiers give each run", s.Run, deadline/time.Duration(runs), floor)
		}
	}
}

// reconcilerFuzzerBudget is the --runs and --deadline a reconciler-fuzzer step
// passes, or zero.
func reconcilerFuzzerBudget(t *testing.T, s step) (runs int, deadline time.Duration) {
	t.Helper()
	if m := regexp.MustCompile(`--runs (\d+)`).FindStringSubmatch(s.Run); m != nil {
		runs, _ = strconv.Atoi(m[1])
	}
	if m := regexp.MustCompile(`--deadline (\S+)`).FindStringSubmatch(s.Run); m != nil {
		var err error
		if deadline, err = time.ParseDuration(m[1]); err != nil {
			t.Fatalf("%q: %v", s.Run, err)
		}
	}
	return runs, deadline
}

// T1: every nightly step that files an issue runs only after failure.
func TestNightlyReportStepsRunOnlyAfterFailure(t *testing.T) {
	nightly := readWorkflow(t, ".github/workflows/nightly.yml")
	for name, job := range nightly.Jobs {
		// The step that runs make or reconciler-fuzzer, whose conclusion the report
		// checks.
		var runID string
		for _, s := range job.Steps {
			if s.ID != "" && strings.Contains(s.Run, "make ") {
				runID = s.ID
			}
		}
		for _, s := range job.Steps {
			if !strings.Contains(s.Run, "gh issue") {
				continue
			}
			if !strings.HasPrefix(s.If, "failure() && ") {
				t.Errorf("job %s, step %q: if: %q does not start with failure()", name, s.ID, s.If)
			}
			if runID != "" && !strings.Contains(s.If, "steps."+runID+".conclusion") {
				t.Errorf("job %s, step %q: if: %q does not name the conclusion of the step that runs make (%s)", name, s.ID, s.If, runID)
			}
		}
	}
}

// T2: the nightly files a find only where a drawn seed failed a check.
func TestNightlyReportStepDistinguishesAFindFromAnError(t *testing.T) {
	nightly := readWorkflow(t, ".github/workflows/nightly.yml")
	for name, job := range nightly.Jobs {
		for _, s := range job.Steps {
			if !strings.Contains(s.Run, "gh issue") {
				continue
			}
			if !strings.Contains(s.Run, "summary.json") {
				t.Errorf("job %s: the report step does not read summary.json to distinguish a find from an error", name)
			}
			// The step's env must carry both a find and an error label.
			hasFind := false
			hasError := false
			for k, v := range s.Env {
				if strings.Contains(k, "FIND") && strings.Contains(v, "nightly-find-") {
					hasFind = true
				}
				if strings.Contains(k, "ERROR") && strings.Contains(v, "nightly-error-") {
					hasError = true
				}
			}
			if !hasFind || !hasError {
				t.Errorf("job %s: the report step's env needs both a nightly-find and a nightly-error label", name)
			}
		}
	}
}

func TestNightlyReportStepFilesAFindForAViolationAndAnErrorOtherwise(t *testing.T) {
	nightly := readWorkflow(t, ".github/workflows/nightly.yml")
	for name, job := range nightly.Jobs {
		for _, s := range job.Steps {
			if !strings.Contains(s.Run, "gh issue") {
				continue
			}

			for _, tc := range []struct {
				name    string
				outcome string // summary.json outcome, or "" for no file
				label   string // expected label substring
			}{
				{"violation", "violation", "nightly-find-"},
				{"error", "error", "nightly-error-"},
				{"no output", "", "nightly-error-"},
				{"existing issue", "violation", "nightly-find-"},
			} {
				t.Run(fmt.Sprintf("%s/%s", name, tc.name), func(t *testing.T) {
					workspace := t.TempDir()
					if tc.outcome != "" {
						invocation := filepath.Join(workspace, "reconciler-fuzzer-out", "20260101T000000Z-1")
						writeFile(t, filepath.Join(invocation, "summary.json"),
							fmt.Sprintf(`{"outcome":%q,"exitCode":1}`, tc.outcome))
					}

					ghScript := `echo "$*"`
					if tc.name == "existing issue" {
						ghScript = `case "$2" in list) echo 42 ;; *) echo "$*" ;; esac`
					}
					// jq stub: the nightly script calls jq -r '.outcome // empty' FILE.
					// $1=-r, $2=expression, $3=FILE.
					jqStub := `eval file=\$$#; grep -o '"outcome":"[^"]*"' "$file" 2>/dev/null | head -1 | sed 's/.*"outcome":"\([^"]*\)".*/\1/' || true`
					ran := runStep(t, s, workspace, map[string]string{
						"gh":   ghScript,
						"jq":   jqStub,
						"date": `echo 2026-01-01`,
					})
					got := string(ran.output)
					if !strings.Contains(got, tc.label) {
						t.Errorf("expected %s label, got:\n%s", tc.label, got)
					}
					if tc.name == "existing issue" && !strings.Contains(got, "comment") {
						t.Errorf("expected gh issue comment for existing issue, got:\n%s", got)
					}
				})
			}
		}
	}
}

// T3: make test-kind-runs exits as reconciler-fuzzer did.
func TestMakeTestKindRunsExitsAsReconcilerFuzzerDid(t *testing.T) {
	makefile := readFile(t, "Makefile")
	// The test-kind-runs target should use must-pass for its reconciler-fuzzer
	// invocations.
	if !strings.Contains(makefile, "must-pass") {
		t.Error("the Makefile does not define a must-pass macro")
	}
}

// T4: reconciler-fuzzer invocations inside tier recipes (apart from negative
// controls) use must-pass so make exits with reconciler-fuzzer's own exit code.
func TestTierReconcilerFuzzerInvocationsUseMustPass(t *testing.T) {
	makefile := readFile(t, "Makefile")
	// Extract recipe blocks for the tier targets.
	tiers := []string{
		"test-example:", "test-example-nightly:",
		"test-example-external-secrets:", "test-example-external-secrets-nightly:",
		"test-kind-runs:",
	}
	for _, tier := range tiers {
		idx := strings.Index(makefile, tier)
		if idx < 0 {
			t.Errorf("Makefile has no %s target", tier)
			continue
		}
		// Read the recipe (tab-indented lines after the target).
		rest := makefile[idx:]
		lines := strings.SplitAfter(rest, "\n")
		for i, line := range lines {
			if i == 0 {
				continue // target line
			}
			if line == "" || (!strings.HasPrefix(line, "\t") && strings.TrimSpace(line) != "") {
				break // next target
			}
			trimmed := strings.TrimLeft(line, "\t @")
			isQuickstart := strings.Contains(trimmed, "quickstart.sh")
			isReconcilerFuzzer := strings.Contains(trimmed, "./bin/reconciler-fuzzer run") || strings.Contains(trimmed, "$(KIND_RECONCILER_FUZZER)")
			if !isQuickstart && !isReconcilerFuzzer {
				continue
			}
			// Negative controls use the negative-control macro.
			if strings.Contains(line, "negative-control") {
				continue
			}
			// The invocation should be inside a must-pass call.
			if !strings.Contains(line, "must-pass") {
				t.Errorf("%s reconciler-fuzzer invocation does not use must-pass: %s", tier, strings.TrimSpace(line))
			}
		}
	}
}

// T5: quickstarts exit 2 when they can't test.
func TestQuickstartsExitTwoWhenTheyCantTest(t *testing.T) {
	for _, qs := range []struct {
		path  string
		stubs map[string]string
	}{
		{
			"examples/cert-manager/quickstart.sh",
			map[string]string{
				"go":   "exit 1",
				"make": "exit 0",
				"lsof": "exit 1",
			},
		},
		{
			"examples/external-secrets/quickstart.sh",
			map[string]string{
				"go":   "exit 1",
				"make": "exit 0",
			},
		},
	} {
		t.Run(qs.path, func(t *testing.T) {
			script := readFile(t, qs.path)
			workspace := t.TempDir()
			dir := t.TempDir()
			for command, stub := range qs.stubs {
				if err := os.WriteFile(filepath.Join(dir, command), []byte("#!/bin/sh\n"+stub+"\n"), 0o755); err != nil {
					t.Fatal(err)
				}
			}
			scriptFile := filepath.Join(dir, "quickstart.sh")
			if err := os.WriteFile(scriptFile, []byte(script), 0o755); err != nil {
				t.Fatal(err)
			}
			cmd := exec.Command("sh", scriptFile)
			cmd.Dir = workspace
			cmd.Env = []string{
				"PATH=" + dir + string(os.PathListSeparator) + os.Getenv("PATH"),
				"HOME=" + workspace,
			}
			output, err := cmd.CombinedOutput()
			if err == nil {
				t.Fatalf("expected failure, got success: %s", output)
			}
			exitErr, ok := err.(*exec.ExitError)
			if !ok {
				t.Fatalf("expected ExitError, got %T: %v", err, err)
			}
			if exitErr.ExitCode() != 2 {
				t.Errorf("expected exit 2, got exit %d: %s", exitErr.ExitCode(), output)
			}
		})
	}
}

func TestCertManagerQuickstartExitsTwoWhenPortIsBound(t *testing.T) {
	script := readFile(t, "examples/cert-manager/quickstart.sh")
	workspace := t.TempDir()
	dir := t.TempDir()
	for command, stub := range map[string]string{
		"go":   "exit 0",
		"make": "exit 0",
		"lsof": "exit 0", // port is bound
	} {
		if err := os.WriteFile(filepath.Join(dir, command), []byte("#!/bin/sh\n"+stub+"\n"), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	scriptFile := filepath.Join(dir, "quickstart.sh")
	if err := os.WriteFile(scriptFile, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("sh", scriptFile)
	cmd.Dir = workspace
	cmd.Env = []string{
		"PATH=" + dir + string(os.PathListSeparator) + os.Getenv("PATH"),
		"HOME=" + workspace,
	}
	output, err := cmd.CombinedOutput()
	if err == nil {
		t.Fatalf("expected failure, got success: %s", output)
	}
	exitErr, ok := err.(*exec.ExitError)
	if !ok {
		t.Fatalf("expected ExitError, got %T: %v", err, err)
	}
	if exitErr.ExitCode() != 2 {
		t.Errorf("expected exit 2 when port 9403 is bound, got exit %d: %s", exitErr.ExitCode(), output)
	}
}

func TestNightlyFindsSayHowToRestoreTheirEvidence(t *testing.T) {
	reports := 0
	for name, job := range readWorkflow(t, ".github/workflows/nightly.yml").Jobs {
		for _, s := range job.Steps {
			if !strings.Contains(s.Run, "gh issue create") {
				continue
			}
			reports++
			upload := stepUsing(t, job.Steps, "actions/upload-artifact")
			hint := downloadHint.FindStringSubmatch(s.Run)
			if hint == nil {
				t.Errorf("job %s files an issue that does not say how to download its evidence", name)
				continue
			}
			expand := func(text string) string {
				return os.Expand(text, func(v string) string {
					if value, ok := s.Env[v]; ok {
						return value
					}
					return "$" + v
				})
			}
			if expand(hint[1]) != "$GITHUB_RUN_ID" {
				t.Errorf("job %s: %q names a run other than the one that failed", name, hint[0])
			}
			if expand(hint[2]) != upload.With["name"] {
				t.Errorf("job %s: %q names an artifact other than %s", name, hint[0], upload.With["name"])
			}
			if expand(hint[3]) != strings.TrimSuffix(upload.With["path"], "/") {
				t.Errorf("job %s: %q puts the evidence elsewhere than %s, where a report's replay command reads it", name, hint[0], upload.With["path"])
			}
		}
	}
	if reports == 0 {
		t.Fatal("no nightly job files an issue")
	}
}

// downloadHint matches a gh command that downloads one artifact of a run into a directory.
var downloadHint = regexp.MustCompile(`gh run download (\S+) --name ([\w.$/-]+) --dir ([\w.$/-]+)`)

func TestTheCIRecipeSetsEveryVariableItReads(t *testing.T) {
	set := readWorkflow(t, ciRecipe).Env
	for _, s := range recipeSteps(t) {
		var read []string
		for _, m := range regexp.MustCompile(`\$\{?([A-Z][A-Z0-9_]*)`).FindAllStringSubmatch(s.Run, -1) {
			_, local := s.Env[m[1]]
			if !local && !strings.HasPrefix(m[1], "GITHUB_") && !strings.HasPrefix(m[1], "RUNNER_") {
				read = append(read, m[1])
			}
		}
		for _, value := range s.With {
			for _, m := range regexp.MustCompile(`\$\{\{ env\.(\w+) \}\}`).FindAllStringSubmatch(value, -1) {
				read = append(read, m[1])
			}
		}
		for _, name := range read {
			if _, ok := set[name]; !ok {
				t.Errorf("a step reads %s, and the recipe's env does not set it", name)
			}
		}
	}
}

func TestTheCIRecipeSetsEachPinForEveryStep(t *testing.T) {
	pins := readWorkflow(t, ciRecipe).Env
	recipe := recipeJob(t)
	envs := map[string]map[string]string{"the job": recipe.Env}
	for i, s := range recipe.Steps {
		envs[fmt.Sprintf("step %d", i)] = s.Env
	}
	for where, env := range envs {
		for name := range env {
			if _, ok := pins[name]; ok {
				t.Errorf("%s sets %s over the pin in the workflow's env", where, name)
			}
		}
	}
}

func TestTheCIRecipeRunsWhereThisRepositorysWorkflowsRun(t *testing.T) {
	paths, err := filepath.Glob(".github/workflows/*.yml")
	if err != nil || len(paths) == 0 {
		t.Fatalf("found workflows %v: %v", paths, err)
	}
	runners, releases := map[string]bool{}, map[string]bool{}
	for _, path := range paths {
		for _, j := range readWorkflow(t, path).Jobs {
			runners[fmt.Sprint(j.RunsOn)] = true
			for _, s := range j.Steps {
				releases[actionRelease(s.Uses)] = true
			}
		}
	}
	recipe := recipeJob(t)
	if !runners[fmt.Sprint(recipe.RunsOn)] {
		t.Errorf("the recipe runs its bash steps on %v, and this repository's workflows run only on %v", recipe.RunsOn, slices.Sorted(maps.Keys(runners)))
	}
	for _, s := range recipe.Steps {
		if s.Uses != "" && !releases[actionRelease(s.Uses)] {
			t.Errorf("the recipe uses %s, a release this repository's workflows do not run", s.Uses)
		}
	}
}

// actionRelease names an action's repository and version, so that
// actions/cache/restore@v4 is actions/cache@v4.
func actionRelease(uses string) string {
	name, version, _ := strings.Cut(uses, "@")
	parts := strings.SplitN(name, "/", 3)
	return strings.Join(parts[:min(len(parts), 2)], "/") + "@" + version
}

func TestTheCIRecipeChecksOutTheRepositoryWithAReadOnlyToken(t *testing.T) {
	recipe := recipeJob(t)
	if len(recipe.Steps) == 0 || !strings.HasPrefix(recipe.Steps[0].Uses, "actions/checkout@") {
		t.Error("the job's first step does not check out the repository")
	}
	if permissions := readWorkflow(t, ciRecipe).Permissions; !reflect.DeepEqual(permissions, map[string]any{"contents": "read"}) {
		t.Errorf("the job's token has %v, more than it needs to read the repository", permissions)
	}
}

func runText(steps []step) string {
	var commands []string
	for _, s := range steps {
		commands = append(commands, s.Run)
	}
	return strings.Join(commands, "\n")
}

// The README shows the recipe's reconciler-fuzzer commands, without the exec a
// step needs.
func TestTheREADMEsCICommandsAreTheRecipes(t *testing.T) {
	var recipe []string
	for _, line := range strings.Split(runText(recipeSteps(t)), "\n") {
		recipe = append(recipe, strings.TrimSpace(line))
	}
	shown := 0
	for _, block := range fencedBlocks(section(t, readFile(t, "README.md"), "## Running in CI")) {
		for _, line := range strings.Split(block, "\n") {
			if !strings.HasPrefix(line, "reconciler-fuzzer ") {
				continue
			}
			shown++
			if !slices.Contains(recipe, "exec "+line) {
				t.Errorf("README.md runs %q, and %s runs no such command.", line, ciRecipe)
			}
		}
	}
	if shown == 0 {
		t.Fatal("README.md's Running in CI section runs no reconciler-fuzzer command, so this test checks nothing.")
	}
}

func TestTheCIRecipeNeedsNoGoMod(t *testing.T) {
	setupGo := stepUsing(t, recipeSteps(t), "actions/setup-go")
	if file, ok := setupGo.With["go-version-file"]; ok {
		t.Errorf("setup-go reads the Go version from %s, which an operator in another language lacks", file)
	}
	if cache := setupGo.With["cache"]; cache != "false" {
		t.Errorf("setup-go has cache: %q, and its cache warns on every run without a go.sum", cache)
	}
	directive := regexp.MustCompile(`(?m)^go (\d+\.\d+)`).FindStringSubmatch(readFile(t, "go.mod"))
	if directive == nil {
		t.Fatal("go.mod has no go directive")
	}
	if version := setupGo.With["go-version"]; version != directive[1] {
		t.Errorf("setup-go installs Go %q, and reconciler-fuzzer's go.mod asks for %s", version, directive[1])
	}
}
