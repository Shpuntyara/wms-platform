"""Independent attempts to inflate or silently truncate a mutation campaign."""
import json
from pathlib import Path
import tempfile
from types import SimpleNamespace
import unittest
from unittest.mock import patch

from mutation_runner import load_core_mutations, run


class MutationCrossCriticTests(unittest.TestCase):
    def catalog(self, root):
        directory = root / 'scripts/mutations'
        directory.mkdir(parents=True)
        cases = []
        for domain in ('inventory', 'authorization', 'background', 'boundary'):
            for index in range(8):
                cases.append({'name': f'core_{domain}_{index}', 'kind': 'go', 'domain': domain,
                              'source': 'internal/api/server.go', 'before': f'{domain}_before_{index}',
                              'after': f'{domain}_after_{index}', 'pattern': '^TestCoreBehavior$',
                              'package': './internal/api'})
        return directory, cases

    def test_renaming_one_mutation_cannot_inflate_domain_coverage(self):
        for kind in ('go', 'sql_function'):
            with self.subTest(kind=kind), tempfile.TemporaryDirectory() as directory:
                root = Path(directory)
                path, cases = self.catalog(root)
                if kind == 'sql_function':
                    cases[0].update(kind=kind, signature='inventory.reserve_stock(uuid,uuid,uuid,bigint,uuid,numeric,timestamptz)', package='./internal/postgres')
                for key in ('kind', 'source', 'before', 'after', 'package'):
                    cases[1][key] = cases[0][key]
                if kind == 'sql_function':
                    cases[1]['signature'] = cases[0]['signature']
                (path / 'core-cases.json').write_text(json.dumps(cases))
                with self.assertRaisesRegex(ValueError, 'duplicate|Duplicate'):
                    load_core_mutations(root)

    def test_different_faults_at_the_same_site_remain_valid(self):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            path, cases = self.catalog(root)
            cases[1]['before'] = cases[0]['before']
            cases[1]['after'] = 'a_different_broken_behavior'
            (path / 'core-cases.json').write_text(json.dumps(cases))
            self.assertEqual(len(load_core_mutations(root)), 32)

    def test_unknown_selector_is_not_silently_ignored_beside_a_valid_case(self):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            for name, text in (
                ('internal/postgres/documents.go', '// source snapshot'),
                ('internal/postgres/store.go', 'args = append(args, f.Limit+1)'),
                ('scripts/migrate_core.py', '# source snapshot'),
                ('db/migrations/0009_warehouse_documents.sql', 'CREATE FUNCTION warehouse.command_document() RETURNS void LANGUAGE plpgsql AS $$ BEGIN NULL; END $$;'),
            ):
                path = root / name
                path.parent.mkdir(parents=True, exist_ok=True)
                path.write_text(text)
            args = SimpleNamespace(go='go', psql='psql', report='work/result.json', case=['go_document_lookahead_removed', 'misspelled_required_case'])
            environment = {'WMS_TEST_ADMIN_DSN': 'postgres://admin@127.0.0.1:55487/wms_kernel_test_gate?sslmode=disable',
                           'WMS_TEST_RUNTIME_DSN': 'postgres://runtime@127.0.0.1:55487/wms_kernel_test_gate?sslmode=disable'}
            core = [{'name': 'core_boundary_dummy', 'kind': 'go', 'domain': 'boundary', 'source': 'internal/api/server.go', 'before': 'before', 'after': 'after', 'pattern': '^TestCoreBehavior$', 'package': './internal/api'}]
            outcomes = [{'status': 'survived', 'failed_tests': []}, {'status': 'survived', 'failed_tests': []}, {'status': 'killed', 'failed_tests': ['TestDocumentPaginationProperties']}]
            with patch('mutation_runner.ROOT', root), patch.dict('os.environ', environment), patch('mutation_runner.subprocess.check_output', return_value='frozen-head\n'), patch('mutation_runner.load_core_mutations', return_value=core), patch('mutation_runner.run_go', side_effect=outcomes):
                with self.assertRaisesRegex(ValueError, 'Unknown|unknown'):
                    run(args)


if __name__ == '__main__':
    unittest.main()
