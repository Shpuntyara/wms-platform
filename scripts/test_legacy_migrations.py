import unittest
from pathlib import Path
from migrate_core import build_script

class LegacyPlanTests(unittest.TestCase):
    def test_legacy_adoption_requires_explicit_known_version(self):
        sql = build_script(Path(__file__).resolve().parent.parent, legacy_through='0007')
        self.assertIn('legacy_baseline', sql)
        self.assertIn('expected legacy schema version', sql)
        self.assertIn('ON CONFLICT (name) DO NOTHING', sql)
        with self.assertRaises(ValueError):
            build_script(Path(__file__).resolve().parent.parent, legacy_through='0006')
