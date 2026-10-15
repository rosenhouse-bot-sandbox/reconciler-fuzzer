// Package report writes what a failing run found, as report.json and
// report.md beside the run's recordings. The caller fills in a Report, so
// that nothing here depends on the Runner.
package report

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"time"

	"github.com/rosenhouse-bot-sandbox/reconciler-fuzzer/internal/invariant"
	"github.com/rosenhouse-bot-sandbox/reconciler-fuzzer/internal/observe"
	"github.com/rosenhouse-bot-sandbox/reconciler-fuzzer/internal/proxy"
)

// A failing run writes these two files.
const (
	JSONFile     = "report.json"
	MarkdownFile = "report.md"
)

// maxEvidence is a backstop, at the bound a violation's own evidence already
// carries (pkg/invariant). A report quotes what it is given; this only stops a
// caller that bounds nothing. The run's recordings hold every entry either way.
const maxEvidence = invariant.MaxEvidence

// Report is one failing run.
type Report struct {
	Check  Check
	Target Target
	// ReconcilerFuzzer is the version of reconciler-fuzzer that ran the sequence.
	// A bug report needs it beside the target's own.
	ReconcilerFuzzer string
	// Seed is the sequence's seed, which the run is reproducible from.
	Seed int64
	// Baseline says the sequence is the baseline, which no seed draws.
	Baseline bool
	// Notes name what a check could not judge, and what the test cluster
	// cannot run. Without them a report claims more than its run showed.
	Notes []string
	// Replay is the one-line command that re-executes the sequence.
	Replay string
	// Sequence is the minimized sequence in its canonical form, as
	// run.Sequence.Marshal writes it.
	Sequence json.RawMessage
	// Applied is how many of the sequence's ops the run reached, and Ops is
	// how many it holds. A run that ended at its violation reached fewer.
	Applied, Ops int
	// Collector says reconciler-fuzzer's garbage collector ran.
	Collector bool
	// Requests and Versions are the evidence the check quoted, and
	// RequestsTotal and VersionsTotal how many entries it chose them from. A
	// caller that names no total is read as having quoted everything it saw.
	Requests                     []proxy.Request
	Versions                     []observe.Version
	RequestsTotal, VersionsTotal int
	// VersionsOf names the object the timeline is the history of, and is
	// empty where the versions are of several.
	VersionsOf string
	// Managed is the state at the violation, one version per object the target
	// managed, and ManagedTotal how many there were. A check that does not
	// quote the state leaves the total nil, so that a total of zero says the
	// target managed nothing.
	Managed      []observe.Version
	ManagedTotal *int
	// Ready is what a readiness verdict read, which the report bounds.
	Ready *invariant.Readiness
	// Differences are what G5 found changed across a restart, DifferencesTotal
	// how many there were, and Compared names the two states.
	Differences      []invariant.Difference
	DifferencesTotal int
	Compared         string
}

// Check is the invariant or property the run broke.
type Check struct {
	ID        string `json:"id"`
	Statement string `json:"statement"`
	// At is the instant the check judged, which the evidence below is aligned
	// on.
	At       time.Time `json:"at,omitzero"`
	Evidence string    `json:"evidence,omitempty"`
}

// Target is the controller the run exercised.
type Target struct {
	Name    string `json:"name"`
	Version string `json:"version,omitempty"`
}

// Write writes report.json and report.md into dir, creating it if it is not
// there. A report that does not encode writes neither file.
func Write(dir string, r Report) error {
	doc := r.document()
	encoded, err := doc.marshal()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("creating the report's directory: %w", err)
	}
	for _, file := range []struct {
		name    string
		content []byte
	}{{JSONFile, encoded}, {MarkdownFile, doc.markdown()}} {
		if err := os.WriteFile(filepath.Join(dir, file.name), file.content, 0o644); err != nil {
			return fmt.Errorf("writing %s: %w", file.name, err)
		}
	}
	return nil
}

