package observe_test

import (
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"os"
	"os/exec"
	"regexp"
	"strings"
	"testing"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"

	"github.com/rosenhouse-bot-sandbox/reconciler-fuzzer/internal/observe"
)

var (
	secretGVK = schema.GroupVersionKind{Version: "v1", Kind: "Secret"}
	marker    = regexp.MustCompile(`^\[redacted (\d+) bytes hmac-sha256:([0-9a-f]{16})\]$`)
)

// secret returns a Secret whose data holds each value base64-encoded, as the
// API server serves it.
func secret(name, resourceVersion string, values map[string]string) *unstructured.Unstructured {
	u := object(secretGVK, name, resourceVersion)
	data := map[string]any{}
	for key, value := range values {
		data[key] = base64.StdEncoding.EncodeToString([]byte(value))
	}
	u.Object["data"] = data
	return u
}

// writtenObjects records each object as a version of one Secret and returns
// the objects objects.jsonl holds.
func writtenObjects(t *testing.T, objects ...*unstructured.Unstructured) []map[string]any {
	t.Helper()
	s := observe.NewStore(managing(secretGVK))
	for i, obj := range objects {
		s.Record(secretGVK, obj, at(i))
	}
	var written []map[string]any
	for _, raw := range writeHistory(t, s) {
		written = append(written, line(t, []byte(raw))["object"].(map[string]any))
	}
	return written
}

func field(t *testing.T, obj map[string]any, fields ...string) map[string]any {
	t.Helper()
	value, found, err := unstructured.NestedMap(obj, fields...)
	if !found || err != nil {
		t.Fatalf("The object carries no map at %v: %v", fields, err)
	}
	return value
}

// digest is the marker's digest, and fails the test for a value that is no
// marker.
func digest(t *testing.T, value any) string {
	t.Helper()
	parts := marker.FindStringSubmatch(fmt.Sprint(value))
	if parts == nil {
		t.Fatalf("%v is not a marker.", value)
	}
	return parts[2]
}

func TestAHistoryLineHidesASecretsValuesAndNamesItsKeys(t *testing.T) {
	written := writtenObjects(t, secret("creds", "10", map[string]string{"token": "s3cr3t"}))[0]

	if strings.Contains(fmt.Sprint(written), "czNjcjN0") {
		t.Errorf("objects.jsonl holds the Secret's value: %v", written)
	}
	digest(t, field(t, written, "data")["token"])
}

func TestAMarkerCountsTheBytesTheSecretHolds(t *testing.T) {
	values := map[string]string{
		"token":  "s3cr3t",
		"padded": "\xfb\xff", // +/8=
	}
	data := field(t, writtenObjects(t, secret("creds", "10", values))[0], "data")

	for key, value := range values {
		want := fmt.Sprint(len(value))
		if got := marker.FindStringSubmatch(fmt.Sprint(data[key])); got == nil || got[1] != want {
			t.Errorf("data.%s is %v, want a marker of %s bytes: the value, not its base64.", key, data[key], want)
		}
	}
}

// The value is longer than the digest, so a digest drawn from it would show
// some of it.
func TestAHistoryLineHoldsNoPartOfASecretsValue(t *testing.T) {
	const value = "correct horse battery staple"
	written := fmt.Sprint(writtenObjects(t, secret("creds", "10", map[string]string{"token": value}))[0])

	for _, form := range recognisable(value) {
		if strings.Contains(written, form) {
			t.Errorf("objects.jsonl holds %q, which is part of the Secret's value: %s", form, written)
		}
	}
}

// recognisable returns each form in which a reader would know part of the
// value: every 4-byte window as it is and in hex, and every base64 group at
// each alignment.
func recognisable(value string) []string {
	var forms []string
	for i := range len(value) - 2 {
		forms = append(forms, base64.StdEncoding.EncodeToString([]byte(value[i:i+3])))
		if i+4 <= len(value) {
			window := value[i : i+4]
			forms = append(forms, window, hex.EncodeToString([]byte(window)))
		}
	}
	return forms
}

func TestAValueThatIsNoStringIsMarkedToo(t *testing.T) {
	obj := object(secretGVK, "creds", "10")
	obj.Object["data"] = map[string]any{"null": nil, "number": int64(42)}
	data := field(t, writtenObjects(t, obj)[0], "data")

	digest(t, data["null"])
	digest(t, data["number"])
}

func TestAMarkerShowsWhetherAValueChanged(t *testing.T) {
	written := writtenObjects(t,
		secret("creds", "10", map[string]string{"token": "s3cr3t-1", "ca": "same"}),
		secret("creds", "11", map[string]string{"token": "s3cr3t-2", "ca": "same"}),
	)
	before, after := field(t, written[0], "data"), field(t, written[1], "data")

	if digest(t, before["ca"]) != digest(t, after["ca"]) {
		t.Errorf("An unchanged value is %v, then %v.", before["ca"], after["ca"])
	}
	if digest(t, before["token"]) == digest(t, after["token"]) {
		t.Errorf("A changed value is %v both times.", before["token"])
	}
}

