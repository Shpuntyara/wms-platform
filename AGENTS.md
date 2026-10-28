# Verification requirements

- Write and run a behavior test before changing implementation. Preserve the actual failing output, test/source hashes and the later passing run. A test which already passes is characterization, not red TDD evidence.
- Exercise new document commands through real PostgreSQL, including unknown COMMIT outcomes, replay with the same command ID, rollback, permissions and concurrent execution.
- Keep property based and mutation checks executable in CI. A timeout, build error, skipped test or missing test is not a killed mutant. Investigate every survivor; retain valid adversarial cases.
- Never change a published migration or its pinned checksum merely to make checks green. Prove historical byte representation and validate source and execution receipts without rewriting history.
- Check document reads with large fixtures and actual custom/generic EXPLAIN ANALYZE plans; verify pagination against an independent result set.
- Run the full CI on the final revision and inspect every required step. Do not claim the pipeline passed when a required step failed or was skipped.
