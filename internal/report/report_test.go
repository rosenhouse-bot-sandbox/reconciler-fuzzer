package report_test

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"

	"github.com/rosenhouse-bot-sandbox/reconciler-fuzzer/internal/invariant"
	"github.com/rosenhouse-bot-sandbox/reconciler-fuzzer/internal/observe"
	"github.com/rosenhouse-bot-sandbox/reconciler-fuzzer/internal/proxy"
	"github.com/rosenhouse-bot-sandbox/reconciler-fuzzer/internal/report"
)

const replayCommand = "reconciler-fuzzer replay --target targets/toy-widget/target.yaml reconciler-fuzzer-out/20260921T055744Z-23/run-1/sequence.json"

// sequence is one canonical sequence as run.Sequence.Marshal writes it.
var sequence = json.RawMessage(`{
  "seed": 23,
  "target": "toy-widget",
  "ops": [
    {
      "i": 0,
      "t": "create"
    }
  ]
}
`)

func failingRun() report.Report {
	return report.Report{
		Check: report.Check{
			ID:        "G3",
			Statement: "the v1/ConfigMap widget-0 was still there 10s after the CR was deleted",
			Evidence:  "at 2026-09-21T05:59:08.980624165Z; 1 version, the first v1/ConfigMap widget-0",
		},
		Target:           report.Target{Name: "toy-widget", Version: "v0.1.0"},
		ReconcilerFuzzer: "v1.2.3",
		Seed:             23,
		Replay:           replayCommand,
		Sequence:         sequence,
	}
}

func TestReportLeadsWithTheFailureAndHowToReproduceIt(t *testing.T) {
	failure := failingRun()

	md, _ := write(t, failure)

	lines := strings.Split(md, "\n")
	if len(lines) < 10 {
		t.Fatalf("The report is %d lines:\n%s", len(lines), md)
	}
	opening := strings.Join(lines[:10], "\n")
	for _, want := range []string{failure.Check.ID, failure.Target.Name, failure.Target.Version,
		failure.Check.Statement, failure.Check.Evidence, failure.Replay} {
		if !strings.Contains(opening, want) {
			t.Errorf("The report's first ten lines do not name %q:\n%s", want, opening)
		}
	}
	if want := "reconciler-fuzzer v1.2.3 exercised toy-widget v0.1.0 on seed 23."; !strings.Contains(md, want) {
		t.Errorf("The report does not say %q:\n%s", want, md)
	}
}

func TestReportLinksTheFailingChecksSection(t *testing.T) {
	for id, anchor := range map[string]string{"G3": "g3-clean-deletion", "P1": "properties"} {
		failure := failingRun()
		failure.Check.ID = id

		md, _ := write(t, failure)

		if want := "](https://github.com/rosenhouse-bot-sandbox/reconciler-fuzzer/blob/main/docs/checks.md#" + anchor + ")"; !strings.Contains(md, want) {
			t.Errorf("A %s report does not link %q:\n%s", id, want, md)
		}
	}
}

func TestReportEmbedsTheSequenceSoItRoundTrips(t *testing.T) {
	md, encoded := write(t, failingRun())

	if got := fencedJSON(t, md); !sameJSON(t, got, string(sequence)) {
		t.Errorf("The report embeds the sequence as\n%s\nwant\n%s", got, sequence)
	}
	if got := field(t, encoded, "sequence"); !sameJSON(t, got, string(sequence)) {
		t.Errorf("report.json holds the sequence as\n%s\nwant\n%s", got, sequence)
	}
}

func TestReportJSONCarriesWhatFailedAndWhatItRanAgainst(t *testing.T) {
	failure := failingRun()

	_, encoded := write(t, failure)

	var got struct {
		Check            report.Check  `json:"check"`
		Target           report.Target `json:"target"`
		ReconcilerFuzzer string        `json:"reconcilerFuzzer"`
		Seed             int64         `json:"seed"`
		Replay           string        `json:"replay"`
	}
	if err := json.Unmarshal([]byte(encoded), &got); err != nil {
		t.Fatalf("report.json does not parse: %v\n%s", err, encoded)
	}
	if got.Check != failure.Check {
		t.Errorf("report.json names the check %+v, want %+v.", got.Check, failure.Check)
	}
	if got.Target != failure.Target {
		t.Errorf("report.json names the target %+v, want %+v.", got.Target, failure.Target)
	}
	if got.ReconcilerFuzzer != failure.ReconcilerFuzzer {
		t.Errorf("report.json names reconciler-fuzzer %q, want %q.", got.ReconcilerFuzzer, failure.ReconcilerFuzzer)
	}
	if got.Seed != failure.Seed {
		t.Errorf("report.json names seed %d, want %d.", got.Seed, failure.Seed)
	}
	if got.Replay != failure.Replay {
		t.Errorf("report.json names the replay command %q, want %q.", got.Replay, failure.Replay)
	}
}

func TestReportJSONIsIndentedNewlineTerminatedAndUnescaped(t *testing.T) {
	failure := failingRun()
	failure.Check.ID = "P1"
	failure.Check.Statement = `!has(status.ready) || status.ready <= managed.size()`

	_, encoded := write(t, failure)

	if !strings.HasSuffix(encoded, "}\n") {
		t.Errorf("report.json does not end with a newline:\n%q", encoded)
	}
	if !strings.Contains(encoded, "\n  \"seed\": 23,") {
		t.Errorf("report.json is not indented two spaces:\n%s", encoded)
	}
	if !strings.Contains(encoded, failure.Check.Statement) {
		t.Errorf("report.json escapes the statement, so it reads:\n%s", encoded)
	}
}

