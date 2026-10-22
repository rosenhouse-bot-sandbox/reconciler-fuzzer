# cert-manager v1.21.2 can give a recreated Certificate the deleted Certificate's private key

This draft is for reconciler-fuzzer's maintainer to file at cert-manager. Triage did not search
cert-manager's tracker, so the maintainer searches it before filing. No agent may file it.
The agents had no registry and only a Kubernetes 1.37.0 node image, while cert-manager
v1.21.2 tests on 1.33 to 1.36. So the maintainer also reruns the documented command on
1.36, against the released chart with `enableCertificateOwnerRef: true`. The issue body
starts below the horizontal rule. Its links are relative to this file, so they need
reconciler-fuzzer's URL before filing.

---

🤖 Created by Claude 🤖

## Summary

reconciler-fuzzer, a black-box test harness for Kubernetes controllers, found this in cert-manager
v1.21.2 with `--enable-certificate-owner-ref=true`.

A client deletes a Certificate and creates one with the same name and `secretName`. The new
Certificate can reach cert-manager before cert-manager's Secret informer has seen the
garbage collector delete the Secret. The new Certificate's Secret then often holds the
deleted Certificate's private key. It gets there in one of two ways:

- **Write-back.** The garbage collector deletes the Secret. cert-manager then creates it
  again, with a new UID, from the copy still in its informer cache. Either the issuing
  controller applies the cached data with the new Certificate as owner, or, under
  `rotationPolicy: Never`, an issuance reuses the cached key.
- **Re-point.** The issuing controller's apply lands before the garbage collector's delete.
  The Secret then names a live owner, so the garbage collector keeps it.

What follows depends on the new Certificate's spec:

- If the spec is unchanged, the new Certificate becomes Ready with the old key. Under
  `rotationPolicy: Always`, the default, cert-manager issues nothing, so the policy never
  applies, and the Secret keeps the deleted Certificate's certificate too.
- If a field such as `dnsNames` changes, cert-manager issues a new certificate. Under
  `Never` it signs the old key. Under `Always` it generates a new key.
- If the spec asks for another key algorithm under `Never`, the new Certificate stays
  `Ready=False` with reason `SecretMismatch`, waiting for a user.

So a client that recreates a Certificate to get a new key can keep the old one, if the
create follows the delete within a window. The window is short on an idle cluster and
widens as cert-manager's Secret informer lags, as the tables under Reproduction show.

## Code path

Paths are relative to cert-manager v1.21.2. The issuing controller writes a Secret back like
this:

- While a Certificate is not `Issuing`, the issuing controller calls `ensureSecretData`
  (`pkg/controller/certificates/issuing/issuing_controller.go:198-206`).
- `ensureSecretData` reads the Secret from the informer's cache
  (`pkg/controller/certificates/issuing/secret_manager.go:40`).
- `SecretOwnerReferenceMismatch` finds no ownerReference with the new Certificate's UID
  (`internal/controller/certificates/policies/checks.go:756-790`). cert-manager logs
  `applying Secret data` with the message `unexpected Secret Owner Reference value on
  Secret --enable-certificate-owner-ref=true`, and calls `UpdateData` with the data it
  read (`secret_manager.go:98-99`).
- `UpdateData` server-side-applies that data with `Force: true` and one controller
  ownerReference to the new Certificate
  (`pkg/controller/certificates/issuing/internal/secret.go:108-127`). Its doc says "If the
  Secret resource does not exist, it will be created on Apply" (`internal/secret.go:92`).

Whether the new Certificate is issued depends on the trigger policy chain
(`internal/controller/certificates/policies/policies.go:71-84`):

- With an unchanged spec, the old Secret passes every check. No CertificateRequest belongs
  to the new Certificate, so `CurrentCertificateRequestMismatchesSpec` compares the Secret's
  certificate with the spec instead (`checks.go:214-223`). The readiness chain
  (`policies.go:89-102`) passes the same way, so the Certificate becomes Ready. Only
  `SecretDoesNotExist` can start an issuance, when the trigger controller runs while the
  Secret is gone.
- The key manager acts only while a Certificate is `Issuing`
  (`pkg/controller/certificates/keymanager/keymanager_controller.go:178-187`). So without
  an issuance, `rotationPolicy` never applies.
- When the spec changes, the trigger chain starts an issuance. Under `Never`, the key
  manager reads the Secret from the informer's cache (`keymanager_controller.go:250`). It
  reuses that Secret's key, or waits for a user when the key does not fit the spec
  (`:262-281`). The cache can still hold a Secret that the garbage collector has deleted.
  Issuance then creates the Secret again with the old key (`issuing_controller.go:464`).
  This is the second write-back path.

