package reconcilerfuzzer_test

import (
	"errors"
	"fmt"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"
)

// huntRun is what one run of examples/hunt.sh did.
type huntRun struct {
	// invocations are reconciler-fuzzer's arguments, one invocation each.
	invocations []string
	output      string
	code        int
}

// runHunt runs examples/hunt.sh over the named families with a
// bin/reconciler-fuzzer that exits with each code in turn, then 0, and a clock
// that moves on a minute each time it is read. TICK in env sets another step in
// seconds. AFTER_<n> in env is a command the n-th invocation runs.
func runHunt(t *testing.T, families []string, codes []int, env ...string) huntRun {
	t.Helper()
	return runHuntWithArgs(t, []string{"target.yaml", "families", "out"}, families, codes, env...)
}

func runHuntWithArgs(t *testing.T, args, families []string, codes []int, env ...string) huntRun {
	t.Helper()
	script, err := filepath.Abs("examples/hunt.sh")
	if err != nil {
		t.Fatal(err)
	}
	workspace, stubs := t.TempDir(), t.TempDir()
	if err := os.MkdirAll(filepath.Join(workspace, "families"), 0o755); err != nil {
		t.Fatal(err)
	}
	for _, family := range families {
		writeFile(t, filepath.Join(workspace, "families", family+".json"), "{}\n")
	}
	var lines []string
	for _, code := range codes {
		lines = append(lines, strconv.Itoa(code))
	}
	writeFile(t, filepath.Join(stubs, "codes"), strings.Join(lines, "\n")+"\n")
	writeFile(t, filepath.Join(stubs, "clock"), "1000000\n")
	if err := os.MkdirAll(filepath.Join(workspace, "bin"), 0o755); err != nil {
		t.Fatal(err)
	}
	writeStub(t, filepath.Join(workspace, "bin", "reconciler-fuzzer"), `printf '%s\n' "$*" >>"$STUBS/invocations"
n=$(wc -l <"$STUBS/invocations")
after=$(printenv "AFTER_$n" || true)
[ -z "$after" ] || sh -c "$after"
exit "$(sed -n "${n}p" "$STUBS/codes" | grep . || echo 0)"`)
	writeStub(t, filepath.Join(stubs, "date"), `now=$(cat "$STUBS/clock")
echo $((now + ${TICK:-60})) >"$STUBS/clock"
echo "$now"`)

	cmd := exec.Command(script, args...)
	cmd.Dir = workspace
	cmd.Env = append([]string{
		"PATH=" + stubs + string(os.PathListSeparator) + os.Getenv("PATH"),
		"STUBS=" + stubs,
	}, env...)
	output, err := cmd.CombinedOutput()
	var exit *exec.ExitError
	if err != nil && !errors.As(err, &exit) {
		t.Fatalf("%s did not run: %v", script, err)
	}
	invocations, _ := os.ReadFile(filepath.Join(stubs, "invocations"))
	return huntRun{
		invocations: strings.Split(strings.TrimSpace(string(invocations)), "\n"),
		output:      string(output),
		code:        cmd.ProcessState.ExitCode(),
	}
}