func TestReportQuotesTheRequestsAndVersionsTheViolationNamed(t *testing.T) {
	failure := failingRun()
	failure.Requests = []proxy.Request{{
		Start: time.Date(2026, 9, 21, 5, 59, 8, 980624165, time.UTC),
		Verb:  "create", Path: "/api/v1/namespaces/reconciler-fuzzer-run-x/configmaps", Status: 500, Fault: "error(500)",
	}}
	failure.Versions = []observe.Version{{
		Key:             observe.Key{GVK: schema.GroupVersionKind{Group: "cert-manager.io", Version: "v1", Kind: "CertificateRequest"}, Name: "widget-0"},
		Time:            time.Date(2026, 9, 21, 5, 59, 9, 0, time.UTC),
		ResourceVersion: "812",
		// A check reading a Ready predicate reads these, so a report quotes them.
		Generation:         4,
		ObservedGeneration: ptr(int64(3)),
		Finalizers:         []string{"widget.reconciler-fuzzer/cleanup"},
		DeletionTimestamp:  &metav1.Time{Time: time.Date(2026, 9, 21, 5, 59, 8, 0, time.UTC)},
		Deleted:            true,
	}, {
		Key:             observe.Key{GVK: schema.GroupVersionKind{Version: "v1", Kind: "ConfigMap"}, Name: "widget-0-0"},
		Time:            time.Date(2026, 9, 21, 5, 59, 10, 0, time.UTC),
		ResourceVersion: "813",
	}}

	md, encoded := write(t, failure)

	for _, excerpt := range []struct {
		heading string
		wants   []string
	}{
		{"Requests", []string{"The violation quotes 1 request.", "requests.jsonl",
			"| start | verb | path | status | fault |\n| --- | --- | --- | --- | --- |\n",
			"| 2026-09-21T05:59:08.980624165Z | create | /api/v1/namespaces/reconciler-fuzzer-run-x/configmaps | 500 | error(500) |"}},
		{"Object versions", []string{"The violation quotes 2 versions.", "objects.jsonl",
			"| time | kind | name | resourceVersion | generation | observed | finalizers | deletionTimestamp | deleted |",
			"| 2026-09-21T05:59:09Z | cert-manager.io/v1/CertificateRequest | widget-0 | 812 | 4 | 3 | widget.reconciler-fuzzer/cleanup | 2026-09-21T05:59:08Z | yes |",
			"| 2026-09-21T05:59:10Z | v1/ConfigMap | widget-0-0 | 813 | 0 |  |  |  |  |"}},
	} {
		body := section(md, excerpt.heading)
		for _, want := range excerpt.wants {
			if !strings.Contains(body, want) {
				t.Errorf("The report's %s do not name %q:\n%s", excerpt.heading, want, body)
			}
		}
		if strings.Contains(body, "the last") {
			t.Errorf("The report says it bounded evidence it quoted whole:\n%s", body)
		}
	}
	if got := field(t, encoded, "requests"); !strings.Contains(got, "error(500)") {
		t.Errorf("report.json quotes the requests as %s", got)
	}
	if got := field(t, encoded, "versions"); !strings.Contains(got, "widget.reconciler-fuzzer/cleanup") {
		t.Errorf("report.json quotes the versions as %s", got)
	}
}

// reconciler-fuzzer's garbage collector runs only on envtest, and its deletes
// never reach requests.jsonl.
func TestReportNamesTheCollectorsDeletesWhereItRan(t *testing.T) {
	for _, tc := range []struct {
		name      string
		collector bool
		want      string
	}{
		{"on a cluster with its own garbage collector", false,
			"The run directory holds `requests.jsonl`, `objects.jsonl` and `target.log`.\n"},
		{"on envtest", true,
			"The run directory holds `requests.jsonl`, `objects.jsonl`, `collector.jsonl` and `target.log`. " +
				"`collector.jsonl` holds each delete that reconciler-fuzzer's garbage collector tried.\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			failure := failingRun()
			failure.Collector = tc.collector

			md, _ := write(t, failure)

			if !strings.Contains(md, "reconciler-fuzzer v1.2.3 exercised toy-widget v0.1.0 on seed 23. "+tc.want) {
				t.Errorf("The report does not say %q:\n%s", tc.want, md)
			}
		})
	}
}

// Only report.md says which recordings the run directory holds.
func TestReportJSONIsTheSameWhetherTheCollectorRan(t *testing.T) {
	ran := failingRun()
	ran.Collector = true

	_, with := write(t, ran)
	_, without := write(t, failingRun())

	if with != without {
		t.Errorf("report.json holds\n%s\nwhere the collector ran, and\n%s\nwhere it did not.", with, without)
	}
}

func TestReportNamesWhatRanWhereNoVersionIsDeclared(t *testing.T) {
	failure := failingRun()
	failure.Target.Version, failure.ReconcilerFuzzer = "", ""

	md, encoded := write(t, failure)

	if want := "# G3 failed on toy-widget\n"; !strings.Contains(md, want) {
		t.Errorf("The report opens with %q, want %q.", strings.SplitN(md, "\n", 2)[0], want)
	}
	if want := "The run exercised toy-widget on seed 23."; !strings.Contains(md, want) {
		t.Errorf("The report does not say %q:\n%s", want, md)
	}
	for _, absent := range []string{"reconciler-fuzzer", "version"} {
		if strings.Contains(encoded, `"`+absent+`"`) {
			t.Errorf("report.json holds an empty %q:\n%s", absent, encoded)
		}
	}
}

