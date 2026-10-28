"""Independent ownership and evidence checks for the crash/restore lab."""
from pathlib import Path
import json
import os
import signal
from types import SimpleNamespace
import tempfile
import threading
import unittest
from unittest.mock import Mock, patch

from crash_restore import Cluster, STAGES, canary_checks, crash_cycle


class ResilienceAssuranceCriticTests(unittest.TestCase):
    def test_saved_command_results_do_not_hide_reverted_nonzero_canary_state(self):
        """Even intact operation receipts cannot substitute for live stock state."""
        records = {}
        operations = {}
        for unit in ('first', 'latest'):
            commands = [unit + '-' + stage for stage in STAGES[:7]]
            result = {stage: {'original': stage} for stage in STAGES[:7]}
            result['reservation'] = {'reservation_id': unit + '-reservation'}
            result['shipment_create'] = {'document_id': unit + '-document', 'lines': [{'id': unit + '-line'}]}
            records[unit] = {'request': {'commands': commands, 'balance': 'canary-balance'}, 'result': result}
            operations.update({command: result[stage] for command, stage in zip(commands, STAGES)})
        state = {'on_hand': '20.000000', 'reserved_qty': '6.000000',
                 'active_qty': '3.000000', 'line_reserved': '3.000000',
                 'fulfilled_qty': '0.000000', 'requested_qty': '10.000000',
                 'state': 'active', 'version': 3}
        def observed_state(sql, *_args):
            unit = 'first' if 'first-reservation' in sql else 'latest'
            return json.dumps(dict(state, balance_id='canary-balance', document_id=unit + '-document',
                                   document_line_id=unit + '-line'))

        cluster = SimpleNamespace(json=Mock(return_value=operations), query=Mock(side_effect=observed_state))
        self.assertEqual(canary_checks(cluster, records)['acknowledged_nonzero_canaries'], 2)
        state.update(on_hand='10.000000', reserved_qty='3.000000')
        with self.assertRaisesRegex(AssertionError, 'nonzero stock/reservation'):
            canary_checks(cluster, records)

    def test_an_acknowledgement_arriving_after_kill_is_checked_before_replay(self):
        """A successful late response must not be hidden by a repairing replay."""
        killed = threading.Event()
        request_lock = threading.Lock()
        requests = []
        inspected = []
        replayed = []

        def request(_fixture, probe=False, canary=False):
            with request_lock:
                # The first request is a persistent canary, followed by two probes;
                # the first concurrent writer returns its result after kill.
                unit = 'late' if len(requests) == 3 else 'unit-' + str(len(requests))
                value = {'unit_id': unit, 'commands': [unit + '-command']}
                requests.append(value)
                return value

        class SyntheticCluster:
            process = SimpleNamespace(pid=43211)
            restarted = False

            def query(self, _sql, *_args, **_kwargs):
                return '0'

            def background(self, _sql, _application):
                return SimpleNamespace(wait=Mock())

            def json(self, sql, *_args, **_kwargs):
                if sql.startswith('SELECT jsonb_build_object'):
                    return {'incomplete': 0, 'documents': []}
                if self.restarted:
                    replayed.append(sql)
                if sql == 'late':
                    if not killed.wait(timeout=5):
                        raise AssertionError('Synthetic late writer never observed the crash')
                return {'delivered': sql}

            def kill(self):
                killed.set()
                return {'pid': 43210}

            def start(self):
                self.restarted = True
                return .001

            def recovery_log(self):
                return 'synthetic recovery, no PostgreSQL process started by this test'

        def recovered_results(_cluster, records):
            inspected.append(set(records))
            return {unit: record['result'] for unit, record in records.items() if unit != 'late'}

        with tempfile.TemporaryDirectory() as directory:
            with patch('crash_restore.unit_request', side_effect=request), \
                    patch('crash_restore.unit_sql', side_effect=lambda value: value['unit_id']), \
                    patch('crash_restore.wait_barrier'), \
                    patch('crash_restore.operation_results', side_effect=recovered_results), \
                    patch('crash_restore.canary_checks', return_value={}), \
                    patch('crash_restore.business_checks', return_value={}):
                with self.assertRaisesRegex(AssertionError, 'Acknowledged.*late'):
                    crash_cycle(SyntheticCluster(), {}, Path(directory), 1, 0, 2)
        self.assertTrue(killed.is_set())
        self.assertIn('late', inspected[0], 'All joined writers belong in the pre-replay witness')
        self.assertEqual(replayed, [], 'Replay must not repair a lost acknowledged result before verification')

    def test_cluster_directory_cannot_escape_the_owned_work_root(self):
        with tempfile.TemporaryDirectory() as directory:
            outer = Path(directory).resolve()
            root = outer / 'owned'
            for label in ('../escaped', str(outer / 'absolute-target'), '../owned-sibling/target'):
                with self.subTest(label=label):
                    with self.assertRaisesRegex(ValueError, 'owned|outside|directory|label'):
                        Cluster(outer / 'tools', root, label)
            self.assertFalse((outer / 'escaped').exists(), 'Rejected labels must not create an external directory')
            self.assertFalse((outer / 'absolute-target').exists(), 'Rejected labels must not create an external directory')

    def test_readiness_from_a_different_pgdata_is_not_the_owned_server(self):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory).resolve()
            cluster = Cluster(root / 'tools', root, 'source')
            cluster.executable = lambda _name: 'unexecuted-postgres-test-double'
            process = SimpleNamespace(pid=43210, poll=lambda: None)

            def query(sql, **_kwargs):
                if 'data_directory' in sql:
                    return str(root / 'foreign-pgdata')
                return '1'

            cluster.query = query
            try:
                with patch('crash_restore.subprocess.Popen', return_value=process):
                    with self.assertRaisesRegex(ValueError, 'owned|PGDATA|data.directory'):
                        cluster.start()
            finally:
                if cluster.log_handle is not None:
                    cluster.log_handle.close()
                    cluster.log_handle = None

    def test_a_previous_cycle_redo_log_cannot_prove_current_recovery(self):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory).resolve()
            cluster = Cluster(root / 'tools', root, 'source')
            old = ('database system was interrupted\nredo starts at 0/100\n'
                   'database system is ready to accept connections\n')
            cluster.log.write_text(old + 'database system is ready to accept connections\n', encoding='utf8')
            cluster.log_offset = len(old.encode())
            with self.assertRaisesRegex(AssertionError, 'unclean|WAL'):
                cluster.recovery_log()
            cluster.log.write_text(old + 'database system was not properly shut down\nredo starts at 0/200\n'
                                   'database system is ready to accept connections\n', encoding='utf8')
            self.assertIn('redo starts at 0/200', cluster.recovery_log())

    def test_terminated_spawn_handle_is_never_reused_for_a_kill(self):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory).resolve()
            cluster = Cluster(root / 'tools', root, 'source')
            cluster.process = SimpleNamespace(pid=43210, poll=lambda: 0)
            with patch('crash_restore.subprocess.run') as external:
                with self.assertRaisesRegex(ValueError, 'not running'):
                    cluster.kill()
            external.assert_not_called()

    def test_the_forced_kill_primitive_is_not_a_graceful_shutdown(self):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory).resolve()
            cluster = Cluster(root / 'tools', root, 'source')
            cluster.data.mkdir()
            (cluster.data / 'postmaster.pid').write_text('43210\n' + str(cluster.data) + '\n')
            process = SimpleNamespace(pid=43210, poll=lambda: None, wait=Mock(), returncode=1)
            cluster.process = process
            if os.name == 'nt':
                with patch('crash_restore.subprocess.run', return_value=SimpleNamespace(returncode=0, stdout=b'', stderr=b'')) as external:
                    evidence = cluster.kill()
                command = external.call_args.args[0]
                self.assertEqual(command, ['taskkill.exe', '/F', '/T', '/PID', '43210'])
                self.assertIn('TerminateProcess', evidence['method'])
            else:
                with patch('crash_restore.os.getpgid', return_value=43210), patch('crash_restore.os.killpg') as kill:
                    evidence = cluster.kill()
                kill.assert_called_once_with(43210, signal.SIGKILL)
                self.assertIn('SIGKILL', evidence['method'])
            process.wait.assert_called_once()


if __name__ == '__main__':
    unittest.main()
