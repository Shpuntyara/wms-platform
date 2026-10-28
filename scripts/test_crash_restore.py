"""Evidence/ownership gates precede the destructive disposable-cluster harness."""
import hashlib
from pathlib import Path
import tempfile
import unittest
from unittest.mock import patch
from types import SimpleNamespace

from crash_restore import (
    validate_kill_target, validate_durability_settings, append_witness,
    read_witness, verify_backup, verify_acknowledged,
    verify_forced_process_exit,
    verify_canary_state,
    cleanup_clusters,
    Cluster,
)


class CrashRestoreGateTests(unittest.TestCase):
    def test_source_manifest_records_raw_and_lf_bytes_and_refuses_drift(self):
        from crash_restore import source_manifest, verify_source_manifest
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            paths = ('scripts/crash_restore.py', 'scripts/migrate_core.py',
                     'scripts/test_crash_restore.py', 'scripts/test_resilience_assurance_critic.py',
                     'db/bootstrap/grants.psql', 'db/migrations/0001_example.sql')
            for name in paths:
                path = root / name
                path.parent.mkdir(parents=True, exist_ok=True)
                path.write_bytes(b'synthetic\r\nsource\r\n')
            before = source_manifest(root)
            self.assertEqual(set(before), set(paths))
            self.assertEqual(before[paths[0]], {
                'sha256_bytes': hashlib.sha256(b'synthetic\r\nsource\r\n').hexdigest(),
                'sha256_lf': hashlib.sha256(b'synthetic\nsource\n').hexdigest(),
            })
            verify_source_manifest(before, source_manifest(root))
            (root / paths[-1]).write_bytes(b'changed migration\n')
            with self.assertRaisesRegex(AssertionError, 'source|Source'):
                verify_source_manifest(before, source_manifest(root))
            with self.assertRaisesRegex(AssertionError, 'source|Source'):
                verify_source_manifest({}, {})

    def test_start_disables_default_unix_socket_directory_for_nonroot_ci(self):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory).resolve()
            cluster = Cluster(root / 'tools', root, 'source')
            cluster.data.mkdir()
            (cluster.data / 'postmaster.pid').write_text('43210\n' + str(cluster.data) + '\n')
            cluster.executable = lambda _name: 'unexecuted-postgres-test-double'
            cluster.query = lambda sql, **_kwargs: str(cluster.data) if 'data_directory' in sql else '1'
            try:
                with patch('crash_restore.subprocess.Popen', return_value=SimpleNamespace(pid=43210, poll=lambda: None)) as start:
                    cluster.start()
                command = start.call_args.args[0]
                self.assertIn('unix_socket_directories=', command)
            finally:
                if cluster.log_handle is not None:
                    cluster.log_handle.close()

    def test_cleanup_failure_does_not_skip_the_second_owned_cluster(self):
        calls = []
        def broken():
            calls.append('source')
            raise RuntimeError('synthetic stop failure')
        source = SimpleNamespace(directory=Path('source'), stop=broken)
        target = SimpleNamespace(directory=Path('restored'), stop=lambda: calls.append('restored'))
        errors = cleanup_clusters((source, target))
        self.assertEqual(calls, ['source', 'restored'])
        self.assertEqual(errors, {'source': 'RuntimeError'})

    def test_nonzero_stock_and_active_reservation_must_survive(self):
        expected = {'one': {'request': {'balance': 'balance'}, 'result': {'shipment_create': {'document_id': 'document', 'lines': [{'id': 'line'}]}}}}
        state = {'on_hand': '10.000000', 'reserved_qty': '3.000000',
                 'active_qty': '3.000000', 'line_reserved': '3.000000',
                 'fulfilled_qty': '0.000000', 'requested_qty': '10.000000',
                 'state': 'active', 'version': 3, 'balance_id': 'balance',
                 'document_id': 'document', 'document_line_id': 'line'}
        verify_canary_state(expected, {'one': state})
        for observed in ({}, {'one': dict(state, on_hand='0.000000')},
                         {'one': dict(state, active_qty='0.000000')},
                         {'one': dict(state, line_reserved='0.000000')},
                         {'one': dict(state, balance_id='another-balance')},
                         {'one': dict(state, state='closed')}):
            with self.subTest(observed=observed):
                with self.assertRaises(AssertionError):
                    verify_canary_state(expected, observed)

    def test_a_normal_or_unobserved_exit_is_not_a_crash(self):
        verify_forced_process_exit(1)
        verify_forced_process_exit(-9)
        for code in (0, None):
            with self.subTest(code=code):
                with self.assertRaises(ValueError):
                    verify_forced_process_exit(code)

    def test_only_the_spawned_cluster_and_its_own_pgdata_can_be_killed(self):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory).resolve()
            data = root / 'source/pgdata'
            data.mkdir(parents=True)
            contents = '43210\n' + str(data) + '\n'
            self.assertEqual(validate_kill_target(root, data, 43210, contents), 43210)
            for target, pid, content in (
                (root.parent, 43210, contents),
                (root, 43210, contents),
                (data, 43211, contents),
                (data, 43210, '43210\n' + str(root / 'other') + '\n'),
                (data, 1, '1\n' + str(data) + '\n'),
            ):
                with self.subTest(target=target, pid=pid):
                    with self.assertRaises(ValueError):
                        validate_kill_target(root, target, pid, content)

    def test_relaxed_persistence_is_not_accepted_as_durability_evidence(self):
        settings = {'fsync': 'on', 'synchronous_commit': 'on', 'full_page_writes': 'on', 'data_checksums': 'on'}
        validate_durability_settings(settings)
        for key in settings:
            with self.subTest(key=key):
                changed = dict(settings, **{key: 'off'})
                with self.assertRaises(ValueError):
                    validate_durability_settings(changed)

    def test_acknowledgement_witness_is_fsynced_and_rejects_conflicting_results(self):
        with tempfile.TemporaryDirectory() as directory:
            path = Path(directory) / 'witness.jsonl'
            record = {'unit_id': 'one', 'request': {'command': 'one'}, 'result': {'value': 1}}
            with patch('crash_restore.os.fsync') as sync:
                append_witness(path, record)
            self.assertEqual(sync.call_count, 1)
            self.assertEqual(read_witness(path), {'one': record})
            append_witness(path, dict(record, result={'value': 2}))
            with self.assertRaisesRegex(ValueError, 'Conflicting'):
                read_witness(path)

    def test_missing_or_changed_acknowledged_operations_fail(self):
        expected = {'one': {'result': {'quantity': '1.000000'}}}
        verify_acknowledged(expected, {'one': {'quantity': '1.000000'}})
        for observed in ({}, {'one': {'quantity': '2.000000'}}):
            with self.subTest(observed=observed):
                with self.assertRaises(AssertionError):
                    verify_acknowledged(expected, observed)

    def test_an_empty_witness_is_not_recovery_evidence(self):
        with self.assertRaisesRegex(ValueError, 'empty|Empty'):
            verify_acknowledged({}, {})

    def test_corrupt_backup_is_refused_before_a_restore_can_start(self):
        with tempfile.TemporaryDirectory() as directory:
            path = Path(directory) / 'backup.dump'
            path.write_bytes(b'PGDMP synthetic archive bytes')
            digest = hashlib.sha256(path.read_bytes()).hexdigest()
            self.assertEqual(verify_backup(path, digest), digest)
            path.write_bytes(path.read_bytes() + b'corruption')
            with self.assertRaisesRegex(ValueError, 'checksum'):
                verify_backup(path, digest)


if __name__ == '__main__':
    unittest.main()
