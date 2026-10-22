package fake_test

import (
	"testing"

	"github.com/rosenhouse-bot-sandbox/reconciler-fuzzer/internal/fake"
	"github.com/rosenhouse-bot-sandbox/reconciler-fuzzer/internal/trace"
)

var secret = trace.Ref{System: "vault", Resource: "secret", Name: "a/b"}

func want(t *testing.T, what, got, want string) {
	t.Helper()
	if got != want {
		t.Errorf("%s = %q, want %q", what, got, want)
	}
}

func TestAnInstanceNumberRisesOnlyAfterItsInstanceEnds(t *testing.T) {
	in := fake.NewInstances()
	if in.Live(secret) {
		t.Fatal("an unknown name is live")
	}
	want(t, "the first create", in.Created(secret), "vault/secret/a/b/1")
	want(t, "a second create of a live name", in.Created(secret), "vault/secret/a/b/1")
	if !in.Live(secret) {
		t.Fatal("a created name is not live")
	}
	uid, ended := in.Deleted(secret)
	want(t, "the deleted instance", uid, "vault/secret/a/b/1")
	if !ended || in.Live(secret) {
		t.Fatal("a delete did not end the instance")
	}
	if _, ended := in.Deleted(secret); ended {
		t.Error("a second delete ended an instance")
	}
	want(t, "a stale listing after the delete", in.Shown(secret), "vault/secret/a/b/1")
	want(t, "a create after the delete", in.Created(secret), "vault/secret/a/b/2")
	in.Ended(secret, "vault/secret/a/b/1")
	if !in.Live(secret) {
		t.Error("the end of an old instance ended the new one")
	}
	in.Ended(secret, "vault/secret/a/b/2")
	want(t, "a listing after the observer saw it gone", in.Shown(secret), "vault/secret/a/b/3")
}

func TestAShownUnknownNameStartsAnInstance(t *testing.T) {
	in := fake.NewInstances()
	want(t, "an unknown name", in.Shown(secret), "vault/secret/a/b/1")
	if !in.Live(secret) {
		t.Error("a shown name is not live")
	}
	object := trace.Ref{System: "s3", Resource: "objects", Namespace: "bkt", Name: "k/1"}
	want(t, "a namespaced name", in.Created(object), "s3/objects/bkt/k/1/1")
}
