package report

import (
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/rosenhouse-bot-sandbox/reconciler-fuzzer/internal/invariant"
	"github.com/rosenhouse-bot-sandbox/reconciler-fuzzer/internal/observe"
	"github.com/rosenhouse-bot-sandbox/reconciler-fuzzer/internal/proxy"
)

// markdown renders report.md. It opens with what failed and the command that
// reproduces it, so that the first lines are enough to act on.
func (d document) markdown() []byte {
	var md strings.Builder
	fmt.Fprintf(&md, "# %s failed on %s\n", d.Check.ID, d.Target.describe())
	fmt.Fprintf(&md, "\n%s\n", d.Check.Statement)
	if !d.Check.At.IsZero() {
		fmt.Fprintf(&md, "\nThe violation is at %s.\n", stamp(d.Check.At))
	}
	if d.Check.Evidence != "" {
		fmt.Fprintf(&md, "\n%s\n", d.Check.Evidence)
	}
	fmt.Fprintf(&md, "\n```sh\n%s\n```\n", d.Replay)
	fmt.Fprintf(&md, "\n[docs/checks.md](%s) says how to read a %s failure.\n", CheckSection(d.Check.ID), d.Check.ID)
	fmt.Fprintf(&md, "\n%s %s\n", d.provenance(), d.recordings())
	if len(d.Notes) > 0 {
		md.WriteString("\n## Notes\n\n")
		for _, note := range d.Notes {
			fmt.Fprintf(&md, "- %s\n", note)
		}
	}
	if len(d.Differences) > 0 {
		md.WriteString("\n## What changed across the restart\n\n")
		md.WriteString(differencesLine(d.Differences, d.DifferencesTotal, d.Compared))
		table(&md, []string{"object", "resourceVersion", "path", "before", "after"}, differenceRows(d.Differences))
	}
	if d.Ready != nil {
		d.Ready.markdown(&md)
	}
	fmt.Fprintf(&md, "\n## Sequence\n\n```json\n%s\n```\n", strings.TrimRight(string(d.Sequence), "\n"))
	if len(d.Requests) > 0 {
		md.WriteString("\n## Requests\n\n")
		md.WriteString(quotedLine(d.RequestsTotal, len(d.Requests), "request", "", "requests.jsonl", "every request the run made"))
		table(&md, []string{"start", "verb", "path", "status", "fault"}, requestRows(d.Requests))
	}
	if len(d.Versions) > 0 {
		md.WriteString("\n## Object versions\n\n")
		md.WriteString(quotedLine(d.VersionsTotal, len(d.Versions), "version", d.VersionsOf, "objects.jsonl", "every version the Observer saw"))
		table(&md, versionHeader, versionRows(d.Versions))
	}
	if d.ManagedTotal != nil {
		md.WriteString("\n## Managed objects at the verdict\n\n")
		md.WriteString(managedLine(*d.ManagedTotal, len(d.Managed)))
		if len(d.Managed) > 0 {
			table(&md, versionHeader, versionRows(d.Managed))
		}
	}
	return []byte(md.String())
}

// versionHeader names the columns both version tables carry.
var versionHeader = []string{"time", "kind", "name", "resourceVersion", "generation", "observed", "finalizers", "deletionTimestamp", "deleted"}

// quotedLine says what the violation quoted of a timeline and what the bound
// left out. A check bounds its evidence before the report sees it, at the
// latest entries of what it chose from. A timeline of one object's history
// names it; evidence drawn from several names none.
func quotedLine(total, shown int, noun, of, recording, holds string) string {
	subject := ""
	if of != "" {
		subject = fmt.Sprintf(" of `%s`", of)
	}
	held := fmt.Sprintf(" `%s` holds %s.\n\n", recording, holds)
	if shown < total {
		return fmt.Sprintf("The violation quotes the last %d of %s%s.", shown, count(total, noun), subject) + held
	}
	return fmt.Sprintf("The violation quotes %s%s.", count(shown, noun), subject) + held
}

// managedLine says what the target held at the verdict and how much of it the
// table quotes. The state has a bound of its own, so it does not crowd out the
// timeline beside it.
func managedLine(total, shown int) string {
	if total == 0 {
		return "The target managed no objects of the kinds it declares.\n\n"
	}
	held := " `objects.jsonl` holds every version the Observer saw.\n\n"
	managed := "The target managed " + count(total, "object") + " of the kinds it declares; "
	if shown < total {
		return managed + fmt.Sprintf("the violation quotes %d of them, the newest of each kind first.", shown) + held
	}
	return managed + "the violation quotes them all." + held
}

// differencesLine says what the violation quotes of the differences, and
// between which states.
func differencesLine(differences []invariant.Difference, total int, compared string) string {
	shown := len(differences)
	quoted := count(shown, "difference")
	if shown < total {
		quoted = fmt.Sprintf("%d of %s", shown, count(total, "difference"))
	}
	if compared != "" {
		quoted += " between " + compared
	}
	held := "`objects.jsonl` holds every version the Observer saw.\n\n"
	if slices.ContainsFunc(differences, invariant.Difference.NamesAField) {
		held = "`equalIgnore` takes each path as written, and " + held
	}
	return "The violation quotes " + quoted + ". " + held
}

