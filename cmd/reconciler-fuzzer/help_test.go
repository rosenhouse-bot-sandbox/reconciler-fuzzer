package main

import (
	"flag"
	"os"
	"slices"
	"strings"
	"testing"
	"unicode/utf8"
)

var helpForms = []struct {
	args    []string
	command string
}{
	{args: []string{"help"}},
	{args: []string{"--help"}},
	{args: []string{"-help"}},
	{args: []string{"-h"}},
	{args: []string{"--h"}},
	{args: []string{"help", "help"}},
	{args: []string{"help", "--help"}},
	{args: []string{"run", "--help"}, command: "run"},
	{args: []string{"run", "-h"}, command: "run"},
	{args: []string{"help", "run"}, command: "run"},
	{args: []string{"replay", "--help"}, command: "replay"},
	{args: []string{"replay", "s.json", "--help"}, command: "replay"},
	{args: []string{"replay", "s.json", "--h"}, command: "replay"},
	{args: []string{"help", "replay"}, command: "replay"},
	{args: []string{"version", "--help"}, command: "version"},
	{args: []string{"matrix", "--help"}, command: "matrix"},
}

func TestEveryHelpFormPrintsItsHelpAndExitsZero(t *testing.T) {
	for _, form := range helpForms {
		asked := "reconciler-fuzzer " + strings.Join(form.args, " ")
		code, stdout, stderr := invoke(t, &fakeSession{}, form.args...)

		if code != exitOK || stderr != "" {
			t.Errorf("%s exited %d and wrote %q to stderr, want 0 and nothing.", asked, code, stderr)
		}
		if stdout != help(form.command) {
			t.Errorf("%s printed\n%s\nwant\n%s", asked, stdout, help(form.command))
		}
	}
}

func TestTheSynopsisListsEveryFlag(t *testing.T) {
	for name, want := range map[string]string{
		"run": "reconciler-fuzzer run --target file [--deadline duration] [--junit file] [--kubeconfig file] " +
			"[--launch-arg arg]... [--no-baseline] [--out dir] [--runs n] [--seed n] [sequence.json...]",
		"replay":  "reconciler-fuzzer replay --target file [--deadline duration] [--junit file] [--kubeconfig file] [--launch-arg arg]... [--out dir] sequence.json",
		"version": "reconciler-fuzzer version",
		"matrix":  "reconciler-fuzzer matrix --target file --sequences dir [--deadline duration] [--kubeconfig file] [--launch-arg arg]... [--out file]",
	} {
		c, _ := lookup(name)
		if got := c.synopsis(); got != want {
			t.Errorf("The synopsis of reconciler-fuzzer %s is\n%s\nwant\n%s", name, got, want)
		}
	}
}

func TestACommandsHelpDescribesEachFlagWithItsDefault(t *testing.T) {
	for _, c := range commands {
		got := help(c.name)
		if !strings.HasPrefix(got, "Usage:\n  "+c.synopsis()+"\n\n"+c.about+"\n") {
			t.Errorf("reconciler-fuzzer %s --help begins %q, want its synopsis and what it does.", c.name, firstLines(got, 4))
		}
		flat := strings.Join(strings.Fields(got), " ")
		(&options{command: c.name}).flags().VisitAll(func(f *flag.Flag) {
			placeholder, usage := flag.UnquoteUsage(f)
			want := strings.Join(strings.Fields("--"+f.Name+" "+placeholder+" "+usage), " ")
			if !strings.Contains(flat, want) {
				t.Errorf("reconciler-fuzzer %s --help does not describe --%s: want %q in\n%s", c.name, f.Name, want, got)
			}
		})
	}
	run := help("run")
	for _, want := range []string{
		"\n  --target file\n      Exercise the controller that this target.yaml file declares. (required)\n",
		"\n  --runs n\n      Draw and run n sequences. (default 10)\n",
		"(default reconciler-fuzzer-out)\n",
	} {
		if !strings.Contains(run, want) {
			t.Errorf("reconciler-fuzzer run --help lacks %q:\n%s", want, run)
		}
	}
	if strings.Contains(run, "(default 0") || strings.Contains(run, "(default )") {
		t.Errorf("reconciler-fuzzer run --help gives a default to a flag that has none:\n%s", run)
	}
	for _, c := range commands {
		if got := strings.Count(help(c.name), "(required)"); got != len(c.required) {
			t.Errorf("reconciler-fuzzer %s --help marks %d flags required, want %d: %v.", c.name, got, len(c.required), c.required)
		}
	}
}

