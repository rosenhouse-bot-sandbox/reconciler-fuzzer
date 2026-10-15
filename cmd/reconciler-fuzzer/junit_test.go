package main

import (
	"context"
	"encoding/xml"
	"errors"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/rosenhouse-bot-sandbox/reconciler-fuzzer/internal/run"
	"github.com/rosenhouse-bot-sandbox/reconciler-fuzzer/internal/target"
)

// parsedSuites is a JUnit XML file as a CI server reads it.
type parsedSuites struct {
	XMLName  xml.Name      `xml:"testsuites"`
	Tests    int           `xml:"tests,attr"`
	Failures int           `xml:"failures,attr"`
	Errors   int           `xml:"errors,attr"`
	Time     *string       `xml:"time,attr"`
	Suites   []parsedSuite `xml:"testsuite"`
}

type parsedSuite struct {
	Name       string  `xml:"name,attr"`
	Tests      int     `xml:"tests,attr"`
	Failures   int     `xml:"failures,attr"`
	Errors     int     `xml:"errors,attr"`
	Skipped    int     `xml:"skipped,attr"`
	Time       *string `xml:"time,attr"`
	Timestamp  string  `xml:"timestamp,attr"`
	Properties []struct {
		Name  string `xml:"name,attr"`
		Value string `xml:"value,attr"`
	} `xml:"properties>property"`
	Cases []parsedCase `xml:"testcase"`
}

type parsedCase struct {
	Name      string         `xml:"name,attr"`
	Classname string         `xml:"classname,attr"`
	Failure   *parsedProblem `xml:"failure"`
	Error     *parsedProblem `xml:"error"`
	Skipped   *parsedProblem `xml:"skipped"`
	SystemOut string         `xml:"system-out"`
}

type parsedProblem struct {
	Type    string `xml:"type,attr"`
	Message string `xml:"message,attr"`
	Body    string `xml:",chardata"`
}

// readJUnit reads the file's one testsuite and the counts it claims, which
// the file's root claims too.
func readJUnit(t *testing.T, path string) (parsedSuite, map[string]int) {
	t.Helper()
	encoded, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reconciler-fuzzer wrote no JUnit file: %v", err)
	}
	var written parsedSuites
	if err := xml.Unmarshal(encoded, &written); err != nil {
		t.Fatalf("The JUnit file does not parse: %v\n%s", err, encoded)
	}
	if len(written.Suites) != 1 {
		t.Fatalf("The JUnit file holds %d testsuites, want one:\n%s", len(written.Suites), encoded)
	}
	suite := written.Suites[0]
	if written.Tests != suite.Tests || written.Failures != suite.Failures || written.Errors != suite.Errors {
		t.Errorf("The JUnit file's root claims %d tests, %d failures and %d errors, and its testsuite %d, %d and %d.",
			written.Tests, written.Failures, written.Errors, suite.Tests, suite.Failures, suite.Errors)
	}
	if timeOf(written.Time) != timeOf(suite.Time) {
		t.Errorf("The JUnit file's root takes %s, and its testsuite %s.", timeOf(written.Time), timeOf(suite.Time))
	}
	return suite, map[string]int{"tests": suite.Tests, "failures": suite.Failures, "errors": suite.Errors, "skipped": suite.Skipped}
}

// timeOf says what a time attribute gives.
func timeOf(attr *string) string {
	if attr == nil {
		return "no time"
	}
	return *attr + " s"
}

// counted is what the testcases hold, which the testsuite's counts must say.
func counted(suite parsedSuite) map[string]int {
	counts := map[string]int{"tests": len(suite.Cases), "failures": 0, "errors": 0, "skipped": 0}
	for _, c := range suite.Cases {
		switch {
		case c.Failure != nil:
			counts["failures"]++
		case c.Error != nil:
			counts["errors"]++
		case c.Skipped != nil:
			counts["skipped"]++
		}
	}
	return counts
}

