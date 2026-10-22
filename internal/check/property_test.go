package check_test

import (
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/rosenhouse-bot-sandbox/reconciler-fuzzer/internal/check"
	"github.com/rosenhouse-bot-sandbox/reconciler-fuzzer/internal/config"
	"github.com/rosenhouse-bot-sandbox/reconciler-fuzzer/internal/trace"
	tt "github.com/rosenhouse-bot-sandbox/reconciler-fuzzer/internal/trace/tracetest"
)

// freshKey is the cert-manager property of #114: a Certificate's Secret
// holds a key first written while that Certificate owned the Secret.
const freshKey = `crs.all(c, world.kube.secrets.all(s, s.metadata.name != c.spec.secretName || !has(s.data) || firstWrittenUnder(s, ["data", "tls.key"]) == c.metadata.uid))`

func certCR(ready bool) map[string]any {
	return map[string]any{"metadata": map[string]any{"name": "a"}, "spec": map[string]any{"secretName": "a-tls"}, "status": map[string]any{"ready": ready}}
}

func keySecret(key string) map[string]any {
	return named("a-tls", map[string]any{"data": map[string]any{"tls.key": key}})
}

// secretWriteBack is #63's trace. Step 0 issues certificate a, whose Secret
// a-tls gets key K. With owns, rule 3 makes a its owner, and otherwise an
// ownerReference does. Step 1 deletes a, the gc collects a-tls, the
// harness creates a again, and the controller writes a-tls with key and an
// ownerReference to the new a.
func secretWriteBack(key string, owns bool) *tt.Script {
	r := tt.NewScript(cert, 2, 1*s)
	r.BodyUIDs = true
	r.Spec.Properties = []trace.Property{{Name: "fresh-key", Expr: freshKey, When: "step"}}
	r.Begin()
	r.Create(trace.Harness, cert, certCR(false))
	r.Act("create", "a", &cert)
	first := keySecret("K")
	if owns {
		r.Spec.Owns = []trace.Owns{{Kind: trace.Kind{Version: "v1", Kind: "Secret", Resource: "secrets"}, Name: "self.spec.secretName"}}
	} else {
		first = owned(first, r, cert)
	}
	r.Create(ctl, tls, first)
	r.Update(ctl, cert, certCR(true)).EndFaults().Settle()

	r.Begin().DeleteCR("a", cert)
	collect(r, tls)
	r.Create(trace.Harness, cert, certCR(false))
	r.Act("create", "a", &cert)
	r.Create(ctl, tls, owned(keySecret(key), r, cert))
	r.Update(ctl, cert, certCR(true)).EndFaults().Settle()
	return finish(r, map[string]trace.Ref{"a": cert}, tls)
}

// summaries lists each finding as "<step> <summary>".
func summaries(findings []check.Finding) []string {
	var got []string
	for _, f := range findings {
		if f.Check != "Property" || f.Kind != "" {
			got = append(got, fmt.Sprintf("unexpected %+v", f))
			continue
		}
		got = append(got, fmt.Sprintf("%d %s", f.Step, f.Summary))
	}
	return got
}

func requireSummaries(t *testing.T, findings []check.Finding, want ...string) {
	t.Helper()
	if got := summaries(findings); !slices.Equal(got, want) {
		t.Errorf("findings = %q, want %q\n%+v", got, want, findings)
	}
}

func TestFreshKeyFailsAWrittenBackKey(t *testing.T) {
	for _, owns := range []bool{false, true} {
		requireSummaries(t, judgeWith(t, secretWriteBack("K", owns), check.Property), "1 property fresh-key does not hold")
	}
}

func TestFreshKeyPassesANewKey(t *testing.T) {
	for _, owns := range []bool{false, true} {
		requireSummaries(t, judgeWith(t, secretWriteBack("K2", owns), check.Property))
	}
}

func TestJudgeRunsProperty(t *testing.T) {
	ix := secretWriteBack("K", false).Index()
	spec, err := check.SpecFor(nil, ix)
	if err != nil {
		t.Fatal(err)
	}
	report := check.Judge(ix, spec)
	requireSummaries(t, report.Findings, "1 property fresh-key does not hold")
	if md := check.Markdown(ix, report); !strings.Contains(md, "## Property: property fresh-key does not hold") {
		t.Errorf("report.md lacks the finding:\n%s", md)
	}
}

