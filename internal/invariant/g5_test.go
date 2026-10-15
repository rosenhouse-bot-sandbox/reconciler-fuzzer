package invariant_test

import (
	"encoding/base64"
	"fmt"
	"maps"
	"math"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"pgregory.net/rapid"

	"github.com/rosenhouse-bot-sandbox/reconciler-fuzzer/internal/invariant"
	"github.com/rosenhouse-bot-sandbox/reconciler-fuzzer/internal/observe"
	"github.com/rosenhouse-bot-sandbox/reconciler-fuzzer/internal/target"
)

// restartRun converges after the create at 5s, restarts the target at 10s and
// converges again after a settle at 15s. The Widget lives across the restart;
// the objects are what G5 compares.
func restartRun(before, after []*unstructured.Unstructured) *run {
	return restartRunOf(newRun(), before, after)
}

func restartRunOf(r *run, before, after []*unstructured.Unstructured) *run {
	return r.
		op(invariant.OpCreate, 0).
		record(time.Second, append([]*unstructured.Unstructured{widget("10", spec(1), status(1, 1))}, before...)...).
		checkpoint(5*time.Second, invariant.Converged).
		op(invariant.OpRestart, 10*time.Second).
		op(invariant.OpSettle, 11*time.Second).
		record(12*time.Second, append([]*unstructured.Unstructured{widget("20", spec(1), status(1, 1))}, after...)...).
		checkpoint(15*time.Second, invariant.Converged)
}

// restarted returns a run whose one managed object changed across the restart.
func restarted(before, after *unstructured.Unstructured) invariant.Input {
	return restartRun([]*unstructured.Unstructured{before}, []*unstructured.Unstructured{after}).
		through(20 * time.Second)
}

func TestG5PassesWhenTheRestartChangesNothing(t *testing.T) {
	in := restarted(child("w-0", "11", data("0")), child("w-0", "21", data("0")))

	silent(t, invariant.RestartStable, in)
}

func TestG5FiresOnAnObjectOnlyTheRestartBroughtBack(t *testing.T) {
	in := restartRun(nil, []*unstructured.Unstructured{child("w-0", "21", data("0"))}).
		through(20 * time.Second)

	violation := fired(t, invariant.RestartStable, in)

	if violation.ID != "G5" {
		t.Errorf("The violation is %q, want G5.", violation.ID)
	}
	if want := "the v1/ConfigMap w-0 appeared only after the Restart at op 1 (restart)"; violation.Statement != want {
		t.Errorf("The statement is %q, want %q.", violation.Statement, want)
	}
	if want := timelineOf(configMapGVK, "w-0"); violation.VersionsOf != want {
		t.Errorf("The timeline is of %q, want %q.", violation.VersionsOf, want)
	}
	if len(violation.Versions) == 0 || violation.Versions[0].Name != "w-0" {
		t.Fatalf("The evidence holds %v, want the ConfigMap's timeline.", violation.Versions)
	}
	want := []invariant.Difference{{
		Object: "v1/ConfigMap w-0", ResourceVersions: [2]string{"", "21"},
		Before: "(absent)", After: "(present)",
	}}
	if !reflect.DeepEqual(violation.Differences, want) {
		t.Errorf("G5 quoted %+v, want %+v.", violation.Differences, want)
	}
}

func TestG5FiresOnAnObjectTheRestartDropped(t *testing.T) {
	in := restartRun([]*unstructured.Unstructured{child("w-0", "11", data("0"))}, nil).
		record(13*time.Second, child("w-0", "22", data("0"))).
		remove(14*time.Second, child("w-0", "23", data("0"))).
		through(20 * time.Second)

	violation := fired(t, invariant.RestartStable, in)

	if want := "the v1/ConfigMap w-0 is gone after the Restart at op 1 (restart)"; violation.Statement != want {
		t.Errorf("The statement is %q, want %q.", violation.Statement, want)
	}
	want := []invariant.Difference{{
		Object: "v1/ConfigMap w-0", ResourceVersions: [2]string{"11", ""},
		Before: "(present)", After: "(absent)",
	}}
	if !reflect.DeepEqual(violation.Differences, want) {
		t.Errorf("G5 quoted %+v, want %+v.", violation.Differences, want)
	}
}

func TestG5NamesTheFieldThatChanged(t *testing.T) {
	for _, c := range []struct {
		before, after      option
		path, was, becomes string
	}{
		{data("0"), data("1"), "data.index", `"0"`, `"1"`},
		{condition("True", 0), condition("False", 0), "status.conditions[*].status", `"True"`, `"False"`},
		{finalizers("keep/one"), finalizers("keep/two", "keep/three"), "metadata.finalizers", `["keep/one"]`, `["keep/two","keep/three"]`},
	} {
		t.Run(c.path, func(t *testing.T) {
			in := restarted(child("w-0", "11", c.before), child("w-0", "21", c.after))

			violation := fired(t, invariant.RestartStable, in)

			if want := "the v1/ConfigMap w-0 changed across the Restart at op 1 (restart)"; violation.Statement != want {
				t.Errorf("The statement is %q, want %q.", violation.Statement, want)
			}
			want := []invariant.Difference{{
				Object: "v1/ConfigMap w-0", ResourceVersions: [2]string{"11", "21"},
				Path: c.path, Before: c.was, After: c.becomes,
			}}
			if !reflect.DeepEqual(violation.Differences, want) || violation.DifferencesTotal != 1 {
				t.Errorf("G5 quoted %+v of %d differences, want %+v.", violation.Differences, violation.DifferencesTotal, want)
			}
		})
	}
}