func TestJUnitHasATestcasePerPlannedRun(t *testing.T) {
	g3 := run.Violation{ID: "G3", Statement: "the target deletes what it manages", Evidence: "the v1/ConfigMap widget-0 was still there"}
	note := "G3 is not evaluated for the deletion of widget"
	// XML 1.0 has no escape for a terminal's color codes.
	colored := "the target exited with exit status 2 after writing \"\x1b[31mpanic\x1b[0m\""
	session := &fakeSession{results: []run.Result{{Notes: []string{note, colored}}, {Violation: &g3}}}
	junit := filepath.Join(t.TempDir(), "junit.xml")

	code, _, stderr := invoke(t, session, "run", "--target", toyTargetYAML, "--out", t.TempDir(), "--runs", "3", "--seed", "7", "--junit", junit)

	if code != exitViolation {
		t.Fatalf("reconciler-fuzzer run exited %d, want %d: %s", code, exitViolation, stderr)
	}
	suite, claimed := readJUnit(t, junit)
	if want := map[string]int{"tests": 4, "failures": 1, "errors": 0, "skipped": 2}; !maps.Equal(claimed, want) || !maps.Equal(counted(suite), want) {
		t.Errorf("The testsuite claims %v and holds %v, want %v.", claimed, counted(suite), want)
	}
	var names []string
	for _, c := range suite.Cases {
		names = append(names, c.Name)
		if c.Classname != "toy-widget" {
			t.Errorf("The testcase %q is of the class %q, want toy-widget.", c.Name, c.Classname)
		}
	}
	if want := []string{"run 1: seed 7", "run 2: seed 8", "run 3: seed 9", "run 4: baseline"}; !slices.Equal(names, want) {
		t.Fatalf("The testcases are %q, want %q.", names, want)
	}
	if suite.Name != "toy-widget" {
		t.Errorf("The testsuite is %q, want toy-widget.", suite.Name)
	}
	passed, failed, unrun := suite.Cases[0], suite.Cases[1], suite.Cases[2]
	if passed.Failure != nil || passed.Error != nil || passed.Skipped != nil || !strings.Contains(passed.SystemOut, note) {
		t.Errorf("Run 1 reads as %+v, want a pass whose output carries the note %q.", passed, note)
	}
	if f := failed.Failure; f == nil || f.Type != "G3" || f.Message != g3.Statement ||
		!strings.Contains(f.Body, g3.Evidence) || !strings.Contains(f.Body, session.dirs[1]) {
		t.Errorf("Run 2 failed with %+v, want G3, its statement, its evidence and %s.", f, session.dirs[1])
	}
	if unrun.Skipped == nil {
		t.Errorf("Run 3 reads as %+v, want it skipped.", unrun)
	}
}

func TestJUnitCountsWhatStoppedTheInvocationAsAnError(t *testing.T) {
	for _, test := range []struct {
		name    string
		session func(cancel context.CancelCauseFunc) *fakeSession
		args    []string
		// errored is the testcase that errs, and says so in its message.
		errored, kind, message string
		counts                 map[string]int
	}{
		{name: "a run that did not finish",
			session: func(context.CancelCauseFunc) *fakeSession {
				return &fakeSession{failures: []error{nil, errors.New("the control plane did not start")}}
			},
			errored: "run 2: seed 8", kind: "error", message: "the control plane did not start",
			counts: map[string]int{"tests": 3, "failures": 0, "errors": 1, "skipped": 1}},
		{name: "an interrupted run",
			session: func(cancel context.CancelCauseFunc) *fakeSession {
				s := &fakeSession{failures: []error{fmt.Errorf("op 0 (settle): %w", context.Canceled)}}
				s.after = func() { cancel(interrupt{syscall.SIGTERM}) }
				return s
			},
			errored: "run 1: seed 7", kind: "interrupted", message: "an interrupt stopped the run",
			counts: map[string]int{"tests": 3, "failures": 0, "errors": 1, "skipped": 2}},
		{name: "the deadline between runs",
			session: func(context.CancelCauseFunc) *fakeSession { return &fakeSession{} },
			args:    []string{"--deadline", "1ns"},
			errored: "reconciler-fuzzer", kind: "error", message: "the --deadline of 1ns stopped the invocation after 1 of 3 runs",
			counts: map[string]int{"tests": 4, "failures": 0, "errors": 1, "skipped": 2}},
		{name: "an interrupt between runs",
			session: func(cancel context.CancelCauseFunc) *fakeSession {
				s := &fakeSession{}
				s.after = func() { cancel(interrupt{syscall.SIGINT}) }
				return s
			},
			errored: "reconciler-fuzzer", kind: "interrupted", message: "an interrupt stopped the invocation after 1 of 3 runs",
			counts: map[string]int{"tests": 4, "failures": 0, "errors": 1, "skipped": 2}},
	} {
		t.Run(test.name, func(t *testing.T) {
			ctx, cancel := context.WithCancelCause(t.Context())
			defer cancel(nil)
			session := test.session(cancel)
			junit := filepath.Join(t.TempDir(), "junit.xml")
			args := slices.Concat([]string{"run", "--target", toyTargetYAML, "--out", t.TempDir(), "--runs", "3", "--seed", "7", "--no-baseline", "--junit", junit}, test.args)

			invokeCtx(t, ctx, session, countingGenerator(nil), args...)

			suite, claimed := readJUnit(t, junit)
			if !maps.Equal(claimed, test.counts) || !maps.Equal(counted(suite), test.counts) {
				t.Errorf("The testsuite claims %v and holds %v, want %v.", claimed, counted(suite), test.counts)
			}
			i := slices.IndexFunc(suite.Cases, func(c parsedCase) bool { return c.Name == test.errored })
			if i < 0 {
				t.Fatalf("The testcases are %+v, want one named %q.", suite.Cases, test.errored)
			}
			if e := suite.Cases[i].Error; e == nil || e.Type != test.kind || !strings.Contains(e.Message, test.message) {
				t.Errorf("%s errs with %+v, want a %s saying %q.", test.errored, e, test.kind, test.message)
			}
			if len(session.dirs) > 0 && strings.HasPrefix(test.errored, "run") &&
				!strings.Contains(suite.Cases[i].Error.Body, session.dirs[len(session.dirs)-1]) {
				t.Errorf("%s errs with %+v, which does not name its directory.", test.errored, suite.Cases[i].Error)
			}
		})
	}
}

