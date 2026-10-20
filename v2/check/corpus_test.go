package check_test

import (
	"bytes"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/rosenhouse-bot-sandbox/reconciler-fuzzer/v2/check"
	"github.com/rosenhouse-bot-sandbox/reconciler-fuzzer/v2/trace"
	tt "github.com/rosenhouse-bot-sandbox/reconciler-fuzzer/v2/trace/tracetest"
)

const corpusDir = "testdata/corpus"

// TestCorpus judges every stored trace and requires exactly the findings
// its .want file lists.
func TestCorpus(t *testing.T) {
	paths, err := filepath.Glob(filepath.Join(corpusDir, "*.jsonl"))
	if err != nil || len(paths) == 0 {
		t.Fatalf("no corpus: %v", err)
	}
	for _, path := range paths {
		name := strings.TrimSuffix(filepath.Base(path), ".jsonl")
		t.Run(name, func(t *testing.T) {
			want, skip := readWant(t, strings.TrimSuffix(path, ".jsonl")+".want")
			events, err := trace.Load(path)
			if err != nil {
				t.Fatal(err)
			}
			ix := trace.NewIndex(events)
			spec, err := check.SpecFor(nil, ix)
			if err != nil {
				t.Fatal(err)
			}
			report := check.Judge(ix, spec)
			var got []string
			for _, f := range report.Findings {
				got = append(got, fmt.Sprintf("%s %d", f.Check, f.Step))
			}
			slices.Sort(got)
			if !slices.Equal(got, want) {
				if skip != "" {
					t.Skipf("%s; got %q, want %q", skip, got, want)
				}
				t.Fatalf("findings = %q, want %q\n%s", got, want, check.Markdown(ix, report))
			}
			if skip != "" {
				t.Fatalf("passes but is marked skipped: %s", skip)
			}
		})
	}
}

// readWant returns the sorted "<check> <step>" lines of a .want file and
// its skip reason.
func readWant(t *testing.T, path string) (want []string, skip string) {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(string(data)), "\n")
	if !strings.HasPrefix(lines[0], "# D") && !strings.HasPrefix(lines[0], "# M1 ") || !strings.Contains(lines[0], ": ") {
		t.Fatalf("%s: first line %q is not \"# D<n>: <sentence>\" or \"# M1 <source>: <sentence>\"", path, lines[0])
	}
	for _, line := range lines[1:] {
		if reason, found := strings.CutPrefix(line, "# skip: "); found {
			skip = reason
			continue
		}
		want = append(want, line)
	}
	slices.Sort(want)
	return want, skip
}

// TestCorpusMatchesItsGenerator requires each stored trace to be what its
// builder writes, and RF_WRITE_TESTDATA rewrites them.
func TestCorpusMatchesItsGenerator(t *testing.T) {
	for name, build := range corpus {
		path := filepath.Join(corpusDir, name+".jsonl")
		data := build().JSONL()
		if os.Getenv("RF_WRITE_TESTDATA") != "" {
			if err := os.WriteFile(path, data, 0o644); err != nil {
				t.Fatal(err)
			}
			continue
		}
		stored, err := os.ReadFile(path)
		if err != nil || !bytes.Equal(stored, data) {
			t.Errorf("%s differs from its builder; rerun with RF_WRITE_TESTDATA=1: %v", path, err)
		}
	}
}

var (
	cert    = tt.Ref("cert-manager.io/certificates", "ns", "a")
	request = tt.Ref("cert-manager.io/certificaterequests", "ns", "a-1")
	tls     = tt.Ref("secrets", "ns", "a-tls")
	fixture = tt.Ref("configmaps", "ns", "widget-config")
)

func owned(body map[string]any, r *tt.Script, cr trace.Ref) map[string]any {
	return tt.OwnedBy(body, r.UID(cr), true)
}

func named(name string, fields map[string]any) map[string]any {
	body := tt.Object(name)
	for k, v := range fields {
		body[k] = v
	}
	return body
}