func TestEqualValuesShareAMarkerAcrossRunsOfOneProcess(t *testing.T) {
	first := writtenObjects(t, secret("creds", "10", map[string]string{"token": "s3cr3t"}))[0]
	second := writtenObjects(t, secret("creds", "10", map[string]string{"token": "s3cr3t"}))[0]

	if a, b := field(t, first, "data")["token"], field(t, second, "data")["token"]; a != b {
		t.Errorf("Two runs wrote one value as %v and %v.", a, b)
	}
}

func TestEachProcessKeysItsMarkersAfresh(t *testing.T) {
	markerOfAProcess := func() string {
		cmd := exec.Command(os.Args[0], "-test.run=^TestPrintAMarker$")
		cmd.Env = append(os.Environ(), "RECONCILER_FUZZER_PRINT_A_MARKER=1")
		out, err := cmd.Output()
		if err != nil {
			t.Fatalf("The helper process failed: %v\n%s", err, out)
		}
		printed, _, _ := strings.Cut(string(out), "\n")
		digest(t, printed)
		return printed
	}

	if first, second := markerOfAProcess(), markerOfAProcess(); first == second {
		t.Errorf("Two processes marked one value %s, so its marker can be looked up.", first)
	}
}

func TestPrintAMarker(t *testing.T) {
	if os.Getenv("RECONCILER_FUZZER_PRINT_A_MARKER") == "" {
		t.Skip("This is TestEachProcessKeysItsMarkersAfresh's helper process.")
	}
	fmt.Println(field(t, observe.Redacted(secretGVK, secret("creds", "10", map[string]string{"token": "s3cr3t"}).Object), "data")["token"])
}

// A controller may annotate a Secret with a copy or an unkeyed hash of its
// data, as kubectl and external-secrets do.
func TestAHistoryLineHidesASecretsAnnotations(t *testing.T) {
	obj := secret("creds", "10", map[string]string{"token": "s3cr3t"})
	obj.SetAnnotations(map[string]string{
		"kubectl.kubernetes.io/last-applied-configuration": `{"apiVersion":"v1","kind":"Secret","data":{"token":"czNjcjN0"}}`,
		"reconcile.external-secrets.io/data-hash":          "56e1b3f734a3d8e2c7932736ca6ff7fb9a9b5a14378c70c27c5e0adf",
	})
	annotations := field(t, writtenObjects(t, obj)[0], "metadata", "annotations")

	for key := range obj.GetAnnotations() {
		digest(t, annotations[key])
	}
}

func TestAnAnnotationsMarkerShowsWhetherItChanged(t *testing.T) {
	annotated := func(resourceVersion, hash string) *unstructured.Unstructured {
		obj := secret("creds", resourceVersion, map[string]string{"token": "s3cr3t"})
		obj.SetAnnotations(map[string]string{"data-hash": hash, "owner": "same"})
		return obj
	}
	written := writtenObjects(t, annotated("10", "hash-1"), annotated("11", "hash-2"))
	before, after := field(t, written[0], "metadata", "annotations"), field(t, written[1], "metadata", "annotations")

	if digest(t, before["owner"]) != digest(t, after["owner"]) {
		t.Errorf("An unchanged annotation is %v, then %v.", before["owner"], after["owner"])
	}
	if digest(t, before["data-hash"]) == digest(t, after["data-hash"]) {
		t.Errorf("A changed annotation is %v both times.", before["data-hash"])
	}
}

func TestAnyOtherKindIsWrittenAsItIs(t *testing.T) {
	for _, gvk := range []schema.GroupVersionKind{
		configMapGVK,
		{Group: "example.io", Version: "v1", Kind: "Secret"},
	} {
		s := observe.NewStore(managing(gvk))
		s.Record(gvk, withData(object(gvk, "child", "10"), "czNjcjN0"), at(0))

		written := line(t, []byte(writeHistory(t, s)[0]))["object"].(map[string]any)
		if got := field(t, written, "data")["value"]; got != "czNjcjN0" {
			t.Errorf("A %v's data.value is written as %v, want it as recorded.", gvk, got)
		}
	}
}

func TestWritingTheHistoryLeavesTheRecordedSecretWhole(t *testing.T) {
	s := observe.NewStore(managing(secretGVK))
	s.Record(secretGVK, secret("creds", "10", map[string]string{"token": "s3cr3t"}), at(0))
	writeHistory(t, s)

	recorded := s.History(key(secretGVK, "creds"))[0].Object.Object
	if got := field(t, recorded, "data")["token"]; got != "czNjcjN0" {
		t.Errorf("The store holds data.token %v after writing, want the value G5 compares.", got)
	}
}

func TestRedactedIsWhatObjectsJSONLHolds(t *testing.T) {
	obj := secret("creds", "10", map[string]string{"token": "s3cr3t"})

	written := field(t, writtenObjects(t, obj)[0], "data")["token"]
	if got := field(t, observe.Redacted(secretGVK, obj.Object), "data")["token"]; got != written {
		t.Errorf("Redacted gives data.token %v, and objects.jsonl %v.", got, written)
	}
}