kube-controller-manager's garbage collector reads the dependent, checks each owner, and
deletes it with UID and resourceVersion preconditions. After a Conflict, it retries without
the resourceVersion only if the ownerReferences did not change. It keeps a dependent that
names a live owner. See Kubernetes v1.37.0,
`pkg/controller/garbagecollector/garbagecollector.go:521-577` and `:654`, and
`operations.go:53-80`.

## Documented behaviour

- The help for `--enable-certificate-owner-ref`
  (`cmd/controller/app/options/options.go:181-183`) says: "When this flag is enabled, the
  secret will be automatically removed when the certificate resource is deleted."
- The doc of `rotationPolicy` (`pkg/apis/certmanager/v1/types_certificate.go:353-356`) says:
  "If set to `Never`, a private key will only be generated if one does not already exist in
  the target `spec.secretName`."

Under the default background cascade, the garbage collector does delete the Secret. The
write-back then creates it again from cert-manager's cache, which breaks the flag's promise.
The comment where `ensureSecretData` finds no Secret (`secret_manager.go:42-43`) states the
intent: "Secret doesn't exist so we can't do anything." The stale cache never reaches that
branch, because the lister still returns the deleted Secret.

The re-point may count as the race a background cascade leaves open.
`CertificateOwnsSecret` decides which Certificate owns a Secret by the
`cert-manager.io/certificate-name` annotation, not by ownerReferences
(`internal/controller/certificates/certificates.go:32-46`). Still, the Secret outlives the
Certificate it was issued for.

## Reproduction

[`cert-manager-secret-write-back/main.go`](cert-manager-secret-write-back/main.go) needs a
cluster with cert-manager's CRDs and cert-manager running with
`--enable-certificate-owner-ref=true`. Each run makes a namespace and a self-signed Issuer.
Then:

1. It creates Certificate `example` with `secretName: example-tls`, DNS name
   `example.test`, the default RSA key and the `-policy` given, and waits for Ready.
2. It records the Secret's UID and the sha256 of its `tls.key` and `tls.crt`.
3. It deletes the Certificate with the `-propagation` given, `Background` by default, and
   polls every 10 ms until the Certificate is gone.
4. It waits for `-pause`, none by default. Then it creates `example` again with the
   `-algorithm`, `-policy` and `-dns` given. Its clients have no rate limit, so the create
   follows within milliseconds.
5. It waits up to 30 s for the new Certificate to stay Ready, with no issuance in flight,
   for 2 s. It waits up to 30 s more for the Secret. Then it reads the Secret's UID,
   controller owner and hashes.

A run keeps the key when the Secret still holds the old one. A new UID means a write-back,
and the old UID owned by the new Certificate means a re-point. The UID does not tell which
write-back path ran. cert-manager's log and Events do. Without `-collect`, the program
never writes the Secret. From a reconciler-fuzzer checkout:

```sh
go run ./docs/findings/cert-manager-secret-write-back -kubeconfig "$KUBECONFIG" -runs 10 -algorithm RSA -policy Always
```

The cluster was kind v0.33.0 with node v1.37.0, newer than the Kubernetes 1.33 to 1.36 that
cert-manager v1.21.2 tests on. cert-manager was v1.21.2 built from source. In all rows but
the last, it ran out of cluster with `--leader-elect=false`. In the last, it ran in the
cluster as a Deployment, with leader election and cluster-admin RBAC. Each batch of 10 runs
had one cert-manager process and one reproducer process, and the batches of every row took
turns.

| flags beside `-runs 10` | runs | key kept | write-back | re-point |
| --- | --- | --- | --- | --- |
| `-policy Always -algorithm RSA` | 30 | 15 | 14 | 1 |
| `-policy Never -algorithm RSA` | 30 | 26 | 25 | 1 |
| `-policy Never -algorithm ECDSA` | 30 | 14 | 9 | 5 |
| `-policy Never -algorithm RSA -dns other.test` | 80 | 42 | 37 | 5 |
| `-policy Always -algorithm RSA -dns other.test` | 30 | 0 | 0 | 0 |
| `-policy Always -algorithm RSA -propagation Foreground` | 30 | 0 | 0 | 0 |
| `-policy Always -algorithm RSA`, cert-manager in cluster | 30 | 9 | 8 | 1 |

- With the same spec under `Always`, every kept key came with the deleted Certificate's
  `tls.crt`, byte for byte, and `Ready=True`. Under `Never`, 17 of 26 did. The other 9 got
  a new `tls.crt` for the old key, after the key manager emitted `Reused`.
