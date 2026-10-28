"""Independent attempts to bypass published migration integrity checks."""
import hashlib
from pathlib import Path
import shutil
import tempfile
import unittest

from migrate_core import build_script, source

ROOT = Path(__file__).resolve().parent.parent


class PublishedMigrationAdversarialTests(unittest.TestCase):
    def test_published_migration_cannot_disappear_from_a_package(self):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            shutil.copytree(ROOT / 'db/migrations', root / 'db/migrations')
            (root / 'db/migrations/0005_inventory_pagination.sql').unlink()
            with self.assertRaisesRegex(ValueError, 'Missing published migration'):
                build_script(root)

    def test_an_empty_package_is_not_a_successful_migration_plan(self):
        with tempfile.TemporaryDirectory() as directory:
            with self.assertRaisesRegex(ValueError, 'Missing published migration'):
                build_script(Path(directory))

    def test_bom_bare_cr_and_unicode_whitespace_do_not_launder_content(self):
        original = (ROOT / 'db/migrations/0006_security_boundary.sql').read_bytes().replace(b'\r\n', b'\n')
        mutations = (
            b'\xef\xbb\xbf' + original,
            original.replace(b'\n', b'\r', 1),
            original.replace(b' ', b'\xc2\xa0', 1),
            original + b'\x00',
            original.replace(b'CREATE', b'create', 1),
        )
        with tempfile.TemporaryDirectory() as directory:
            path = Path(directory) / '0006_security_boundary.sql'
            for changed in mutations:
                with self.subTest(digest=hashlib.sha256(changed).hexdigest()):
                    path.write_bytes(changed)
                    with self.assertRaisesRegex(ValueError, 'Published migration checksum changed'):
                        source(path)


if __name__ == '__main__':
    unittest.main()
