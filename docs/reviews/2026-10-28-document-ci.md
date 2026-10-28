# Document commands, query plans and CI verification

The original CI run on `ea0eb2f` failed while preparing PostgreSQL. Its integration, SQL and vulnerability steps did not run. The migration checksum investigation is recorded in [the migration review](2026-10-28-migration-checksum.md); published migrations and SHA pins remain unchanged.

Document create/transition commands could return HTTP 500 when a transport wrapper failed while writing COMMIT. They now return HTTP 503 with instructions to retry the same command identifier. A definitive PostgreSQL constraint rejection at COMMIT still returns its conflict response. Real socket fault injection checks both commands before COMMIT is sent and after its acknowledgement is received and discarded. Retry checks inspect the actual committed operation, document, lines, audit and outbox effects.

Document reads also had excessive work under generic prepared plans with several permitted owners. The query now selects ordered, bounded ID candidates per owner, materializes the final page, and only then builds document line JSON. A separate critic proved that the initial LATERAL implementation hydrated 672 documents for 21 candidates when 32 owners each had 200-line documents. The final query hydrates exactly 21 in both custom and generic plans.

## Evidence and execution

- Tests were recorded separately in `a6ffe19`, `01b1edf` and `47dcb78`, before the implementation commit. Actual earlier red logs, source/test hashes and subsequent green results are retained in `docs/verification/`.
- Real PostgreSQL 17.11, Go 1.27.3, `go test -race -tags=integration ./... -count=1 -timeout=600s -json`: 305 passing test events including subtests, zero failures or skips. All 41 existing SQL kernel scenarios pass.
- Property based tests generate 64 document state/replay cases, 40 invalid-line rollback cases and 64 scoped pagination cases. Fixed seeds make failures reproducible. Properties which already passed the original implementation are explicitly characterization coverage rather than invented red evidence.
- Read plans use 10,000 and 100,000 documents, up to 300,000 lines, two organizations and four warehouses. All 28 actual custom/generic plans pass; the recorded final ordinary fixture has at most 450 units of row work and 260 shared blocks. The independent high-fanout fixture also checks all 200 returned lines and the sorted page against a separate SQL oracle.
- Twenty focused, non-equivalent SQL, Go and Python mutants are killed; zero survive and zero runs are invalid. This is a bounded campaign over document recovery, identity, permissions, transitions, atomic effects, pagination, hydration and migration integrity, not a claim about every line in the repository.
- Three separate critics checked other agents' work. Their tests found real migration, evidence-gate and query-hydration defects. Direct runtime SQL and concurrent command tests also kill removal of actor and owner checks. Independent findings and follow-up results are preserved.

The CI workflow runs all Python verification tests, migration and historical Windows receipt checks, formatting/static analysis, complete race/integration tests, SQL checks, the mutation gate and the vulnerability scan. New critical document regressions are mandatory in the summary; omitting any one fails its own regression test. Mutation timeouts, build/import failures, skips and missing tests cannot count as kills. Mutation evidence is isolated from unmutated query plans. The final GitHub Actions run on the implementation revision is the authoritative remote result; local reports do not replace it.

## Reproduction

CI provisions its own PostgreSQL service. For an already prepared disposable loopback test database, set the existing `WMS_TEST_ADMIN_DSN` and `WMS_TEST_RUNTIME_DSN` variables, then run:

```sh
python3 -m unittest discover -s scripts -p 'test_*.py'
go test -race -tags=integration ./... -count=1 -timeout=600s
python3 scripts/mutation_runner.py --go go --psql psql --report work/document-mutations.json
```

The Windows `scripts/test-postgres.ps1` harness provisions and stops a fresh isolated cluster for the complete Python, migration, Go and SQL regression run. Mutation testing also requires a passing unmutated baseline and uses Go overlays or cloned disposable databases; it never edits published SQL or the working tree.

These are bounded integration and planner checks. They do not establish production throughput, HA, external IdP availability, or durability under power loss.
