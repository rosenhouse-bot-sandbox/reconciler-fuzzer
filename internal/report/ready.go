package report

import (
	"bytes"
	"encoding/json"
	"fmt"
	"maps"
	"strings"
	"unicode/utf8"

	"github.com/rosenhouse-bot-sandbox/reconciler-fuzzer/internal/invariant"
)

// A CR's status holds whatever its controller wrote there, so a report quotes
// a bounded part of it: maxText bytes of each condition's field, and
// maxStatus bytes of the rest. objects.jsonl keeps the whole CR.
const (
	maxText   = 200
	maxStatus = 1000
)

// ready is the Ready predicate as both files quote it.
type ready struct {
	Expr            string      `json:"expr,omitempty"`
	CR              string      `json:"cr,omitempty"`
	Error           string      `json:"error,omitempty"`
	Conditions      []condition `json:"conditions,omitempty"`
	ConditionsTotal int         `json:"conditionsTotal,omitempty"`
	// Status is the rest of the CR's status as JSON, cut to maxStatus bytes,
	// and StatusBytes its length before the cut.
	Status      string `json:"status,omitempty"`
	StatusBytes int    `json:"statusBytes,omitempty"`
}

type condition struct {
	Type               string `json:"type"`
	Status             string `json:"status"`
	Reason             string `json:"reason,omitempty"`
	Message            string `json:"message,omitempty"`
	ObservedGeneration string `json:"observedGeneration,omitempty"`
}

func quoteReady(r *invariant.Readiness) *ready {
	if r == nil {
		return nil
	}
	quoted := &ready{Expr: r.Expr, CR: r.CR, Error: r.Error}
	rest := maps.Clone(r.Status)
	if listed, isList := rest["conditions"].([]any); isList && len(listed) > 0 {
		delete(rest, "conditions")
		quoted.ConditionsTotal = len(listed)
		for _, entry := range listed[:min(len(listed), maxEvidence)] {
			fields, _ := entry.(map[string]any)
			quoted.Conditions = append(quoted.Conditions, condition{
				Type:               text(fields["type"]),
				Status:             text(fields["status"]),
				Reason:             text(fields["reason"]),
				Message:            text(fields["message"]),
				ObservedGeneration: text(fields["observedGeneration"]),
			})
		}
	}
	if len(rest) > 0 {
		var encoded bytes.Buffer
		encoder := json.NewEncoder(&encoded)
		encoder.SetEscapeHTML(false)
		_ = encoder.Encode(rest) // Status came from JSON, so it encodes.
		whole := strings.TrimSuffix(encoded.String(), "\n")
		quoted.Status, quoted.StatusBytes = cut(whole, maxStatus), len(whole)
	}
	return quoted
}

// text is one field of a condition, cut to maxText bytes.
func text(value any) string {
	if value == nil {
		return ""
	}
	return cut(fmt.Sprint(value), maxText)
}

// cut keeps at most n bytes of s, ending on a whole character, and marks the
// cut.
func cut(s string, n int) string {
	if len(s) <= n {
		return s
	}
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	return s[:n] + "…"
}

func (r *ready) markdown(md *strings.Builder) {
	md.WriteString("\n## Ready predicate\n\n")
	fmt.Fprintf(md, "The verdict evaluated the target's `ready` on the CR `%s`:\n\n%s\n", r.CR, fenced("", r.Expr))
	if r.Error != "" {
		fmt.Fprintf(md, "\nEvaluating it on the CR at the verdict failed:\n\n%s\n", fenced("", r.Error))
	}
	if r.ConditionsTotal > len(r.Conditions) {
		fmt.Fprintf(md, "\nThe CR carried %d conditions at the verdict; the report quotes the first %d.\n\n", r.ConditionsTotal, len(r.Conditions))
	} else if len(r.Conditions) > 0 {
		md.WriteString("\nThe CR's conditions at the verdict:\n\n")
	}
	if len(r.Conditions) > 0 {
		rows := make([][]string, len(r.Conditions))
		for i, c := range r.Conditions {
			rows[i] = []string{cell(c.Type), cell(c.Status), cell(c.Reason), cell(c.Message), cell(c.ObservedGeneration)}
		}
		table(md, []string{"type", "status", "reason", "message", "observedGeneration"}, rows)
	}
	if r.Status != "" {
		status := "The CR's status at the verdict"
		if r.ConditionsTotal > 0 {
			status = "The rest of its status"
		}
		if r.StatusBytes > len(r.Status) {
			status += fmt.Sprintf(", cut to %d of %d bytes", len(strings.TrimSuffix(r.Status, "…")), r.StatusBytes)
		}
		fmt.Fprintf(md, "\n%s:\n\n%s\n", status, fenced("json", r.Status))
	}
	if r.ConditionsTotal == 0 && r.Status == "" {
		md.WriteString("\nThe CR carried no status at the verdict.\n")
	}
	md.WriteString("\n`objects.jsonl` holds every version of the CR whole.\n")
}

// fenced is s in a code fence longer than any run of backticks in it.
func fenced(info, s string) string {
	fence := "```"
	for strings.Contains(s, fence) {
		fence += "`"
	}
	return fence + info + "\n" + s + "\n" + fence
}

// cell keeps a table row on one line.
func cell(s string) string {
	return strings.NewReplacer("|", `\|`, "\n", " ").Replace(s)
}