func TestG5QuotesNoFieldItIgnores(t *testing.T) {
	live := metav1.OwnerReference{APIVersion: "toy.reconciler-fuzzer/v1", Kind: "Widget", Name: widgetName, UID: widgetUID}
	gone := metav1.OwnerReference{APIVersion: "toy.reconciler-fuzzer/v1", Kind: "Widget", Name: "gone", UID: "uid-gone"}
	in := restarted(
		child("w-0", "11", data("0"), uid("uid-old"), generation(1), created(0), managedFields("one"),
			ownedBy(gone, live), annotations(map[string]string{startedAt: "1"})),
		child("w-0", "21", data("1"), uid("uid-new"), generation(7), created(11*time.Second), managedFields("two"),
			ownedBy(live), annotations(map[string]string{startedAt: "2"})))
	in.Target.EqualIgnore = []target.Path{target.MustParsePath(`metadata.annotations["` + startedAt + `"]`)}

	violation := fired(t, invariant.RestartStable, in)

	if got := paths(violation); !slices.Equal(got, []string{"data.index"}) {
		t.Errorf("G5 quoted the paths %q, want only data.index.", got)
	}
}

func TestG5QuotesAnAnnotationKeyThatHoldsADot(t *testing.T) {
	in := restarted(
		child("w-0", "11", annotations(map[string]string{startedAt: "1"})),
		child("w-0", "21", annotations(map[string]string{startedAt: "2"})))

	violation := fired(t, invariant.RestartStable, in)

	if got, want := paths(violation), []string{`metadata.annotations["` + startedAt + `"]`}; !slices.Equal(got, want) {
		t.Errorf("G5 quoted the paths %q, want %q.", got, want)
	}
}

// A map only one version holds compares as an empty one, so a restart's first
// stamp names its own key.
func TestG5NamesEachKeyOfAMapOnlyOneVersionHolds(t *testing.T) {
	stamped := annotations(map[string]string{startedAt: "1"})
	nested := func(u *unstructured.Unstructured) {
		u.Object["spec"] = map[string]any{"a": map[string]any{"b": int64(1)}, "c": map[string]any{}}
	}
	for _, c := range []struct {
		name          string
		before, after option
		want          [][3]string
	}{
		{"a stamp the restart added", nothing, stamped, [][3]string{{`metadata.annotations["` + startedAt + `"]`, "(absent)", `"1"`}}},
		{"a stamp the restart dropped", stamped, nothing, [][3]string{{`metadata.annotations["` + startedAt + `"]`, `"1"`, "(absent)"}}},
		{"nested maps", nothing, nested, [][3]string{{"spec.a.b", "(absent)", "1"}, {"spec.c", "(absent)", "{}"}}},
	} {
		t.Run(c.name, func(t *testing.T) {
			in := restarted(child("w-0", "11", c.before), child("w-0", "21", c.after))

			violation := fired(t, invariant.RestartStable, in)

			var got [][3]string
			for _, d := range violation.Differences {
				got = append(got, [3]string{d.Path, d.Before, d.After})
			}
			if !slices.Equal(got, c.want) {
				t.Errorf("G5 quoted %q, want %q.", got, c.want)
			}
		})
	}
}

// Ignoring a path G5 prints drops its rows and those below it, and keeps every
// row that is neither above nor below it.
func TestG5PrintsNoPathBroaderThanItsRow(t *testing.T) {
	rapid.Check(t, func(rt *rapid.T) {
		in := restarted(child("w-0", "11", drawnFields(rt, "before")), child("w-0", "21", drawnFields(rt, "after")))
		was := differences(rt, in)
		if len(was) == 0 {
			return
		}
		ignored := target.MustParsePath(rapid.SampledFrom(was).Draw(rt, "ignored").Path)
		in.Target.EqualIgnore = []target.Path{ignored}
		is := differences(rt, in)
		for _, d := range is {
			if below(d.Path, ignored) {
				rt.Fatalf("Ignoring %s left %+v.", ignored, d)
			}
		}
		for _, d := range was {
			if !below(d.Path, ignored) && !below(ignored.String(), target.MustParsePath(d.Path)) && !slices.Contains(is, d) {
				rt.Fatalf("Ignoring %s dropped %+v; G5 then quoted %+v.", ignored, d, is)
			}
		}
	})
}

func differences(rt *rapid.T, in invariant.Input) []invariant.Difference {
	result, err := invariant.RestartStable(in)
	if err != nil {
		rt.Fatal(err)
	}
	if len(result.Violations) == 0 {
		return nil
	}
	return result.Violations[0].Differences
}

// below reports whether path is ancestor or a path under it.
func below(path string, ancestor target.Path) bool {
	p := target.MustParsePath(path)
	return len(p) >= len(ancestor) && slices.Equal(p[:len(ancestor)], ancestor)
}

// Ignoring each path G5 prints leaves fewer differences, until none remain.
func TestG5PrintsPathsThatEqualIgnoreTakes(t *testing.T) {
	rapid.Check(t, func(rt *rapid.T) {
		in := restarted(child("w-0", "11", drawnFields(rt, "before")), child("w-0", "21", drawnFields(rt, "after")))
		for left := math.MaxInt; ; {
			result, err := invariant.RestartStable(in)
			if err != nil {
				rt.Fatal(err)
			}
			if len(result.Violations) == 0 {
				return
			}
			violation := result.Violations[0]
			if violation.DifferencesTotal >= left {
				rt.Fatalf("Ignoring every path G5 printed left %d differences, want fewer than %d: %+v",
					violation.DifferencesTotal, left, violation.Differences)
			}
			left = violation.DifferencesTotal
			for _, difference := range violation.Differences {
				path, err := target.ParsePath(difference.Path)
				if err != nil {
					rt.Fatalf("G5 printed %s, which equalIgnore refuses: %v", difference.Path, err)
				}
				in.Target.EqualIgnore = append(in.Target.EqualIgnore, path)
			}
		}
	})
}

// drawnFields sets top-level fields and annotations whose keys need quoting,
// or name labels or annotations further down.
func drawnFields(rt *rapid.T, label string) option {
	key := rapid.SampledFrom([]string{"a", "b", "x.y", "p/q", `"`, "*", "", "metadata", "labels", "annotations"})
	fields := rapid.MapOfN(rapid.SampledFrom([]string{"data", "spec", "x.y"}), jsonValue(key, 4), 0, 3).Draw(rt, label)
	annotated := rapid.MapOfN(key, rapid.SampledFrom([]string{"0", "1"}), 0, 3).Draw(rt, label+" annotations")
	return func(u *unstructured.Unstructured) {
		maps.Copy(u.Object, fields)
		if len(annotated) > 0 {
			u.SetAnnotations(annotated)
		}
	}
}

