# WMS core adversarial audit

The audit starts at `80f7d66`. Three critics independently examined inventory/accounting, authorization/modules, and background recovery. A fourth critic challenged the resulting fixes and the expanded mutation gate. Their function maps, actual failing runs, characterization runs, source/test hashes, mutation results and stated limitations are retained under `docs/verification/core-audit/`.

## Confirmed behavior defects fixed through TDD

| Trigger | Before | After |
|---|---|---|
| A caller changes a Manifest slice or policy bytes after registry construction | The registry's supposedly immutable accepted contract changes with the caller's memory | The registry retains a canonical immutable projection |
| Reading an outbox claim returns a partial batch plus an error | The relay publishes and acknowledges unconfirmed work | The partial batch is discarded; backoff and a later confirmed claim still work |
| A valid JSON value is followed by enough whitespace or a second value to exceed 1 MiB | HTTP 400 hides the exceeded body limit | HTTP 413; no backend command is called; exactly 1 MiB remains accepted |
| An owner/item/position filter contains malformed URL encoding | `URL.Query` silently drops malformed values and the backend receives a broader read | Strict parsing returns HTTP 400 before the backend is called |

The fourth defect was found by the independent cross-critic after the first three fixes. It is a read-validation defect, not an authorization bypass. Published SQL migrations and their SHA pins are unchanged.

Tests are preserved in separate commits `8a9d675` and `c4f26c0`, preceding the implementation commit. Red runs were actually executed before each corresponding fix. Passing characterization is identified separately and is not claimed as red TDD.

## Coverage and mutation sensitivity

| Area | Adversarial checks | New mutations |
|---|---|---:|
| Inventory and warehouse SQL | Version/state/kind, reserve/release/picking/shipment, corrections, expiry, last-stock races, command identity, immutable history and item accounting; three independent reconciliations after each command | 19 |
| Authorization and modules | Immutable manifests, exact grant scopes, guard bindings and SDK compatibility, tenant/revision/unique facts, AND/OR alternatives, strict types and exact decimals | 16 |
| Background and recovery | Partial claim errors, cancellation/draining, failed acknowledgements, sender signatures/key isolation, concurrent leases and fencing, retries/dead letters, expiry rollback and batch bounds | 21 |
| HTTP, identity and numeric boundary | RSA/ES256, issuer/audience/expiry/signature/access-marker checks, near-future nbf, cancelled startup context, malformed query filters, request limits/deadlines, canonical decimal and UUID properties | 17 |
| Retained document/migration campaign | COMMIT recovery, document command identity/state/atomicity, pagination/hydration, published checksums and historical receipt pairs | 20 |
| Total selected, distinct faults | A scoped mutation campaign, not a whole-repository mutation score | 93 |

New property checks generate 32 complete inventory workflows, 128 grant-scope cases, 256 canonical decimal cases, 256 identifier cases and 64 changed-signature cases. Grant tests additionally enumerate 256 combinations. Existing document and pagination properties remain mandatory.

An initial inventory mutant survived because the API preparation path repeated a state check. This was a genuine test gap: direct kernel commands still require an active document. A direct kernel test killed the mutant without changing correct inventory code. The inventory critic did not reproduce another accounting defect in this bounded audit; it made no speculative SQL fix.

The SQL runtime remains a trusted backend role, as defined by the existing architecture. Low-level SQL actor identifiers are server metadata. The absence of end-user grant checks in those low-level functions was not relabeled as an unproven security bug or used to change that trust model.

## Verification and evidence gate

The runner now executes each core test package against an unmutated baseline, fetches current SQL function definitions for isolated mutation, and retains the original twenty cases. Go mutations use overlays; SQL mutations use fresh disposable database clones. It records source and mutant hashes. Every domain must retain at least eight distinct configured faults; renamed copies of an identical fault are rejected. Unknown `--case` names fail instead of silently shortening a selected campaign. Build/import errors, timeouts, skips and missing tests cannot count as killed mutants.

The local complete pilot killed all 92 cases then present. The subsequently found malformed-filter defect has its own passing mutation check. The full, unfiltered 93-case campaign runs in the final GitHub CI. These local pilot artifacts are labeled as such and do not substitute for that final revision's CI result.

All newly added core regressions are required by the complete Go/SQL summary. Unit tests prove that omitting each required regression fails the report. The workflow also runs all Python tests, migration and historical Windows-ledger checks, formatting, `go vet`, `go fix`, race/integration tests, the 41 SQL kernel scenarios, mutation checks, vulnerability scanning and evidence upload.

The authoritative final remote result and its artifacts are the GitHub Actions run on the implementation revision. Reports are available as `work/verification.json` and `work/kernel-mutations.json` in its `security-test-evidence` artifact. Full runs must have no skipped test events or required steps.

This audit does not prove that every possible defect has been eliminated. It does not validate production throughput, real IdP availability, HA, or power-loss durability. Source inspection, exercised behavior, generated cases and mutation sensitivity are distinguished in the individual critics' maps.
