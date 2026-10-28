import unittest
import json
from pathlib import Path
import tempfile
from types import SimpleNamespace
from unittest.mock import patch

from hardened_gate import main, validate_receipt, validate_plan_evidence, BASELINE_FAILURES


class HardenedGateTests(unittest.TestCase):
    def test_green_requires_a_retained_complete_bounded_plan_artifact(self):
        with tempfile.TemporaryDirectory() as directory:
            path = Path(directory) / 'plans.json'
            with self.assertRaises(ValueError):
                validate_plan_evidence(path, 'profile', 'test')
            report = {'seeded_documents': 100000, 'seeded_lines': 300000, 'property_cases': 30,
                'profile_sha256': 'profile', 'test_sha256': 'test', 'plans': [
                    {'case': case, 'mode': mode, 'row_work': 585, 'blocks': 250, 'line_hydrations': 21}
                    for case in ('one-owner-first','one-owner-deep','multiple-owner-first','multiple-owner-deep','all-owner-first','all-owner-deep','explicit-owner')
                    for mode in ('force_custom_plan','force_generic_plan')]}
            path.write_text(json.dumps(report))
            validate_plan_evidence(path, 'profile', 'test')
            for changed in ({'plans': []}, {'property_cases': 0}, {'profile_sha256': 'wrong'}):
                path.write_text(json.dumps(report | changed))
                with self.assertRaises(ValueError):
                    validate_plan_evidence(path, 'profile', 'test')

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
            'TestHardenedOnPremiseMultiWarehouse',
            'TestHardenedOnPremCriticReadinessTracksInstallationAndSigningKey',
            'TestHardenedOnPremCriticCrossOwnerAndStaleReadGrants',
            'TestHardenedOnPremCriticWorkerRejectsInstallationDrift',
            'TestHardenedDocumentReadsAtScale/independent_randomized_pagination_oracle'))
        validate_receipt('green', 0, good, output)
        for name in ('TestHardenedOnPremiseMultiWarehouse', 'TestHardenedOnPremCriticReadinessTracksInstallationAndSigningKey', 'TestHardenedOnPremCriticCrossOwnerAndStaleReadGrants', 'TestHardenedOnPremCriticWorkerRejectsInstallationDrift'):
            with self.subTest(missing=name), self.assertRaises(ValueError):
                validate_receipt('green', 0, good, output.replace('--- PASS: ' + name, ''))
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
        installation = {'passed': False, 'results': [{'name': 'signed context for another installation rejected', 'status': 'fail'}]}
        validate_receipt('installation', 1, installation, '')

    def test_worker_build_failure_and_skip_are_not_kills(self):
        receipt = {'passed': False, 'results': [{'name': 'SQL', 'status': 'pass'}], 'go_exit_code': 1}
        expected = '--- FAIL: TestHardenedHTTPAndContainment/worker_services_installation_and_stale_ack_fenced'
        validate_receipt('worker_fence', 1, receipt, expected)
        for output in ('build failed', expected + '\n--- SKIP: something', expected + '\npanic: test timed out after 180s', ''):
            with self.subTest(output=output), self.assertRaises(ValueError):
                validate_receipt('worker_fence', 1, receipt, output)

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
                    'installation': {'signed context for another installation rejected'},
                }.get(kind, set())
                report = {'passed': kind == 'green', 'results': [
                    {'name': name, 'status': 'fail'} for name in names] or [
                    {'name': str(n), 'status': 'pass'} for n in range(26)],
                    'go_exit_code': 0 if kind == 'green' else 1, 'go_passed': kind == 'green'}
                path.write_text(json.dumps(report))
                output = ('--- PASS: TestHardenedHTTPAndContainment\n'
                          '--- PASS: TestHardenedDocumentReadsAtScale\n'
                          '--- PASS: TestHardenedOnPremiseMultiWarehouse\n'
                          '--- PASS: TestHardenedOnPremCriticReadinessTracksInstallationAndSigningKey\n'
                          '--- PASS: TestHardenedOnPremCriticCrossOwnerAndStaleReadGrants\n'
                          '--- PASS: TestHardenedOnPremCriticWorkerRejectsInstallationDrift\n'
                          '--- PASS: TestHardenedDocumentReadsAtScale/independent_randomized_pagination_oracle') if kind == 'green' else {
                              'worker_fence': '--- FAIL: TestHardenedHTTPAndContainment/worker_services_installation_and_stale_ack_fenced',
                              'document_page': '--- FAIL: TestHardenedDocumentReadsAtScale/one-owner-first\nread page scanned history instead of bounded candidates',
                          }.get(kind, '')
                path.with_suffix('.go.txt').write_text(output)
                return SimpleNamespace(returncode=0 if kind == 'green' else 1, stdout='', stderr='')
            with patch.dict('os.environ', {'WMS_HARDENED_PLAN_DIR': 'work/relative-plans'}), \
                    patch('hardened_gate.validate_plan_evidence'), \
                    patch('sys.argv', ['gate', '--psql', 'psql', '--port', '5432', '--go', 'go', '--output', directory]), \
                    patch('hardened_gate.subprocess.run', side_effect=execute):
                main()
            self.assertEqual(len(plans), 7)
            self.assertNotIn(None, plans, 'Missing plan directory lets children share the caller directory')
            self.assertEqual(len(set(plans)), 7, 'A mutant can overwrite unmutated plans')
            self.assertTrue(all(Path(path).is_absolute() for path in plans), 'Go package cwd redirects relative plan directories')


if __name__ == '__main__':
    unittest.main()
