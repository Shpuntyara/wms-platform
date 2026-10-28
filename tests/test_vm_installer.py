"""Executable trust/identity/update contract for the independent VM installer."""
import hashlib
import importlib.util
import io
import json
from pathlib import Path
import random
import tarfile
import tempfile
import unittest

ROOT = Path(__file__).resolve().parents[1]
spec = importlib.util.spec_from_file_location('wmsctl', ROOT / 'deploy/linux/wmsctl.py')
wms = importlib.util.module_from_spec(spec)
spec.loader.exec_module(wms)
REV = 'd68d652' + '0' * 33


def archive(directory, names=None, revision=REV):
    files = {'bin/wms-api': b'ELF-fixture', 'db/migrations/example.sql': b'SELECT 1;'}
    manifest = {'schema': 1, 'revision': revision, 'files': {
        name: hashlib.sha256(value).hexdigest() for name, value in files.items()}}
    files['release-manifest.json'] = json.dumps(manifest).encode()
    files.update(names or {})
    target = Path(directory) / 'release.tar.gz'
    with tarfile.open(target, 'w:gz') as out:
        for name, value in files.items():
            item = tarfile.TarInfo(name)
            item.size = len(value)
            out.addfile(item, io.BytesIO(value))
    return target, hashlib.sha256(target.read_bytes()).hexdigest()


class InstallerContract(unittest.TestCase):
    def test_verified_bundle_rejects_hash_revision_untracked_and_links(self):
        with tempfile.TemporaryDirectory() as tmp:
            path, digest = archive(tmp)
            self.assertEqual(wms.verify_archive(path, digest, REV)['revision'], REV)
            for bad_hash, bad_rev in [('0' * 64, REV), (digest, '1' * 40)]:
                with self.assertRaises(ValueError):
                    wms.verify_archive(path, bad_hash, bad_rev)
            path, digest = archive(tmp, {'untracked': b'x'})
            with self.assertRaises(ValueError):
                wms.verify_archive(path, digest, REV)
            with tarfile.open(Path(tmp) / 'link.tar', 'w') as out:
                link = tarfile.TarInfo('link')
                link.type = tarfile.SYMTYPE
                link.linkname = '/etc/shadow'
                out.addfile(link)
            link_path = Path(tmp) / 'link.tar'
            with self.assertRaises(ValueError):
                wms.verify_archive(link_path, hashlib.sha256(link_path.read_bytes()).hexdigest(), REV)

    def test_path_property_rejects_traversal_absolute_and_platform_ambiguity(self):
        rng = random.Random(77031)
        attacks = ['/etc/shadow', '../escape', 'a/../../escape', 'a\\..\\escape', 'C:/escape', './a', 'a//b']
        attacks += ['a/' * rng.randrange(1, 12) + '../escape' for _ in range(60)]
        for name in attacks:
            with self.subTest(name=name), self.assertRaises(ValueError):
                wms.safe_member_name(name)
        for _ in range(100):
            name = 'db/' + ''.join(rng.choice('abcdef012345') for _ in range(20)) + '.sql'
            self.assertEqual(wms.safe_member_name(name), name)

    def test_secrets_are_persistent_private_and_independent(self):
        with tempfile.TemporaryDirectory() as tmp:
            one = Path(tmp) / 'one.json'
            a = wms.ensure_secrets(one)
            before = one.read_bytes()
            self.assertEqual(a, wms.ensure_secrets(one))
            self.assertEqual(before, one.read_bytes())
            b = wms.ensure_secrets(Path(tmp) / 'two.json')
            for key in ('instance_id', 'db_admin_password', 'api_password', 'worker_password',
                        'keycloak_password', 'oidc_client_secret', 'request_key', 'outbox_key', 'grafana_password'):
                self.assertNotEqual(a[key], b[key], key)
            self.assertNotEqual(a['api_password'], a['worker_password'])
            self.assertNotIn(a['request_key'], json.dumps(wms.public_identity(a)))

    def test_secrets_do_not_regenerate_when_existing_file_is_invalid(self):
        with tempfile.TemporaryDirectory() as tmp:
            path = Path(tmp) / 'secrets.json'
            path.write_text('{}')
            with self.assertRaises(ValueError):
                wms.ensure_secrets(path)
            self.assertEqual(path.read_text(), '{}')

    def test_upgrade_requires_durable_database_and_verified_backup(self):
        safe = {'fsync': 'on', 'full_page_writes': 'on', 'synchronous_commit': 'on',
                'data_checksums': 'on', 'ignore_checksum_failure': 'off',
                'zero_damaged_pages': 'off', 'ignore_invalid_pages': 'off'}
        with tempfile.TemporaryDirectory() as tmp:
            dump = Path(tmp) / 'db.dump'
            dump.write_bytes(b'nonempty real backup placeholder')
            kc_dump = Path(tmp) / 'keycloak.dump'
            kc_dump.write_bytes(b'nonempty keycloak backup placeholder')
            artifacts = {}
            for name in ('globals.sql', 'identity.tar.gz'):
                artifact = Path(tmp) / name
                artifact.write_bytes(('synthetic ' + name).encode())
                artifacts[name] = {'path': str(artifact), 'sha256': hashlib.sha256(artifact.read_bytes()).hexdigest()}
            receipt = {'schema': 2, 'path': str(dump), 'sha256': hashlib.sha256(dump.read_bytes()).hexdigest(),
                       'restore_verified': True, 'instance_id': 'this-instance',
                       'artifacts': artifacts,
                       'keycloak': {'path': str(kc_dump), 'sha256': hashlib.sha256(kc_dump.read_bytes()).hexdigest(),
                           'restore_verified': True, 'source_fingerprint_sha256': '1' * 64,
                           'restored_fingerprint_sha256': '1' * 64}}
            wms.validate_update_preflight(safe, receipt, 'this-instance')
            for key in safe:
                bad = dict(safe, **{key: 'off' if safe[key] == 'on' else 'on'})
                with self.subTest(setting=key), self.assertRaises(ValueError):
                    wms.validate_update_preflight(bad, receipt, 'this-instance')
            for change in ({'sha256': '0' * 64}, {'restore_verified': False}, {'instance_id': 'foreign'}):
                with self.assertRaises(ValueError):
                    wms.validate_update_preflight(safe, dict(receipt, **change), 'this-instance')
            dump.write_bytes(b'tampered')
            with self.assertRaises(ValueError):
                wms.validate_update_preflight(safe, receipt, 'this-instance')


if __name__ == '__main__':
    unittest.main()