// No seed draws the baseline, so its report names the baseline instead.
func TestAReportOfTheBaselineSaysSo(t *testing.T) {
	failure := failingRun()
	failure.Seed, failure.Baseline = 0, true

	md, encoded := write(t, failure)

	if want := "reconciler-fuzzer v1.2.3 exercised toy-widget v0.1.0 with the baseline."; !strings.Contains(md, want) {
		t.Errorf("The report does not say %q:\n%s", want, md)
	}
	if !strings.Contains(encoded, "\n  \"baseline\": true,") {
		t.Errorf("report.json does not mark the baseline:\n%s", encoded)
	}
}

// A check that judged nothing reads like one that passed, so the report names
// what each check could not judge (DESIGN.md §6, D31).
func TestReportNamesWhatTheChecksCouldNotJudge(t *testing.T) {
	failure := failingRun()
	failure.Notes = []string{
		"G3 did not judge the deletion: the run ended before the deadline.",
		"G5 skipped the restart at op 3: a converged snapshot is missing.",
	}

	md, encoded := write(t, failure)

	want := "## Notes\n\n- " + failure.Notes[0] + "\n- " + failure.Notes[1] + "\n"
	if !strings.Contains(md, want) {
		t.Errorf("The report's notes read\n%s\nwant\n%s", section(md, "Notes"), want)
	}
	var held []string
	if err := json.Unmarshal([]byte(field(t, encoded, "notes")), &held); err != nil {
		t.Fatalf("report.json's notes do not parse: %v", err)
	}
	if !reflect.DeepEqual(held, failure.Notes) {
		t.Errorf("report.json holds the notes %q, want %q.", held, failure.Notes)
	}
}

func TestReportBoundsTheEvidenceAndSaysHowMuchThereWas(t *testing.T) {
	const quoted = 25
	failure := failingRun()
	for i := range quoted {
		failure.Requests = append(failure.Requests, proxy.Request{Verb: "get", Path: fmt.Sprintf("/api/v1/path-%d", i)})
		failure.Versions = append(failure.Versions, observe.Version{
			Key: observe.Key{GVK: schema.GroupVersionKind{Version: "v1", Kind: "ConfigMap"}, Name: fmt.Sprintf("widget-%d", i)},
		})
	}

	md, encoded := write(t, failure)

	for _, excerpt := range []struct{ heading, key, nearest, dropped, row string }{
		{"Requests", "requests", "/api/v1/path-24", "/api/v1/path-4", "/api/v1/path-"},
		{"Object versions", "versions", "widget-24", "widget-4", "widget-"},
	} {
		body := section(md, excerpt.heading)
		if !strings.Contains(body, excerpt.nearest) {
			t.Errorf("The report drops the %s nearest the violation:\n%s", excerpt.key, body)
		}
		if strings.Contains(body, excerpt.dropped) {
			t.Errorf("The report quotes %s, which the bound drops as the furthest away:\n%s", excerpt.dropped, body)
		}
		if rows := strings.Count(body, excerpt.row); rows != 20 {
			t.Errorf("The report quotes %d %s, want 20.", rows, excerpt.key)
		}
		if want := fmt.Sprintf("The violation quotes the last 20 of %d %s.", quoted, excerpt.key); !strings.Contains(body, want) {
			t.Errorf("The report does not say %q:\n%s", want, body)
		}
		var held []json.RawMessage
		if err := json.Unmarshal([]byte(field(t, encoded, excerpt.key)), &held); err != nil {
			t.Fatalf("report.json's %s do not parse: %v", excerpt.key, err)
		}
		if len(held) != 20 {
			t.Errorf("report.json holds %d %s, want 20.", len(held), excerpt.key)
		}
		if total := field(t, encoded, excerpt.key+"Total"); total != fmt.Sprint(quoted) {
			t.Errorf("report.json says %s of %s, want %d.", total, excerpt.key, quoted)
		}
	}
}

// The two kinds of row a readiness verdict carries are tables of their own, so
// a reader can tell a history from a state (#24).
func TestReportQuotesTheTimelineAndTheStateApart(t *testing.T) {
	failure := failingRun()
	failure.Versions = []observe.Version{{
		Key:             observe.Key{GVK: schema.GroupVersionKind{Group: "toy.reconciler-fuzzer", Version: "v1", Kind: "Widget"}, Name: "widget"},
		ResourceVersion: "219",
	}}
	failure.Managed = []observe.Version{{
		Key:             observe.Key{GVK: schema.GroupVersionKind{Version: "v1", Kind: "ConfigMap"}, Name: "widget-0"},
		ResourceVersion: "221",
	}}
	failure.ManagedTotal = ptr(1)
	failure.VersionsOf = "toy.reconciler-fuzzer/v1/Widget widget"

	md, encoded := write(t, failure)

	if want := "The violation quotes 1 version of `toy.reconciler-fuzzer/v1/Widget widget`."; !strings.Contains(md, want) {
		t.Errorf("The report does not say %q:\n%s", want, md)
	}
	if got := field(t, encoded, "versionsOf"); got != `"toy.reconciler-fuzzer/v1/Widget widget"` {
		t.Errorf("report.json says the timeline is of %s, want the CR it quotes.", got)
	}
	timeline, state := section(md, "Object versions"), section(md, "Managed objects at the verdict")
	if !strings.Contains(timeline, "| widget | 219 |") || strings.Contains(timeline, "widget-0") {
		t.Errorf("The timeline is\n%s\nwant the CR's versions alone.", timeline)
	}
	if !strings.Contains(state, "| widget-0 | 221 |") || strings.Contains(state, "| widget | 219 |") {
		t.Errorf("The state is\n%s\nwant the managed objects alone.", state)
	}
}

