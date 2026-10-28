"""Independent deployment boundaries: failures must preserve a usable service."""
import argparse
import contextlib
import datetime
import importlib.util
import io
import json
import os
from pathlib import Path
import subprocess
import tarfile
import tempfile
import types
import unittest
from unittest.mock import patch

from test_vm_installer import archive, REV

ROOT = Path(__file__).resolve().parents[1]
spec = importlib.util.spec_from_file_location('installer_adversarial', ROOT / 'deploy/linux/wmsctl.py')
wms = importlib.util.module_from_spec(spec)
spec.loader.exec_module(wms)


class InstallerAdversarial(unittest.TestCase):
    def test_extracted_release_is_executable_but_not_writable_by_service_user(self):
        with tempfile.TemporaryDirectory() as tmp:
            base = Path(tmp)
            os.chmod(base, 0o755)
            path, digest = archive(base)
            calls = []
            original_chmod = os.chmod
            def chmod(target, mode):
                calls.append((Path(target), mode))
                return original_chmod(target, mode)
            with patch.object(wms, 'BASE', base / 'installation'), patch.object(wms.os, 'chmod', side_effect=chmod):
                release = wms.release_extract(path, digest, REV)
            # Simulate the root-owned 0700 release left by the first installer.
            # Reusing a verified directory must repair permissions too.
            os.chmod(release, 0o700)
            calls.clear()
            with patch.object(wms, 'BASE', base / 'installation'), patch.object(wms.os, 'chmod', side_effect=chmod):
                self.assertEqual(wms.release_extract(path, digest, REV), release)
            self.assertIn((release, 0o755), calls, 'Existing release mode was not reconciled')
            if os.name == 'posix':
                # Real permission check as nobody, not the root test process.
                # CI must have working sudo; a missing dependency is a failure.
                prefix = ['sudo', '-n', '-u', 'nobody', '--']
                executable = subprocess.run(prefix + ['/usr/bin/test', '-x', str(release / 'bin/wms-api')])
                writable = subprocess.run(prefix + ['/usr/bin/test', '-w', str(release / 'bin/wms-api')])
                self.assertEqual(executable.returncode, 0, 'Service UID cannot traverse/execute extracted binary')
                self.assertEqual(writable.returncode, 1, 'Service UID can alter release binary')
            else:
                # Windows cannot implement Unix mode semantics. The Linux CI
                # branch above still executes, with no skip or silent fallback.
                self.assertTrue(any(mode == 0o755 and 'bin' not in target.parts for target, mode in calls),
                                'Extracted release directory must grant service traversal')

    def test_image_pull_retries_transient_failure_then_continues_sequentially(self):
        calls = []
        def command(argv, *args, **kwargs):
            calls.append(list(argv))
            self.assertTrue(kwargs.get('sensitive'), 'Image errors may contain dynamic registry URLs')
            if len(calls) == 1:
                raise RuntimeError('temporary registry failure')
            return ''
        with patch.object(wms, 'run', side_effect=command), patch.object(wms.time, 'sleep'):
            try:
                wms.fetch_images()
            except RuntimeError:
                self.fail('A transient image pull failure must retry and recover')
        images = list(wms.IMAGES.values())
        self.assertEqual([call[-1] for call in calls], [images[0], images[0], images[1], images[2]])
        self.assertTrue(all(call[:4] == ['docker', 'pull', '--platform', 'linux/amd64'] for call in calls))

    def test_image_pull_permanent_failure_aborts_before_database_mutation(self):
        with tempfile.TemporaryDirectory() as tmp:
            base = Path(tmp)
            args = argparse.Namespace(address='192.168.50.11', action='install', archive=base / 'release.tar',
                                      sha256='0' * 64, revision=REV)
            calls = []
            def command(argv, *args, **kwargs):
                calls.append(list(argv))
                raise RuntimeError('registry unavailable')
            with patch.object(wms, 'BASE', base), patch.object(wms, 'CONFIG', base / 'config'), \
                 patch.object(wms, 'release_extract', return_value=base / 'release'), \
                 patch.object(wms, 'configuration', return_value='https://192.168.50.11/auth/realms/test'), \
                 patch.object(wms, 'compose') as compose, \
                 patch.object(wms, 'sql', side_effect=AssertionError('SQL reached after permanent image failure')) as sql, \
                 patch.object(wms, 'database_bootstrap') as bootstrap, \
                 patch.object(wms, 'run', side_effect=command), patch.object(wms.time, 'sleep'):
                with self.assertRaisesRegex(RuntimeError, '3 attempts'):
                    wms.install(args, {'instance_id': 'test'})
            self.assertEqual(len(calls), 3)
            self.assertEqual([call[-1] for call in calls], [wms.IMAGES['postgres']] * 3)
            compose.assert_called_once_with('config', '--quiet')
            sql.assert_not_called()
            bootstrap.assert_not_called()

    def test_operator_backup_failure_also_restarts_existing_writer(self):
        commands = []
        def command(argv, *args, **kwargs):
            commands.append(list(argv))
            return 'active'
        with patch.object(wms, 'run', side_effect=command), \
             patch.object(wms, 'backup', side_effect=RuntimeError('backup device full')):
            with self.assertRaisesRegex(RuntimeError, 'backup device full'):
                wms.backup_while_quiesced({'instance_id': 'test'})
        self.assertIn(['systemctl', 'start', 'wms-api'], commands)

    def test_failed_backup_before_migration_restarts_existing_writer(self):
        with tempfile.TemporaryDirectory() as tmp:
            base = Path(tmp)
            (base / 'current').mkdir()
            commands = []
            def command(argv, *args, **kwargs):
                commands.append([str(x) for x in argv])
                return 'active' if argv[:2] == ['systemctl', 'is-active'] else ''
            args = argparse.Namespace(address='192.168.50.11', action='update', archive=base / 'release.tar',
                                      sha256='0' * 64, revision=REV)
            with patch.object(wms, 'BASE', base), patch.object(wms, 'CONFIG', base / 'config'), \
                 patch.object(wms, 'release_extract', return_value=base / 'release'), \
                 patch.object(wms, 'run', side_effect=command), \
                 patch.object(wms, 'backup', side_effect=RuntimeError('disk full')):
                with self.assertRaisesRegex(RuntimeError, 'disk full'):
                    wms.install(args, {'instance_id': 'installation'})
            self.assertIn(['systemctl', 'stop', 'wms-api'], commands)
            self.assertIn(['systemctl', 'start', 'wms-api'], commands)

    def test_keycloak_and_grafana_mount_roots_are_traversable(self):
        with tempfile.TemporaryDirectory() as tmp:
            base = Path(tmp)
            config = base / 'config'
            value = wms.ensure_secrets(config / 'secrets.json')
            actual_write = wms.private_write
            def write(path, data, mode=0o600):
                if config in Path(path).parents:
                    actual_write(path, data, mode)
            with patch.object(wms, 'CONFIG', config), patch.object(wms, 'STATE', base / 'state'), \
                 patch.object(wms, 'private_write', side_effect=write), \
                 patch.object(wms, 'run', return_value='172.17.0.1'), \
                 patch.object(wms.os, 'chmod') as chmod:
                wms.configuration(value, '192.168.50.11')
            chmod.assert_any_call(config, 0o750)
            chmod.assert_any_call(config / 'keycloak', 0o700)
            for directory in ('grafana', 'grafana/datasources', 'grafana/dashboards'):
                chmod.assert_any_call(config / directory, 0o755)

    def test_database_and_oidc_secret_leaves_are_not_readable_by_caddy_group(self):
        with tempfile.TemporaryDirectory() as tmp:
            base = Path(tmp)
            config = base / 'config'
            value = wms.ensure_secrets(config / 'secrets.json')
            writes, commands = {}, []
            actual_write = wms.private_write
            def write(path, data, mode=0o600):
                writes[Path(path)] = mode
                if config in Path(path).parents:
                    actual_write(path, data, mode)
            def command(argv, *args, **kwargs):
                commands.append([str(item) for item in argv])
                return '172.17.0.1'
            with patch.object(wms, 'CONFIG', config), patch.object(wms, 'STATE', base / 'state'), \
                 patch.object(wms, 'private_write', side_effect=write), patch.object(wms, 'run', side_effect=command):
                wms.configuration(value, '192.168.50.11')
            for leaf in ('postgres-password', 'keycloak/realm.json'):
                self.assertEqual(writes[config / leaf] & 0o077, 0, leaf + ' readable by caddy group/others')
            self.assertIn(['chown', '1000:0', str(config / 'keycloak/realm.json')], commands)
            compose = (config / 'compose.yaml').read_text()
            self.assertIn('/etc/wms/keycloak/realm.json:/opt/keycloak/data/import/realm.json:ro', compose)

    def test_archive_reopen_cannot_install_unverified_changed_bytes(self):
        with tempfile.TemporaryDirectory() as tmp:
            base = Path(tmp)
            path, digest = archive(base)
            bad = io.BytesIO()
            with tarfile.open(fileobj=bad, mode='w:gz') as target:
                item = tarfile.TarInfo('unverified-file')
                item.size = 4
                target.addfile(item, io.BytesIO(b'evil'))
            actual_open = tarfile.open
            count = 0
            def reopen(*args, **kwargs):
                nonlocal count
                count += 1
                if count == 2:
                    path.write_bytes(bad.getvalue())
                return actual_open(*args, **kwargs)
            with patch.object(wms, 'BASE', base / 'installation'), patch.object(wms.tarfile, 'open', side_effect=reopen):
                try:
                    release = wms.release_extract(path, digest, REV)
                except ValueError:
                    return  # Refusal is also a valid trust-boundary outcome.
            self.assertFalse((release / 'unverified-file').exists())
            self.assertEqual((release / 'bin/wms-api').read_bytes(), b'ELF-fixture')

    def test_http_health_rejects_empty_false_and_stale_payload(self):
        now = datetime.datetime.now(datetime.timezone.utc).isoformat()
        good = {'relay_at': now, 'expiry_at': now, 'monitor_at': now,
                'queue': {'dead': 0, 'oldest_pending_seconds': 0, 'oldest_expired_seconds': 0}}
        for kind, value in [('ready', {}), ('ready', {'status': 'not_ready'}),
                            ('background', {}), ('background', dict(good, monitor_at='2000-01-01T00:00:00Z')),
                            ('background', dict(good, queue={'dead': 1}))]:
            with self.subTest(kind=kind, value=value), self.assertRaises(ValueError):
                wms.verify_health_payload(value, kind)
        self.assertEqual(wms.verify_health_payload({'status': 'ready'}, 'ready'), {'status': 'ready'})
        self.assertEqual(wms.verify_health_payload(good, 'background'), good)

    def test_monitor_route_requires_separate_viewer_auth_without_plaintext(self):
        with tempfile.TemporaryDirectory() as tmp:
            base = Path(tmp)
            config = base / 'config'
            value = wms.ensure_secrets(config / 'secrets.json')
            self.assertTrue('monitor_password' in value, 'Missing independent monitor credential')
            written = {}
            actual_write = wms.private_write
            def write(path, data, mode=0o600):
                written[str(path)] = data
                if config in Path(path).parents:
                    actual_write(path, data, mode)
            with patch.object(wms, 'CONFIG', config), patch.object(wms, 'STATE', base / 'state'), \
                 patch.object(wms, 'private_write', side_effect=write), \
                 patch.object(wms, 'run', return_value='synthetic-password-hash'):
                wms.configuration(value, '192.168.50.11')
            caddy = written['/etc/caddy/Caddyfile']
            monitor = caddy.split('handle /monitor/*')[1].split('handle /lab/events')[0]
            self.assertIn('basicauth', monitor)
            self.assertIn('wms-viewer', monitor)
            self.assertNotIn(value['monitor_password'], caddy)

    def test_prometheus_waits_for_docker_bridge_and_retries_failed_boot(self):
        with tempfile.TemporaryDirectory() as tmp:
            base = Path(tmp)
            config = base / 'config'
            value = wms.ensure_secrets(config / 'secrets.json')
            written = {}
            actual_write = wms.private_write
            def write(path, data, mode=0o600):
                written[str(path)] = data
                if config in Path(path).parents:
                    actual_write(path, data, mode)
            with patch.object(wms, 'CONFIG', config), patch.object(wms, 'STATE', base / 'state'), \
                 patch.object(wms, 'private_write', side_effect=write), \
                 patch.object(wms, 'run', return_value='synthetic-password-hash'):
                wms.configuration(value, '192.168.50.11')
            dropin = written.get('/etc/systemd/system/prometheus.service.d/wms.conf', '')
            self.assertIn('After=docker.service', dropin)
            self.assertIn('Requires=docker.service', dropin)
            self.assertIn('Restart=on-failure', dropin)
            self.assertIn('RestartSec=5s', dropin)


