package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/rosenhouse-bot-sandbox/reconciler-fuzzer/internal/report"
	"github.com/rosenhouse-bot-sandbox/reconciler-fuzzer/internal/run"
	"github.com/rosenhouse-bot-sandbox/reconciler-fuzzer/internal/target"
)

// summaryJSONFile is written into the invocation's directory whatever the
// outcome.
const summaryJSONFile = "summary.json"

// summarySchema changes when a field changes meaning or goes away.
const summarySchema = 2

type outcome string

const (
	outcomePassed      outcome = "passed"
	outcomeViolation   outcome = "violation"
	outcomeError       outcome = "error"
	outcomeInterrupted outcome = "interrupted"
	outcomeNotRun      outcome = "not run"
	outcomeUnfinished  outcome = "unfinished"
)

// summary is what an invocation planned, ran and found.
type summary struct {
	Schema           int           `json:"schema"`
	Command          string        `json:"command"`
	ReconcilerFuzzer string        `json:"reconcilerFuzzer"`
	Target           summaryTarget `json:"target"`
	Seed             int64         `json:"seed"`
	LaunchArgs       []string      `json:"launchArgs,omitempty"`
	Cluster          string        `json:"cluster"`
	Deadline         run.Duration  `json:"deadline,omitempty"`
	DeadlineDerived  bool          `json:"deadlineDerived,omitempty"`
	Start            time.Time     `json:"start"`
	Finish           time.Time     `json:"finish,omitzero"`
	Outcome          outcome       `json:"outcome"`
	ExitCode         *int          `json:"exitCode,omitempty"`
	// Error is what stopped the invocation where no run did.
	Error string       `json:"error,omitempty"`
	Runs  []runSummary `json:"runs"`
}

type summaryTarget struct {
	Name    string `json:"name"`
	Version string `json:"version,omitempty"`
	File    string `json:"file"`
}

// runSummary is one planned run. Its counts are of the planned sequence's
// run. A failing run's violation and notes are its report's, and Dir is
// relative to the summary.
type runSummary struct {
	Run             int                `json:"run"`
	Seed            int64              `json:"seed"`
	File            string             `json:"file,omitempty"`
	Baseline        bool               `json:"baseline,omitempty"`
	Outcome         outcome            `json:"outcome"`
	Duration        *run.Duration      `json:"duration,omitempty"`
	Ops             int                `json:"ops"`
	Applied         int                `json:"applied"`
	OpTypes         map[run.OpType]int `json:"opTypes"`
	FaultsApplied   int                `json:"faultsApplied"`
	FaultedRequests int                `json:"faultedRequests"`
	Checkpoints     int                `json:"checkpoints"`
	Exits           []exitSummary      `json:"exits,omitempty"`
	Notes           []string           `json:"notes,omitempty"`
	Violation       *report.Check      `json:"violation,omitempty"`
	Error           string             `json:"error,omitempty"`
	Dir             string             `json:"dir,omitempty"`
	Sequence        run.Sequence       `json:"sequence"`
	// rerunning is set once reconciler-fuzzer starts to run the minimized sequence
	// into Dir.
	rerunning bool
}

// exitSummary is one time the target stopped on its own.
type exitSummary struct {
	At    time.Time `json:"at"`
	Error string    `json:"error,omitempty"`
	Said  string    `json:"said,omitempty"`
}

func newSummary(opts options, t *target.Target, runs []planned, start time.Time) *summary {
	s := &summary{
		Schema:           summarySchema,
		Command:          opts.command,
		ReconcilerFuzzer: version(),
		Target:           summaryTarget{Name: t.Name, Version: t.Version, File: opts.target},
		Seed:             opts.invocationSeed(runs[0].sequence),
		LaunchArgs:       opts.launchArgs,
		Cluster:          "envtest",
		Start:            start.UTC(),
		Outcome:          outcomeUnfinished,
		Runs:             make([]runSummary, len(runs)),
	}
	if opts.kubeconfig != "" {
		s.Cluster = "kubeconfig"
	}
	for i, planned := range runs {
		s.Runs[i] = runSummary{
			Run:      i + 1,
			Seed:     planned.sequence.Seed,
			File:     planned.path,
			Baseline: planned.baseline,
			Outcome:  outcomeNotRun,
			Ops:      len(planned.sequence.Ops),
			OpTypes:  map[run.OpType]int{},
			Sequence: planned.sequence,
		}
		for _, op := range planned.sequence.Ops {
			s.Runs[i].OpTypes[op.Type]++
		}
	}
	return s
}