// The object bodies are in objects.jsonl, and a report that embedded them
// would be too big to read (DESIGN.md §11).
func TestReportQuotesTheVersionTimelineWithoutTheObjects(t *testing.T) {
	failure := failingRun()
	failure.Versions = []observe.Version{{
		Key:    observe.Key{GVK: schema.GroupVersionKind{Version: "v1", Kind: "ConfigMap"}, Name: "widget-0"},
		Object: object("widget-0"),
	}}

	_, encoded := write(t, failure)

	if strings.Contains(encoded, "reconciler-fuzzer-the-whole-object") {
		t.Errorf("report.json embeds the object bodies:\n%s", encoded)
	}
	if failure.Versions[0].Object == nil {
		t.Error("Writing the report emptied the caller's versions.")
	}
}

func TestReportQuotesWhatARestartChanged(t *testing.T) {
	failure := failingRun()
	failure.Differences = []invariant.Difference{
		{Object: "v1/ConfigMap widget-0", ResourceVersions: [2]string{"", "21"}, Before: "(absent)", After: "(present)"},
		{Object: "v1/ConfigMap widget-1", ResourceVersions: [2]string{"12", "22"},
			Path: `metadata.annotations["probe.example.com/started-at"]`, Before: `"1"`, After: `"2"`},
	}
	failure.DifferencesTotal = 3
	failure.Compared = "the state converged after op 0 (create) and the one after op 2 (settle)"

	md, encoded := write(t, failure)

	body := section(md, "What changed across the restart")
	for _, want := range []string{
		"The violation quotes 2 of 3 differences between the state converged after op 0 (create) and the one after op 2 (settle). " +
			"`equalIgnore` takes each path as written, and `objects.jsonl` holds every version the Observer saw.\n",
		"| object | resourceVersion | path | before | after |\n",
		"| v1/ConfigMap widget-0 | (absent) → 21 | (whole object) | `(absent)` | `(present)` |\n",
		"| v1/ConfigMap widget-1 | 12 → 22 | `metadata.annotations[\"probe.example.com/started-at\"]` | `\"1\"` | `\"2\"` |\n",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("The section is\n%s\nwant it to hold %q.", body, want)
		}
	}
	var carried []invariant.Difference
	if err := json.Unmarshal([]byte(field(t, encoded, "differences")), &carried); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(carried, failure.Differences) {
		t.Errorf("report.json holds %+v, want %+v.", carried, failure.Differences)
	}
	if got := field(t, encoded, "differencesTotal"); got != "3" {
		t.Errorf("report.json counts %s differences, want 3.", got)
	}
	if got := field(t, encoded, "compared"); got != `"`+failure.Compared+`"` {
		t.Errorf("report.json says it compared %s, want %q.", got, failure.Compared)
	}
}

// equalIgnore cannot ignore a whole object, so the report offers no path.
func TestReportQuotesAWholeObjectWithoutAPath(t *testing.T) {
	failure := failingRun()
	failure.Differences = []invariant.Difference{
		{Object: "v1/ConfigMap widget-0", ResourceVersions: [2]string{"11", ""}, Before: "(present)", After: "(absent)"},
		{Object: "v1/ConfigMap widget-1", ResourceVersions: [2]string{"12", "22"}, Before: "(present)", After: "(changed)"},
	}
	failure.DifferencesTotal = 2
	failure.Compared = "the state converged after op 0 (create) and the one after op 2 (settle)"

	md, _ := write(t, failure)

	body := section(md, "What changed across the restart")
	for _, want := range []string{
		"The violation quotes 2 differences between the state converged after op 0 (create) and the one after op 2 (settle). " +
			"`objects.jsonl` holds every version the Observer saw.\n",
		"| v1/ConfigMap widget-0 | 11 → (absent) | (whole object) | `(present)` | `(absent)` |\n",
		"| v1/ConfigMap widget-1 | 12 → 22 | (whole object) | `(present)` | `(changed)` |\n",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("The section is\n%s\nwant it to hold %q.", body, want)
		}
	}
}

func TestReportBoundsTheDifferencesItWasHanded(t *testing.T) {
	failure := failingRun()
	for i := range 25 {
		failure.Differences = append(failure.Differences, invariant.Difference{Object: "v1/ConfigMap widget-0", Path: fmt.Sprintf("data.k%02d", i)})
	}

	md, encoded := write(t, failure)

	body := section(md, "What changed across the restart")
	if !strings.Contains(body, "The violation quotes 20 of 25 differences.") {
		t.Errorf("The section is\n%s\nwant it to say it quotes 20 of 25 differences.", body)
	}
	if !strings.Contains(body, "`data.k19`") || strings.Contains(body, "`data.k20`") {
		t.Errorf("The section is\n%s\nwant the first 20 differences.", body)
	}
	if got := field(t, encoded, "differencesTotal"); got != "25" {
		t.Errorf("report.json counts %s differences, want 25.", got)
	}
}

// A table cell holds a value whatever it quotes.
func TestReportQuotesAValueAsCode(t *testing.T) {
	failure := failingRun()
	failure.Differences = []invariant.Difference{{Object: "v1/ConfigMap widget-0", Path: "data.a`", Before: `"a|b"`, After: `"*x*"`}}

	md, _ := write(t, failure)

	if want := "| `` data.a` `` | `\"a\\|b\"` | `\"*x*\"` |"; !strings.Contains(md, want) {
		t.Errorf("The report is\n%s\nwant the row to end %q.", md, want)
	}
}

func TestReportSaysWhyAheadOfTheSequence(t *testing.T) {
	failure := deploymentBacked()
	failure.Differences = []invariant.Difference{{Object: "v1/ConfigMap widget-0", Path: "data.a"}}

	md, encoded := write(t, failure)

	for _, heading := range []string{"## What changed across the restart", "## Ready predicate"} {
		if at := strings.Index(md, heading); at < 0 || at > strings.Index(md, "## Sequence") {
			t.Errorf("The report is\n%s\nwant %q ahead of the sequence.", md, heading)
		}
	}
	fields := keys(t, encoded)
	for _, key := range []string{"differences", "ready"} {
		if at := slices.Index(fields, key); at < 0 || at > slices.Index(fields, "sequence") {
			t.Errorf("report.json holds %v, want %q ahead of the sequence.", fields, key)
		}
	}
}