// document is the report as both files render it: the evidence bounded, and
// the totals that say what the bound left out. Field order is report.json's.
type document struct {
	Check            Check                  `json:"check"`
	Target           Target                 `json:"target"`
	ReconcilerFuzzer string                 `json:"reconcilerFuzzer,omitempty"`
	Seed             int64                  `json:"seed"`
	Baseline         bool                   `json:"baseline,omitempty"`
	Notes            []string               `json:"notes,omitempty"`
	Replay           string                 `json:"replay"`
	Differences      []invariant.Difference `json:"differences,omitempty"`
	DifferencesTotal int                    `json:"differencesTotal,omitempty"`
	Compared         string                 `json:"compared,omitempty"`
	Ready            *ready                 `json:"ready,omitempty"`
	Sequence         json.RawMessage        `json:"sequence"`
	Applied          int                    `json:"applied,omitempty"`
	Ops              int                    `json:"ops,omitempty"`
	Requests         []proxy.Request        `json:"requests,omitempty"`
	RequestsTotal    int                    `json:"requestsTotal,omitempty"`
	Versions         []observe.Version      `json:"versions,omitempty"`
	VersionsTotal    int                    `json:"versionsTotal,omitempty"`
	VersionsOf       string                 `json:"versionsOf,omitempty"`
	Managed          []observe.Version      `json:"managed,omitempty"`
	ManagedTotal     *int                   `json:"managedTotal,omitempty"`
	Collector        bool                   `json:"-"`
}

func (r Report) document() document {
	return document{
		Check:            r.Check,
		Target:           r.Target,
		ReconcilerFuzzer: r.ReconcilerFuzzer,
		Seed:             r.Seed,
		Baseline:         r.Baseline,
		Notes:            r.Notes,
		Replay:           r.Replay,
		Differences:      leading(r.Differences),
		DifferencesTotal: max(r.DifferencesTotal, len(r.Differences)),
		Compared:         r.Compared,
		Ready:            quoteReady(r.Ready),
		Sequence:         r.Sequence,
		Applied:          r.Applied,
		Ops:              r.Ops,
		Requests:         recent(r.Requests),
		RequestsTotal:    max(r.RequestsTotal, len(r.Requests)),
		Versions:         timeline(r.Versions),
		VersionsTotal:    max(r.VersionsTotal, len(r.Versions)),
		VersionsOf:       r.VersionsOf,
		Managed:          state(r.Managed),
		ManagedTotal:     managedTotal(r.ManagedTotal, len(r.Managed)),
		Collector:        r.Collector,
	}
}

// timeline quotes when each version appeared and what it carried, and drops
// the object bodies that objects.jsonl holds.
func timeline(versions []observe.Version) []observe.Version { return quoting(recent(versions)) }

// state quotes what the target managed at the violation. The check ordered it
// by what a reader needs first, so the bound keeps the entries it opens with.
func state(versions []observe.Version) []observe.Version { return quoting(leading(versions)) }

func quoting(versions []observe.Version) []observe.Version {
	quoted := slices.Clone(versions)
	for i := range quoted {
		quoted[i].Object = nil
	}
	return quoted
}

// leading keeps the maxEvidence first entries.
func leading[T any](evidence []T) []T { return evidence[:min(len(evidence), maxEvidence)] }

// recent keeps the maxEvidence last entries, which are the ones the report
// says it quotes.
func recent[T any](evidence []T) []T {
	if len(evidence) > maxEvidence {
		return evidence[len(evidence)-maxEvidence:]
	}
	return evidence
}

// managedTotal is what the state was chosen from, and nil only where the
// check quoted no state: a report that quotes a state counts it, whatever the
// caller named.
func managedTotal(total *int, rows int) *int {
	if total == nil && rows == 0 {
		return nil
	}
	held := rows
	if total != nil {
		held = max(*total, rows)
	}
	return &held
}

// marshal renders report.json: two spaces of indentation and a trailing
// newline, as a sequence file has. HTML escaping stays off, so that a CEL
// statement's < and & read as themselves.
func (d document) marshal() ([]byte, error) {
	var encoded bytes.Buffer
	encoder := json.NewEncoder(&encoded)
	encoder.SetEscapeHTML(false)
	encoder.SetIndent("", "  ")
	if err := encoder.Encode(d); err != nil {
		return nil, fmt.Errorf("encoding the report: %w", err)
	}
	return encoded.Bytes(), nil
}