func jsonValue(key *rapid.Generator[string], depth int) *rapid.Generator[any] {
	scalar := rapid.OneOf(
		rapid.Map(rapid.SampledFrom([]string{"0", "1"}), func(s string) any { return s }),
		rapid.Map(rapid.Int64Range(0, 1), func(n int64) any { return n }),
		rapid.Just[any](nil),
	)
	if depth == 0 {
		return scalar
	}
	return rapid.OneOf(
		scalar,
		rapid.Map(rapid.MapOfN(key, jsonValue(key, depth-1), 0, 3), func(m map[string]any) any { return m }),
		rapid.Map(rapid.SliceOfN(jsonValue(key, depth-1), 0, 2), func(l []any) any { return l }),
	)
}

// A path ends one step below labels or annotations, so G5 quotes a label whole
// whatever it holds.
func TestG5QuotesALabelWhole(t *testing.T) {
	labelled := func(value string) option {
		return func(u *unstructured.Unstructured) {
			u.Object["spec"] = map[string]any{"metadata": map[string]any{"labels": map[string]any{"app": map[string]any{"x": value}}}}
		}
	}
	in := restarted(child("w-0", "11", labelled("0")), child("w-0", "21", labelled("1")))

	violation := fired(t, invariant.RestartStable, in)

	if got := paths(violation); !slices.Equal(got, []string{"spec.metadata.labels.app"}) {
		t.Errorf("G5 quoted the paths %q, want spec.metadata.labels.app.", got)
	}
}

func TestG5BoundsTheDifferencesItQuotes(t *testing.T) {
	in := restarted(child("w-0", "11", keys(25, "0")), child("w-0", "21", keys(25, "1")))

	violation := fired(t, invariant.RestartStable, in)

	if violation.DifferencesTotal != 25 {
		t.Errorf("G5 counted %d differences, want 25.", violation.DifferencesTotal)
	}
	var want []string
	for i := range invariant.MaxEvidence {
		want = append(want, fmt.Sprintf("data.k%02d", i))
	}
	if got := paths(violation); !slices.Equal(got, want) {
		t.Errorf("G5 quoted the paths %q, want the first %d in order.", got, invariant.MaxEvidence)
	}
}

// keys sets n data keys to the value.
func keys(n int, value string) option {
	return func(u *unstructured.Unstructured) {
		fields := map[string]any{}
		for i := range n {
			fields[fmt.Sprintf("k%02d", i)] = value
		}
		u.Object["data"] = fields
	}
}

func TestG5ReportsEveryObjectARestartChangedAtOnce(t *testing.T) {
	in := restartRun(
		[]*unstructured.Unstructured{child("w-0", "11", data("0")), child("w-1", "12", data("0"))},
		[]*unstructured.Unstructured{child("w-0", "21", data("1")), child("w-1", "22", data("1"))}).
		through(20 * time.Second)

	violation := fired(t, invariant.RestartStable, in)

	if want := "2 objects differ across the Restart at op 1 (restart), the first the v1/ConfigMap w-0, which changed across it"; violation.Statement != want {
		t.Errorf("The statement is %q, want %q.", violation.Statement, want)
	}
	if got, want := objectsOf(violation), []string{"v1/ConfigMap w-0", "v1/ConfigMap w-1"}; !slices.Equal(got, want) {
		t.Errorf("G5 quoted the differences of %q, want %q.", got, want)
	}
}

// The bound leaves out the differences of the object with the most, not
// every object but the first.
func TestG5QuotesEveryObjectWithinItsBound(t *testing.T) {
	in := restartRun(
		[]*unstructured.Unstructured{child("w-0", "11", keys(25, "0")), child("w-1", "12", keys(25, "0")), child("w-2", "13", data("0"))},
		[]*unstructured.Unstructured{child("w-0", "21", keys(25, "1")), child("w-1", "22", keys(25, "1")), child("w-2", "23", data("1"))}).
		through(20 * time.Second)

	violation := fired(t, invariant.RestartStable, in)

	quotedOf := map[string]int{}
	for _, difference := range violation.Differences {
		quotedOf[difference.Object]++
	}
	if want := map[string]int{"v1/ConfigMap w-0": 10, "v1/ConfigMap w-1": 9, "v1/ConfigMap w-2": 1}; !maps.Equal(quotedOf, want) {
		t.Errorf("G5 quoted %v differences of each object, want %v.", quotedOf, want)
	}
	if len(violation.Differences) != invariant.MaxEvidence || violation.DifferencesTotal != 51 {
		t.Errorf("G5 quoted %d of %d differences, want %d of 51.",
			len(violation.Differences), violation.DifferencesTotal, invariant.MaxEvidence)
	}
}

func TestG5QuotesAValueAsCompactJSON(t *testing.T) {
	for _, c := range []struct {
		name  string
		value any
		want  string
	}{
		{"a list", []any{map[string]any{"b": int64(1), "a": "<&>"}}, `[{"a":"<&>","b":1}]`},
		{"a string of 80 runes", strings.Repeat("é", 78), `"` + strings.Repeat("é", 78) + `"`},
		{"a long string", strings.Repeat("é", 100), `"` + strings.Repeat("é", 79) + "…"},
		{"null", nil, "null"},
	} {
		t.Run(c.name, func(t *testing.T) {
			in := restarted(child("w-0", "11"), child("w-0", "21", func(u *unstructured.Unstructured) { u.Object["spec"] = c.value }))

			violation := fired(t, invariant.RestartStable, in)

			if len(violation.Differences) != 1 {
				t.Fatalf("G5 quoted %+v, want one difference.", violation.Differences)
			}
			if got := violation.Differences[0]; got.Before != "(absent)" || got.After != c.want {
				t.Errorf("G5 quoted %+v, want spec (absent) before and %s after.", got, c.want)
			}
		})
	}
}