func TestReportOmitsTheSectionsWithNothingToSay(t *testing.T) {
	md, encoded := write(t, failingRun())

	for _, absent := range []string{"## Requests", "## Object versions", "## Managed objects at the verdict", "## Notes", "## Ready predicate", "## What changed across the restart"} {
		if strings.Contains(md, absent) {
			t.Errorf("The report holds an empty %q section:\n%s", absent, md)
		}
	}
	for _, absent := range []string{"requests", "versions", "managed", "managedTotal", "notes", "ready", "differences", "differencesTotal", "compared"} {
		if strings.Contains(encoded, `"`+absent+`"`) {
			t.Errorf("report.json holds an empty %q:\n%s", absent, encoded)
		}
	}
}

func TestReportRefusesASequenceThatIsNotJSON(t *testing.T) {
	failure := failingRun()
	failure.Sequence = json.RawMessage("not json")
	dir := t.TempDir()

	err := report.Write(dir, failure)

	if err == nil {
		t.Fatal("Write accepted a sequence that is not JSON.")
	}
	for _, name := range []string{report.JSONFile, report.MarkdownFile} {
		if _, err := os.Stat(filepath.Join(dir, name)); !os.IsNotExist(err) {
			t.Errorf("Write left a half-written %s: %v", name, err)
		}
	}
}

func TestReportCreatesTheRunDirectory(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "run-1")

	if err := report.Write(dir, failingRun()); err != nil {
		t.Fatalf("Write failed: %v", err)
	}

	for _, name := range []string{report.JSONFile, report.MarkdownFile} {
		if _, err := os.Stat(filepath.Join(dir, name)); err != nil {
			t.Errorf("Write did not write %s: %v", name, err)
		}
	}
}

// write writes the report and returns report.md and report.json.
func write(t *testing.T, r report.Report) (string, string) {
	t.Helper()
	dir := t.TempDir()
	if err := report.Write(dir, r); err != nil {
		t.Fatalf("Write failed: %v", err)
	}
	return read(t, dir, report.MarkdownFile), read(t, dir, report.JSONFile)
}

func read(t *testing.T, dir, name string) string {
	t.Helper()
	content, err := os.ReadFile(filepath.Join(dir, name))
	if err != nil {
		t.Fatalf("Reading %s failed: %v", name, err)
	}
	return string(content)
}

// fencedJSON returns the content of the Sequence section's json code block.
func fencedJSON(t *testing.T, md string) string {
	t.Helper()
	_, after, found := strings.Cut(section(md, "Sequence"), "```json\n")
	if !found {
		t.Fatalf("The report embeds no json block:\n%s", md)
	}
	block, _, found := strings.Cut(after, "```")
	if !found {
		t.Fatalf("The report's json block does not close:\n%s", md)
	}
	return block
}

// field returns one top-level field of report.json, as it was written.
func field(t *testing.T, encoded, name string) string {
	t.Helper()
	var fields map[string]json.RawMessage
	if err := json.Unmarshal([]byte(encoded), &fields); err != nil {
		t.Fatalf("report.json does not parse: %v\n%s", err, encoded)
	}
	held, ok := fields[name]
	if !ok {
		t.Fatalf("report.json holds no %q:\n%s", name, encoded)
	}
	return string(held)
}

// keys returns report.json's top-level fields in the order it writes them.
func keys(t *testing.T, encoded string) []string {
	t.Helper()
	decoder := json.NewDecoder(strings.NewReader(encoded))
	if _, err := decoder.Token(); err != nil {
		t.Fatalf("report.json does not parse: %v\n%s", err, encoded)
	}
	var fields []string
	for decoder.More() {
		key, err := decoder.Token()
		if err != nil {
			t.Fatalf("report.json does not parse: %v\n%s", err, encoded)
		}
		fields = append(fields, key.(string))
		var value json.RawMessage
		if err := decoder.Decode(&value); err != nil {
			t.Fatalf("report.json does not parse: %v\n%s", err, encoded)
		}
	}
	return fields
}

// section returns one markdown section, from its heading to the next.
func section(md, heading string) string {
	_, after, _ := strings.Cut(md, "## "+heading+"\n")
	body, _, _ := strings.Cut(after, "\n## ")
	return body
}

// object is a body the report must not embed: objects.jsonl holds it.
func object(name string) *unstructured.Unstructured {
	return &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "v1",
		"kind":       "ConfigMap",
		"metadata":   map[string]any{"name": name},
		"data":       map[string]any{"marker": "reconciler-fuzzer-the-whole-object"},
	}}
}

func sameJSON(t *testing.T, a, b string) bool {
	t.Helper()
	var parsedA, parsedB any
	for _, parse := range []struct {
		text string
		into *any
	}{{a, &parsedA}, {b, &parsedB}} {
		if err := json.Unmarshal([]byte(parse.text), parse.into); err != nil {
			t.Fatalf("%s does not parse: %v", parse.text, err)
		}
	}
	return reflect.DeepEqual(parsedA, parsedB)
}

func ptr[T any](v T) *T { return &v }

