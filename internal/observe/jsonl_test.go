package observe_test

import (
	"bytes"
	"encoding/json"
	"fmt"
	"maps"
	"slices"
	"strings"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/rosenhouse-bot-sandbox/reconciler-fuzzer/internal/observe"
)

// line decodes one objects.jsonl line into the generic shape a report reads.
func line(t *testing.T, raw []byte) map[string]any {
	t.Helper()
	var decoded map[string]any
	if err := json.Unmarshal(raw, &decoded); err != nil {
		t.Fatalf("Decoding %q failed: %v", raw, err)
	}
	return decoded
}

func writeHistory(t *testing.T, s *observe.Store) []string {
	t.Helper()
	var out bytes.Buffer
	if err := s.WriteHistory(&out); err != nil {
		t.Fatalf("WriteHistory returned an error: %v", err)
	}
	written := out.String()
	if written == "" {
		return nil
	}
	if !strings.HasSuffix(written, "\n") {
		t.Errorf("The history does not end with a newline: %q", written)
	}
	return strings.Split(strings.TrimSuffix(written, "\n"), "\n")
}

func TestWriteHistoryWritesOneLinePerVersionInOrder(t *testing.T) {
	s := observe.NewStore(managing(configMapGVK))
	s.Record(configMapGVK, object(configMapGVK, "child", "10"), at(0))
	s.Record(widgetGVK, object(widgetGVK, "widget", "11"), at(1))
	s.RecordDeletion(configMapGVK, object(configMapGVK, "child", "12"), at(2))

	lines := writeHistory(t, s)
	if len(lines) != 3 {
		t.Fatalf("WriteHistory wrote %d lines, want 3.", len(lines))
	}
	var recorded []string
	for _, raw := range lines {
		decoded := line(t, []byte(raw))
		recorded = append(recorded, fmt.Sprintf("%s@%s", decoded["kind"], decoded["resourceVersion"]))
	}
	want := []string{"v1/ConfigMap@10", "toy.reconciler-fuzzer/v1/Widget@11", "v1/ConfigMap@12"}
	if !slices.Equal(recorded, want) {
		t.Errorf("The lines hold %v, want %v: the versions in the order recorded.", recorded, want)
	}
}

func TestWriteHistoryOfAnEmptyStoreWritesNothing(t *testing.T) {
	if lines := writeHistory(t, observe.NewStore(managing(configMapGVK))); len(lines) != 0 {
		t.Errorf("WriteHistory wrote %v for an empty store.", lines)
	}
}

func TestAHistoryLineCarriesWhatTheInvariantsRead(t *testing.T) {
	deletionTimestamp := metav1.NewTime(at(9))
	obj := withStatus(withOwner(withData(object(configMapGVK, "child", "10"), "content"), "uid-widget"), map[string]any{
		"observedGeneration": int64(4),
	})
	obj.SetGeneration(5)
	obj.SetFinalizers([]string{"widget.reconciler-fuzzer/cleanup"})
	obj.SetLabels(map[string]string{"app": "toy"})
	obj.SetDeletionTimestamp(&deletionTimestamp)

	s := observe.NewStore(managing(configMapGVK))
	s.RecordDeletion(configMapGVK, obj, at(0))

	decoded := line(t, []byte(writeHistory(t, s)[0]))
	want := []string{
		"deleted", "deletionTimestamp", "finalizers", "generation", "kind", "labels", "name",
		"namespace", "object", "observedGeneration", "ownerReferences", "resourceVersion", "time", "uid",
	}
	if got := slices.Sorted(maps.Keys(decoded)); !slices.Equal(got, want) {
		t.Errorf("The line holds fields %v, want %v.", got, want)
	}
	for field, want := range map[string]any{
		"kind":               "v1/ConfigMap",
		"namespace":          namespace,
		"name":               "child",
		"uid":                "uid-child",
		"resourceVersion":    "10",
		"time":               at(0).Format(time.RFC3339Nano),
		"generation":         float64(5),
		"observedGeneration": float64(4),
		"deleted":            true,
	} {
		if got := decoded[field]; got != want {
			t.Errorf("The line holds %s=%v, want %v.", field, got, want)
		}
	}
	if got := decoded["object"].(map[string]any)["data"]; got == nil {
		t.Error("The line does not carry the whole object, which G5 compares.")
	}
}

func TestAHistoryLineOmitsWhatTheObjectDoesNotCarry(t *testing.T) {
	s := observe.NewStore(managing(configMapGVK))
	s.Record(configMapGVK, object(configMapGVK, "child", "10"), at(0))

	decoded := line(t, []byte(writeHistory(t, s)[0]))
	want := []string{"kind", "name", "namespace", "object", "resourceVersion", "time", "uid"}
	if got := slices.Sorted(maps.Keys(decoded)); !slices.Equal(got, want) {
		t.Errorf("The line holds fields %v, want %v.", got, want)
	}
}
