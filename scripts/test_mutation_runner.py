"""The mutation gate must distinguish an assertion kill from a broken run."""
import unittest
import json
from pathlib import Path
import tempfile
from types import SimpleNamespace
from unittest.mock import patch

from mutation_runner import classify_go_run, classify_python_run, database_environment, replace_once, run, run_go


class MutationRunnerTests(unittest.TestCase):
    def test_mutant_plans_cannot_overwrite_unmutated_plan_evidence(self):
        def fake_go(_command, **kwargs):
            self.assertNotEqual(kwargs['env']['WMS_DOCUMENT_PLAN_DIR'], 'green/plans')
            self.assertNotEqual(kwargs['env']['WMS_DOCUMENT_CRITIC_PLAN_DIR'], 'green/critic-plans')
            kwargs['stdout'].write(json.dumps({'Action': 'pass', 'Test': 'TestDocumentPlan'}))
            return SimpleNamespace(returncode=0)

        with tempfile.TemporaryDirectory() as directory, patch('mutation_runner.subprocess.run', side_effect=fake_go):
            outcome = run_go('go', '^TestDocumentPlan$', Path(directory), {'WMS_DOCUMENT_PLAN_DIR': 'green/plans', 'WMS_DOCUMENT_CRITIC_PLAN_DIR': 'green/critic-plans'})
        self.assertEqual(outcome['status'], 'survived')

    def test_python_import_errors_and_skips_are_not_mutation_kills(self):
        self.assertEqual(classify_python_run(1, {'tests': 1, 'failures': ['test_integrity'], 'errors': [], 'skipped': []})['status'], 'killed')
        self.assertEqual(classify_python_run(1, {'tests': 1, 'failures': [], 'errors': ['test_missing_source'], 'skipped': []})['status'], 'killed')
        for result in (
            {'tests': 0, 'failures': [], 'errors': [], 'skipped': []},
            {'tests': 1, 'failures': [], 'errors': ['unittest.loader._FailedTest.test_import'], 'skipped': []},
            {'tests': 2, 'failures': ['test_integrity'], 'errors': [], 'skipped': ['test_database']},
        ):
            with self.subTest(result=result):
                self.assertEqual(classify_python_run(1, result)['status'], 'invalid')

    def test_only_a_failing_test_kills_a_mutant(self):
        events = [
            {'Action': 'run', 'Package': 'p', 'Test': 'TestDocumentRetry'},
            {'Action': 'fail', 'Package': 'p', 'Test': 'TestDocumentRetry'},
            {'Action': 'fail', 'Package': 'p'},
        ]
        self.assertEqual(classify_go_run(1, events)['status'], 'killed')
        self.assertEqual(classify_go_run(0, [{'Action': 'pass', 'Test': 'TestDocumentRetry'}])['status'], 'survived')

    def test_build_errors_timeouts_and_missing_tests_are_not_kills(self):
        for code, events in [
            (1, [{'Action': 'fail', 'Package': 'p'}]),
            (0, []),
            (1, [{'Action': 'run', 'Test': 'TestDocumentRetry'}]),
            (1, [{'Action': 'skip', 'Test': 'TestDocumentRetry'}]),
            (1, [{'Action': 'fail', 'Test': 'TestDocumentRetry'}, {'Action': 'skip', 'Test': 'TestOther'}]),
        ]:
            with self.subTest(code=code, events=events):
                self.assertEqual(classify_go_run(code, events)['status'], 'invalid')

    def test_mutation_must_change_one_known_site(self):
        self.assertEqual(replace_once('a b c', 'b', 'd'), 'a d c')
        for source, before, after in [('a', 'b', 'c'), ('a a', 'a', 'b'), ('a', 'a', 'a')]:
            with self.subTest(source=source):
                with self.assertRaises(ValueError):
                    replace_once(source, before, after)

    def test_database_name_is_a_safe_sql_identifier_not_just_a_prefix(self):
        for name in (
            'wms_kernel_test_good;DROP DATABASE production;--',
            'wms_kernel_test_good/other',
            'wms_kernel_test_good%3BDROP_DATABASE_production',
            'wms_kernel_test_good"',
            'wms_kernel_test_good another',
        ):
            with self.subTest(name=name):
                with self.assertRaises(ValueError):
                    database_environment('postgres://admin@127.0.0.1:55487/' + name)

    def test_dsn_options_cannot_redirect_go_or_libpq_connections(self):
        for query in ('host=outside.invalid', 'hostaddr=192.0.2.1', 'service=production', 'options=-csearch_path=public'):
            with self.subTest(query=query):
                with self.assertRaises(ValueError):
                    database_environment('postgres://admin@127.0.0.1:55487/wms_kernel_test_ok?' + query)

    def test_inherited_libpq_options_cannot_escape_the_isolated_server(self):
        dangerous = {
            'PGHOSTADDR': '192.0.2.1',
            'PGSERVICE': 'production',
            'PGSERVICEFILE': 'C:/production/pg_service.conf',
            'PGOPTIONS': '-c search_path=public',
        }
        with patch.dict('os.environ', dangerous):
            name, environment = database_environment('postgres://admin@127.0.0.1:55487/wms_kernel_test_ok?sslmode=disable')
        self.assertEqual(name, 'wms_kernel_test_ok')
        for key in dangerous:
            with self.subTest(key=key):
                self.assertTrue(key not in environment, 'unsafe libpq option was inherited: ' + key)

    def test_timeout_after_an_assertion_failure_is_an_invalid_run(self):
        events = [
            {'Action': 'run', 'Package': 'p', 'Test': 'TestDocumentRetry'},
            {'Action': 'fail', 'Package': 'p', 'Test': 'TestDocumentRetry'},
            {'Action': 'output', 'Package': 'p', 'Output': 'panic: test timed out after 3m0s\n'},
            {'Action': 'fail', 'Package': 'p'},
        ]
        self.assertEqual(classify_go_run(1, events)['status'], 'invalid')

    def test_failed_baseline_still_writes_a_failed_evidence_artifact(self):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            args = SimpleNamespace(go='go', psql='psql', report='work/result.json', case=None)
            environment = {
                'WMS_TEST_ADMIN_DSN': 'postgres://admin@127.0.0.1:55487/wms_kernel_test_baseline?sslmode=disable',
                'WMS_TEST_RUNTIME_DSN': 'postgres://runtime@127.0.0.1:55487/wms_kernel_test_baseline?sslmode=disable',
            }
            with patch('mutation_runner.ROOT', root), patch.dict('os.environ', environment), patch('mutation_runner.subprocess.check_output', return_value='frozen-head\n'), patch('mutation_runner.run_go', return_value={'status': 'invalid', 'failed_tests': [], 'reason': 'baseline build failure'}):
                with self.assertRaises(RuntimeError):
                    run(args)
            target = root / args.report
            self.assertTrue(target.is_file(), 'A failed baseline lost the mutation evidence artifact')
            report = json.loads(target.read_text(encoding='utf8'))
            self.assertEqual(report['status'], 'failed')
            self.assertEqual(report['baseline']['status'], 'invalid')
            self.assertEqual(report['mutants'], [])

    def test_missing_go_executable_still_writes_failed_evidence(self):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            args = SimpleNamespace(go='missing-go', psql='psql', report='work/result.json', case=None)
            environment = {
                'WMS_TEST_ADMIN_DSN': 'postgres://admin@127.0.0.1:55487/wms_kernel_test_baseline?sslmode=disable',
                'WMS_TEST_RUNTIME_DSN': 'postgres://runtime@127.0.0.1:55487/wms_kernel_test_baseline?sslmode=disable',
            }
            with patch('mutation_runner.ROOT', root), patch.dict('os.environ', environment), patch('mutation_runner.subprocess.check_output', return_value='frozen-head\n'), patch('mutation_runner.run_go', side_effect=FileNotFoundError('missing Go executable')):
                with self.assertRaises(FileNotFoundError):
                    run(args)
            target = root / args.report
            self.assertTrue(target.is_file(), 'A failed process launch lost the mutation evidence artifact')
            self.assertEqual(json.loads(target.read_text(encoding='utf8'))['status'], 'failed')


if __name__ == '__main__':
    unittest.main()