// The two tables are aligned on the instant the check judged, so the report
// says it once, under the statement (#24).
func TestReportStampsTheViolation(t *testing.T) {
	failure := failingRun()
	failure.Check.At = time.Date(2026, 9, 21, 5, 59, 8, 980624165, time.UTC)

	md, encoded := write(t, failure)

	want := failure.Check.Statement + "\n\nThe violation is at 2026-09-21T05:59:08.980624165Z.\n"
	if !strings.Contains(md, want) {
		t.Errorf("The report reads\n%s\nwant the statement followed by %q.", md, want)
	}
	if got := field(t, encoded, "check"); !strings.Contains(got, `"at": "2026-09-21T05:59:08.980624165Z"`) {
		t.Errorf("report.json names the check as\n%s\nwant the instant it judged.", got)
	}
}

// A violation that carries no instant is stamped with none.
func TestReportStampsNoViolationThatCarriesNoInstant(t *testing.T) {
	md, encoded := write(t, failingRun())

	if strings.Contains(md, "The violation is at") {
		t.Errorf("The report stamps a violation that carries none:\n%s", md)
	}
	if strings.Contains(encoded, `"at"`) {
		t.Errorf("report.json holds an empty instant:\n%s", encoded)
	}
}

// A run ends at its first violation, so the sequence a report carries can hold
// ops that never ran. A reader who attributes the finding to all of them has
// been told the wrong thing (DESIGN.md §5.7).
func TestReportSaysHowManyOpsTheRunApplied(t *testing.T) {
	for _, run := range []struct {
		name         string
		applied, ops int
		says         bool
	}{
		{name: "a run that ended early", applied: 1, ops: 6, says: true},
		{name: "a run that applied every op", applied: 6, ops: 6, says: false},
	} {
		t.Run(run.name, func(t *testing.T) {
			failure := failingRun()
			failure.Applied, failure.Ops = run.applied, run.ops

			md, _ := write(t, failure)

			want := fmt.Sprintf("applied %d of the sequence's %d ops", run.applied, run.ops)
			if says := strings.Contains(md, want); says != run.says {
				t.Errorf("The report is\n%s\nand %s.", md, run.name)
			}
		})
	}
}

// A readiness verdict quotes the state as a table of its own, within a bound
// of its own (#24).
func TestReportQuotesTheStateAtTheVerdict(t *testing.T) {
	for _, c := range []struct {
		name        string
		objects     int
		total       int
		wantLead    string
		wantNoTable bool
	}{
		{name: "a state quoted whole", objects: 3, total: 3,
			wantLead: "The target managed 3 objects of the kinds it declares; the violation quotes them all."},
		{name: "a state the bound cut", objects: 20, total: 35,
			wantLead: "The target managed 35 objects of the kinds it declares; the violation quotes 20 of them, the newest of each kind first."},
		{name: "a target that managed nothing", objects: 0, total: 0, wantNoTable: true,
			wantLead: "The target managed no objects of the kinds it declares."},
	} {
		t.Run(c.name, func(t *testing.T) {
			failure := failingRun()
			for i := range c.objects {
				failure.Managed = append(failure.Managed, observe.Version{
					Key:             observe.Key{GVK: schema.GroupVersionKind{Version: "v1", Kind: "ConfigMap"}, Name: fmt.Sprintf("widget-%d", i)},
					ResourceVersion: fmt.Sprint(100 + i),
				})
			}
			failure.ManagedTotal = ptr(c.total)

			md, encoded := write(t, failure)

			body := section(md, "Managed objects at the verdict")
			if !strings.Contains(body, c.wantLead) {
				t.Errorf("The state section does not say %q:\n%s", c.wantLead, body)
			}
			// A target that managed nothing leaves nothing to look up.
			if named := strings.Contains(body, "objects.jsonl"); named == (c.total == 0) {
				t.Errorf("The state section is\n%s\nand the target managed %d objects.", body, c.total)
			}
			if held := strings.Contains(body, "| time | kind |"); held == c.wantNoTable {
				t.Errorf("The state section is\n%s\nand the target managed %d objects.", body, c.total)
			}
			if rows := strings.Count(body, "| v1/ConfigMap |"); rows != c.objects {
				t.Errorf("The state section holds %d rows, want %d:\n%s", rows, c.objects, body)
			}
			if total := field(t, encoded, "managedTotal"); total != fmt.Sprint(c.total) {
				t.Errorf("report.json says the target managed %s objects, want %d.", total, c.total)
			}
		})
	}
}

// A state a caller bounded and did not count is still quoted, and both files
// say how many objects it holds (#24).
func TestReportCountsAStateItWasHandedWithoutATotal(t *testing.T) {
	failure := failingRun()
	failure.Managed = []observe.Version{{
		Key: observe.Key{GVK: schema.GroupVersionKind{Version: "v1", Kind: "ConfigMap"}, Name: "widget-0"},
	}}

	md, encoded := write(t, failure)

	if want := "The target managed 1 object of the kinds it declares"; !strings.Contains(md, want) {
		t.Errorf("The report does not say %q:\n%s", want, md)
	}
	if total := field(t, encoded, "managedTotal"); total != "1" {
		t.Errorf("report.json says the target managed %s objects, want the 1 it quotes.", total)
	}
}

// A check that never asked what the target managed quotes no state, and a
// reader can tell that from a target that managed nothing (#24).
func TestReportQuotesNoStateWhereNoCheckAsked(t *testing.T) {
	md, encoded := write(t, failingRun())

	if strings.Contains(md, "## Managed objects at the verdict") {
		t.Errorf("The report holds a state section G3 never filled:\n%s", md)
	}
	if strings.Contains(encoded, "managedTotal") {
		t.Errorf("report.json counts objects no check counted:\n%s", encoded)
	}
}