func TestG5QuotesASecretsValuesAsMarkers(t *testing.T) {
	const old, current = "s3cr3t", "t0ps3cr3t"
	for _, c := range []struct {
		name          string
		before, after option
	}{
		{"a value that changed", secretData(old), secretData(current)},
		{"data only one side holds", nothing, secretData(current)},
		{"an annotation", annotations(map[string]string{"hash": old}), annotations(map[string]string{"hash": current})},
		{"a map the API server never serves", secretField(map[string]any{"x": old}), secretField(map[string]any{"x": current})},
		{"a list the API server never serves", secretField([]any{old}), secretField([]any{current})},
	} {
		t.Run(c.name, func(t *testing.T) {
			in := restartRunOf(newRunManaging(configMapGVK, secretGVK),
				[]*unstructured.Unstructured{secret("s-0", "11", c.before)},
				[]*unstructured.Unstructured{secret("s-0", "21", c.after)}).
				through(20 * time.Second)

			violation := fired(t, invariant.RestartStable, in)

			if len(violation.Differences) == 0 {
				t.Fatal("G5 quoted no difference.")
			}
			for _, difference := range violation.Differences {
				for _, value := range []string{old, current, base64.StdEncoding.EncodeToString([]byte(old)), base64.StdEncoding.EncodeToString([]byte(current))} {
					if strings.Contains(difference.Before+difference.After, value) {
						t.Errorf("G5 quoted %+v, which holds %q.", difference, value)
					}
				}
				if !strings.Contains(difference.After, "[redacted ") {
					t.Errorf("G5 quoted %+v, want the marker objects.jsonl writes.", difference)
				}
			}
		})
	}
}

func secretData(value string) option {
	return secretField(base64.StdEncoding.EncodeToString([]byte(value)))
}

func secretField(value any) option {
	return func(u *unstructured.Unstructured) { u.Object["data"] = map[string]any{"token": value} }
}

func TestG5JudgesAtTheStateAfterTheRestart(t *testing.T) {
	in := restartRun([]*unstructured.Unstructured{child("w-0", "11", data("0"))}, []*unstructured.Unstructured{child("w-0", "21", data("1"))}).
		record(17*time.Second, child("w-0", "31", data("2"))).
		through(20 * time.Second)

	violation := fired(t, invariant.RestartStable, in)

	if !violation.At.Equal(at(15 * time.Second)) {
		t.Errorf("G5 is stamped %v, want the converged state after the restart at %v.", violation.At, at(15*time.Second))
	}
	if got := quoted(violation); !slices.Equal(got, []string{"w-0@11", "w-0@21"}) {
		t.Errorf("G5 quoted the versions %v, want none recorded after it judged.", got)
	}
	if want := "the state converged after op 0 (create) and the one after op 2 (settle)"; violation.Compared != want {
		t.Errorf("G5 says it compared %q, want %q.", violation.Compared, want)
	}
}

func TestG5NamesAnObjectTheTargetsOwnEqualityFoundChanged(t *testing.T) {
	in := restarted(child("w-0", "11", data("0")), child("w-0", "21", data("1")))
	in.Target.Equal = func(a, b observe.Snapshot) bool {
		return reflect.DeepEqual(a.Object.Object["data"], b.Object.Object["data"])
	}

	violation := fired(t, invariant.RestartStable, in)

	want := []invariant.Difference{{
		Object: "v1/ConfigMap w-0", ResourceVersions: [2]string{"11", "21"},
		Before: "(present)", After: "(changed)",
	}}
	if !reflect.DeepEqual(violation.Differences, want) {
		t.Errorf("G5 quoted %+v, want %+v.", violation.Differences, want)
	}
}

func paths(v invariant.Violation) []string {
	out := make([]string, len(v.Differences))
	for i, difference := range v.Differences {
		out[i] = difference.Path
	}
	return out
}

func objectsOf(v invariant.Violation) []string {
	var out []string
	for _, difference := range v.Differences {
		if !slices.Contains(out, difference.Object) {
			out = append(out, difference.Object)
		}
	}
	return out
}

func TestG5IgnoresThePathsSection6Names(t *testing.T) {
	for _, ignored := range []struct {
		path          string
		before, after option
	}{
		{"metadata.resourceVersion", nothing, nothing},
		{"metadata.uid", uid("uid-old"), uid("uid-new")},
		{"metadata.creationTimestamp", created(0), created(11 * time.Second)},
		{"metadata.generation", generation(1), generation(7)},
		{"metadata.managedFields", managedFields("one"), managedFields("two")},
		{"status.conditions[*].lastTransitionTime", condition("True", 0), condition("True", 12*time.Second)},
	} {
		t.Run(ignored.path, func(t *testing.T) {
			in := restarted(child("w-0", "11", ignored.before), child("w-0", "21", ignored.after))

			silent(t, invariant.RestartStable, in)
		})
	}
}

func TestG5CountsAnEmptyConditionListAsNone(t *testing.T) {
	noConditions := func(u *unstructured.Unstructured) { u.Object["status"] = map[string]any{"conditions": []any{}} }
	in := restarted(child("w-0", "11", noConditions), child("w-0", "21"))

	silent(t, invariant.RestartStable, in)
}

func TestG5IgnoresAnOwnerReferenceWhoseOwnerIsGone(t *testing.T) {
	for _, gone := range []struct {
		kind  string
		owner option
	}{
		{"the primary kind", ownedByGhost},
		{"a managed kind", ownedBy(metav1.OwnerReference{APIVersion: "v1", Kind: "ConfigMap", Name: "gone", UID: "uid-gone"})},
	} {
		t.Run(gone.kind, func(t *testing.T) {
			in := restarted(child("w-0", "11", gone.owner), child("w-0", "21", orphaned))

			silent(t, invariant.RestartStable, in)
		})
	}
}

