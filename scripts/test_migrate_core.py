import unittest
from pathlib import Path
from migrate_core import migration_sql, build_script

ROOT = Path(__file__).resolve().parent.parent

class MigrationPlanTests(unittest.TestCase):
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
