"""Expanded mutation coverage must be present, valid and executable by domain."""
import json
from pathlib import Path
import tempfile
import unittest

from mutation_runner import load_core_mutations


class CoreMutationCatalogTests(unittest.TestCase):
    def fixture(self, root):
        directory = root / 'scripts/mutations'
        directory.mkdir(parents=True)
        cases = []
        for domain in ('inventory', 'authorization', 'background', 'boundary'):
            cases.extend({'name': f'core_{domain}_{index}', 'kind': 'go', 'domain': domain, 'source': 'internal/api/server.go', 'before': 'before', 'after': 'after', 'pattern': '^TestCoreBehavior$', 'package': './internal/api'} for index in range(8))
        (directory / 'core-cases.json').write_text(json.dumps(cases))
        return directory, cases

    def test_each_core_domain_has_at_least_eight_unique_mutations(self):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            _, cases = self.fixture(root)
            self.assertEqual(load_core_mutations(root), cases)

    def test_missing_catalog_or_domain_is_not_a_smaller_green_campaign(self):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            with self.assertRaisesRegex(ValueError, 'core mutation coverage'):
                load_core_mutations(root)
            path, cases = self.fixture(root)
            (path / 'core-cases.json').write_text(json.dumps(cases[:-1]))
            with self.assertRaisesRegex(ValueError, 'core mutation coverage'):
                load_core_mutations(root)

    def test_item_accounting_trigger_is_part_of_the_core(self):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            path, cases = self.fixture(root)
            cases[0].update(kind='sql_function', signature='catalog.protect_item_accounting()', package='./internal/postgres')
            (path / 'core-cases.json').write_text(json.dumps(cases))
            self.assertEqual(len(load_core_mutations(root)), 32)

    def test_ambiguous_unsafe_or_non_behavioral_cases_are_rejected(self):
        mutations = [
            {'name': 'core_inventory_1'},
            {'source': '../outside.go'},
            {'source': 'internal/api/../../outside.go'},
            {'kind': 'shell'},
            {'after': 'before'},
            {'before': ''},
            {'pattern': ''},
            {'package': './...'},
            {'kind': 'sql_function', 'signature': "inventory.fn();DROP DATABASE production;--"},
        ]
        for change in mutations:
            with self.subTest(change=change), tempfile.TemporaryDirectory() as directory:
                root = Path(directory)
                path, cases = self.fixture(root)
                cases[0].update(change)
                (path / 'core-cases.json').write_text(json.dumps(cases))
                with self.assertRaises(ValueError):
                    load_core_mutations(root)


if __name__ == '__main__':
    unittest.main()