// An ownerReference may name any version the API server serves.
func TestG5IgnoresADanglingOwnerReferenceThatNamesAnotherVersion(t *testing.T) {
	live := metav1.OwnerReference{APIVersion: "toy.reconciler-fuzzer/v1", Kind: "Widget", Name: widgetName, UID: widgetUID}
	for _, dangling := range []struct {
		name string
		ref  metav1.OwnerReference
	}{
		{"the owner is gone", metav1.OwnerReference{APIVersion: "toy.reconciler-fuzzer/v1alpha1", Kind: "Widget", Name: "gone", UID: "uid-gone"}},
		{"the owner's name has a new UID", metav1.OwnerReference{APIVersion: "toy.reconciler-fuzzer/v1alpha1", Kind: "Widget", Name: widgetName, UID: "uid-w-before"}},
	} {
		t.Run(dangling.name, func(t *testing.T) {
			in := restarted(child("w-0", "11", ownedBy(dangling.ref, live)), child("w-0", "21", ownedBy(live)))

			silent(t, invariant.RestartStable, in)
		})
	}
}

func TestG5ComparesALiveOwnerReferenceThatNamesAnotherVersion(t *testing.T) {
	live := metav1.OwnerReference{APIVersion: "toy.reconciler-fuzzer/v1alpha1", Kind: "Widget", Name: widgetName, UID: widgetUID}
	in := restarted(child("w-0", "11", ownedBy(live)), child("w-0", "21", orphaned))

	fired(t, invariant.RestartStable, in)
}

// An owner the restart recreated is live on each side, under a new UID.
func TestG5ComparesAnOwnerReferenceThatFollowedARecreatedOwner(t *testing.T) {
	was := metav1.OwnerReference{APIVersion: "v1", Kind: "ConfigMap", Name: "p", UID: "uid-p-1"}
	is := metav1.OwnerReference{APIVersion: "v1", Kind: "ConfigMap", Name: "p", UID: "uid-p-2"}
	in := restartRun(
		[]*unstructured.Unstructured{child("c", "11", ownedBy(was)), child("p", "12", uid("uid-p-1"))},
		[]*unstructured.Unstructured{child("c", "21", ownedBy(is)), child("p", "22", uid("uid-p-2"))}).
		through(20 * time.Second)

	violation := fired(t, invariant.RestartStable, in)

	want := []invariant.Difference{{
		Object: "v1/ConfigMap c", ResourceVersions: [2]string{"11", "21"},
		Path: "metadata.ownerReferences[*].uid", Before: `"uid-p-1"`, After: `"uid-p-2"`,
	}}
	if !reflect.DeepEqual(violation.Differences, want) {
		t.Errorf("G5 quoted %+v, want %+v.", violation.Differences, want)
	}
}

// reconciler-fuzzer cannot tell that an owner of a kind the target does not
// declare is gone.
func TestG5ComparesAnOwnerReferenceOfAnUndeclaredKind(t *testing.T) {
	absent := metav1.OwnerReference{APIVersion: "apps/v1", Kind: "Deployment", Name: "absent", UID: "uid-absent"}
	in := restarted(child("w-0", "11", ownedBy(absent)), child("w-0", "21", orphaned))

	fired(t, invariant.RestartStable, in)
}

func ownedBy(refs ...metav1.OwnerReference) option {
	return func(u *unstructured.Unstructured) { u.SetOwnerReferences(refs) }
}

func TestG5ComparesEverythingElse(t *testing.T) {
	for _, compared := range []struct {
		path          string
		before, after option
	}{
		{"data", data("0"), data("1")},
		{"metadata.labels", labelled("one"), labelled("two")},
		{"metadata.annotations", annotated("one"), annotated("two")},
		{"metadata.finalizers", finalizers("keep/one"), finalizers("keep/two")},
		{"metadata.ownerReferences", ownedByWidget, orphaned},
		{"status.conditions[*].status", condition("True", 0), condition("False", 0)},
	} {
		t.Run(compared.path, func(t *testing.T) {
			in := restarted(child("w-0", "11", compared.before), child("w-0", "21", compared.after))

			violation := fired(t, invariant.RestartStable, in)
			if !strings.Contains(violation.Statement, "w-0") {
				t.Errorf("The statement is %q, want it to name the ConfigMap that changed.", violation.Statement)
			}
		})
	}
}

func TestG5IgnoresThePathsTheTargetExcludes(t *testing.T) {
	in := restarted(
		child("w-0", "11", data("0"), annotations(map[string]string{startedAt: "1"})),
		child("w-0", "21", data("1"), annotations(map[string]string{startedAt: "2"})))
	in.Target.EqualIgnore = []target.Path{
		target.MustParsePath("data.index"),
		target.MustParsePath(`metadata.annotations["` + startedAt + `"]`),
	}

	silent(t, invariant.RestartStable, in)
}

// The owner's UID is what tells a live owner from a dangling reference, so
// G5 reads it before it ignores it.
func TestG5ComparesALiveOwnerWhoseUIDItIgnores(t *testing.T) {
	in := restarted(child("w-0", "11", ownedByWidget), child("w-0", "21", orphaned))
	in.Target.EqualIgnore = []target.Path{target.MustParsePath("metadata.ownerReferences[*].uid")}

	fired(t, invariant.RestartStable, in)
}

const startedAt = "probe.example.com/started-at"

func annotations(values map[string]string) option {
	return func(u *unstructured.Unstructured) { u.SetAnnotations(values) }
}

func TestG5IgnoresAnAnnotationWhoseKeyHoldsADot(t *testing.T) {
	for _, stamped := range []struct {
		name          string
		before, after option
	}{
		{"again", annotations(map[string]string{startedAt: "1", "note": "a"}), annotations(map[string]string{startedAt: "2", "note": "a"})},
		{"only by the restart", nothing, annotations(map[string]string{startedAt: "2"})},
	} {
		t.Run(stamped.name, func(t *testing.T) {
			in := restarted(child("w-0", "11", stamped.before), child("w-0", "21", stamped.after))
			in.Target.EqualIgnore = []target.Path{target.MustParsePath(`metadata.annotations["` + startedAt + `"]`)}

			silent(t, invariant.RestartStable, in)
		})
	}
}