- With another DNS name under `Never`, every kept key came with a new `tls.crt` and
  `Ready=True`.
- With ECDSA under `Never`, every kept key ended `Ready=False` with reason
  `SecretMismatch`, and still `Issuing`.
- Under the foreground cascade, cert-manager never logged `applying Secret data`.
- No run ended with the old Secret uncollected.

On this idle cluster the window is short: only a create within about 10 ms of the delete
kept the key. With `-policy Always -algorithm RSA`, a pause before the create kept the key
in fewer runs:

| `-pause` | runs | key kept |
| --- | --- | --- |
| none | 50 | 29 |
| `5ms` | 20 | 2 |
| `10ms` | 20 | 1 |
| `20ms`, `50ms`, `100ms` or `200ms` | 20 each | 0 |

The 50 runs without a pause are the first row's 30 and 20 that took turns with the pauses.

`kubectl replace --force` (kubectl v1.37.0) deletes the Certificate, sees it gone, and
creates it 35 to 58 ms after the delete. With the first row's Certificate, it kept the key
in none of 20 tries.

cert-manager's Secret informer sets the window, not the client. A foreground cascade
(`kubectl delete --cascade=foreground`) deletes the Secret before the Certificate, but
cert-manager's cache does not wait for the garbage collector. So the cascade adds only its
own duration. In the batches below, cert-manager ran out of cluster behind a proxy. At each
`DELETED` event on one of cert-manager's Secret watches, the proxy held that watch for a set
time, and later events queued behind it. The reproducer and kubectl went to the API server
directly. The reproducer ran with `-algorithm RSA -propagation Foreground`, and kubectl
replaced the first row's Certificate. Each cell counts the runs that kept the key:

| cert-manager's Secret watch | `-policy Always` | `-policy Never` | `kubectl replace --force` |
| --- | --- | --- | --- |
| not held | 0 of 20 | 0 of 20 | 0 of 20 |
| held 50 ms at each delete | 8 of 10 | 10 of 10 | not run |
| held 100 ms at each delete | 19 of 20 | 20 of 20 | 19 of 20 |

- Every kept key came by write-back.
- With the watch not held, the new Certificate reached cert-manager 15 to 41 ms after the
  Secret's delete under the foreground cascade, and 22 to 38 ms after it under
  `kubectl replace --force`. Under the reproducer's default background cascade, it arrived
  between 12 ms before and 3 ms after the delete, and 3 of 10 runs kept the key.
- In 10 runs where the proxy delayed every Secret event by 100 ms instead, cert-manager
  issued each first Certificate more than once before any delete. Under `Never`, the first
  Certificate did not settle within a minute, twice. So those batches do not measure this
  window.

## How reconciler-fuzzer found it

reconciler-fuzzer runs cert-manager against envtest, with its own garbage collector. Its
cert-manager config holds the property `fresh-key`: a Certificate's Secret holds a `tls.key`
first written while that Certificate owned the Secret
([config.yaml](../../examples/cert-manager/config.yaml)).
[write-back-always.json](../../examples/cert-manager/bugs/write-back-always.json) issues a
Certificate under `rotationPolicy: Always`. Its next step holds cert-manager's Secret watch
for 2 s, deletes the Certificate, and creates it again with the same spec.
It fails `fresh-key` whether the Secret was written back or re-pointed, and the finding's
details say which. `make repro-cert-manager` runs it 20 times.

| plan | runs | `fresh-key` failed | cert-manager logged `applying Secret data` |
| --- | --- | --- | --- |
| `write-back-always.json` | 20 | 20 | 20 |
| the same, without the `holdWatch` fault | 20 | 0 | 1 |

- Every failure came by write-back.
- In the run without the hold that applied the data, `fresh-key` still held at the step's
  end.
- Generated plans find it too. A scan of the first 3000 cert-manager seeds found 279 that
  draw a step holding the Secret watch while it deletes a CR and creates it again. The
  first six, seeds 14, 40, 53, 54, 55 and 66, each failed `fresh-key` at that step in one
  run, by write-back.
- The program's `-collect` deletes what the old Certificate controls, much as
  kube-controller-manager would. On envtest with `-policy Always -algorithm RSA`, it kept the
  key in 3 of 10 runs.

## Possible fix