// finish appends the runner's delete-all step, in which the gc model
// collects children, and its restart step.
func finish(r *tt.Script, crs map[string]trace.Ref, children ...trace.Ref) *tt.Script {
	r.Begin()
	for _, slot := range slices.Sorted(maps.Keys(crs)) {
		r.DeleteCR(slot, crs[slot])
	}
	for _, child := range children {
		collect(r, child)
	}
	r.EndFaults().Settle()
	return r.Begin().Restart().EndFaults().Settle()
}

// toy settles step 0, which creates slot a with its owned child a-0.
func toy(planSteps int) *tt.Script {
	r := newRun(planSteps)
	r.Begin()
	withChild(r, "a", true)
	return r.EndFaults().Settle()
}

// certificate settles step 0, which creates certificate a, its request a-1
// and its Secret a-tls.
func certificate(planSteps int, ownedSecret bool) *tt.Script {
	r := tt.NewScript(cert, planSteps, 1*s)
	r.Begin()
	r.CreateCR("a", cert)
	r.Create(ctl, request, owned(tt.Object("a-1"), r, cert))
	secret := named("a-tls", map[string]any{"data": "key-1"})
	if ownedSecret {
		secret = owned(secret, r, cert)
	}
	r.Create(ctl, tls, secret)
	return r.Ready(cert).EndFaults().Settle()
}