func TestG5ComparesTheAnnotationsTheTargetDoesNotIgnore(t *testing.T) {
	in := restarted(
		child("w-0", "11", annotations(map[string]string{startedAt: "1", "note": "a"})),
		child("w-0", "21", annotations(map[string]string{startedAt: "2", "note": "b"})))
	in.Target.EqualIgnore = []target.Path{target.MustParsePath(`metadata.annotations["` + startedAt + `"]`)}

	fired(t, invariant.RestartStable, in)
}

// conditions sets two conditions, each with the heartbeat and the message.
func conditions(heartbeat time.Duration, message string) option {
	return func(u *unstructured.Unstructured) {
		stamp := at(heartbeat).Format(time.RFC3339)
		u.Object["status"] = map[string]any{"conditions": []any{
			map[string]any{"type": "Ready", "status": "True", "lastHeartbeatTime": stamp, "message": message},
			map[string]any{"type": "Synced", "status": "True", "lastHeartbeatTime": stamp, "message": message},
		}}
	}
}

func TestG5IgnoresAFieldOfEveryCondition(t *testing.T) {
	heartbeats := []target.Path{target.MustParsePath("status.conditions[*].lastHeartbeatTime")}

	t.Run("the ignored field", func(t *testing.T) {
		in := restarted(child("w-0", "11", conditions(0, "ok")), child("w-0", "21", conditions(12*time.Second, "ok")))
		in.Target.EqualIgnore = heartbeats

		if notes := silent(t, invariant.RestartStable, in).Notes; len(notes) > 0 {
			t.Errorf("G5 noted %v, want nothing.", notes)
		}
	})
	t.Run("another field", func(t *testing.T) {
		in := restarted(child("w-0", "11", conditions(0, "ok")), child("w-0", "21", conditions(12*time.Second, "retrying")))
		in.Target.EqualIgnore = heartbeats

		fired(t, invariant.RestartStable, in)
	})
}

func TestG5NotesEachIgnoredKeyThatMeetsAList(t *testing.T) {
	in := restarted(child("w-0", "11", conditions(0, "ok")), child("w-0", "21", conditions(12*time.Second, "ok")))
	in.Target.EqualIgnore = []target.Path{
		target.MustParsePath("status.conditions.lastHeartbeatTime"),
		target.MustParsePath("metadata.ownerReferences.uid"),
	}

	result := evaluate(t, invariant.RestartStable, in)

	if len(result.Violations) != 1 {
		t.Errorf("G5 reported %v, want the heartbeat it could not ignore.", statements(result))
	}
	want := []string{
		"G5 could not follow equalIgnore status.conditions.lastHeartbeatTime: status.conditions is a list; write status.conditions[*].lastHeartbeatTime; a path with brackets goes in a block-style list",
		"G5 could not follow equalIgnore metadata.ownerReferences.uid: metadata.ownerReferences is a list; write metadata.ownerReferences[*].uid; a path with brackets goes in a block-style list",
	}
	if !slices.Equal(result.Notes, want) {
		t.Errorf("G5 noted %q, want %q.", result.Notes, want)
	}
}

func TestG5UsesTheTargetsOwnEquality(t *testing.T) {
	in := restarted(child("w-0", "11", data("0")), child("w-0", "21", data("1")))
	in.Target.Equal = func(a, b observe.Snapshot) bool { return a.Name == b.Name }

	silent(t, invariant.RestartStable, in)
}

func TestG5SaysSoWhenASnapshotIsMissing(t *testing.T) {
	for _, missing := range []struct {
		side        string
		checkpoints []invariant.SettleResult
	}{
		{"before", []invariant.SettleResult{invariant.Expired, invariant.Converged}},
		{"after", []invariant.SettleResult{invariant.Converged, invariant.Expired}},
	} {
		t.Run(missing.side, func(t *testing.T) {
			in := restarted(child("w-0", "11", data("0")), child("w-0", "21", data("1")))
			in.Checkpoints[0].Settle = missing.checkpoints[0]
			in.Checkpoints[1].Settle = missing.checkpoints[1]

			result := silent(t, invariant.RestartStable, in)
			if len(result.Notes) != 1 || !strings.Contains(result.Notes[0], missing.side) {
				t.Fatalf("G5 noted %v, want one note saying the snapshot %s the Restart is missing.", result.Notes, missing.side)
			}
		})
	}
}

func TestG5RunsOncePerRestart(t *testing.T) {
	in := restartRun([]*unstructured.Unstructured{child("w-0", "11", data("0"))}, []*unstructured.Unstructured{child("w-0", "21", data("1"))}).
		op(invariant.OpRestart, 16*time.Second).
		record(17*time.Second, child("w-0", "31", data("2"))).
		checkpoint(18*time.Second, invariant.Converged).
		through(20 * time.Second)

	result, err := invariant.RestartStable(in)
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Violations) != 2 {
		t.Fatalf("G5 reported %v, want one violation per Restart.", statements(result))
	}
}

// changedAcross converges at 5s and at 15s on states that differ, with
// whatever ops reconciler-fuzzer runs in between.
func changedAcross(ops func(*run) *run) invariant.Input {
	return ops(newRun().
		record(time.Second, widget("10", spec(1), status(1, 1)), child("w-0", "11", data("0"))).
		checkpoint(5*time.Second, invariant.Converged)).
		record(13*time.Second, widget("20", spec(1), status(1, 1)), child("w-0", "21", data("1"))).
		checkpoint(15*time.Second, invariant.Converged).
		through(20 * time.Second)
}