// transient settles step 0 of a toy run in which the controller creates a
// second configmap and deletes it before the step ends.
func transient(when string) *tt.Script {
	r := newRun(0)
	r.Spec.Properties = []trace.Property{{Name: "one-configmap", Expr: "size(world.kube.configmaps) <= 1", When: when}}
	r.Begin()
	withChild(r, "a", true)
	extra := tt.Ref("configmaps", "ns", "a-extra")
	r.Create(ctl, extra, tt.Object("a-extra"))
	r.Delete(ctl, extra)
	return r.Tick(s).EndFaults().Settle()
}

func TestAnAlwaysPropertyCatchesATransientViolation(t *testing.T) {
	requireSummaries(t, judgeWith(t, transient("always"), check.Property), "0 property one-configmap does not hold")
	requireSummaries(t, judgeWith(t, transient("step"), check.Property))
}

func TestAnAlwaysPropertyHoldsAtEachVersionTime(t *testing.T) {
	r := transient("always")
	r.Spec.Properties[0].Expr = "size(world.kube.configmaps) <= 2"
	requireSummaries(t, judgeWith(t, r, check.Property))
}

func TestPropertyVariables(t *testing.T) {
	r := newRun(1)
	r.Spec.Properties = []trace.Property{
		{Name: "one-cr", Expr: `size(crs) == 1 && crs[0].metadata.name == "a"`, When: "step"},
		{Name: "world", Expr: `world.kube.configmaps.all(m, m.metadata.name == "a-0") && size(world.kube["widgets.toy"]) == size(crs) &&
			size(world.cloud.buckets) == 1 && world.cloud.buckets[0].name == "b1" && size(world.empty) == 0`, When: "step"},
	}
	r.Spec.Systems = []trace.System{{Name: "empty", API: "crud"}}
	r.Begin()
	r.Create(trace.Harness, trace.Ref{System: "cloud", Resource: "buckets", Name: "b1"}, map[string]any{"name": "b1"})
	withChild(r, "a", true)
	r.EndFaults().Settle()
	finish(r, map[string]trace.Ref{"a": widgetA}, tt.Ref("configmaps", "ns", "a-0"))
	requireSummaries(t, judgeWith(t, r, check.Property), "1 property one-cr does not hold", "2 property one-cr does not hold")
}

func TestAPropertyThatCannotBeEvaluated(t *testing.T) {
	r := newRun(0)
	r.Spec.Properties = []trace.Property{{Name: "typo", Expr: "size(world.kube.secrets) == 0", When: "always"}}
	r.Begin()
	withChild(r, "a", true)
	r.EndFaults().Settle()
	findings := judgeWith(t, r, check.Property)
	requireSummaries(t, findings, "0 property typo could not be evaluated")
	if len(findings) == 1 && !strings.Contains(strings.Join(findings[0].Details, "\n"), "secrets") {
		t.Errorf("details = %q, want the error", findings[0].Details)
	}
}

func TestFirstWrittenUnderNeedsAKnownUID(t *testing.T) {
	r := newRun(0)
	r.Spec.Properties = []trace.Property{{Name: "uid", Expr: `crs.all(c, firstWrittenUnder(c, ["spec"]) == "")`, When: "step"}}
	r.Begin()
	withChild(r, "a", true)
	r.EndFaults().Settle()
	findings := judgeWith(t, r, check.Property)
	requireSummaries(t, findings, "0 property uid could not be evaluated")
	if len(findings) == 1 && !strings.Contains(strings.Join(findings[0].Details, "\n"), "metadata.uid") {
		t.Errorf("details = %q, want the missing uid", findings[0].Details)
	}
}

func TestFirstWrittenUnderAMissingValueIsEmpty(t *testing.T) {
	r := secretWriteBack("K", false)
	r.Spec.Properties[0].Expr = `crs.all(c, world.kube.secrets.all(s, firstWrittenUnder(s, ["data", "missing"]) == ""))`
	requireSummaries(t, judgeWith(t, r, check.Property))
}

func TestFirstWrittenUnderARule4OwnerIsEmpty(t *testing.T) {
	r := tt.NewScript(cert, 0, 1*s)
	r.BodyUIDs = true
	r.Spec.Properties = []trace.Property{{Name: "unowned", Expr: `world.kube.secrets.all(s, firstWrittenUnder(s, ["data"]) == "")`, When: "step"}}
	r.Begin()
	r.Create(trace.Harness, cert, certCR(false))
	r.Act("create", "a", &cert)
	r.Create(ctl, tls, keySecret("K"))
	r.Update(ctl, cert, certCR(true)).EndFaults().Settle()
	requireSummaries(t, judgeWith(t, r, check.Property))
}