`ensureSecretData` can hand `UpdateData` the UID of the Secret it read at
`secret_manager.go:40`, and `UpdateData` can put that UID in the apply. An apply that names
a UID and finds no object fails with Conflict: "uid mismatch: the provided object specified
uid %s, and no existing object was found" (k8s.io/apiserver v0.37.0,
`pkg/endpoints/handlers/patch.go:612-618`). The controller's retry then reads that the
Secret is gone. Issuance (`issuing_controller.go:464`) passes no UID, so it still creates a
missing Secret.

```diff
--- a/pkg/controller/certificates/issuing/internal/secret.go
+++ b/pkg/controller/certificates/issuing/internal/secret.go
@@ -25,6 +25,7 @@
 	corev1 "k8s.io/api/core/v1"
 	apierrors "k8s.io/apimachinery/pkg/api/errors"
 	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
+	"k8s.io/apimachinery/pkg/types"
 	applycorev1 "k8s.io/client-go/applyconfigurations/core/v1"
 	applymetav1 "k8s.io/client-go/applyconfigurations/meta/v1"
 	coreclient "k8s.io/client-go/kubernetes/typed/core/v1"
@@ -66,6 +67,9 @@
 	PrivateKey, Certificate, CA         []byte // #nosec G117 -- holds runtime certificate material; not a hardcoded secret
 	CertificateName                     string
 	IssuerName, IssuerKind, IssuerGroup string
+	// UID, if set, is the UID of the Secret the data was read from. The apply
+	// then fails rather than create a Secret that has since been deleted.
+	UID types.UID
 }
 
 // NewSecretsManager returns a new SecretsManager. Setting
@@ -122,6 +126,10 @@
 		})
 	}
 
+	if data.UID != "" {
+		applyCnf = applyCnf.WithUID(data.UID)
+	}
+
 	log.V(logf.DebugLevel).Info("applying secret")
 
 	_, err = s.secretClient.Secrets(secret.Namespace).Apply(ctx, applyCnf, applyOpts)
--- a/pkg/controller/certificates/issuing/secret_manager.go
+++ b/pkg/controller/certificates/issuing/secret_manager.go
@@ -73,6 +73,7 @@
 		IssuerName:      secret.Annotations[cmapi.IssuerNameAnnotationKey],
 		IssuerKind:      secret.Annotations[cmapi.IssuerKindAnnotationKey],
 		IssuerGroup:     secret.Annotations[cmapi.IssuerGroupAnnotationKey],
+		UID:             secret.UID,
 	}
 
 	// Check whether the Certificate's Secret has correct output format and
```

Against kube-apiserver 1.37.0, a forced apply of a Secret behaved like this:

- With the live Secret's UID, it updated the Secret.
- With the UID of a deleted Secret, it failed with that Conflict and created nothing.
- With the UID of a Secret that another of the same name had replaced, it failed with
  `metadata.uid: field is immutable`.

The patch closes only the issuing controller's write-back. A build of v1.21.2 with it took
turns with the stock batches above, on the table's first five rows:

| flags beside `-runs 10` | runs | key kept | write-back | re-point |
| --- | --- | --- | --- | --- |
| `-policy Always -algorithm RSA` | 30 | 0 | 0 | 0 |
| `-policy Never -algorithm RSA` | 30 | 2 | 0 | 2 |
| `-policy Never -algorithm ECDSA` | 30 | 4 | 0 | 4 |
| `-policy Never -algorithm RSA -dns other.test` | 80 | 9 | 3 | 6 |
| `-policy Always -algorithm RSA -dns other.test` | 30 | 0 | 0 | 0 |

- With the same spec under `Always`, it logged the Conflict above in 19 of 30 runs, and
  then generated a new key.
- With another DNS name under `Never`, it still wrote back in 3 of 80 runs. In each, the
  apply failed with the Conflict. The key manager then emitted `Reused` for the deleted
  Secret's key, and issuance created the Secret again with it.

Two paths stay open:

- The key manager still reads the Secret from its cache (`keymanager_controller.go:250`). It
  could read the Secret from the API server before it reuses a key under `Never`.
- The issuing controller still re-points a Secret that the garbage collector has yet to
  delete. cert-manager could leave a Secret alone while its controller ownerReference names
  another UID, so that the garbage collector deletes it.

No run tested either change.

## Unknown

- cert-manager may mean to adopt a Secret whose owner was deleted.
- Every run used an idle cluster. A garbage collector that lags would widen the re-point
  window, and no run measured that. No run measured how far cert-manager's Secret informer
  lags on a busy cluster, such as one with many Secrets.
- No run used the released image, or a Kubernetes version that v1.21.2 tests on.
- Only the tests of `pkg/controller/certificates/issuing/...` ran against the patch, and
  they pass.