// changedAround restarts the target at 10s between states converged at 5s and
// 15s that differ, and then runs whatever ops reconciler-fuzzer runs next.
func changedAround(next func(*run) *run) invariant.Input {
	return changedAcross(func(r *run) *run { return next(r.op(invariant.OpRestart, 10*time.Second)) })
}

// updatedAround is changedAround with reconciler-fuzzer's update at the given
// time.
func updatedAround(update time.Duration) invariant.Input {
	if update > 10*time.Second {
		return changedAround(func(r *run) *run { return r.op(invariant.OpUpdate, update) })
	}
	return changedAcross(func(r *run) *run {
		return r.op(invariant.OpUpdate, update).op(invariant.OpRestart, 10*time.Second)
	})
}

// An op stamped at the instant a state converged came after that state.
func TestG5LeavesARestartUnjudgedOnlyForAnUpdateBetweenTheStatesItCompares(t *testing.T) {
	for _, update := range []struct {
		at   time.Duration
		note string
	}{
		{0, ""},
		{5 * time.Second, "for op 1 (restart): op 0 (update) ran"},
		{7 * time.Second, "for op 1 (restart): op 0 (update) ran"},
		{11 * time.Second, "for op 0 (restart): op 1 (update) ran"},
		{15 * time.Second, ""},
		{16 * time.Second, ""},
	} {
		t.Run(update.at.String(), func(t *testing.T) {
			in := updatedAround(update.at)

			if update.note == "" {
				fired(t, invariant.RestartStable, in)
				return
			}
			noted(t, invariant.RestartStable, in, update.note)
		})
	}
}

func TestG5LeavesARestartUnjudgedOnlyForAnOpThatChangesTheRun(t *testing.T) {
	for _, between := range []struct {
		name     string
		apply    func(*run) *run
		confound bool
	}{
		{"create", func(r *run) *run { return r.op(invariant.OpCreate, 11*time.Second) }, true},
		{"update", func(r *run) *run { return r.op(invariant.OpUpdate, 11*time.Second) }, true},
		{"delete", func(r *run) *run { return r.op(invariant.OpDelete, 11*time.Second) }, true},
		{"recreate", func(r *run) *run { return r.op(invariant.OpRecreate, 11*time.Second) }, true},
		{"deleteManaged of w-0", func(r *run) *run { return r.deletedManaged(11*time.Second, "w-0") }, true},
		{"deleteManaged of nothing", func(r *run) *run { return r.op(invariant.OpDeleteManaged, 11*time.Second) }, false},
		{"updateFixture", func(r *run) *run { return r.op(invariant.OpUpdateFixture, 11*time.Second) }, true},
		{"deleteFixture", func(r *run) *run { return r.op(invariant.OpDeleteFixture, 11*time.Second) }, true},
		{"a settle that restored a fixture", func(r *run) *run { return r.restoring(invariant.OpSettle, 11*time.Second) }, true},
		{"fault", func(r *run) *run { return r.op(invariant.OpFault, 11*time.Second) }, false},
		{"settle", func(r *run) *run { return r.op(invariant.OpSettle, 11*time.Second) }, false},
		{"restart", func(r *run) *run { return r.op(invariant.OpRestart, 11*time.Second) }, false},
	} {
		t.Run(between.name, func(t *testing.T) {
			in := changedAround(between.apply)

			if between.confound {
				noted(t, invariant.RestartStable, in, "for op 0 (restart): op 1 ("+string(in.Ops[1].Type)+")")
				return
			}
			result := evaluate(t, invariant.RestartStable, in)
			if len(result.Violations) == 0 || len(result.Notes) > 0 {
				t.Fatalf("G5 reported %v and noted %v, want the restart judged.", statements(result), result.Notes)
			}
		})
	}
}

func TestG5NamesEveryOpThatKeptItFromJudgingARestart(t *testing.T) {
	in := changedAround(func(r *run) *run {
		return r.op(invariant.OpUpdate, 11*time.Second).op(invariant.OpDelete, 12*time.Second).op(invariant.OpRecreate, 12500*time.Millisecond)
	})

	noted(t, invariant.RestartStable, in, "op 1 (update), op 2 (delete) and op 3 (recreate) ran")
}

func TestG5LeavesARestartUnjudgedWhereAFaultReachedBetweenTheStatesItCompares(t *testing.T) {
	for _, fault := range []struct {
		name     string
		from, to time.Duration
		judged   bool
	}{
		{"after the state before", 6 * time.Second, 7 * time.Second, false},
		{"before the state after", 11 * time.Second, 12 * time.Second, false},
		{"until the state before", 2 * time.Second, 5 * time.Second, true},
		{"after the state after", 16 * time.Second, 18 * time.Second, true},
	} {
		t.Run(fault.name, func(t *testing.T) {
			in := changedAround(func(r *run) *run { return r.fault(fault.from, fault.to) })

			if fault.judged {
				fired(t, invariant.RestartStable, in)
				return
			}
			noted(t, invariant.RestartStable, in, "for op 0 (restart): a fault was active")
		})
	}
}

// An op on one CR leaves the other CRs and what they own judged. An object
// that names no CR may be the changed CR's.
func TestG5JudgesWhatTheOpsBetweenItsStatesLeftAlone(t *testing.T) {
	for _, c := range []struct {
		name          string
		before, after *unstructured.Unstructured
		fires         bool
	}{
		{"w's child", child("w-0", "11", data("0")), child("w-0", "21", data("1")), true},
		{"w2's child", secondChild("w2-0", "12", data("0")), secondChild("w2-0", "22", data("1")), false},
		{"an object that names no CR", child("kept", "13", data("0"), orphaned), child("kept", "23", data("1"), orphaned), false},
	} {
		t.Run(c.name, func(t *testing.T) {
			in := newRun().withSecondWidget().
				record(time.Second, widget("10", spec(1), status(1, 1)), secondWidget("30", spec(1), status(1, 1)), c.before).
				checkpoint(5*time.Second, invariant.Converged).
				op(invariant.OpRestart, 10*time.Second).
				opOn(invariant.OpUpdate, 11*time.Second, secondName).
				record(12*time.Second, secondWidget("31", spec(2), generation(2), status(2, 2)), c.after).
				checkpoint(15*time.Second, invariant.Converged).
				through(20 * time.Second)

			result := evaluate(t, invariant.RestartStable, in)

			if fired := len(result.Violations) > 0; fired != c.fires {
				t.Errorf("G5 reported %v, want a violation: %t.", statements(result), c.fires)
			}
			if len(result.Notes) != 1 || !strings.Contains(result.Notes[0], "on what op 1 (update) may have changed") {
				t.Errorf("G5 noted %v, want one note that it leaves out what the update to w2 may have changed.", result.Notes)
			}
		})
	}
}

