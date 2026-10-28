# Independent on-premise security/reliability critic

One customer has one dedicated installation, database and server. This review
does not assume a SaaS tenant router or a separate runtime login per warehouse.
The critic used synthetic data in owned loopback PostgreSQL 17.11 clusters only.

## Finding reproduced and fixed: false positive readiness after configuration drift

After constructing the real `postgres.HardenedStore` and HTTP API, the fixture
owner changed either the installation UUID or the request signing key in the
disposable database. An authenticated request to the actor's own document then
failed, while `HardenedStore.Ping` returned nil and `/readyz` returned 200.
This can hide a broken API following an administrator's key rotation or an
installation configuration change. This finding concerns availability signals;
the document request itself failed closed.

The critic wrote and executed the behavior test before the author changed the
implementation. Both cases failed on the readiness assertions, with successful
baseline and restored document requests. No build error, skipped test, timeout,
or fixture failure is counted as red evidence.

- Actual RED: `security-critic-red.txt` and `security-critic-red.json`, exit 1.
- Unchanged-test GREEN: `security-critic-green.txt` and
  `security-critic-green.json`, exit 0.
- The exact initial test source is archived as
  `security-critic-readiness-test.go.txt`; SHA-256
  `d46d79547ad9bc0d08ef0b913774c7dcd2cad758bb6e4033d4ff2f4c54ad5ab9`
  matches both receipts.
- RED `hardened.go` SHA-256:
  `596e12562c2d08294110167caed4e861ddf9e3c25603af4bf6be3815b87ac93e`.
- GREEN `hardened.go` SHA-256:
  `d9cc42075a3879fb7d9cbb2cec67a242374728478b341dcee831883a8def56e2`.

The author changed `Ping` to compare the live installation/login with the
store's cached identity and to execute the constructor's signed, empty resolve
probe. The runtime does not read the private signing key. The unchanged critic
test proves that both changes now produce readiness failure and HTTP 503, and
that restoring the original configuration heals the same API/store instance.

## Additional characterization, not red TDD evidence

`TestHardenedOnPremCriticCrossOwnerAndStaleReadGrants` independently creates two
stock owners in the same organization and warehouse, two documents and an actor
with document access only to the first owner. It proves:

- HTTP, Get and List do not expose the second owner's document.
- Forged in-memory `AllOwners`/`OwnerIDs` values cannot replace SQL grants.
- An existing actor with no grants cannot become authorized through a forged
  scope.
- Revoking `document.read` prevents an already resolved, cached read scope from
  reading documents, even while an unrelated `stock.read` grant remains.
- Restoring the document grant restores the cached scope's legitimate read.

These assertions passed immediately and required no production change. The
combined API critic run is in `security-critic-characterization.txt` and
`security-critic-characterization.json`: exit 0, race detector enabled, no skips,
and unchanged executable sources during the run. Its test source is preserved in
`security-critic-characterization-test.go.txt`, with SHA-256
`1291fa9a8312ad22b6304b0f0a50323a302d763ffd4ce3454e5097e947f4f22b`.

The source review also checked protocol-2 installation binding, action-byte
binding, actor/current-grant checks, and the restricted runtime role surface.
It found no additional demonstrated bypass in this bounded review. Existing
public integration tests already cover a revoked command permission and a
foreign warehouse; those are not represented as newly discovered failures.

## Follow-up reproduced and fixed: worker accepts a changed installation identity

`TestHardenedOnPremCriticWorkerRejectsInstallationDrift` constructs the real
worker store, seeds a valid ready event and a separate pre-existing lease in an
independent organization/warehouse, and snapshots the owned outbox and stock
rows. After the owner changes the installation UUID, the original implementation
continues claiming events, accepts an expiry pass, acknowledges the old valid
lease, and returns queue monitoring successfully. The outbox snapshot changes.
The worker had cached the UUID but did not enforce it for these operations.

Actual RED is preserved in `security-critic-worker-red.txt` and
`security-critic-worker-red.json` (exit 1, PostgreSQL 17.11, race detector,
unchanged sources during the run). This is a behavior failure, not a setup or
compilation failure. The minimal author fix checks the live profile's type,
login and installation UUID against the cached identity inside the same durable
transaction before each worker RPC. Queue monitoring uses that same guard.
It adds no SQL migration or runtime signing-key access.

The unchanged test then passes in `security-critic-worker-green.txt` and
`security-critic-worker-green.json` (exit 0, race detector, no skips, unchanged
sources). Claim, expiry, acknowledgement and queue monitoring all reject the
changed UUID; owned outbox and stock snapshots remain exactly unchanged; queue
monitoring recovers after the original UUID is restored in the same worker
instance. Both worker receipts record the same test file SHA-256:
`56f7a163e56e4d4aedef19f0009a3116085fe76461ea729d3b5cf3f11459286f`.
API key rotation is outside this worker guard because the worker does not use
the API signing key.

## Reproduction and scope

Run the isolated fixture from the repository root:

```powershell
python docs/verification/onprem/security-critic-run.py --phase characterization --go <go-executable> --work-root <scratch-directory>
```

The runner creates and stops its own PostgreSQL process, applies published core
migrations plus the forward on-premise profile, and runs:

```text
go test -race -tags=integration,hardened ./internal/postgres -run ^TestHardenedOnPremCritic -count=1 -v -timeout=60s
```

Both tests fail explicitly when fixture environment variables are missing. The
tests seed independent actors and business rows and do not assume identifiers
from another test. Singleton/key mutation is deliberately sequential, and every
mutation restores the original configuration. No credentials, signing keys or
request signatures are present in the evidence output. These targeted runs do
not establish a complete CI pass or hardware/power-loss durability.