func TestARequiredFlagIsMarkedRequiredWhateverItsDefault(t *testing.T) {
	c := command{required: []string{"target"}}
	if got := c.annotation(&flag.Flag{Name: "target", DefValue: "target.yaml"}); got != " (required)" {
		t.Errorf("A required flag with a default is annotated %q, want %q.", got, " (required)")
	}
}

func TestEveryHelpLineButTheSynopsisFitsEightyColumns(t *testing.T) {
	for _, c := range slices.Concat(commands, []command{{}}) {
		for _, line := range strings.Split(help(c.name), "\n") {
			if line != "  "+c.synopsis() && utf8.RuneCountInString(line) > 80 {
				t.Errorf("reconciler-fuzzer %s --help has a line of %d columns:\n%s", c.name, utf8.RuneCountInString(line), line)
			}
		}
	}
}

func TestWrapFillsEachLineUpToEightyColumns(t *testing.T) {
	short, long := strings.Repeat("x", 38), strings.Repeat("x", 39)
	if got, want := wrap("  ", short+" "+long), "  "+short+" "+long; got != want {
		t.Errorf("wrap gave %q, want one line of 80 columns: %q.", got, want)
	}
	if got, want := wrap("  ", long+" "+long), "  "+long+"\n  "+long; got != want {
		t.Errorf("wrap gave %q, want two lines rather than one of 81 columns: %q.", got, want)
	}
}

func TestRunAndReplaySayWhichSequencesTheyRunAndMinimize(t *testing.T) {
	for name, says := range map[string][]string{
		"run": {
			"reconciler-fuzzer run draws --runs sequences of ops on the target's custom resources and runs each against a fresh namespace, then runs the baseline.",
			"reconciler-fuzzer stops at the first sequence that fails a check, minimizes it, and writes a report.",
			"Sequence files given as arguments run as written instead, and reconciler-fuzzer does not minimize them.",
		},
		"replay": {"reconciler-fuzzer replay runs one sequence file as written, such as the sequence.json of a failing run, and writes a report if it fails."},
	} {
		got := strings.Join(strings.Fields(help(name)), " ")
		for _, want := range says {
			if !strings.Contains(got, want) {
				t.Errorf("reconciler-fuzzer %s --help does not say %q:\n%s", name, want, help(name))
			}
		}
	}
}

func TestTheHelpOfACommandThatRunsSaysTheExitCodesAndTheAPIServer(t *testing.T) {
	for _, name := range []string{"", "run", "replay", "matrix"} {
		got := help(name)
		for _, want := range []string{"Exit codes:\n  0 ", "\n  1 ", "\n  2 ", "\n  128+N ", "KUBEBUILDER_ASSETS", "--kubeconfig"} {
			if !strings.Contains(got, want) {
				t.Errorf("The help of %q lacks %q:\n%s", name, want, got)
			}
		}
	}
}

func TestVersionsHelpSaysOnlyWhatItDoes(t *testing.T) {
	if got, want := help("version"), "Usage:\n  reconciler-fuzzer version\n\nreconciler-fuzzer version prints reconciler-fuzzer's version.\n"; got != want {
		t.Errorf("reconciler-fuzzer version --help is\n%s\nwant\n%s", got, want)
	}
}