// count writes a number of things, in the singular where there is one.
func count(n int, noun string) string {
	if n == 1 {
		return "1 " + noun
	}
	return fmt.Sprintf("%d %ss", n, noun)
}

func requestRows(requests []proxy.Request) [][]string {
	rows := make([][]string, len(requests))
	for i, request := range requests {
		rows[i] = []string{
			stamp(request.Start), request.Verb, request.Path,
			strconv.Itoa(request.Status), request.Fault,
		}
	}
	return rows
}

// versionRows carry the generation and the observed generation, because a
// check that reads a Ready predicate reads those.
func versionRows(versions []observe.Version) [][]string {
	rows := make([][]string, len(versions))
	for i, version := range versions {
		rows[i] = []string{
			stamp(version.Time), observe.KindName(version.GVK), version.Name, version.ResourceVersion,
			strconv.FormatInt(version.Generation, 10), observed(version.ObservedGeneration),
			strings.Join(version.Finalizers, ", "), deletionTimestamp(version.DeletionTimestamp), yes(version.Deleted),
		}
	}
	return rows
}

func differenceRows(differences []invariant.Difference) [][]string {
	rows := make([][]string, len(differences))
	for i, d := range differences {
		path := "(whole object)"
		if d.NamesAField() {
			path = code(d.Path)
		}
		rows[i] = []string{
			d.Object, resourceVersion(d.ResourceVersions[0]) + " → " + resourceVersion(d.ResourceVersions[1]),
			path, code(d.Before), code(d.After),
		}
	}
	return rows
}

func resourceVersion(version string) string {
	if version == "" {
		return "(absent)"
	}
	return version
}

// code writes text as a code span that a table cell can hold.
func code(text string) string {
	fence := "`"
	for strings.Contains(text, fence) {
		fence += "`"
	}
	if fence != "`" {
		text = " " + text + " "
	}
	return fence + strings.ReplaceAll(text, "|", `\|`) + fence
}

// observed writes the observedGeneration a version carried, and nothing for
// one whose status had none.
func observed(generation *int64) string {
	if generation == nil {
		return ""
	}
	return strconv.FormatInt(*generation, 10)
}

func deletionTimestamp(at *metav1.Time) string {
	if at == nil {
		return ""
	}
	return stamp(at.Time)
}

func table(md *strings.Builder, header []string, rows [][]string) {
	fmt.Fprintf(md, "| %s |\n", strings.Join(header, " | "))
	fmt.Fprintf(md, "|%s\n", strings.Repeat(" --- |", len(header)))
	for _, row := range rows {
		fmt.Fprintf(md, "| %s |\n", strings.Join(row, " | "))
	}
}

// provenance says what ran the sequence, in the versions each declares, and
// how far the run got.
func (d document) provenance() string {
	who := "The run"
	if d.ReconcilerFuzzer != "" {
		who = "reconciler-fuzzer " + d.ReconcilerFuzzer
	}
	ran := fmt.Sprintf("%s exercised %s on seed %d", who, d.Target.describe(), d.Seed)
	if d.Baseline {
		ran = fmt.Sprintf("%s exercised %s with the baseline", who, d.Target.describe())
	}
	if d.Applied < d.Ops {
		ran += fmt.Sprintf(" and applied %d of the sequence's %d ops", d.Applied, d.Ops)
	}
	return ran + "."
}

// recordings names the files beside the report.
func (d document) recordings() string {
	if !d.Collector {
		return "The run directory holds `requests.jsonl`, `objects.jsonl` and `target.log`."
	}
	return "The run directory holds `requests.jsonl`, `objects.jsonl`, `collector.jsonl` and `target.log`. " +
		"`collector.jsonl` holds each delete that reconciler-fuzzer's garbage collector tried."
}

// describe names the target and the version it declares.
func (t Target) describe() string {
	if t.Version == "" {
		return t.Name
	}
	return t.Name + " " + t.Version
}

// stamp writes a time as the request log and the object history do.
func stamp(at time.Time) string { return at.Format(time.RFC3339Nano) }

func yes(set bool) string {
	if set {
		return "yes"
	}
	return ""
}

// ChecksPage says what each check requires.
const ChecksPage = "https://github.com/rosenhouse-bot-sandbox/reconciler-fuzzer/blob/main/docs/checks.md"

// checkAnchors are the anchors of the generic checks' sections of ChecksPage.
var checkAnchors = map[string]string{
	"G1": "g1-bounded-reconciliation",
	"G2": "g2-no-churn",
	"G3": "g3-clean-deletion",
	"G4": "g4-convergence",
	"G5": "g5-restart-stable",
	"G6": "g6-no-error-loop",
	"G7": "g7-self-healing",
}

// CheckSection links the section of ChecksPage for the check id, where any id
// but a generic check's names a property.
func CheckSection(id string) string {
	anchor, generic := checkAnchors[id]
	if !generic {
		anchor = "properties"
	}
	return ChecksPage + "#" + anchor
}
