# PostgreSQL process-crash and restore verification

`scripts/crash_restore.py` runs a bounded recovery experiment against two PostgreSQL
clusters that it creates and owns. All data is synthetic. The source repository
can be read only; PostgreSQL data, witnesses, logs and the backup live beneath a
separate writable `--work-root`.

The retained native result is
[`verification/resilience/windows-final.json`](verification/resilience/windows-final.json).
Its `source_files` records both the exact byte SHA256 and the canonical LF SHA256
of the harness, migrator, relevant tests, migrations and bootstrap SQL. The
experiment rejects changes to these inputs while it runs. A passing local
experiment does not establish that the final GitHub Actions pipeline passed;
the pipeline must separately run and inspect every required job.

The final Windows/PostgreSQL 17.11 run completed three 10-second workloads, 309
concurrent reads, and 401 normal units plus three canaries: 3,630 final command,
audit and outbox receipts. The total includes commands first confirmed by replay;
each cycle's already acknowledged results were checked before that replay.

| Cycle | Witnessed units checked before replay | Recovery to owned readiness | Read requests |
| --- | ---: | ---: | ---: |
| 1 | 126 | 1.813 s | 99 |
| 2 | 257 | 1.656 s | 102 |
| 3 | 399 | 1.578 s | 108 |

The separate restore verified a 1,305,475-byte archive; backup, restore and
verification took 39.750 seconds altogether. Source and restored offline
`pg_checksums --check` both found zero bad checksums. Both owned clusters stopped.
These are observations from this machine and workload, not deployment targets.

## What is exercised

Each normal unit calls the real document, movement and reservation commands:
create/activate/post/close a receipt, then create/activate/reserve/post/close a
shipment. Nine command IDs form one database transaction. The workload uses two
concurrent writers and a concurrent reader of the published document contract.

Before each crash the harness runs `CHECKPOINT`, then commits a new canary:
receipt quantity 10, active shipment reserve 3. Its original IDs and results are
fsynced to a separate witness. After every restart and after restore, **before
any replay**, it checks those original receipts and the live balance,
reservation, document and line identities, state, version and quantities. The
three final canaries retain stock 30 and active reserve 9.

For normal work the candidate command IDs are fsynced before submission. An
acknowledgement witness is appended, flushed and fsynced only after a successful
client return and parsing its complete result. The snapshot at kill is protected
by the same lock as the witness writer. All writers then join, and the stable
witness, including acknowledgements recorded after the kill, is verified before
replay can conceal a missing operation.

Two held clients provide separate probes in every cycle:

* A transaction whose commands are visible as committed to an independent
  connection, while the application coordinator has not consumed or witnessed
  the client result. It must survive and replay with the same IDs.
* An open transaction whose writes are still uncommitted at the crash. Its
  operations and documents must be absent after recovery and before replay.

The committed-without-application-witness probe is **not a literal TCP COMMIT
acknowledgement drop**. The separate Go integration tests cover the application
transport-error handling. This harness exercises the SQL kernel rather than the
HTTP/OIDC request path.

## Actual crash and safety boundary

The harness launches `postgres` directly using an owned `Popen` handle. It checks
that its `postmaster.pid` contains that process ID and the owned PGDATA path;
PGDATA must resolve below the disposable work root. Directory labels cannot
escape that root. Readiness also compares `SHOW data_directory` with owned
PGDATA before schema creation. Both clusters bind only to loopback on ephemeral
ports, with Unix sockets disabled, and have `fsync`, `synchronous_commit`,
`full_page_writes` and data checksums enabled.

Windows uses `taskkill /F /T` on the owned process tree; Linux sends `SIGKILL`
to its own process group after verifying that group identity. A normal or
unobserved process exit cannot pass. A new server process must start, and the
**current restart's** log range must contain unclean shutdown, WAL redo and
readiness evidence. Earlier-cycle recovery text cannot satisfy this check.

Windows tree-tool status is retained independently of the observed postmaster
exit. The tool can return a nonzero status while another child is already
terminating; the report does not hide that status. Abnormal owned postmaster
exit, successful restart, new WAL redo and all database invariants remain
mandatory. Graceful `pg_ctl -m fast` shutdown is used only during final cleanup.
Cleanup attempts both clusters even when one fails, and a cleanup failure marks
the evidence failed.

