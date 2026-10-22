# external-secrets

`make test-example-external-secrets` builds external-secrets at the pinned tag with its Vault
provider (`-tags vault`). Each run starts a Vault dev server as the system `vault`, writes the
KV version 2 secrets in [vault.yaml](vault.yaml), and creates the SecretStore and its token
Secret from [secretstore.yaml](secretstore.yaml).

- [plans/](plans) and [seeds](seeds) must give no findings.
- [controls/orphan.json](controls/orphan.json) sets `creationPolicy: Orphan`, so the Secret
  outlives its ExternalSecret. It must fail Clean.
- [rotation/](rotation/config.yaml) syncs under `refreshPolicy: Periodic` every 2 s. Its
  property `synced` says the Secret holds Vault's current value. [rotate.json](rotation/plans/rotate.json)
  writes a new KV version, and must pass. Seeds can draw `changeFixture`, which writes one too, and
  seed 2 does. [controls/onchange.json](rotation/controls/onchange.json)
  runs the same plan under `OnChange`, which never reads Vault again, and must fail `synced`.

## The OnChange bug

Seeds 24 and 921 drew plans that left an `OnChange` ExternalSecret `Ready=False` under the
fake provider. With the `vault` system, the generator draws other plans for those seeds, and
they gave no findings in 3 runs each. Their old plans, moved to Vault, still fail Converge in
3 of 3 runs each. It is a real bug:
[docs/findings/external-secrets-onchange-stale-failure.md](../../docs/findings/external-secrets-onchange-stale-failure.md).
[bugs/onchange-stale-failure.json](bugs/onchange-stale-failure.json) is the shortest plan,
and `make repro-external-secrets` runs it.