// The state is bounded and its rows leave their bodies to objects.jsonl, as
// the timeline does. A report never counts fewer objects than it quotes (#24).
func TestReportBoundsTheStateItWasHanded(t *testing.T) {
	failure := failingRun()
	for i := range 25 {
		failure.Managed = append(failure.Managed, observe.Version{
			Key:    observe.Key{GVK: schema.GroupVersionKind{Version: "v1", Kind: "ConfigMap"}, Name: fmt.Sprintf("widget-%d", i)},
			Object: object(fmt.Sprintf("widget-%d", i)),
		})
	}
	failure.ManagedTotal = ptr(3)

	_, encoded := write(t, failure)

	var held []json.RawMessage
	if err := json.Unmarshal([]byte(field(t, encoded, "managed")), &held); err != nil {
		t.Fatalf("report.json's managed objects do not parse: %v", err)
	}
	if len(held) != 20 {
		t.Errorf("report.json holds %d managed objects, want the bound of 20.", len(held))
	}
	if first := string(held[0]); !strings.Contains(first, `"widget-0"`) {
		t.Errorf("report.json opens the state at %s, want the object the check put first.", first)
	}
	if total := field(t, encoded, "managedTotal"); total != "25" {
		t.Errorf("report.json says the target managed %s objects, want the 25 it was handed.", total)
	}
	if strings.Contains(encoded, "reconciler-fuzzer-the-whole-object") {
		t.Errorf("report.json embeds the object bodies:\n%s", encoded)
	}
}

// A check bounds its own evidence at the same number the report does, so a
// report that counted only what it was handed could never say the bound
// dropped anything (#22).
func TestReportSaysWhatTheChecksOwnBoundLeftOut(t *testing.T) {
	failure := failingRun()
	for i := range 20 {
		failure.Requests = append(failure.Requests, proxy.Request{Verb: "get", Path: fmt.Sprintf("/api/v1/path-%d", i)})
		failure.Versions = append(failure.Versions, observe.Version{
			Key: observe.Key{GVK: schema.GroupVersionKind{Version: "v1", Kind: "ConfigMap"}, Name: fmt.Sprintf("widget-%d", i)},
		})
	}
	failure.RequestsTotal, failure.VersionsTotal = 133, 41
	failure.VersionsOf = "toy.reconciler-fuzzer/v1/Widget widget"

	md, encoded := write(t, failure)

	for _, excerpt := range []struct {
		heading, key string
		chose        int
	}{{"Requests", "requests", 133}, {"Object versions", "versions", 41}} {
		body := section(md, excerpt.heading)
		subject := ""
		if excerpt.key == "versions" {
			subject = " of `toy.reconciler-fuzzer/v1/Widget widget`"
		}
		if want := fmt.Sprintf("The violation quotes the last 20 of %d %s%s.", excerpt.chose, excerpt.key, subject); !strings.Contains(body, want) {
			t.Errorf("The report does not say %q:\n%s", want, body)
		}
		if total := field(t, encoded, excerpt.key+"Total"); total != fmt.Sprint(excerpt.chose) {
			t.Errorf("report.json says %s of %s, want the %d the check chose from.", total, excerpt.key, excerpt.chose)
		}
	}
}

// deploymentBacked is a readiness verdict on a CR that waits on a Deployment
// envtest never runs.
func deploymentBacked() report.Report {
	failure := failingRun()
	failure.Check.ID = "G4"
	failure.Ready = &invariant.Readiness{
		Expr:  `status.conditions.exists(c, c.type == "Ready" && c.status == "True")`,
		CR:    "widget",
		Error: `evaluating ready "…": no such key: conditions`,
		Status: map[string]any{
			"observedGeneration": int64(1),
			"replicas":           int64(0),
			"phase":              "<Pending> & waiting",
			"conditions": []any{map[string]any{
				"type": "Ready", "status": "False", "reason": "Pending",
				"message": "0/10 replicas available", "observedGeneration": int64(1),
			}},
		},
	}
	return failure
}

func TestReportQuotesTheReadyPredicate(t *testing.T) {
	failure := deploymentBacked()

	md, encoded := write(t, failure)

	body := section(md, "Ready predicate")
	for _, want := range []string{
		"The verdict evaluated the target's `ready` on the CR `widget`:\n\n```\n" + failure.Ready.Expr + "\n```",
		"Evaluating it on the CR at the verdict failed:\n\n```\n" + failure.Ready.Error + "\n```",
		"The CR's conditions at the verdict:\n\n| type | status | reason | message | observedGeneration |",
		"| Ready | False | Pending | 0/10 replicas available | 1 |",
		"The rest of its status:\n\n```json\n" + `{"observedGeneration":1,"phase":"<Pending> & waiting","replicas":0}` + "\n```",
		"`objects.jsonl` holds every version of the CR whole.",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("The Ready predicate section does not quote %q:\n%s", want, body)
		}
	}
	var ready struct {
		Expr, CR, Error string
		Conditions      []map[string]string
		Status          string
	}
	if err := json.Unmarshal([]byte(field(t, encoded, "ready")), &ready); err != nil {
		t.Fatalf("report.json's ready does not parse: %v", err)
	}
	if ready.Expr != failure.Ready.Expr || ready.CR != "widget" || ready.Error != failure.Ready.Error ||
		len(ready.Conditions) != 1 || ready.Conditions[0]["message"] != "0/10 replicas available" ||
		ready.Status != `{"observedGeneration":1,"phase":"<Pending> & waiting","replicas":0}` {
		t.Errorf("report.json quotes the ready predicate as %+v.", ready)
	}
}

func TestReportQuotesAStatusWithoutConditionsWhole(t *testing.T) {
	failure := deploymentBacked()
	failure.Ready.Status = map[string]any{"ready": int64(1)}

	md, _ := write(t, failure)

	if want := "The CR's status at the verdict:\n\n```json\n{\"ready\":1}\n```"; !strings.Contains(section(md, "Ready predicate"), want) {
		t.Errorf("The report does not say %q:\n%s", want, md)
	}
}