## Backup and isolated restore

After the crash cycles the harness creates and fsyncs a real custom-format
`pg_dump` archive. Its SHA256 and size enter the report. A deliberately changed
archive must fail the integrity gate **before the target cluster is initialized
or `pg_restore` starts**.

The verified archive restores into the second isolated PGDATA and database.
Original acknowledged results, live nonzero canaries, migration receipt history,
exact audit/outbox/effect counts and the published document read contract must
match. Replaying every original ID after restore must preserve the same results
and effect counts. Independent reconciliation compares the movement ledger to
balances, active reservations to balances, and reservations to document lines.
No application relay or consumer starts during the test.

## Running it

Python 3.9+ and matching PostgreSQL 17 tools are required. Do not run as root on
Linux; `initdb` must run as a normal user that can write the evidence directory.
The database superuser, roles, trust authentication and schema fixtures exist
only in the newly initialized disposable clusters. No existing DSN or PostgreSQL
environment variables are used.

```powershell
python -m unittest discover -s scripts -p test_crash_restore.py
python -m unittest discover -s scripts -p test_resilience_assurance_critic.py
python scripts/crash_restore.py --pg-bin 'C:\Program Files\PostgreSQL\17\bin' --cycles 3 --seconds 10 --report work/crash-restore-final-windows.json
```

The `resilience` CI job uses a separate disposable image based on
`postgres:17.11`, with Python installed and `USER postgres`. Its checkout is a
read-only `/repo` mount; `/evidence` is writable. This job kills only processes
it spawns, never the PostgreSQL service used by another suite.

```sh
python3 /repo/scripts/crash_restore.py --repo /repo --work-root /evidence \
  --pg-bin /usr/lib/postgresql/17/bin --report /evidence/final.json \
  --cycles 3 --seconds 10 --writers 2
```

Retain `final.json`, the per-run report, acknowledgement/canary/candidate JSONL,
per-cycle WAL logs, server/client logs and `synthetic-wms.dump` as CI artifacts.
Do not publish PGDATA. Native selected logs, unit-test receipts and artifact
digests are under `docs/verification/resilience`; the binary backup and complete
witnesses stay in the local report's `work_directory` and in CI artifacts.

The collector runs as the cluster's container user, prunes `pgdata` before traversal,
copies only the selected synthetic evidence to `published/`, and records a digest
manifest. The upload action scans only that directory. This preserves PGDATA's
private permissions while making the selected files readable to the CI runner.

## Test provenance and limits

[`assurance-critic.json`](verification/resilience/assurance-critic.json) records
the independent critic's original RED source/test hashes, two ownership defects,
the later GREEN run, and additional false-assurance regression checks. These
include rejecting late acknowledgement loss before replay, rejecting old-cycle
WAL text, and detecting a rollback of live nonzero state even when operation
receipts remain intact. The final source-manifest/socket-directory iteration
also retains RED and GREEN hashes. New helper exports initially failed as
missing scaffolding; these are not PostgreSQL or WMS product defects.

Earlier author empty-witness and canary-identity RED outputs are retained, but
their contemporaneous source/test hashes were not captured. They are supporting
logs, not a complete hashed RED-to-GREEN receipt. No retrospective hashes are
presented as if captured at that earlier point.

This is process termination on local storage and logical backup restore with a
bounded synthetic workload. It does not demonstrate hardware power-loss safety,
honest disk write caches, disk failure recovery, PITR, replication, failover, HA,
credential compromise isolation, or a production throughput/RPO/RTO promise.
Timing includes local process startup, SQL verification and replay overhead;
the report gives the measured scope rather than extrapolating an SLA.

PostgreSQL documents the distinction between
[forced process termination](https://www.postgresql.org/docs/17/server-shutdown.html),
[storage reliability requirements](https://www.postgresql.org/docs/17/wal-reliability.html)
and [logical dump/restore](https://www.postgresql.org/docs/17/backup-dump.html).
