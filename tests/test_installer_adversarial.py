"""Independent deployment boundaries: failures must preserve a usable service."""
import argparse
import datetime
import importlib.util
import io
import json
from pathlib import Path
import tarfile
import tempfile
import unittest
from unittest.mock import patch

from test_vm_installer import archive, REV

ROOT = Path(__file__).resolve().parents[1]
spec = importlib.util.spec_from_file_location('installer_adversarial', ROOT / 'deploy/linux/wmsctl.py')
wms = importlib.util.module_from_spec(spec)
spec.loader.exec_module(wms)


class InstallerAdversarial(unittest.TestCase):
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


if __name__ == '__main__':
    unittest.main()