@contextlib.contextmanager
def backup_harness(*, fail_dump=None, fail_restore=None, corrupt_restore=False):
    """Fake process boundaries, retaining real backup files and private modes."""
    with tempfile.TemporaryDirectory() as temporary:
        directory = Path(temporary)
        config = directory / 'config'
        config.mkdir()
        (config / 'identity.json').write_text(json.dumps({'issuer': 'https://192.168.50.11/auth/realms/wms-test'}))
        events, queries, restores = [], [], []
        value = {'instance_id': 'b0e77e81-1a64-48cd-96b3-4d93a3f80947'}
        def command(argv, *args, **kwargs):
            items = [str(item) for item in argv]
            events.append(items)
            if items[:3] == ['systemctl', 'is-active', 'wms-api']:
                return 'active'
            if items[:2] == ['docker', 'inspect']:
                return 'true'
            return '-- synthetic protected globals' if 'pg_dumpall' in items else ''
        def database(query, database='wms'):
            queries.append((query, database))
            if 'SELECT id::text' in query:
                return value['instance_id']
            return '1' if 'SELECT count(*)' in query or query == 'SELECT 1;' else ''
        def process(argv, **kwargs):
            items = [str(item) for item in argv]
            if 'pg_dump' in items:
                name = items[items.index('-d') + 1]
                events.append(['dump', name])
                kwargs['stdout'].write(('private dump for ' + name).encode())
                return types.SimpleNamespace(returncode=1 if name == fail_dump else 0)
            if 'pg_restore' in items:
                name = items[items.index('-d') + 1]
                restores.append(name)
                return types.SimpleNamespace(returncode=1 if fail_restore and name.startswith(fail_restore + '_restore_') else 0)
            raise AssertionError('Unexpected subprocess boundary')
        def fingerprints(database):
            keycloak = database == 'keycloak' or database.startswith('keycloak_restore_')
            digest = 'a' * 64
            if corrupt_restore and database.startswith('keycloak_restore_'):
                digest = 'b' * 64
            return {('public.component_config' if keycloak else 'platform.operations'): {'rows': 4, 'sha256': digest}}
        with contextlib.ExitStack() as stack:
            for name, replacement in [('CONFIG', config), ('STATE', directory / 'state'),
                    ('run', command), ('sql', database), ('database_fingerprints', fingerprints),
                    ('keycloak_identity', lambda _database, _value: {'realm': True, 'client': True, 'private_signing_key': True}),
                    ('http_json', lambda *_args, **_kwargs: {'issuer': 'https://192.168.50.11/auth/realms/wms-test'}),
                    ('wait_for', lambda function, *_args, **_kwargs: function())]:
                stack.enter_context(patch.object(wms, name, replacement, create=True))
            stack.enter_context(patch.object(wms.subprocess, 'run', side_effect=process))
            yield value, events, queries, restores, directory