func TestTheTopLevelHelpListsTheCommandsUsersRun(t *testing.T) {
	got := help("")
	if want := "reconciler-fuzzer finds bugs in a Kubernetes controller."; !strings.HasPrefix(got, want) {
		t.Errorf("reconciler-fuzzer --help begins %q, want %q.", firstLines(got, 1), want)
	}
	for _, want := range []string{
		"\n  run ", "\n  replay ", "\n  version ",
		"\nRun 'reconciler-fuzzer <command> --help' or 'reconciler-fuzzer help <command>'\nfor a command's flags.\n",
		"\nThe README at https://github.com/rosenhouse-bot-sandbox/reconciler-fuzzer\nshows a first run.\n",
		"\ndocs/reference.md there lists every key of target.yaml and every op.\n",
		"\ndocs/checks.md there says what each check requires and what its failure says.\n",
		"\ndocs/failures.md there says what each file of a failure holds.\n",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("reconciler-fuzzer --help lacks %q:\n%s", want, got)
		}
	}
	if strings.Contains(got, "matrix") {
		t.Errorf("reconciler-fuzzer --help names the toy's matrix command:\n%s", got)
	}
}

// A drawn sequence injects no fault, so the help promises none.
func TestTheTopLevelHelpSaysReconcilerFuzzerRunsARealAPIServer(t *testing.T) {
	got := strings.Join(strings.Fields(help("")), " ")
	if want := "runs your controller against a real kube-apiserver and etcd"; !strings.Contains(got, want) {
		t.Errorf("reconciler-fuzzer --help does not say it %q:\n%s", want, help(""))
	}
	if says := "injects faults"; strings.Contains(got, says) {
		t.Errorf("reconciler-fuzzer --help says it %q:\n%s", says, help(""))
	}
}

func TestAUsageErrorPrintsTheUsage(t *testing.T) {
	for _, test := range []struct {
		name        string
		args        []string
		error, says string
	}{
		{name: "an unknown flag", args: []string{"run", "--target", toyTargetYAML, "--bogus"},
			error: "flag provided but not defined: --bogus", says: usage("run")},
		{name: "an unknown short flag", args: []string{"run", "-bogus"},
			error: "flag provided but not defined: --bogus", says: usage("run")},
		{name: "a flag without its value", args: []string{"run", "--target"},
			error: "flag needs an argument: --target", says: usage("run")},
		{name: "a flag with a bad value", args: []string{"run", "--runs", "-x"},
			error: `invalid value "-x" for flag --runs: parse error`, says: usage("run")},
		{name: "a bad value with a dash inside", args: []string{"run", "--runs", "1 -x"},
			error: `invalid value "1 -x" for flag --runs: parse error`, says: usage("run")},
		{name: "no target", args: []string{"run"}, error: "the --target flag is required", says: usage("run")},
		{name: "replay without a sequence", args: []string{"replay", "--target", toyTargetYAML},
			error: "replay takes one sequence file, and 0 were given", says: usage("replay")},
		{name: "matrix without its sequences", args: []string{"matrix", "--target", toyTargetYAML},
			error: "the --sequences flag is required", says: usage("matrix")},
		{name: "matrix with a sequence file", args: []string{"matrix", "--target", toyTargetYAML, "--sequences", "d", "s.json"},
			error: "matrix takes no sequence file, since it runs the --sequences directory: s.json", says: usage("matrix")},
		{name: "version with an argument", args: []string{"version", "extra"},
			error: "version takes no argument: extra", says: usage("version")},
		{name: "a flag after a sequence file", args: []string{"run", "s.json", "--target", toyTargetYAML},
			error: "--target follows s.json, and flags go first", says: usage("run")},
		{name: "a short flag after a sequence file", args: []string{"run", "s.json", "-target", toyTargetYAML},
			error: "-target follows s.json, and flags go first", says: usage("run")},
		{name: "a flag after an argument", args: []string{"version", "extra", "-x"},
			error: "-x follows extra, and flags go first", says: usage("version")},
		{name: "-- after a sequence file", args: []string{"run", "--target", toyTargetYAML, "s.json", "--", "t.json"},
			error: "-- follows s.json, and it goes before the first sequence file", says: usage("run")},
		{name: "an unknown command", args: []string{"frobnicate"}, error: `"frobnicate" is not a reconciler-fuzzer command`, says: usage("")},
		{name: "help on an unknown command", args: []string{"help", "frobnicate"}, error: `"frobnicate" is not a reconciler-fuzzer command`, says: usage("")},
		{name: "help on two commands", args: []string{"help", "run", "replay"}, error: "help takes one command, and 2 were given", says: usage("")},
	} {
		t.Run(test.name, func(t *testing.T) {
			code, stdout, stderr := invoke(t, &fakeSession{}, test.args...)

			if code != exitError {
				t.Errorf("reconciler-fuzzer %v exited %d, want %d.", test.args, code, exitError)
			}
			if want := "reconciler-fuzzer: " + test.error + "\n" + test.says; stderr != want || stdout != "" {
				t.Errorf("reconciler-fuzzer %v wrote\n%s\nto stderr and %q to stdout, want\n%s", test.args, stderr, stdout, want)
			}
		})
	}
}

