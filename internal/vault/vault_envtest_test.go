//go:build envtest

package vault_test

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/rosenhouse-bot-sandbox/reconciler-fuzzer/internal/fake"
	"github.com/rosenhouse-bot-sandbox/reconciler-fuzzer/internal/system"
	"github.com/rosenhouse-bot-sandbox/reconciler-fuzzer/internal/trace"
	"github.com/rosenhouse-bot-sandbox/reconciler-fuzzer/internal/vault"
)

func TestTheMapReadsAVaultDevServerThroughAFake(t *testing.T) {
	binary, err := filepath.Abs("../../bin/vault")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(binary); err != nil {
		t.Fatalf("%v; run make setup", err)
	}
	rec, err := trace.NewRecorder(filepath.Join(t.TempDir(), "trace.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	defer rec.Close()
	f := fake.New(fake.Options{
		Name: "vault", Dir: t.TempDir(), API: vault.New("vault", "root"),
		Command:  []string{binary, "server", "-dev", "-dev-root-token-id=root", "-dev-listen-address=${addr}"},
		Fixtures: []fake.Fixture{{Resource: "secret", Name: "app/db", Body: json.RawMessage(`{"password":"p1"}`)}},
		Vars:     map[string]string{"token": "root"},
	})
	defer f.Stop(context.Background())
	endpoints, err := f.Start(context.Background(), &system.Env{Record: rec.Record, Dir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	call := func(verb, uid string) func(trace.Event) bool {
		return func(e trace.Event) bool {
			return e.Call != nil && e.Call.Verb == verb && e.Call.ResultUID == uid && e.Call.Status/100 == 2
		}
	}
	version := func(uid, data string, deleted bool) func(trace.Event) bool {
		return func(e trace.Event) bool {
			v := e.Version
			return v != nil && v.UID == uid && v.Deleted == deleted && strings.Contains(string(v.Body), `"data":`+data)
		}
	}
	has := func(what string, match func(trace.Event) bool) {
		t.Helper()
		deadline := time.Now().Add(5 * time.Second)
		for {
			for _, e := range rec.Events() {
				if match(e) {
					return
				}
			}
			if time.Now().After(deadline) {
				t.Fatalf("the trace never showed %s", what)
			}
			time.Sleep(10 * time.Millisecond)
		}
	}
	if !slices.ContainsFunc(rec.Events(), version("vault/secret/app/db/1", `{"password":"p1"}`, false)) ||
		!slices.ContainsFunc(rec.Events(), func(e trace.Event) bool {
			return call("create", "vault/secret/app/db/1")(e) && e.Call.Caller == trace.Harness
		}) {
		t.Fatal("Start returned before the trace held the harness's fixture")
	}

	do := func(method, path, body string) string {
		t.Helper()
		req, _ := http.NewRequest(method, endpoints.Vars["url"]+path, strings.NewReader(body))
		req.Header.Set("X-Vault-Token", endpoints.Vars["token"])
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		data, _ := io.ReadAll(resp.Body)
		if resp.StatusCode/100 != 2 {
			t.Fatalf("%s %s = %d %s", method, path, resp.StatusCode, data)
		}
		return string(data)
	}
	do("POST", "/v1/secret/data/app/db", `{"data":{"password":"p2"}}`)
	has("the controller's update", call("update", "vault/secret/app/db/1"))
	has("the second version", version("vault/secret/app/db/1", `{"password":"p2"}`, false))
	do("DELETE", "/v1/secret/metadata/app/db", "")
	has("the delete", call("delete", "vault/secret/app/db/1"))
	has("the instance gone", version("vault/secret/app/db/1", `{"password":"p2"}`, true))
	do("POST", "/v1/secret/data/app/db", `{"data":{"password":"p3"}}`)
	has("the second instance's create", call("create", "vault/secret/app/db/2"))
	has("the second instance", version("vault/secret/app/db/2", `{"password":"p3"}`, false))

	var answer struct {
		Data struct{ Data json.RawMessage } `json:"data"`
	}
	json.Unmarshal([]byte(do("GET", "/v1/secret/data/app/db", "")), &answer)
	resources, err := f.Snapshot(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	var listed vault.Listed
	if len(resources) == 1 {
		json.Unmarshal(resources[0].Body, &listed)
	}
	if len(resources) != 1 || resources[0].UID != "vault/secret/app/db/2" || listed.CurrentVersion != 1 || string(listed.Data) != string(answer.Data.Data) {
		t.Errorf("Snapshot = %+v, want the get's data %s", resources, answer.Data.Data)
	}
}
