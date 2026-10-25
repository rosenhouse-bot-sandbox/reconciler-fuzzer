package interpose_test

import (
	"fmt"
	"io"
	"net/http"
	"reflect"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/rosenhouse-bot-sandbox/reconciler-fuzzer/internal/interpose"
	"github.com/rosenhouse-bot-sandbox/reconciler-fuzzer/internal/trace"
)

// negotiating answers as an API server does: in protobuf when the Accept
// header allows it, and else in JSON. It keeps each Accept header.
type negotiating struct {
	mu      sync.Mutex
	accepts []string
}

func (n *negotiating) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	accept := r.Header.Get("Accept")
	n.mu.Lock()
	n.accepts = append(n.accepts, accept)
	n.mu.Unlock()
	watch := r.URL.Query().Get("watch") == "true"
	if strings.Contains(accept, "protobuf") {
		contentType := "application/vnd.kubernetes.protobuf"
		if watch {
			contentType += ";stream=watch"
		}
		w.Header().Set("Content-Type", contentType)
		w.Write([]byte("k8s\x00"))
		return
	}
	w.Header().Set("Content-Type", "application/json")
	switch {
	case watch:
		fmt.Fprintf(w, `{"type":"ADDED","object":%s}`+"\n", gadgetJSON("g1", "u1", "5"))
	case strings.HasSuffix(r.URL.Path, "/g1"):
		w.Write([]byte(gadgetJSON("g1", "u1", "5")))
	default:
		fmt.Fprintf(w, `{"kind":"GadgetList","metadata":{"resourceVersion":"9"},"items":[%s]}`, gadgetJSON("g1", "u1", "5"))
	}
}

func (n *negotiating) accepted() []string {
	n.mu.Lock()
	defer n.mu.Unlock()
	return slices.Clone(n.accepts)
}

func getAccepting(t *testing.T, l *interpose.Listener, path, accept string) {
	t.Helper()
	req, err := http.NewRequest("GET", l.URL()+path, nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Accept", accept)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET %s: %v", path, err)
	}
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
}

func TestARequestOfASeenKindAsksForJSON(t *testing.T) {
	t.Skip("enabled by the run package")
	for _, accept := range []string{"application/vnd.kubernetes.protobuf,application/json", "application/vnd.kubernetes.protobuf"} {
		for _, path := range []string{gadgetsPath + "/g1", gadgetsPath, gadgetsPath + "?watch=true"} {
			t.Run(accept+" "+path, func(t *testing.T) {
				upstream := &negotiating{}
				l, recorded := listen(t, upstream, seeingGadgets)

				getAccepting(t, l, path, accept)

				recorded.waitCalls(t, 1)
				l.Close()
				if got := upstream.accepted(); len(got) != 1 || strings.Contains(got[0], "protobuf") {
					t.Errorf("The upstream got Accept %q, want one without protobuf.", got)
				}
				want := seenGadget("g1", "u1", "5")
				if strings.Contains(path, "watch") {
					want.Stream = 1
				}
				if got := recorded.seen(); !reflect.DeepEqual(got, []trace.Seen{want}) {
					t.Errorf("Recorded %+v, want %+v.", got, []trace.Seen{want})
				}
			})
		}
	}
}

func TestARequestOfAnotherKindKeepsItsAccept(t *testing.T) {
	const accept = "application/vnd.kubernetes.protobuf,application/json"
	upstream := &negotiating{}
	l, recorded := listen(t, upstream, seeingGadgets)

	getAccepting(t, l, "/api/v1/namespaces/ns1/configmaps/g1", accept)

	recorded.waitCalls(t, 1)
	if got := upstream.accepted(); !slices.Equal(got, []string{accept}) {
		t.Errorf("The upstream got Accept %q, want %q.", got, accept)
	}
}

func TestAListOrWatchRecordsWhetherItHasASelector(t *testing.T) {
	t.Skip("enabled by the run package")
	for path, want := range map[string]bool{
		gadgetsPath:                                       false,
		gadgetsPath + "?labelSelector=app%3Dx":            true,
		gadgetsPath + "?fieldSelector=metadata.name%3Dg1": true,
		gadgetsPath + "?labelSelector=&fieldSelector=":    false,
		gadgetsPath + "?watch=true":                       false,
		gadgetsPath + "?watch=true&labelSelector=app%3Dx": true,
	} {
		l, recorded := listen(t, ok())

		do(t, l, "GET", path)

		if got := recorded.waitCalls(t, 1)[0].Selector; got != want {
			t.Errorf("GET %s records Selector %v, want %v.", path, got, want)
		}
	}
}