func writeStub(t *testing.T, path, script string) {
	t.Helper()
	if err := os.WriteFile(path, []byte("#!/bin/sh\n"+script+"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
}

var anyDeadline = regexp.MustCompile(`--deadline \d+s`)

func withoutDeadlines(invocations []string) []string {
	var stripped []string
	for _, invocation := range invocations {
		stripped = append(stripped, anyDeadline.ReplaceAllString(invocation, "--deadline D"))
	}
	return stripped
}

func TestTheHuntRunsEachFamilyAndThenEachSeedInAnInvocationOfItsOwnWithOneBaseline(t *testing.T) {
	h := runHunt(t, []string{"a", "b"}, nil, "HUNT_MINUTES=1000", "HUNT_RUNS=2", "HUNT_SEED=7")

	want := []string{
		"run --target target.yaml --deadline D --out out/a families/a.json",
		"run --target target.yaml --deadline D --out out/b families/b.json",
		"run --target target.yaml --deadline D --out out/seed-7 --seed 7 --runs 1",
		"run --target target.yaml --deadline D --out out/seed-8 --seed 8 --runs 1 --no-baseline",
	}
	if got := withoutDeadlines(h.invocations); !slices.Equal(got, want) {
		t.Errorf("The hunt ran reconciler-fuzzer as\n%s\nnot as\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
	if h.code != 0 || !strings.Contains(h.output, "4 passed.") {
		t.Errorf("The hunt exited %d when every invocation passed, and printed\n%s", h.code, h.output)
	}
	for _, name := range []string{"a", "b", "seed-7", "seed-8"} {
		if !strings.Contains(h.output, "==> out/"+name+"\n") {
			t.Errorf("The hunt did not announce out/%s:\n%s", name, h.output)
		}
	}
}

// An invocation stops at its first failing run, before the baseline.
func TestTheHuntRunsTheBaselineUntilAnInvocationPasses(t *testing.T) {
	h := runHunt(t, nil, []int{1, 2, 0}, "HUNT_MINUTES=1000", "HUNT_RUNS=4", "HUNT_SEED=7")

	want := []string{
		"run --target target.yaml --deadline D --out out/seed-7 --seed 7 --runs 1",
		"run --target target.yaml --deadline D --out out/seed-8 --seed 8 --runs 1",
		"run --target target.yaml --deadline D --out out/seed-9 --seed 9 --runs 1",
		"run --target target.yaml --deadline D --out out/seed-10 --seed 10 --runs 1 --no-baseline",
	}
	if got := withoutDeadlines(h.invocations); !slices.Equal(got, want) {
		t.Errorf("The hunt ran reconciler-fuzzer as\n%s\nnot as\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
}

func TestTheHuntSkipsAFamilyGoneByItsTurn(t *testing.T) {
	h := runHunt(t, []string{"a", "b", "c"}, nil, "HUNT_MINUTES=1000", "HUNT_RUNS=1", "HUNT_SEED=7", "AFTER_1=rm families/b.json")

	want := []string{
		"run --target target.yaml --deadline D --out out/a families/a.json",
		"run --target target.yaml --deadline D --out out/c families/c.json",
		"run --target target.yaml --deadline D --out out/seed-7 --seed 7 --runs 1",
	}
	if got := withoutDeadlines(h.invocations); !slices.Equal(got, want) {
		t.Errorf("The hunt ran reconciler-fuzzer as\n%s\nnot as\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
}

func TestTheHuntWithNoFamiliesDrawsSeeds(t *testing.T) {
	h := runHunt(t, nil, nil, "HUNT_MINUTES=1000", "HUNT_RUNS=1", "HUNT_SEED=7")

	want := []string{"run --target target.yaml --deadline D --out out/seed-7 --seed 7 --runs 1"}
	if got := withoutDeadlines(h.invocations); !slices.Equal(got, want) {
		t.Errorf("The hunt ran reconciler-fuzzer as %q, not as %q", got, want)
	}
}

func TestTheHuntKeepsGoingAfterAFailureAndNamesEachOne(t *testing.T) {
	h := runHunt(t, []string{"a"}, []int{1, 2, 0}, "HUNT_MINUTES=1000", "HUNT_RUNS=2", "HUNT_SEED=7")

	if len(h.invocations) != 3 {
		t.Errorf("The hunt ran %d invocations, not every family and seed:\n%s", len(h.invocations), h.output)
	}
	if h.code != 1 {
		t.Errorf("The hunt exited %d, and an invocation that did not pass makes it exit 1", h.code)
	}
	for _, failed := range []string{"Each is a candidate to triage, not yet a bug:", "out/a exited 1", "out/seed-7 exited 2"} {
		if !strings.Contains(h.output, failed) {
			t.Errorf("The hunt did not say %q:\n%s", failed, h.output)
		}
	}
	if strings.Contains(h.output, "out/seed-8 exited") {
		t.Errorf("The hunt named an invocation that passed:\n%s", h.output)
	}
}

func TestTheHuntGivesEachInvocationWhatTheTimeBoxHasLeftAndStopsWhenItRunsOut(t *testing.T) {
	h := runHunt(t, []string{"a"}, nil, "HUNT_MINUTES=3", "HUNT_RUNS=5", "HUNT_SEED=7")

	want := []string{
		"run --target target.yaml --deadline 120s --out out/a families/a.json",
		"run --target target.yaml --deadline 60s --out out/seed-7 --seed 7 --runs 1",
	}
	if !slices.Equal(h.invocations, want) {
		t.Errorf("The hunt ran reconciler-fuzzer as\n%s\nnot as\n%s", strings.Join(h.invocations, "\n"), strings.Join(want, "\n"))
	}
	if h.code != 0 || !strings.Contains(h.output, "the time box ran out before seed-8.") ||
		strings.Count(h.output, "the time box ran out") != 1 {
		t.Errorf("The hunt exited %d and printed\n%s", h.code, h.output)
	}
}

func TestTheHuntRunsAnInvocationInTheBoxsLastSecond(t *testing.T) {
	h := runHunt(t, []string{"a"}, nil, "HUNT_MINUTES=1", "HUNT_RUNS=1", "HUNT_SEED=7", "TICK=59")

	if want := "run --target target.yaml --deadline 1s --out out/a families/a.json"; len(h.invocations) != 1 || h.invocations[0] != want {
		t.Errorf("The hunt ran reconciler-fuzzer as %q, not as %q:\n%s", h.invocations, want, h.output)
	}
}

func TestTheHuntDrawsNoSeedOnceItsTimeBoxRunsOutAmongTheFamilies(t *testing.T) {
	h := runHunt(t, []string{"a", "b", "c"}, nil, "HUNT_MINUTES=2", "HUNT_RUNS=2", "HUNT_SEED=7")

	if len(h.invocations) != 1 || !strings.Contains(h.output, "the time box ran out before b.") ||
		strings.Count(h.output, "the time box ran out") != 1 {
		t.Errorf("The hunt ran %q and printed\n%s", h.invocations, h.output)
	}
}

func TestTheHuntBlamesNothingForARunItsTimeBoxCut(t *testing.T) {
	h := runHunt(t, []string{"a", "b"}, []int{2}, "HUNT_MINUTES=2", "HUNT_RUNS=2", "HUNT_SEED=7")

	if len(h.invocations) != 1 || h.code != 0 || !strings.Contains(h.output, "the time box ran out during a.") ||
		strings.Count(h.output, "the time box ran out") != 1 || strings.Contains(h.output, "exited 2") {
		t.Errorf("The hunt ran %d invocations, exited %d and printed\n%s", len(h.invocations), h.code, h.output)
	}
}

// reconciler-fuzzer can report a find after its deadline, since it finishes the
// run it is in.
func TestTheHuntKeepsAFindThatEndsAfterItsTimeBox(t *testing.T) {
	h := runHunt(t, []string{"a", "b"}, []int{1}, "HUNT_MINUTES=2", "HUNT_RUNS=2", "HUNT_SEED=7")

	if h.code != 1 || !strings.Contains(h.output, "out/a exited 1") || strings.Contains(h.output, "ran out during") {
		t.Errorf("The hunt exited %d and printed\n%s", h.code, h.output)
	}
}

func TestARebuildDuringAHuntChangesNothing(t *testing.T) {
	rebuild := `printf '#!/bin/sh\nexit 3\n' >bin/new && chmod +x bin/new && mv bin/new bin/reconciler-fuzzer`
	h := runHunt(t, []string{"a"}, nil, "HUNT_MINUTES=1000", "HUNT_RUNS=2", "HUNT_SEED=7", "AFTER_1="+rebuild)

	if len(h.invocations) != 3 || h.code != 0 {
		t.Errorf("The hunt ran %d invocations and exited %d:\n%s", len(h.invocations), h.code, h.output)
	}
}

func TestTheHuntNeedsItsThreeArguments(t *testing.T) {
	for _, args := range [][]string{nil, {"target.yaml", "families"}, {"target.yaml", "families", "out", "more"}} {
		h := runHuntWithArgs(t, args, []string{"a"}, nil, "HUNT_MINUTES=1000", "HUNT_RUNS=2", "HUNT_SEED=7")
		if h.code != 2 || !strings.Contains(h.output, "Usage: examples/hunt.sh <target.yaml> <families directory> <output directory>") ||
			h.invocations[0] != "" {
			t.Errorf("Given %q, the hunt exited %d after running %q, and printed\n%s", args, h.code, h.invocations, h.output)
		}
	}
}

// The Makefile holds the defaults.
func TestTheHuntNeedsItsTimeBoxRunsAndSeed(t *testing.T) {
	all := []string{"HUNT_MINUTES=1000", "HUNT_RUNS=2", "HUNT_SEED=7"}
	for i, missing := range all {
		h := runHunt(t, []string{"a"}, nil, slices.Delete(slices.Clone(all), i, i+1)...)
		if h.code == 0 || len(h.invocations) > 1 || h.invocations[0] != "" {
			t.Errorf("Without %s, the hunt exited %d after running %q", missing, h.code, h.invocations)
		}
	}
}

func TestTheHuntStopsWhenReconcilerFuzzerDiesOfASignal(t *testing.T) {
	h := runHunt(t, []string{"a", "b"}, []int{130}, "HUNT_MINUTES=1000", "HUNT_RUNS=2", "HUNT_SEED=7")

	if len(h.invocations) != 1 || h.code != 130 {
		t.Errorf("The hunt ran %d invocations and exited %d after reconciler-fuzzer died of SIGINT:\n%s", len(h.invocations), h.code, h.output)
	}
}

// outsideMake drops what a make running these tests passes to a make they run,
// so that it runs as a reader's would.
func outsideMake(env []string) []string {
	return slices.DeleteFunc(env, func(variable string) bool {
		name, _, _ := strings.Cut(variable, "=")
		return slices.Contains([]string{"MAKEFLAGS", "MAKELEVEL", "MFLAGS", "MAKEOVERRIDES"}, name)
	})
}

// dryRun is what make would run for the targets, without running it.
func dryRun(t *testing.T, targets ...string) string {
	t.Helper()
	if _, err := exec.LookPath("make"); err != nil {
		t.Skipf("The Makefile needs make: %v", err)
	}
	cmd := exec.Command("make", append([]string{"-n", "-B"}, targets...)...)
	cmd.Env = outsideMake(os.Environ())
	output, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("make -n -B %s: %v\n%s", strings.Join(targets, " "), err, output)
	}
	return string(output)
}

func TestEachHuntTargetPreparesItsExampleAndHuntsIt(t *testing.T) {
	assetsPath := lastLine(dryRun(t, "assets-path"))
	for _, example := range []struct {
		name  string
		needs []string
	}{
		{"cert-manager", []string{"verify-cert-manager-port", "verify-cert-manager-pin", "setup", "build", "cert-manager"}},
		{"external-secrets", []string{"verify-external-secrets-pin", "setup", "build", "external-secrets"}},
	} {
		t.Run(example.name, func(t *testing.T) {
			dry := dryRun(t, "hunt-"+example.name, "HUNT_MINUTES=5", "HUNT_RUNS=3", "HUNT_SEED=9")

			hunt := regexp.MustCompile(`(?m)^.*examples/hunt\.sh .*$`).FindString(dry)
			want := `HUNT_MINUTES=5 HUNT_RUNS=3 HUNT_SEED=9 KUBEBUILDER_ASSETS="$(` + assetsPath + `)" examples/hunt.sh examples/` +
				example.name + "/target.yaml examples/" + example.name + "/sequences/hunt reconciler-fuzzer-out/hunt-" + example.name
			if hunt != want {
				t.Fatalf("make hunt-%s runs\n%s\nnot\n%s", example.name, hunt, want)
			}
			before, _, _ := strings.Cut(dry, hunt)
			for _, need := range example.needs {
				if !strings.Contains(before, dryRun(t, need)) {
					t.Errorf("make hunt-%s does not make %s before it hunts:\n%s", example.name, need, dry)
				}
			}

			pins := makefilePins(t)
			defaults := fmt.Sprintf("HUNT_MINUTES=%s HUNT_RUNS=%s HUNT_SEED=%s ", pins["HUNT_MINUTES"], pins["HUNT_RUNS"], pins["HUNT_SEED"])
			if defaulted := dryRun(t, "hunt-"+example.name); !strings.Contains(defaulted, defaults) {
				t.Errorf("make hunt-%s does not pass the hunt %q:\n%s", example.name, defaults, defaulted)
			}
		})
	}
}

func lastLine(output string) string {
	lines := strings.Split(strings.TrimSpace(output), "\n")
	return lines[len(lines)-1]
}

func TestTheREADMEGivesTheHuntsDefaults(t *testing.T) {
	pins, readme := makefilePins(t), readFile(t, "README.md")
	for _, name := range []string{"HUNT_MINUTES", "HUNT_RUNS", "HUNT_SEED"} {
		if want := fmt.Sprintf("`%s` (default %s)", name, pins[name]); pins[name] == "" || !strings.Contains(readme, want) {
			t.Errorf("README.md does not say %q", want)
		}
	}
}

// cert-manager's healthz port is fixed, so its runs cannot overlap.
func TestTheCertManagerPortCheckRefusesABoundPort(t *testing.T) {
	makePath, err := exec.LookPath("make")
	if err != nil {
		t.Skipf("The Makefile needs make: %v", err)
	}
	for _, test := range []struct {
		name string
		lsof string // The stub's script; empty for no lsof.
		code int
		says string
	}{
		{"a free port", "exit 1", 0, ""},
		{"a bound port", `[ "$*" = "-nP -iTCP:9403 -sTCP:LISTEN" ]`, 2, "port 9403 is bound. cert-manager listens there, so its runs cannot overlap.\n"},
		{"no lsof", "", 0, "lsof is missing, so nothing checked whether port 9403 is free.\n"},
	} {
		t.Run(test.name, func(t *testing.T) {
			stubs := t.TempDir()
			if test.lsof != "" {
				writeStub(t, filepath.Join(stubs, "lsof"), test.lsof)
			}
			cmd := exec.Command(makePath, "--no-print-directory", "verify-cert-manager-port")
			cmd.Env = append(slices.DeleteFunc(outsideMake(os.Environ()), func(variable string) bool {
				return strings.HasPrefix(variable, "PATH=")
			}), "PATH="+stubs)
			output, _ := cmd.CombinedOutput()

			if code := cmd.ProcessState.ExitCode(); code != test.code || !strings.HasPrefix(string(output), test.says) ||
				test.code == 0 && string(output) != test.says {
				t.Errorf("make verify-cert-manager-port exited %d and printed %q", code, output)
			}
		})
	}
}

func TestNoPullRequestReachesAHunt(t *testing.T) {
	paths, err := filepath.Glob(".github/workflows/*.yml")
	if err != nil || len(paths) == 0 {
		t.Fatalf("found workflows %v: %v", paths, err)
	}
	checked := 0
	for _, path := range paths {
		w := readWorkflow(t, path)
		if !slices.Contains(triggers(w.On), "pull_request") {
			continue
		}
		for name, j := range w.Jobs {
			for _, s := range j.Steps {
				for _, run := range j.expandMatrix(s.Run) {
					for _, m := range regexp.MustCompile(`(?m)\bmake ((?:[\w-]+ ?)+)`).FindAllStringSubmatch(run, -1) {
						checked++
						if dry := dryRun(t, strings.Fields(m[1])...); strings.Contains(dry, "hunt") {
							t.Errorf("%s, job %s: make %s reaches a hunt:\n%s", path, name, m[1], dry)
						}
					}
				}
			}
		}
	}
	if checked == 0 {
		t.Fatal("no pull request runs make, so this test checks nothing")
	}
}

func TestMakeHelpListsEveryTarget(t *testing.T) {
	if _, err := exec.LookPath("make"); err != nil {
		t.Skipf("The Makefile needs make: %v", err)
	}
	cmd := exec.Command("make", "--no-print-directory", "help")
	cmd.Env = outsideMake(os.Environ())
	help, err := cmd.Output()
	if err != nil {
		t.Fatalf("make help: %v", err)
	}
	targets := regexp.MustCompile(`(?m)^\.PHONY: (\S+)$`).FindAllStringSubmatch(readFile(t, "Makefile"), -1)
	if len(targets) == 0 {
		t.Fatal("the Makefile declares no phony target, so this test checks nothing")
	}
	for _, m := range targets {
		if listed := regexp.MustCompile(`(?m)^  ` + regexp.QuoteMeta(m[1]) + ` `); m[1] != "help" && !listed.Match(help) {
			t.Errorf("make help does not list %s:\n%s", m[1], help)
		}
	}
}

// triggers names the events a workflow's on key lists, in any of its forms.
func triggers(on any) []string {
	switch on := on.(type) {
	case string:
		return []string{on}
	case []any:
		var events []string
		for _, event := range on {
			events = append(events, fmt.Sprint(event))
		}
		return events
	case map[string]any:
		return slices.Collect(maps.Keys(on))
	}
	return nil
}