func TestSpecForTakesPropertiesAndPeriodic(t *testing.T) {
	c, err := config.Load("../../targets/toy-widget/config.yaml")
	if err != nil {
		t.Fatal(err)
	}
	c.Properties = []config.Property{{Name: "c", Expr: "size(crs) == 0", When: config.Always}}
	c.Primary.Periodic = &config.Periodic{Every: s, Paths: []string{"status.at"}}
	run := tt.Run("toy.reconciler-fuzzer", "widgets")
	run.Run.Ready, run.Run.Stable, run.Run.Reads, run.Run.Writes = "true", s, 1, 1
	run.Run.Properties = []trace.Property{{Name: "r", Expr: "true", When: "step"}}
	run.Run.Periodic = &trace.Periodic{Every: 2 * s, Paths: []string{"status.r"}}
	var b tt.Builder
	ix := b.Add(run).Index()
	fromConfig, err := check.SpecFor(c, ix)
	if err != nil {
		t.Fatal(err)
	}
	if len(fromConfig.Properties) != 1 || fromConfig.Properties[0].Name != "c" || !fromConfig.Properties[0].Always ||
		fromConfig.Periodic == nil || fromConfig.Periodic.Every != s || !slices.Equal(fromConfig.Periodic.Paths, []string{"status.at"}) {
		t.Errorf("spec from the config = %+v, %+v", fromConfig.Properties, fromConfig.Periodic)
	}
	fromRun, err := check.SpecFor(nil, ix)
	if err != nil {
		t.Fatal(err)
	}
	if len(fromRun.Properties) != 1 || fromRun.Properties[0].Name != "r" || fromRun.Properties[0].Always ||
		fromRun.Periodic == nil || fromRun.Periodic.Every != 2*s {
		t.Errorf("spec from the run = %+v, %+v", fromRun.Properties, fromRun.Periodic)
	}
	run.Run.Properties[0].Expr = "crs.("
	b = tt.Builder{}
	if _, err := check.SpecFor(nil, b.Add(run).Index()); err == nil {
		t.Error("SpecFor accepts a property that does not compile")
	}
}

// rePoint is #63's other path: the controller writes the Secret of the
// deleted a, which the gc has not collected, with an ownerReference to the
// new a.
func rePoint() *tt.Script {
	r := tt.NewScript(cert, 2, 1*s)
	r.BodyUIDs = true
	r.Spec.Properties = []trace.Property{{Name: "fresh-key", Expr: freshKey, When: "step"}}
	r.Begin()
	r.Create(trace.Harness, cert, certCR(false))
	r.Act("create", "a", &cert)
	r.Create(ctl, tls, owned(keySecret("K"), r, cert))
	r.Update(ctl, cert, certCR(true)).EndFaults().Settle()
	r.Begin().DeleteCR("a", cert)
	r.Create(trace.Harness, cert, certCR(false))
	r.Act("create", "a", &cert)
	r.Update(ctl, tls, owned(keySecret("K"), r, cert))
	r.Update(ctl, cert, certCR(true)).EndFaults().Settle()
	return finish(r, map[string]trace.Ref{"a": cert}, tls)
}

func TestAPropertyFindingSaysWhereAFirstWrittenValueCameFrom(t *testing.T) {
	for name, c := range map[string]struct {
		r    *tt.Script
		want string
	}{
		"a write-back": {secretWriteBack("K", false), "secrets ns/a-tls: its data.tls.key was first written at"},
		"a re-point":   {rePoint(), ", in this instance, under CR"},
	} {
		findings := judgeWith(t, c.r, check.Property)
		if len(findings) != 1 || len(findings[0].Details) != 2 || !strings.Contains(findings[0].Details[1], c.want) {
			t.Errorf("%s: findings = %+v, want a detail containing %q", name, findings, c.want)
		}
	}
	if f := judgeWith(t, secretWriteBack("K", false), check.Property); len(f) == 1 && !strings.Contains(f[0].Details[1], ", in instance ") {
		t.Errorf("a write-back's detail %q names no earlier instance", f[0].Details[1])
	}
}