var corpus = map[string]func() *tt.Script{
	"d14-duplicate-after-drop-response": func() *tt.Script {
		r := newRun(1)
		r.Begin()
		uid := r.CreateCR("a", widgetA)
		first := tt.Ref("configmaps", "ns", "a-x1")
		lost := tt.Create(ctl, first, "u-lost", "lost")
		lost.Call.Injected = trace.DropResponse
		r.Add(lost, tt.Version(first, "u-lost", "lost", tt.OwnedBy(tt.Generated("a-", "a-x1"), uid, true)))
		second := tt.Ref("configmaps", "ns", "a-x2")
		r.Create(ctl, second, tt.OwnedBy(tt.Generated("a-", "a-x2"), uid, true))
		r.Ready(widgetA).EndFaults().Settle()
		r.Begin().DeleteCR("a", widgetA).EndFaults()
		r.At(r.Now, trace.Event{Work: &trace.Work{System: "gc", Pending: true}})
		r.Add(tt.Call(trace.GC, "delete", first, 200, "gc1"), tt.Deleted(first, "u-lost", "gc1", nil))
		collect(r, second).Settle()
		return r.Begin().Restart().EndFaults().Settle()
	},
	"d14-retry-after-drop-response": func() *tt.Script {
		r := newRun(1)
		r.Begin()
		uid := r.CreateCR("a", widgetA)
		lost := tt.Create(ctl, childA, "u-lost", "lost")
		lost.Call.Injected = trace.DropResponse
		r.Add(lost, tt.Version(childA, "u-lost", "lost", tt.OwnedBy(tt.Object("a-0"), uid, true)))
		r.Call(ctl, "create", tt.Ref("configmaps", "ns", ""), 409).Call(ctl, "get", childA, 200)
		r.Ready(widgetA).EndFaults().Settle()
		r.Begin().DeleteCR("a", widgetA).EndFaults()
		r.At(r.Now, trace.Event{Work: &trace.Work{System: "gc", Pending: true}})
		r.Add(tt.Call(trace.GC, "delete", childA, 200, "gc1"), tt.Deleted(childA, "u-lost", "gc1", nil))
		r.At(r.Now, trace.Event{Work: &trace.Work{System: "gc", Pending: false}})
		r.Settle()
		return r.Begin().Restart().EndFaults().Settle()
	},
	"d17-orphaned-secret": func() *tt.Script {
		r := certificate(1, false)
		r.Begin().DeleteCR("a", cert).EndFaults()
		collect(r, request).Settle()
		return r.Begin().Restart().EndFaults().Settle()
	},
	"d17-retained-secret": func() *tt.Script {
		r := certificate(1, false)
		r.Spec.Retain = `self.metadata.name == "a"`
		return finish(r, map[string]trace.Ref{"a": cert}, request)
	},
	"d18-singleton-webhook-secret": func() *tt.Script {
		r := newRun(1)
		r.Create(ctl, tt.Ref("secrets", "ns", "webhook-cert"), tt.Object("webhook-cert"))
		r.Begin()
		withChild(r, "a", true)
		r.EndFaults().Settle()
		r.Begin().DeleteCR("a", widgetA).EndFaults()
		collect(r, childA).Settle()
		r.Begin().Restart().Call(ctl, "get", tt.Ref("secrets", "ns", "webhook-cert"), 200)
		return r.EndFaults().Settle()
	},
	"d18-singleton-per-restart": func() *tt.Script {
		r := newRun(1)
		r.Create(ctl, tt.Ref("secrets", "ns", "webhook-x1"), tt.Generated("webhook-", "webhook-x1"))
		r.Begin()
		withChild(r, "a", true)
		r.EndFaults().Settle()
		r.Begin().DeleteCR("a", widgetA).EndFaults()
		collect(r, childA).Settle()
		r.Begin().Restart()
		r.Create(ctl, tt.Ref("secrets", "ns", "webhook-x2"), tt.Generated("webhook-", "webhook-x2"))
		return r.EndFaults().Settle()
	},
	"d20-noop-status-write": func() *tt.Script {
		r := newRun(1)
		r.Begin()
		withChild(r, "a", true)
		r.EndFaults()
		for range 5 {
			r.Tick(600*ms).NoOp(ctl, widgetA)
		}
		r.Settle()
		return finish(r, map[string]trace.Ref{"a": widgetA}, childA)
	},
	"d27-stale-cache-conflict": func() *tt.Script {
		r := toy(2)
		r.Begin().Update(trace.Harness, widgetA, named("a", map[string]any{"spec": map[string]any{"count": 2}, "status": map[string]any{"ready": false}}))
		r.Act("update", "a", &widgetA).EndFaults()
		r.Call(ctl, "update", widgetA, 409).Call(ctl, "get", widgetA, 200)
		r.Update(ctl, widgetA, named("a", map[string]any{"spec": map[string]any{"count": 2}, "status": map[string]any{"ready": true}}))
		r.Settle()
		return finish(r, map[string]trace.Ref{"a": widgetA}, childA)
	},
	"d30-lease-and-event-traffic": func() *tt.Script {
		r := newRun(1)
		r.Create(ctl, lease, tt.Object("l"))
		renew := func(n int) {
			for i := range n {
				r.Tick(300*ms).Update(ctl, lease, named("l", map[string]any{"spec": map[string]any{"renewTime": i}}))
			}
		}
		r.Begin()
		withChild(r, "a", true)
		r.Create(ctl, tt.Ref("events", "ns", "a.1"), tt.Object("a.1"))
		r.EndFaults()
		renew(8)
		r.Settle()
		r.Begin().DeleteCR("a", widgetA).EndFaults()
		collect(r, childA)
		renew(8)
		r.Settle()
		r.Begin().Restart().Call(ctl, "get", lease, 200).EndFaults()
		renew(8)
		return r.Settle()
	},
	"d40-refresh-writes": func() *tt.Script {
		r := newRun(1)
		r.Begin()
		withChild(r, "a", true)
		r.EndFaults()
		for i := range 4 {
			r.Tick(1500*ms).Update(ctl, widgetA, named("a", map[string]any{"status": map[string]any{"ready": true, "refreshTime": i}}))
		}
		r.At(r.Now, tt.Phase(0, trace.Settled))
		return r.Tick(1 * s).Add(tt.Phase(0, trace.Confirmed))
	},
	"d42-restart-beside-update": func() *tt.Script {
		r := toy(2)
		r.Begin().Update(trace.Harness, widgetA, named("a", map[string]any{"spec": map[string]any{"label": "blue"}, "status": map[string]any{"ready": false}}))
		r.Act("update", "a", &widgetA).Restart()
		r.Update(ctl, childA, owned(named("a-0", map[string]any{"data": map[string]any{"label": "blue"}}), r, widgetA))
		r.Update(ctl, widgetA, named("a", map[string]any{"spec": map[string]any{"label": "blue"}, "status": map[string]any{"ready": true}}))
		r.EndFaults().Settle()
		return finish(r, map[string]trace.Ref{"a": widgetA}, childA)
	},
	"d42-replaced-after-restart": func() *tt.Script {
		r := toy(2)
		r.Begin().Restart()
		r.Delete(ctl, childA)
		r.Create(ctl, childA, owned(tt.Object("a-0"), r, widgetA))
		r.EndFaults().Settle()
		return finish(r, map[string]trace.Ref{"a": widgetA}, childA)
	},
	"d53-restart-mid-reconcile": func() *tt.Script {
		r := newRun(1)
		r.Begin()
		uid := r.CreateCR("a", widgetA)
		r.Create(ctl, childA, tt.OwnedBy(tt.Object("a-0"), uid, true))
		r.Restart().Call(ctl, "get", childA, 200)
		r.Create(ctl, tt.Ref("configmaps", "ns", "a-1"), tt.OwnedBy(tt.Object("a-1"), uid, true))
		r.Ready(widgetA).EndFaults().Settle()
		return finish(r, map[string]trace.Ref{"a": widgetA}, childA, tt.Ref("configmaps", "ns", "a-1"))
	},
	"d53-duplicate-after-restart": func() *tt.Script {
		r := newRun(1)
		r.Begin()
		uid := r.CreateCR("a", widgetA)
		first, second := tt.Ref("configmaps", "ns", "a-x1"), tt.Ref("configmaps", "ns", "a-x2")
		r.Create(ctl, first, tt.OwnedBy(tt.Generated("a-", "a-x1"), uid, true))
		r.Restart()
		r.Create(ctl, second, tt.OwnedBy(tt.Generated("a-", "a-x2"), uid, true))
		r.Ready(widgetA).EndFaults().Settle()
		return finish(r, map[string]trace.Ref{"a": widgetA}, first, second)
	},
	"d53-lease-fault-exit": func() *tt.Script {
		r := toy(2)
		r.Begin().Add(trace.Event{Fault: &trace.Fault{Step: 1, What: "error(500) on verb=update resource=leases"}})
		failed := tt.Call(ctl, "update", lease, 500, "")
		failed.Call.Injected = "error(500)"
		r.Add(failed).Tick(500 * ms).Add(tt.Exit("exit status 1", false, "leader election lost"))
		r.Add(trace.Event{Fault: &trace.Fault{Step: 1, Ended: true, What: "error(500) on verb=update resource=leases"}})
		r.EndFaults().Add(tt.Start(9)).Call(ctl, "list", widgetA, 200)
		r.Settle()
		return finish(r, map[string]trace.Ref{"a": widgetA}, childA)
	},
	"d56-cluster-objects": func() *tt.Script {
		r := newRun(1)
		r.At(0, tt.Version(tt.Ref("configmaps", "ns", "kube-root-ca.crt"), "u-root", "r1", tt.Object("kube-root-ca.crt")),
			tt.Version(tt.Ref("serviceaccounts", "ns", "default"), "u-sa", "r2", tt.Object("default")))
		r.Create(trace.Harness, fixture, named("widget-config", map[string]any{"data": map[string]any{"label": "red"}}))
		r.Begin()
		withChild(r, "a", true)
		r.EndFaults().Settle()
		return finish(r, map[string]trace.Ref{"a": widgetA}, childA)
	},
	"d60-damage-recreated": func() *tt.Script {
		r := toy(2)
		r.Begin().Delete(trace.Harness, childA).Act("damage", "a", &childA).Call(ctl, "get", childA, 404)
		r.Create(ctl, childA, owned(tt.Object("a-0"), r, widgetA))
		r.Tick(500 * ms).EndFaults().Settle()
		return finish(r, map[string]trace.Ref{"a": widgetA}, childA)
	},
	"d60-damage-recreated-after-end-faults": func() *tt.Script {
		r := toy(2)
		damageA(r).Call(ctl, "get", childA, 404)
		r.Create(ctl, childA, owned(tt.Object("a-0"), r, widgetA))
		r.Settle()
		return finish(r, map[string]trace.Ref{"a": widgetA}, childA)
	},
	"d60-damage-not-restored": func() *tt.Script {
		r := toy(2)
		damageA(r).Call(ctl, "get", childA, 404).Settle()
		return finish(r, map[string]trace.Ref{"a": widgetA})
	},
	"d60-reissue-after-damage": func() *tt.Script {
		r := certificate(2, true)
		r.Begin().Delete(trace.Harness, tls).Act("damage", "a", &tls)
		next := tt.Ref("cert-manager.io/certificaterequests", "ns", "a-2")
		r.Call(ctl, "get", tls, 404)
		r.Create(ctl, next, owned(tt.Object("a-2"), r, cert))
		r.Create(ctl, tls, owned(named("a-tls", map[string]any{"data": "key-2"}), r, cert))
		r.Delete(ctl, request)
		r.Update(ctl, cert, named("a", map[string]any{"status": map[string]any{"ready": true, "revision": 2}}))
		r.Tick(500 * ms).EndFaults().Settle()
		return finish(r, map[string]trace.Ref{"a": cert}, next, tls)
	},
	"d60-request-not-recreated": func() *tt.Script {
		r := certificate(2, true)
		r.Begin().Delete(trace.Harness, request).Act("damage", "a", &request).EndFaults()
		r.Call(ctl, "get", request, 404).Settle()
		return finish(r, map[string]trace.Ref{"a": cert}, tls)
	},
	"d62-cleanup-after-delete": func() *tt.Script {
		r := newRun(1)
		r.Begin()
		withChild(r, "a", true)
		r.EndFaults().Settle()
		finalizing := named("a", map[string]any{"metadata": map[string]any{"name": "a", "finalizers": []any{"toy"},
			"deletionTimestamp": "2026-10-20T00:00:00Z"}, "status": map[string]any{"ready": true}})
		r.Begin().Update(trace.Harness, widgetA, finalizing).Act("delete", "a", &widgetA).EndFaults().Tick(500 * ms)
		r.Delete(ctl, childA).Delete(ctl, widgetA).Settle()
		return r.Begin().Restart().EndFaults().Settle()
	},
	"d62-recreate-after-delete": func() *tt.Script {
		r := toy(2)
		r.Begin().DeleteCR("a", widgetA)
		collect(r, childA)
		r.CreateCR("a", widgetA)
		r.Create(ctl, childA, owned(tt.Object("a-0"), r, widgetA))
		r.Ready(widgetA).EndFaults().Settle()
		return finish(r, map[string]trace.Ref{"a": widgetA}, childA)
	},
	"d70-fixture-change-followed": func() *tt.Script {
		r := newRun(3)
		r.Create(trace.Harness, fixture, named("widget-config", map[string]any{"data": map[string]any{"label": "red"}}))
		r.Begin()
		uid := r.CreateCR("a", widgetA)
		r.Create(ctl, childA, tt.OwnedBy(named("a-0", map[string]any{"data": map[string]any{"label": "red"}}), uid, true))
		r.Ready(widgetA).EndFaults().Settle()
		r.Begin().Update(trace.Harness, fixture, named("widget-config", map[string]any{"data": map[string]any{"label": "blue"}}))
		r.Act("changeFixture", "", &fixture).Call(ctl, "get", fixture, 200)
		r.Update(ctl, childA, tt.OwnedBy(named("a-0", map[string]any{"data": map[string]any{"label": "blue"}}), uid, true))
		r.Tick(500 * ms).EndFaults().Settle()
		r.Begin().Restart().EndFaults().Settle()
		return finish(r, map[string]trace.Ref{"a": widgetA}, childA)
	},
	"d70-fixture-change-unwatched": func() *tt.Script {
		r := newRun(3)
		r.Create(trace.Harness, fixture, named("widget-config", map[string]any{"data": map[string]any{"label": "red"}}))
		r.Begin()
		uid := r.CreateCR("a", widgetA)
		r.Create(ctl, childA, tt.OwnedBy(named("a-0", map[string]any{"data": map[string]any{"label": "red"}}), uid, true))
		r.Ready(widgetA).EndFaults().Settle()
		r.Begin().Update(trace.Harness, fixture, named("widget-config", map[string]any{"data": map[string]any{"label": "blue"}}))
		r.Act("changeFixture", "", &fixture).EndFaults().Tick(1500 * ms).Settle()
		r.Begin().Restart().Call(ctl, "get", fixture, 200)
		r.Update(ctl, childA, tt.OwnedBy(named("a-0", map[string]any{"data": map[string]any{"label": "blue"}}), uid, true))
		r.EndFaults().Settle()
		return finish(r, map[string]trace.Ref{"a": widgetA}, childA)
	},
	"d71-one-of-two-deleted": func() *tt.Script {
		r := newRun(2)
		r.Begin()
		withChild(r, "a", true)
		withChild(r, "b", true)
		r.EndFaults().Settle()
		r.Begin().DeleteCR("a", widgetA).EndFaults()
		collect(r, childA).Settle()
		return finish(r, map[string]trace.Ref{"b": widgetB}, childB)
	},
	"d71-rule4-outlives-every-cr": func() *tt.Script {
		r := newRun(2)
		r.Begin()
		r.CreateCR("a", widgetA)
		r.CreateCR("b", widgetB)
		r.Create(ctl, cm, tt.Object("c"))
		r.Ready(widgetA).Ready(widgetB).EndFaults().Settle()
		r.Begin().DeleteCR("a", widgetA).EndFaults().Settle()
		return finish(r, map[string]trace.Ref{"b": widgetB})
	},
	"m1-inflight-write-clause7": func() *tt.Script {
		r := toy(2)
		r.Begin().Restart().EndFaults().Tick(1*s - 4*tt.Tick)
		r.UpdateInFlight(ctl, widgetA, named("a", map[string]any{"status": map[string]any{"ready": true, "observed": 2}}), 2*tt.Tick)
		r.Tick(50 * ms).Settle()
		return finish(r, map[string]trace.Ref{"a": widgetA}, childA)
	},
	"m1-restart-cut-create": func() *tt.Script {
		r := newRun(1)
		r.Begin()
		r.CreateCR("a", widgetA)
		r.CreateInFlight(ctl, childA, owned(tt.Object("a-0"), r, widgetA), 6*tt.Tick)
		r.Restart().Call(ctl, "create", tt.Ref("configmaps", "ns", ""), 409).Call(ctl, "get", childA, 200)
		r.Ready(widgetA).EndFaults().Settle()
		return finish(r, map[string]trace.Ref{"a": widgetA}, childA)
	},
	"m1-resurrected-child": func() *tt.Script {
		r := toy(1)
		r.Begin().DeleteCR("a", widgetA)
		collect(r, childA)
		r.Create(ctl, childA, tt.Object("a-0"))
		r.EndFaults().Settle()
		return r.Begin().Restart().EndFaults().Settle()
	},
	"d92-gc-after-owner-delete": func() *tt.Script {
		r := toy(1)
		r.Begin().Delete(trace.Harness, widgetA)
		r.At(r.Now, trace.Event{Work: &trace.Work{System: "gc", Pending: true}})
		r.Act("delete", "a", &widgetA).EndFaults()
		r.Tick(2300*ms).Delete(trace.GC, childA)
		r.At(r.Now, trace.Event{Work: &trace.Work{System: "gc", Pending: false}})
		r.Settle()
		return r.Begin().Restart().EndFaults().Settle()
	},
}
