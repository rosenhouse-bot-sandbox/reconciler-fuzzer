package main

import (
	"os"
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/rosenhouse-bot-sandbox/reconciler-fuzzer/internal/run"
)

// quickstartRun is a page's first cert-manager quickstart command and the
// block that shows what reconciler-fuzzer prints.
var quickstartRun = regexp.MustCompile("(?s)```sh\nexamples/cert-manager/quickstart\\.sh ([^\n]*)\n```\n\n```\n(.*?)```\n")

// What reconciler-fuzzer prints in the cert-manager quickstart depends on what
// its seeds draw.
func TestTheExamplesPageShowsWhatReconcilerFuzzerPrintsInTheCertManagerQuickstart(t *testing.T) {
	t.Chdir("../..")
	args, shown := quickstart(t, readFile(t, "docs/examples.md"), readFile(t, "examples/cert-manager/quickstart.sh"))
	args = append(args, "--out", t.TempDir())

	// The fake session passes every run, as cert-manager does.
	code, stdout, stderr := invokeWith(t, &fakeSession{}, rapidGenerator, args...)
	if code != exitOK {
		t.Fatalf("reconciler-fuzzer %s exited %d: %s", strings.Join(args, " "), code, stderr)
	}
	if shown := withoutNotes(shown); stdout != shown {
		t.Errorf("docs/examples.md shows\n%s\nand reconciler-fuzzer %s printed\n%s", shown, strings.Join(args, " "), stdout)
	}
}

// withoutNotes drops the checks a real run notes it did not evaluate, which the
// fake session cannot.
func withoutNotes(shown string) string {
	return regexp.MustCompile(`(?m)^run \d+: \S+ is not evaluated at .*\n`).ReplaceAllString(shown, "")
}

// docs/failures.md quotes the line a drawn run of twelve ops prints once it fails.
func TestTheFailuresPageQuotesTheLineReconcilerFuzzerPrintsBeforeItMinimizes(t *testing.T) {
	violation := run.Violation{ID: "G3"}
	session := &fakeSession{fails: func(run.Sequence, string) *run.Violation { return &violation }}
	twelve := slices.Repeat([]run.OpType{run.OpSettle}, 12)

	_, stdout, _ := invokeWith(t, session, countingGenerator(nil, twelve...),
		"run", "--target", toyTargetYAML, "--out", t.TempDir(), "--runs", "1", "--seed", "1")

	line := regexp.MustCompile(`(?m)^run 1: G3 failed.*$`).FindString(stdout)
	page := strings.Join(strings.Fields(readFile(t, "../../docs/failures.md")), " ")
	if line == "" || !strings.Contains(page, "`"+line+"`") {
		t.Errorf("reconciler-fuzzer printed\n%s\nand docs/failures.md does not quote its line %q.", stdout, line)
	}
}

func TestQuickstartPassesReconcilerFuzzerThePagesArgs(t *testing.T) {
	page := "```sh\nexamples/cert-manager/quickstart.sh --seed 7\n```\n\n```\nprinted\n```\n"
	script := "KUBEBUILDER_ASSETS=x ./bin/reconciler-fuzzer run \\\n  --runs 5 \"$@\"\necho done\n"
	args, shown := quickstart(t, page, script)
	if want := []string{"run", "--runs", "5", "--seed", "7"}; !slices.Equal(args, want) || shown != "printed\n" {
		t.Errorf("quickstart returned %q and %q, want %q and %q.", args, shown, want, "printed\n")
	}
}

// quickstart returns what script passes reconciler-fuzzer when it runs the
// page's first cert-manager quickstart command, and the block after that
// command.
func quickstart(t *testing.T, page, script string) (args []string, shown string) {
	t.Helper()
	m := quickstartRun.FindStringSubmatch(page)
	if m == nil {
		t.Fatal("The page runs no cert-manager quickstart and shows no output after it.")
	}
	_, invocation, found := strings.Cut(script, "./bin/reconciler-fuzzer ")
	if !found {
		t.Fatal("quickstart.sh runs no ./bin/reconciler-fuzzer.")
	}
	invocation, _, _ = strings.Cut(strings.ReplaceAll(invocation, "\\\n", " "), "\n")
	for _, arg := range strings.Fields(invocation) {
		if arg == `"$@"` {
			args = append(args, strings.Fields(m[1])...)
		} else {
			args = append(args, arg)
		}
	}
	return args, m[2]
}

func readFile(t *testing.T, path string) string {
	t.Helper()
	content, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(content)
}