// A JUnit file an earlier invocation left would otherwise read as this one's.
func TestJUnitSaysWhatStoppedAnInvocationBeforeItsRuns(t *testing.T) {
	bogus := filepath.Join(t.TempDir(), "bogus.json")
	if err := os.WriteFile(bogus, []byte(`{"seed": 1, "target": "toy-widget", "ops": [{"type": "bogus"}]}`), 0o644); err != nil {
		t.Fatal(err)
	}
	taken := filepath.Join(t.TempDir(), "taken")
	if err := os.WriteFile(taken, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	undrawable := func(*target.Target) (Generator, error) {
		return nil, errors.New("the CRD has no schema")
	}
	for _, test := range []struct {
		name, suite, message string
		generator            func(*target.Target) (Generator, error)
		args                 []string
	}{
		{name: "a flag reconciler-fuzzer refuses", suite: "reconciler-fuzzer", message: "--runs is 0",
			args: []string{"run", "--target", toyTargetYAML, "--runs", "0"}},
		{name: "a target reconciler-fuzzer cannot read", suite: "reconciler-fuzzer", message: "absent.yaml",
			args: []string{"run", "--target", "absent.yaml"}},
		{name: "a sequence reconciler-fuzzer cannot read", suite: "toy-widget", message: "bogus",
			args: []string{"replay", "--target", toyTargetYAML, bogus}},
		{name: "a sequence reconciler-fuzzer cannot draw", suite: "toy-widget", message: "the CRD has no schema", generator: undrawable,
			args: []string{"run", "--target", toyTargetYAML}},
		{name: "an output directory reconciler-fuzzer cannot make", suite: "toy-widget", message: "creating the output directory",
			args: []string{"run", "--target", toyTargetYAML, "--out", taken}},
	} {
		t.Run(test.name, func(t *testing.T) {
			junit := filepath.Join(t.TempDir(), "junit.xml")
			if err := os.WriteFile(junit, []byte("<testsuites></testsuites>\n"), 0o644); err != nil {
				t.Fatal(err)
			}
			berlin := time.FixedZone("CEST", 2*60*60)
			now := time.Date(2026, 9, 24, 3, 2, 3, 0, berlin)
			session := &fakeSession{now: func() time.Time { return now }}
			generator := countingGenerator(nil)
			if test.generator != nil {
				generator = test.generator
			}

			code, _, stderr := invokeWith(t, session, generator, slices.Concat(test.args[:1], []string{"--junit", junit}, test.args[1:])...)

			if code != exitError {
				t.Fatalf("reconciler-fuzzer exited %d, want %d: %s", code, exitError, stderr)
			}
			suite, _ := readJUnit(t, junit)
			if suite.Name != test.suite || suite.Timestamp != "2026-09-24T01:02:03" || len(suite.Properties) != 1 || suite.Properties[0].Name != "reconciler-fuzzer" {
				t.Errorf("The testsuite is %q at %s, with the properties %+v, want %s at 2026-09-24T01:02:03 UTC, with reconciler-fuzzer's version alone.",
					suite.Name, suite.Timestamp, suite.Properties, test.suite)
			}
			if timeOf(suite.Time) != "0.000 s" {
				t.Errorf("The testsuite takes %s, want 0.000 s, since reconciler-fuzzer stopped when it started.", timeOf(suite.Time))
			}
			if len(suite.Cases) != 1 {
				t.Fatalf("The testcases are %+v, want one.", suite.Cases)
			}
			if c := suite.Cases[0]; c.Name != "reconciler-fuzzer" || c.Classname != test.suite || c.Error == nil || c.Error.Type != "error" ||
				!strings.Contains(c.Error.Message, test.message) {
				t.Errorf("The testcase is %+v, want reconciler-fuzzer of %s erring with %q.", c, test.suite, test.message)
			}
		})
	}
}

func TestAJUnitFileGoesWhereItsNameSays(t *testing.T) {
	junit := filepath.Join(t.TempDir(), "reports", "reconciler-fuzzer.xml")

	code, _, stderr := invoke(t, &fakeSession{}, "replay", "--target", toyTargetYAML, "--out", t.TempDir(), "--junit", junit, writeSequence(t, 1))

	if code != exitOK {
		t.Fatalf("reconciler-fuzzer replay exited %d: %s", code, stderr)
	}
	if suite, _ := readJUnit(t, junit); len(suite.Cases) != 1 {
		t.Errorf("The testcases are %+v, want the one run.", suite.Cases)
	}
}

func TestAJUnitFileThatCannotBeWrittenOnlyWarns(t *testing.T) {
	file := filepath.Join(t.TempDir(), "file")
	if err := os.WriteFile(file, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	junit := filepath.Join(file, "junit.xml")
	for _, test := range []struct {
		name string
		args []string
		code int
	}{
		{"a run that passed", []string{"replay", "--target", toyTargetYAML, "--out", t.TempDir(), "--junit", junit, writeSequence(t, 1)}, exitOK},
		{"an invocation that stopped before its runs", []string{"run", "--target", "absent.yaml", "--junit", junit}, exitError},
	} {
		t.Run(test.name, func(t *testing.T) {
			code, _, stderr := invoke(t, &fakeSession{}, test.args...)

			if code != test.code {
				t.Errorf("reconciler-fuzzer exited %d, want %d.", code, test.code)
			}
			if strings.Count(stderr, "reconciler-fuzzer: writing "+junit) != 1 || strings.Contains(stderr, ".junit.xml.") {
				t.Errorf("reconciler-fuzzer printed %q on stderr, want it to say once that it could not write %s, and name no temporary file.", stderr, junit)
			}
		})
	}
}

func TestAnInvocationWithoutJUnitWritesNone(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)

	_, _, stderr := invoke(t, &fakeSession{}, "run", "--target", "absent.yaml")

	if lines := strings.Count(stderr, "\n"); lines != 1 {
		t.Errorf("reconciler-fuzzer printed\n%s\nwant the one line that says why it stopped.", stderr)
	}
	if entries, _ := os.ReadDir(dir); len(entries) != 0 {
		t.Errorf("reconciler-fuzzer wrote %v.", entries)
	}
}

func TestAJUnitFailureWithNoEvidenceNamesTheDirectory(t *testing.T) {
	s := sampleSummary(t)
	s.Runs[1].Violation.Evidence = ""
	junit := filepath.Join(t.TempDir(), "junit.xml")

	if err := s.writeJUnit(junit, "reconciler-fuzzer-out"); err != nil {
		t.Fatal(err)
	}

	suite, _ := readJUnit(t, junit)
	if f := suite.Cases[1].Failure; f == nil || f.Body != "reconciler-fuzzer-out/run-2 holds the report and the evidence." {
		t.Errorf("Run 2 failed with %+v, want its directory named and nothing else.", f)
	}
}

func TestJUnitKeepsItsForm(t *testing.T) {
	encoded, err := sampleSummary(t).junit(filepath.Join("reconciler-fuzzer-out", "20260924T010203Z-7"))
	if err != nil {
		t.Fatal(err)
	}

	checkGolden(t, "testdata/junit.golden.xml", encoded)
}
