package invariant

import (
	"cmp"
	"slices"
	"strings"
	"time"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/rosenhouse-bot-sandbox/reconciler-fuzzer/internal/observe"
)

// state is what the Observer had seen of the run namespace at one instant.
type state struct {
	at time.Time
	// live holds the latest version of every object that still existed,
	// ordered by kind and name.
	live []observe.Version
}

// statesAt returns the state at each instant. The instants must be in order.
func (in Input) statesAt(times []time.Time) []state {
	versions := in.versions()
	latest := map[observe.Key]observe.Version{}
	states := make([]state, 0, len(times))
	next := 0
	for _, t := range times {
		for ; next < len(versions) && !versions[next].Time.After(t); next++ {
			latest[versions[next].Key] = versions[next]
		}
		states = append(states, state{at: t, live: live(latest)})
	}
	return states
}

func (in Input) stateAt(t time.Time) state { return in.statesAt([]time.Time{t})[0] }

// version returns one object, which a state holds until it is deleted.
func (s state) version(key observe.Key) (observe.Version, bool) {
	for _, v := range s.live {
		if v.Key == key {
			return v, true
		}
	}
	return observe.Version{}, false
}

func (s state) managed(in Input) []observe.Version {
	var managed []observe.Version
	for _, v := range s.live {
		if in.History.IsManaged(v.Key) {
			managed = append(managed, v)
		}
	}
	return managed
}

func objects(versions []observe.Version) []*unstructured.Unstructured {
	out := make([]*unstructured.Unstructured, len(versions))
	for i, v := range versions {
		out[i] = v.Object
	}
	return out
}

func live(latest map[observe.Key]observe.Version) []observe.Version {
	out := make([]observe.Version, 0, len(latest))
	for _, v := range latest {
		if !v.Deleted {
			out = append(out, v)
		}
	}
	slices.SortFunc(out, func(a, b observe.Version) int {
		return cmp.Or(strings.Compare(observe.KindName(a.GVK), observe.KindName(b.GVK)), strings.Compare(a.Name, b.Name))
	})
	return out
}