// A ready that evaluated on a CR with no status leaves nothing to quote but
// the expression, and the report says the CR had no status.
func TestReportQuotesNoStatusOrErrorItWasNotGiven(t *testing.T) {
	for name, status := range map[string]map[string]any{"no status": nil, "an empty status": {}} {
		t.Run(name, func(t *testing.T) {
			failure := deploymentBacked()
			failure.Ready.Error, failure.Ready.Status = "", status

			md, _ := write(t, failure)

			body := section(md, "Ready predicate")
			if want := "The CR carried no status at the verdict."; !strings.Contains(body, want) {
				t.Errorf("The Ready predicate section does not say %q:\n%s", want, body)
			}
			for _, absent := range []string{"failed:", "The CR's status", "rest of its status", "conditions at the verdict", "```json"} {
				if strings.Contains(body, absent) {
					t.Errorf("The Ready predicate section says %q:\n%s", absent, body)
				}
			}
		})
	}
}

func TestReportQuotesAStatusOfConditionsAlone(t *testing.T) {
	failure := deploymentBacked()
	failure.Ready.Status = map[string]any{"conditions": failure.Ready.Status["conditions"]}

	md, _ := write(t, failure)

	body := section(md, "Ready predicate")
	if want := "| Ready | False | Pending | 0/10 replicas available | 1 |"; !strings.Contains(body, want) {
		t.Errorf("The Ready predicate section does not quote %q:\n%s", want, body)
	}
	for _, absent := range []string{"no status", "rest of its status"} {
		if strings.Contains(body, absent) {
			t.Errorf("The Ready predicate section says %q:\n%s", absent, body)
		}
	}
}

func TestReportQuotesAStatusOfNoConditions(t *testing.T) {
	failure := deploymentBacked()
	failure.Ready.Status = map[string]any{"conditions": []any{}}

	md, _ := write(t, failure)

	body := section(md, "Ready predicate")
	if want := "The CR's status at the verdict:\n\n```json\n{\"conditions\":[]}\n```"; !strings.Contains(body, want) {
		t.Errorf("The Ready predicate section does not say %q:\n%s", want, body)
	}
	if strings.Contains(body, "no status") {
		t.Errorf("The Ready predicate section says the CR carried no status:\n%s", body)
	}
}

// A status carries whatever its controller put there, so the report quotes a
// bounded part of it and objects.jsonl keeps the rest.
func TestReportBoundsTheCRsStatus(t *testing.T) {
	failure := deploymentBacked()
	var conditions []any
	for i := range 25 {
		conditions = append(conditions, map[string]any{"type": fmt.Sprintf("C%d", i), "status": "False"})
	}
	conditions[0].(map[string]any)["message"] = "one | two\nthree " + strings.Repeat("x", 1000)
	conditions[1].(map[string]any)["message"] = strings.Repeat("y", 200)
	// The key's odd length puts the cut inside a character.
	failure.Ready.Status = map[string]any{"conditions": conditions, "logs": strings.Repeat("é", 2000)}

	md, encoded := write(t, failure)

	body := section(md, "Ready predicate")
	if want := "The CR carried 25 conditions at the verdict; the report quotes the first 20."; !strings.Contains(body, want) {
		t.Errorf("The report does not say %q:\n%s", want, body)
	}
	if rows := strings.Count(body, "| False |"); rows != 20 {
		t.Errorf("The report quotes %d conditions, want 20.", rows)
	}
	if !strings.Contains(body, `| one \| two three xxx`) {
		t.Errorf("The report does not keep a message's bar and newline out of the table:\n%s", body)
	}
	if strings.Contains(body, strings.Repeat("x", 185)) || strings.Contains(encoded, strings.Repeat("x", 185)) {
		t.Errorf("The report quotes a condition's message whole.")
	}
	if !strings.Contains(body, strings.Repeat("x", 184)+"… |") {
		t.Errorf("The report does not mark where it cut a message:\n%s", body)
	}
	if !strings.Contains(body, "| "+strings.Repeat("y", 200)+" |") {
		t.Errorf("The report cut a message that fits:\n%s", body)
	}
	if strings.Contains(body, "<nil>") || strings.Contains(encoded, "<nil>") {
		t.Errorf("The report quotes a missing reason or message as <nil>:\n%s", body)
	}
	if strings.Contains(body, strings.Repeat("é", 600)) || strings.Contains(encoded, strings.Repeat("é", 600)) {
		t.Errorf("The report quotes the rest of the status whole.")
	}
	if !strings.Contains(body, "The rest of its status, cut to 999 of 4011 bytes:") {
		t.Errorf("The report does not say it cut the status:\n%s", body)
	}
	if !utf8.ValidString(md) || !utf8.ValidString(encoded) {
		t.Errorf("The report cut a character in two.")
	}
}

// A controller can write a fence into its status, and the report's own fences
// must outlast it by one backtick.
func TestReportFencesTheReadyPredicateAroundTheFencesItQuotes(t *testing.T) {
	failure := deploymentBacked()
	failure.Ready.Expr = "status.note == \"```\""
	failure.Ready.Error = "no such key: ````"
	failure.Ready.Status = map[string]any{"note": "```"}

	md, _ := write(t, failure)

	body := section(md, "Ready predicate")
	for _, want := range []string{
		"\n\n````\n" + failure.Ready.Expr + "\n````\n",
		"\n\n`````\n" + failure.Ready.Error + "\n`````\n",
		"\n\n````json\n{\"note\":\"```\"}\n````\n",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("The Ready predicate section does not fence %q:\n%s", want, body)
		}
	}
}