func (s *summary) deadline(opts options) {
	s.Deadline, s.DeadlineDerived = run.Duration(opts.deadline), !opts.deadlineGiven
}

// finish records how the invocation ended, with the code reconciler-fuzzer
// exits with.
func (s *summary) finish(ctx context.Context, code int, at time.Time) {
	s.Finish = at.UTC()
	status := exitStatus(ctx, code)
	s.ExitCode = &status
	switch _, stopped := interruption(ctx); {
	case stopped:
		s.Outcome = outcomeInterrupted
		if code == exitOK {
			s.Error = "an interrupt arrived after every run passed"
		}
	case code == exitOK:
		s.Outcome = outcomePassed
	case code == exitViolation:
		s.Outcome = outcomeViolation
	default:
		s.Outcome = outcomeError
	}
}

// ran records what a run did, however it ended.
func (r *runSummary) ran(result run.Result, took time.Duration) {
	duration := run.Duration(took.Round(time.Millisecond))
	r.Duration = &duration
	r.Applied = len(result.Timeline.Ops)
	r.Checkpoints = len(result.Timeline.Checkpoints)
	for _, window := range result.Timeline.Faults {
		if !window.Start.IsZero() {
			r.FaultsApplied++
		}
	}
	for _, request := range result.Recorded.Requests {
		if request.Fault != "" {
			r.FaultedRequests++
		}
	}
	for _, exit := range result.Timeline.Exits {
		exited := exitSummary{At: exit.At, Said: exit.Said}
		if exit.Err != nil {
			exited.Error = exit.Err.Error()
		}
		r.Exits = append(r.Exits, exited)
	}
	r.Notes = result.Notes
}

func (r *runSummary) found(violation run.Violation, notes []string, dir string) {
	r.Outcome = outcomeViolation
	r.Violation = &report.Check{ID: violation.ID, Statement: violation.Statement, At: violation.At, Evidence: violation.Evidence}
	r.Notes = notes
	r.Dir = filepath.Base(dir)
}

// holds is what a failing run's directory holds. The report comes last.
func (r runSummary) holds(reported bool) string {
	switch {
	case reported:
		return "the report and the evidence"
	case r.rerunning:
		return "a partial run of the minimized sequence"
	}
	return "the evidence"
}

// stopped records a run that did not finish, and the directory it made.
func (r *runSummary) stopped(err error, dir string) {
	r.Outcome = outcomeError
	if errors.Is(err, errRunInterrupted) {
		r.Outcome = outcomeInterrupted
	}
	r.Error = err.Error()
	if _, statErr := os.Stat(dir); statErr == nil {
		r.Dir = filepath.Base(dir)
	}
}

// write puts the summary in the invocation's directory.
func (s *summary) write(dir string) error {
	encoded, err := s.marshal()
	if err != nil {
		return err
	}
	return errors.Join(run.WriteAtomic(filepath.Join(dir, summaryJSONFile), encoded),
		run.WriteAtomic(filepath.Join(dir, summaryMarkdownFile), s.markdown()))
}

func (s *summary) marshal() ([]byte, error) {
	var encoded bytes.Buffer
	encoder := json.NewEncoder(&encoded)
	encoder.SetEscapeHTML(false)
	encoder.SetIndent("", "  ")
	if err := encoder.Encode(s); err != nil {
		return nil, fmt.Errorf("encoding the summary: %w", err)
	}
	return encoded.Bytes(), nil
}