func TestG5JudgesACRNoOpBetweenItsStatesActedOn(t *testing.T) {
	in := newRun().withSecondWidget().
		record(time.Second, widget("10", spec(1), status(1, 1)), secondWidget("30", spec(1), status(1, 1))).
		checkpoint(5*time.Second, invariant.Converged).
		op(invariant.OpRestart, 10*time.Second).
		opOn(invariant.OpUpdate, 11*time.Second, secondName).
		record(12*time.Second, widget("11", spec(1), status(1, 1), labelled("restarted")),
			secondWidget("31", spec(2), generation(2), status(2, 2))).
		checkpoint(15*time.Second, invariant.Converged).
		through(20 * time.Second)

	violation := fired(t, invariant.RestartStable, in)

	if !strings.Contains(violation.Statement, "Widget w changed") {
		t.Errorf("The statement is %q, want it to name the Widget w.", violation.Statement)
	}
}

func TestG5LeavesOutWhatEachOpBetweenItsStatesReached(t *testing.T) {
	in := newRun().withSecondWidget().
		record(time.Second, widget("10", spec(1), status(1, 1)), secondWidget("30", spec(1), status(1, 1)),
			child("w-0", "11", data("0")), secondChild("w2-0", "31", data("0")), child("spared", "40", ownedByGhost)).
		checkpoint(5*time.Second, invariant.Converged).
		op(invariant.OpRestart, 10*time.Second).
		op(invariant.OpUpdate, 11*time.Second).
		opOn(invariant.OpUpdate, 11500*time.Millisecond, secondName).
		record(12*time.Second, widget("12", spec(2), generation(2), status(2, 2)), secondWidget("32", spec(2), generation(2), status(2, 2)),
			child("w-0", "13", data("1")), secondChild("w2-0", "33", data("1"))).
		checkpoint(15*time.Second, invariant.Converged).
		through(20 * time.Second)

	noted(t, invariant.RestartStable, in, "on what op 1 (update) and op 2 (update) may have changed")
}

// A recreate reaches the CR it deleted and the one it created.
func TestG5LeavesOutBothCRsARecreateWrote(t *testing.T) {
	in := newRun().withSecondWidget().
		record(time.Second, widget("10", spec(1), status(1, 1)), secondWidget("30", spec(1), status(1, 1)),
			child("w-0", "11", data("0")), secondChild("w2-0", "31", data("0"))).
		checkpoint(5*time.Second, invariant.Converged).
		op(invariant.OpRestart, 10*time.Second).
		op(invariant.OpRecreate, 11*time.Second).
		remove(11*time.Second, widget("12", spec(1), status(1, 1)), child("w-0", "13", data("0"))).
		record(12*time.Second, widget("20", spec(1), status(1, 1), uid("uid-w-again")),
			child("w-0", "21", data("0"), uid("uid-w-0-again"), ownedByRecreated)).
		checkpoint(15*time.Second, invariant.Converged).
		through(20 * time.Second)

	noted(t, invariant.RestartStable, in, "on what op 1 (recreate) may have changed")
}

// Either state may hold what the ops between them left alone.
func TestG5JudgesWhatEitherStateHoldsThatTheOpsLeftAlone(t *testing.T) {
	for _, c := range []struct {
		name          string
		before, after []*unstructured.Unstructured
	}{
		{"before", []*unstructured.Unstructured{child("spared", "40", ownedByGhost)}, nil},
		{"after", nil, []*unstructured.Unstructured{child("spared", "40", ownedByGhost)}},
	} {
		t.Run(c.name, func(t *testing.T) {
			in := newRun().
				record(time.Second, append([]*unstructured.Unstructured{widget("10", spec(1), status(1, 1))}, c.before...)...).
				checkpoint(5*time.Second, invariant.Converged).
				op(invariant.OpRestart, 10*time.Second).
				op(invariant.OpUpdate, 11*time.Second).
				remove(11*time.Second, c.before...).
				record(12*time.Second, append([]*unstructured.Unstructured{widget("11", spec(2), generation(2), status(2, 2))}, c.after...)...).
				checkpoint(15*time.Second, invariant.Converged).
				through(20 * time.Second)

			violation := fired(t, invariant.RestartStable, in)

			if !strings.Contains(violation.Statement, "spared") {
				t.Errorf("The statement is %q, want it to name the object only one state holds.", violation.Statement)
			}
		})
	}
}

// An object that names no CR may be any CR's, so deleting one may change any.
func TestG5LeavesARestartUnjudgedWhereReconcilerFuzzerTookAnObjectThatNamesNoCR(t *testing.T) {
	in := changedAround(func(r *run) *run {
		return r.record(time.Second, child("kept", "12", orphaned)).deletedManaged(11*time.Second, "kept")
	})

	noted(t, invariant.RestartStable, in, "for op 0 (restart): op 1 (deleteManaged) ran")
}

func TestG5ComparesTheMetadataSection6DoesNotIgnore(t *testing.T) {
	in := restarted(child("w-0", "11", data("0")), child("w-0", "21", data("0"), deleting(12*time.Second)))

	violation := fired(t, invariant.RestartStable, in)

	if !strings.Contains(violation.Statement, "w-0") {
		t.Errorf("The statement is %q, want it to name the ConfigMap the restart left terminating.", violation.Statement)
	}
}
