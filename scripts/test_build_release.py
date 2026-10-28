import hashlib
import importlib.util
import io
import json
from pathlib import Path
import tarfile
import tempfile
import unittest

PATH = Path(__file__).with_name('build_release.py')
spec = importlib.util.spec_from_file_location('build_release', PATH)
release = importlib.util.module_from_spec(spec)
spec.loader.exec_module(release)


class ReleaseBundleTests(unittest.TestCase):
    def test_bundle_exact_source_binary_hashes_and_revision(self):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            source = root / 'source'
            for name, data in {'db/migrations/test.sql': b'SELECT 1;\n',
                               'scripts/migrate_core.py': b'# runner\n',
                               'deploy/linux/bootstrap.sh': b'#!/bin/sh\n'}.items():
                path = source / name
                path.parent.mkdir(parents=True, exist_ok=True)
                path.write_bytes(data)
            binary = root / 'wms-api'
            binary.write_bytes(b'\x7fELF-real-build-fixture')
            target = root / 'release.tar.gz'
            receipt = release.package(source, binary, 'a' * 40, target)
            self.assertEqual(receipt['sha256'], hashlib.sha256(target.read_bytes()).hexdigest())
            with tarfile.open(target) as archive:
                manifest = json.load(archive.extractfile('release-manifest.json'))
                self.assertEqual(manifest['revision'], 'a' * 40)
                self.assertEqual(set(manifest['files']), set(archive.getnames()) - {'release-manifest.json'})
                for name, digest in manifest['files'].items():
                    self.assertEqual(hashlib.sha256(archive.extractfile(name).read()).hexdigest(), digest)
                self.assertEqual(archive.getmember('bin/wms-api').mode, 0o755)

    def test_bad_revision_missing_runner_and_existing_target_fail_closed(self):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            binary = root / 'binary'
            binary.write_bytes(b'\x7fELF')
            target = root / 'release.tar.gz'
            for revision in ('main', 'a' * 39, '../bad'):
                with self.assertRaises(ValueError):
                    release.package(root, binary, revision, target)
            with self.assertRaises(ValueError):
                release.package(root, binary, 'a' * 40, target)
            target.write_bytes(b'existing')
            with self.assertRaises((FileExistsError, ValueError)):
                release.package(root, binary, 'a' * 40, target)
            self.assertEqual(target.read_bytes(), b'existing')


if __name__ == '__main__':
    unittest.main()
