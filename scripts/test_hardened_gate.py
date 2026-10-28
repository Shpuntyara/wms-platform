import unittest
import json
from pathlib import Path
import tempfile
from types import SimpleNamespace
from unittest.mock import patch

from hardened_gate import main, validate_receipt, BASELINE_FAILURES


class HardenedGateTests(unittest.TestCase):
    def test_stale_receipt_cannot_turn_setup_failure_into_a_baseline_proof(self):
        with tempfile.TemporaryDirectory() as directory:
            path = Path(directory) / 'baseline.json'
            path.write_text(json.dumps({'passed': False, 'results': [
                {'name': name, 'status': 'fail'} for name in BASELINE_FAILURES]}))
            with patch('sys.argv', ['gate', '--psql', 'psql', '--port', '5432', '--go', 'go', '--output', directory]), \
                    patch('hardened_gate.subprocess.run', return_value=SimpleNamespace(returncode=1, stdout='setup failed', stderr='')) as run:
                with self.assertRaises(ValueError):
                    main()
                self.assertEqual(run.call_count, 1, 'Stale baseline was consumed as a fresh behavioral proof')

    def test_green_requires_all_sql_and_go_tests(self):
        good = {'passed': True, 'results': [{'name': str(i), 'status': 'pass'} for i in range(26)], 'go_passed': True, 'go_exit_code': 0}
        output = '\n'.join('--- PASS: ' + name for name in (
            'TestHardenedHTTPAndContainment', 'TestHardenedDocumentReadsAtScale',
            'TestHardenedDocumentReadsAtScale/independent_randomized_pagination_oracle'))
        validate_receipt('green', 0, good, output)
        with self.assertRaises(ValueError):
            validate_receipt('green', 0, good, '')
        with self.assertRaises(ValueError):
            validate_receipt('green', 0, good, output + '\n--- FAIL: another assertion')
        for changed in ({'go_exit_code': 1}, {'go_passed': False}, {'results': []}):
            with self.subTest(changed=changed), self.assertRaises(ValueError):
                validate_receipt('green', 0, good | changed, output)

    def test_mutant_requires_corresponding_behavioral_failure(self):
        receipt = {'passed': False, 'results': [{'name': 'unrelated assertion', 'status': 'fail'}]}
        with self.assertRaises(ValueError):
            validate_receipt('signature', 1, receipt, '')
        receipt['results'][0]['name'] = 'tampered actor signature rejected'
        validate_receipt('signature', 1, receipt, '')
        with self.assertRaises(ValueError):
            validate_receipt('signature', 2, receipt, '')

    def test_worker_build_failure_and_skip_are_not_kills(self):
        receipt = {'passed': False, 'results': [{'name': 'SQL', 'status': 'pass'}], 'go_exit_code': 1}
        expected = '--- FAIL: TestHardenedHTTPAndContainment/worker_sees_only_own_warehouse'
        validate_receipt('worker_scope', 1, receipt, expected)
        for output in ('build failed', expected + '\n--- SKIP: something', expected + '\npanic: test timed out after 180s', ''):
            with self.subTest(output=output), self.assertRaises(ValueError):
                validate_receipt('worker_scope', 1, receipt, output)

    def test_document_page_mutant_requires_the_large_fixture_plan_assertion(self):
        receipt = {'passed': False, 'results': [{'name': 'SQL', 'status': 'pass'}], 'go_exit_code': 1}
        output = ('--- FAIL: TestHardenedDocumentReadsAtScale/one-owner-first\n'
                  'read page scanned history instead of bounded candidates')
        validate_receipt('document_page', 1, receipt, output)
        for wrong in ('unrelated failure', output + '\npanic: test timed out after 180s'):
            with self.assertRaises(ValueError):
                validate_receipt('document_page', 1, receipt, wrong)

    def test_mutations_cannot_overwrite_the_unmutated_query_plans(self):
        with tempfile.TemporaryDirectory() as directory:
            plans = []
            def execute(command, **kwargs):
                path = Path(command[command.index('--report') + 1])
                kind = path.stem
                plans.append(kwargs.get('env', {}).get('WMS_HARDENED_PLAN_DIR'))
                names = BASELINE_FAILURES if kind == 'baseline' else {
                    'signature': {'tampered actor signature rejected'},
                    'action': {'signature for read cannot authorize a command'},
                }.get(kind, set())
                report = {'passed': kind == 'green', 'results': [
                    {'name': name, 'status': 'fail'} for name in names] or [
                    {'name': str(n), 'status': 'pass'} for n in range(26)],
                    'go_exit_code': 0 if kind == 'green' else 1, 'go_passed': kind == 'green'}
                path.write_text(json.dumps(report))
                output = ('--- PASS: TestHardenedHTTPAndContainment\n'
                          '--- PASS: TestHardenedDocumentReadsAtScale\n'
                          '--- PASS: TestHardenedDocumentReadsAtScale/independent_randomized_pagination_oracle') if kind == 'green' else {
                              'worker_scope': '--- FAIL: TestHardenedHTTPAndContainment/worker_sees_only_own_warehouse',
                              'document_page': '--- FAIL: TestHardenedDocumentReadsAtScale/one-owner-first\nread page scanned history instead of bounded candidates',
                          }.get(kind, '')
                path.with_suffix('.go.txt').write_text(output)
                return SimpleNamespace(returncode=0 if kind == 'green' else 1, stdout='', stderr='')
            with patch('sys.argv', ['gate', '--psql', 'psql', '--port', '5432', '--go', 'go', '--output', directory]), \
                    patch('hardened_gate.subprocess.run', side_effect=execute):
                main()
            self.assertEqual(len(plans), 6)
            self.assertNotIn(None, plans, 'Missing plan directory lets children share the caller directory')
            self.assertEqual(len(set(plans)), 6, 'A mutant can overwrite unmutated plans')


if __name__ == '__main__':
    unittest.main()
