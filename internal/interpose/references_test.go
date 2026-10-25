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

	"github.com/rosenhouse-bot-sandbox/reconciler-fuzzer/internal/crud"
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

// getAccepting sends a GET with one Accept header line per accept.
func getAccepting(t *testing.T, l *interpose.Listener, path string, accept ...string) {
	t.Helper()
	req, err := http.NewRequest("GET", l.URL()+path, nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, a := range accept {
		req.Header.Add("Accept", a)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET %s: %v", path, err)
	}
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
}

func TestARequestOfASeenKindAsksForJSON(t *testing.T) {
	const metadataList = "as=PartialObjectMetadataList;g=meta.k8s.io;v=v1"
	for _, c := range []struct {
		accept []string
		want   string
	}{
		{[]string{"application/vnd.kubernetes.protobuf,application/json"}, "application/json"},
		{[]string{"application/vnd.kubernetes.protobuf"}, "application/json"},
		{[]string{"application/vnd.kubernetes.protobuf, */*"}, "*/*"},
		{[]string{"application/vnd.kubernetes.protobuf;" + metadataList + ",application/json;" + metadataList + ",application/json"}, "application/json;" + metadataList + ",application/json"},
		{[]string{"application/vnd.kubernetes.protobuf", "application/json;" + metadataList}, "application/json;" + metadataList},
	} {
		for _, path := range []string{gadgetsPath + "/g1", gadgetsPath, gadgetsPath + "?watch=true"} {
			t.Run(strings.Join(c.accept, " ")+" "+path, func(t *testing.T) {
				upstream := &negotiating{}
				l, recorded := listen(t, upstream, seeingGadgets)

				getAccepting(t, l, path, c.accept...)

				recorded.waitCalls(t, 1)
				l.Close()
				if got := upstream.accepted(); !slices.Equal(got, []string{c.want}) {
					t.Errorf("The upstream got Accept %q, want %q.", got, c.want)
				}
				seen := seenGadget("g1", "u1", "5")
				if strings.Contains(path, "watch") {
					seen.Stream = 1
				}
				if got := recorded.seen(); !reflect.DeepEqual(got, []trace.Seen{seen}) {
					t.Errorf("Recorded %+v, want %+v.", got, []trace.Seen{seen})
				}
			})
		}
	}
}

func TestARequestOfAnotherKindOrToAFakeKeepsItsAccept(t *testing.T) {
	const accept = "application/vnd.kubernetes.protobuf,application/json"
	toCloud := func(o *interpose.Options) {
		m := crud.New("cloud")
		m.SetCollections([]string{"buckets"})
		o.System, o.API, o.SeenAll = "cloud", m, true
	}
	for path, configure := range map[string]func(*interpose.Options){
		"/api/v1/namespaces/ns1/configmaps/g1": seeingGadgets,
		"/buckets/b-1":                         toCloud,
	} {
		upstream := &negotiating{}
		l, recorded := listen(t, upstream, configure)

		getAccepting(t, l, path, accept)

		recorded.waitCalls(t, 1)
		if got := upstream.accepted(); !slices.Equal(got, []string{accept}) {
			t.Errorf("GET %s: the upstream got Accept %q, want %q.", path, got, accept)
		}
	}
}

func TestAListOrWatchRecordsWhetherItIsPartial(t *testing.T) {
	for path, want := range map[string]bool{
		gadgetsPath:                                       false,
		gadgetsPath + "?labelSelector=app%3Dx":            true,
		gadgetsPath + "?fieldSelector=metadata.name%3Dg1": true,
		gadgetsPath + "?labelSelector=&fieldSelector=":    false,
		gadgetsPath + "?limit=500":                        false,
		gadgetsPath + "?limit=500&continue=next":          true,
		gadgetsPath + "?watch=true":                       false,
		gadgetsPath + "?watch=true&labelSelector=app%3Dx": true,
		gadgetsPath + "/g1?labelSelector=app%3Dx":         false,
	} {
		l, recorded := listen(t, ok())

		do(t, l, "GET", path)

		if got := recorded.waitCalls(t, 1)[0].Partial; got != want {
			t.Errorf("GET %s records Partial %v, want %v.", path, got, want)
		}
	}
	for _, c := range []struct {
		query, page string
		want        bool
	}{
		{"?limit=1", `"continue":"next"`, true},
		{"?limit=1", `"continue":""`, false},
		{"?labelSelector=app%3Dx", `"continue":""`, true},
	} {
		body := `{"kind":"GadgetList","metadata":{` + c.page + `},"items":[` + gadgetJSON("g1", "u1", "5") + `]}`
		l, recorded := listen(t, respondOK("application/json", "", body), seeingGadgets)

		do(t, l, "GET", gadgetsPath+c.query)

		if got := recorded.waitCalls(t, 1)[0].Partial; got != c.want {
			t.Errorf("A list %s that answers with %s records Partial %v, want %v.", c.query, c.page, got, c.want)
		}
	}
}
