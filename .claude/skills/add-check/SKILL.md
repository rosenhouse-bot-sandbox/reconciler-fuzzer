---
name: add-check
description: Add or change a check in reconciler-fuzzer, such as Converge, Clean, Restore or Unique, or an exception to one. Covers the DESIGN.md entries, the check function, tracetest and corpus tests, a toy bug that trips it, and the bug matrix. Use when asked to add, split, redefine or relax a check.
---

# Add a check

A check is a pure function of the trace and the spec. DESIGN.md §12
defines each one. Follow these steps in order.

1. **Design first.** Add the check's rule to DESIGN.md §12, and an entry to
   the §18 decision log. An entry is one line that says what changed and
   which bug or target needs it. DESIGN.md must stay under 500 lines.

2. **Write a failing test.** In `internal/check/<name>_test.go`, build
   traces with `internal/trace/tracetest` (`tt.NewScript`, `Begin`,
   `CreateCR`, `Create`, `EndFaults`, `Settle`). Use the helpers in
   `clean_test.go`: `newRun`, `judgeWith` and `requireFinding`. Write one
   trace that fails the check and one that passes. Run them red.

3. **Implement it.** Write `internal/check/<name>.go` as
   `func <Name>(ix *trace.Index, s Spec) []Finding`. Give each finding a
   `Check`, `Step`, `At`, a one-line `Summary`, `Details` and a `Kind`.
   Hunt and shrink match findings by `Signature()`, so keep names and
   steps out of the summary. Register it in `Judge` in
   `internal/check/check.go`. Unless the check is about settling, judge
   only settled steps (§18, entry 15): `newView` in `clean.go` gives them,
   and the groups of DESIGN.md §11.

4. **Guard against false positives.** For each false positive a real
   target showed, add `internal/check/testdata/corpus/<name>.jsonl` and a
   `<name>.want`. Its first line is `# <source>: <sentence>`, such as
   `# M2 kro: <what the trace shows>`. Each other line is an expected
   finding, `<check> <step>` (DESIGN.md §12, Corpus). `TestCorpus` requires
   exactly those.

5. **Add a toy bug that trips it.** Unless an existing bug already does:
   - add `B<n>` to the enum in `targets/toy-widget/controller/bug.go`,
     raise `MaxBug`, and update `controller/bug_test.go`;
   - add its case behind `r.Bug` in `controller/widget_controller.go`, with
     a test in `controller/bugs_test.go`;
   - describe it in the `bugs` map in `targets/toy-widget/bugmatrix_test.go`;
   - add `targets/toy-widget/plans/b<n>.json`, a plan that catches it;
   - add a row to `gate` in `targets/toy-widget/gate_test.go`: plan, bug,
     report heading, step and a detail line.

6. **Verify.**

   ```
   make bug-matrix
   make test
   make test-envtest
   ```

   `make test-envtest` runs every plan under its bug and under `--bug=0`,
   which must give no findings and no notes.

Never retry or sleep to make a check pass. Tune its window, or add an
exception with a decision-log entry.