func TestABareReconcilerFuzzerPrintsItsHelpAndExitsTwo(t *testing.T) {
	code, stdout, stderr := invoke(t, &fakeSession{})

	if code != exitError || stdout != "" || stderr != help("") {
		t.Errorf("reconciler-fuzzer exited %d and wrote %q to stdout and\n%s\nto stderr, want %d, nothing and its help.", code, stdout, stderr, exitError)
	}
}

func TestTheUsageOfACommandIsItsSynopsis(t *testing.T) {
	if got, want := usage("version"), "Usage:\n  reconciler-fuzzer version\n"; got != want {
		t.Errorf("The usage of reconciler-fuzzer version is\n%s\nwant\n%s", got, want)
	}
	if got, want := usage("replay"), "Usage:\n  "+replaySynopsis(t)+"\nRun 'reconciler-fuzzer replay --help' for its flags.\n"; got != want {
		t.Errorf("The usage of reconciler-fuzzer replay is\n%s\nwant\n%s", got, want)
	}
	if got := usage(""); !strings.Contains(got, "\n  replay ") || strings.Contains(got, "matrix") {
		t.Errorf("The usage of reconciler-fuzzer is\n%s\nwant the commands users run.", got)
	}
}

// DESIGN.md lists every command's synopsis in a fenced block under its CLI
// convention. A flag or a command missing there fails this test.
func TestTheDesignGivesEachCommandsSynopsis(t *testing.T) {
	design, err := os.ReadFile("../../DESIGN.md")
	if err != nil {
		t.Fatal(err)
	}
	_, convention, found := strings.Cut(string(design), "\n- **CLI.**")
	if !found {
		t.Fatal("DESIGN.md has no CLI convention.")
	}
	convention, _, _ = strings.Cut(convention, "\n- **")
	_, block, opened := strings.Cut(convention, "```\n")
	block, _, closed := strings.Cut(block, "```")
	if !opened || !closed {
		t.Fatal("DESIGN.md's CLI convention has no fenced block.")
	}
	var listed, want []string
	for _, line := range strings.Split(strings.TrimSpace(block), "\n") {
		listed = append(listed, strings.TrimSpace(line))
	}
	for _, c := range commands {
		want = append(want, c.synopsis())
	}
	if !slices.Equal(listed, want) {
		t.Errorf("DESIGN.md gives the synopses\n%s\nwant\n%s", strings.Join(listed, "\n"), strings.Join(want, "\n"))
	}
}

func replaySynopsis(t *testing.T) string {
	t.Helper()
	c, found := lookup("replay")
	if !found {
		t.Fatal("reconciler-fuzzer has no replay command.")
	}
	return c.synopsis()
}

func firstLines(s string, n int) string {
	return strings.Join(strings.SplitN(s, "\n", n+1)[:min(n, strings.Count(s, "\n"))], "\n")
}