class InstallationBackupContract(unittest.TestCase):
    def test_recovery_globals_and_private_identity_artifact_tampering_is_rejected(self):
        with backup_harness() as (value, _events, _queries, _restores, _directory):
            receipt = wms.backup(value)
            settings = dict(wms.DURABILITY, synchronous_commit='on')
            wms.validate_update_preflight(settings, receipt, value['instance_id'])
            for filename in ('globals.sql', 'identity.tar.gz'):
                artifact = Path(receipt['path']).parent / filename
                original = artifact.read_bytes()
                artifact.write_bytes(original + b'tampered')
                with self.subTest(artifact=filename), self.assertRaisesRegex(ValueError, 'recovery artifact'):
                    wms.validate_update_preflight(settings, receipt, value['instance_id'])
                artifact.write_bytes(original)

    def test_backup_includes_keycloak_private_dump_and_real_isolated_rehearsal(self):
        with backup_harness() as (value, events, _queries, restores, _directory):
            receipt = wms.backup(value)
            self.assertEqual([event for event in events if event[0] == 'dump'], [['dump', 'wms'], ['dump', 'keycloak']])
            self.assertIn('keycloak', receipt)
            self.assertTrue(receipt['keycloak']['restore_verified'])
            self.assertEqual(receipt['keycloak']['sha256'], wms.sha256(receipt['keycloak']['path']))
            self.assertEqual(receipt['schema'], 2)
            self.assertTrue(any(name.startswith('wms_restore_') for name in restores))
            self.assertTrue(any(name.startswith('keycloak_restore_') for name in restores))
            self.assertIn(['docker', 'start', 'wms-keycloak'], events)
            stop = next(index for index, event in enumerate(events) if event[:2] == ['docker', 'stop'])
            start = events.index(['docker', 'start', 'wms-keycloak'])
            self.assertLess(stop, events.index(['dump', 'wms']))
            self.assertLess(events.index(['dump', 'keycloak']), start)
            if os.name == 'posix':
                self.assertEqual(Path(receipt['keycloak']['path']).stat().st_mode & 0o077, 0)

    def test_keycloak_dump_failure_resumes_keycloak_and_api_without_receipt(self):
        with backup_harness(fail_dump='keycloak') as (value, events, _queries, restores, directory):
            with self.assertRaisesRegex(RuntimeError, 'Backup failed'):
                wms.backup_while_quiesced(value)
            self.assertIn(['docker', 'start', 'wms-keycloak'], events)
            self.assertIn(['systemctl', 'start', 'wms-api'], events)
            self.assertEqual(restores, [])
            self.assertFalse(list((directory / 'state').rglob('receipt.json')))

    def test_keycloak_restore_failure_resumes_services_and_removes_only_clone(self):
        with backup_harness(fail_restore='keycloak') as (value, events, queries, restores, directory):
            with self.assertRaisesRegex(RuntimeError, 'restore failed'):
                wms.backup_while_quiesced(value)
            self.assertIn(['docker', 'start', 'wms-keycloak'], events)
            self.assertIn(['systemctl', 'start', 'wms-api'], events)
            clones = [name for name in restores if name.startswith('keycloak_restore_')]
            self.assertEqual(len(clones), 1)
            self.assertTrue(any('DROP DATABASE ' + clones[0] + ' ' in query for query, _ in queries))
            self.assertFalse(list((directory / 'state').rglob('receipt.json')))

    def test_keycloak_restored_key_or_client_drift_cannot_be_reported_verified(self):
        with backup_harness(corrupt_restore=True) as (value, events, _queries, _restores, directory):
            with self.assertRaisesRegex(ValueError, 'fingerprint mismatch'):
                wms.backup_while_quiesced(value)
            self.assertIn(['docker', 'start', 'wms-keycloak'], events)
            self.assertIn(['systemctl', 'start', 'wms-api'], events)
            self.assertFalse(list((directory / 'state').rglob('receipt.json')))

    def test_old_wms_only_backup_is_rejected_as_update_recovery_evidence(self):
        with tempfile.TemporaryDirectory() as temporary:
            dump = Path(temporary) / 'wms.dump'
            dump.write_bytes(b'wms only')
            settings = dict(wms.DURABILITY, synchronous_commit='on')
            old_receipt = {'instance_id': 'this-installation', 'path': str(dump),
                           'sha256': wms.sha256(dump), 'restore_verified': True}
            with self.assertRaisesRegex(ValueError, 'Keycloak'):
                wms.validate_update_preflight(settings, old_receipt, 'this-installation')


if __name__ == '__main__':
    unittest.main()
