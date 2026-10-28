import hashlib
import shutil
import tempfile
import unittest
from pathlib import Path
from migrate_core import migration_sql, build_script, source, PINNED

ROOT = Path(__file__).resolve().parent.parent

class MigrationPlanTests(unittest.TestCase):
    def test_legacy_pin_preserves_the_original_trust_root(self):
        canonical = (ROOT / 'db/migrations/0006_security_boundary.sql').read_bytes().replace(b'\r\n', b'\n')
        historical_pin = 'd905ffbcf713ba19de3e96523c803a74c436963332d008068cdddef03d135574'
        self.assertEqual(PINNED['0006_security_boundary.sql'], historical_pin)
        self.assertEqual(hashlib.sha256(canonical.replace(b'\n', b'\r\n')).hexdigest(), historical_pin)

    def test_published_sources_use_canonical_lf_on_every_checkout(self):
        """An unchanged published SQL file must work in Linux and Windows checkouts."""
        with tempfile.TemporaryDirectory() as directory:
            for name in (
                '0005_inventory_pagination.sql',
                '0006_security_boundary.sql',
                '0008_background_health_and_policy_v2.sql',
            ):
                canonical = (ROOT / 'db/migrations' / name).read_bytes().replace(b'\r\n', b'\n')
                for checkout in ('LF', 'CRLF'):
                    with self.subTest(migration=name, checkout=checkout):
                        path = Path(directory) / name
                        path.write_bytes(canonical if checkout == 'LF' else canonical.replace(b'\n', b'\r\n'))
                        sql, digest = source(path)
                        self.assertEqual(sql.encode('utf8'), canonical)
                        self.assertEqual(digest, hashlib.sha256(canonical).hexdigest())

    def test_published_source_still_rejects_content_drift(self):
        with tempfile.TemporaryDirectory() as directory:
            for name in (
                '0005_inventory_pagination.sql',
                '0006_security_boundary.sql',
                '0008_background_health_and_policy_v2.sql',
            ):
                canonical = (ROOT / 'db/migrations' / name).read_bytes().replace(b'\r\n', b'\n')
                for tampered in (
                    canonical + b'-- unexpected edit\n',
                    canonical + b' ',
                    canonical.replace(b'CREATE', b'DROP', 1),
                    canonical.replace(b'\n', b'\r\n') + b'-- unexpected edit\r\n',
                ):
                    with self.subTest(migration=name, digest=hashlib.sha256(tampered).hexdigest()):
                        path = Path(directory) / name
                        path.write_bytes(tampered)
                        with self.assertRaisesRegex(ValueError, 'Published migration checksum changed'):
                            source(path)

    def test_entire_plan_and_execution_receipts_are_newline_independent(self):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            migrations = root / 'db/migrations'
            shutil.copytree(ROOT / 'db/migrations', migrations)
            for path in migrations.glob('*.sql'):
                path.write_bytes(path.read_bytes().replace(b'\r\n', b'\n'))
            linux_plan = build_script(root)
            for path in migrations.glob('*.sql'):
                path.write_bytes(path.read_bytes().replace(b'\n', b'\r\n'))
            self.assertEqual(build_script(root), linux_plan)

    def test_online_health_upgrade_preserves_guard_identity(self):
        sql = migration_sql(ROOT / 'db/migrations/0008_background_health_and_policy_v2.sql')
        self.assertIn('CREATE INDEX CONCURRENTLY', sql)
        self.assertNotIn('RENAME TO', sql)
        self.assertIn('CREATE OR REPLACE FUNCTION platform.check_declarative_guard(', sql)
        self.assertIn('CREATE FUNCTION platform.check_declarative_guard_v1(', sql)
        self.assertLess(sql.index('CREATE INDEX CONCURRENTLY'), sql.index('BEGIN;'))

    def test_runner_serializes_records_checksums_and_bounds_locks(self):
        sql = build_script(ROOT)
        for required in ['pg_try_advisory_lock', 'checksum', 'lock_timeout', 'indisvalid', '\\if', 'wms_migrations.history']:
            self.assertIn(required, sql)
        self.assertNotIn('RENAME TO', sql)

if __name__ == '__main__':
    unittest.main()
