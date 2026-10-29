"""Independent accounting checks using the runner's actual Node event collector.

These characterize the existing classifier. Passing cases are not RED evidence.
"""
import copy
import json
from pathlib import Path
import subprocess
import tempfile
import unittest

from verify_selection_mutations import NODE_HARNESS, catalog, classify


class SelectionMutationAccountingTests(unittest.TestCase):
    names = ['canary', 'fault']

    def collect(self, fault, *, extra_files=None, prefix=''):
        with tempfile.TemporaryDirectory(prefix='selection-classifier-') as directory:
            base = Path(directory)
            (base / 'events.mjs').write_text(NODE_HARNESS, encoding='utf8')
            (base / 'fixture.mjs').write_text(
                "import test from 'node:test';\n"
                "import assert from 'node:assert/strict';\n"
                + prefix
                + "test('canary',()=>assert.equal(1,1));\n"
                + fault + '\n', encoding='utf8')
            for name, source in (extra_files or {}).items():
                (base / name).write_text(source, encoding='utf8')
            command = subprocess.run(
                ['node', str(base / 'events.mjs'), 'fixture.mjs'], cwd=base,
                capture_output=True, text=True, encoding='utf8', timeout=15)
            self.assertEqual(command.returncode, 0, command.stderr)
            return json.loads(command.stdout)

    def test_unchanged_complete_baseline_survives(self):
        result = self.collect("test('fault',()=>assert.equal(2,2));")
        self.assertEqual(result['passed'], 2)
        self.assertEqual(result['failed'], 0)
        self.assertEqual(classify(result, self.names), 'survived')

    def test_actual_assertion_with_all_known_cases_kills(self):
        result = self.collect("test('fault',()=>assert.equal(1,2));")
        failure = next(row for row in result['results'] if row['name'] == 'fault')
        self.assertTrue(failure['assertion'])
        self.assertFalse(failure['infrastructure_error'])
        self.assertEqual(classify(result, self.names), 'killed')

    def test_generated_property_style_assertion_cause_chain_kills(self):
        result = self.collect("""
test('fault',()=>{
  try {assert.deepEqual(['wrong balance'],['selected balance']);}
  catch(cause) {throw new Error('Generated property failed',{cause});}
});
""")
        failure = next(row for row in result['results'] if row['name'] == 'fault')
        self.assertTrue(any(error['code'] == 'ERR_ASSERTION' for error in failure['errors']))
        self.assertTrue(any(error['name'] == 'Error' for error in failure['errors']))
        self.assertEqual(classify(result, self.names), 'killed')

    def test_type_reference_import_and_syntax_errors_are_invalid(self):
        cases = [
            ("test('fault',()=>{throw new TypeError('bad receiver');});", {}),
            ("test('fault',()=>{throw new ReferenceError('missing binding');});", {}),
            ("test('fault',async()=>import('./missing-module.mjs'));", {}),
            ("test('fault',async()=>import('./broken.mjs'));", {'broken.mjs': 'export const = ;'}),
        ]
        for source, files in cases:
            with self.subTest(source=source):
                result = self.collect(source, extra_files=files)
                self.assertEqual(sorted(row['name'] for row in result['results']), self.names)
                failure = next(row for row in result['results'] if row['name'] == 'fault')
                self.assertTrue(failure['infrastructure_error'])
                self.assertEqual(classify(result, self.names), 'invalid')

    def test_type_error_wrapping_an_assertion_is_still_invalid(self):
        result = self.collect("""
test('fault',()=>{
  try {assert.equal(1,2);} catch(cause) {throw new TypeError('broken test machinery',{cause});}
});
""")
        failure = next(row for row in result['results'] if row['name'] == 'fault')
        self.assertTrue(failure['assertion'])
        self.assertTrue(failure['infrastructure_error'])
        self.assertEqual(classify(result, self.names), 'invalid')

    def test_unexpected_non_assertion_exception_is_invalid(self):
        result = self.collect("test('fault',()=>{throw new Error('unexpected execution failure');});")
        failure = next(row for row in result['results'] if row['name'] == 'fault')
        self.assertFalse(failure['assertion'])
        self.assertEqual(classify(result, self.names), 'invalid')

    def test_node_test_timeout_is_not_a_kill(self):
        result = self.collect("test('fault',{timeout:10},async()=>new Promise(resolve=>setTimeout(resolve,100)));")
        failure = next(row for row in result['results'] if row['name'] == 'fault')
        self.assertFalse(failure['assertion'])
        self.assertTrue(any(error['failureType'] == 'testTimeoutFailure' for error in failure['errors']))
        self.assertEqual(classify(result, self.names), 'invalid')

    def test_process_timeout_import_failure_and_missing_report_are_invalid(self):
        for reason in ['timeout', 'process_failure', 'invalid_event_report']:
            with self.subTest(reason=reason):
                self.assertEqual(classify({'runner_error': reason}, self.names), 'invalid')

    def test_skip_and_todo_are_invalid_even_with_a_valid_assertion_failure(self):
        for decorator in ['skip', 'todo']:
            with self.subTest(decorator=decorator):
                result = self.collect(
                    f"test.{decorator}('unexecuted',()=>{{}});\n"
                    "test('fault',()=>assert.equal(1,2));")
                self.assertEqual(result['tests'], 3)
                self.assertEqual(classify(result, ['canary', 'fault', 'unexecuted']), 'invalid')

    def test_missing_renamed_or_duplicate_case_inventory_is_invalid(self):
        sources = [
            '',
            "test('renamed',()=>assert.equal(1,2));",
            "test('canary',()=>assert.equal(1,2));",
        ]
        for source in sources:
            with self.subTest(source=source):
                self.assertEqual(classify(self.collect(source), self.names), 'invalid')

    def test_summary_cannot_claim_a_complete_inventory_when_a_case_is_missing(self):
        result = self.collect('')
        result['tests'] = 2
        result['failed'] = 1
        self.assertEqual(classify(result, self.names), 'invalid')

    def test_missing_selection_helper_assertion_is_invalid(self):
        result = self.collect("test('fault',()=>assert.equal(typeof undefined,'function','Selection helper balanceChoices must exist before the employee can safely choose a balance'));")
        failure = next(row for row in result['results'] if row['name'] == 'fault')
        self.assertTrue(failure['assertion'])
        self.assertTrue(failure['unavailable_helper'])
        self.assertEqual(classify(result, self.names), 'invalid')

    def test_one_real_assertion_cannot_hide_another_broken_test(self):
        result = self.collect(
            "test('fault',()=>assert.equal(1,2));\n"
            "test('broken',()=>{throw new TypeError('broken fixture');});")
        self.assertEqual(result['failed'], 2)
        self.assertEqual(classify(result, ['broken', 'canary', 'fault']), 'invalid')

    def test_dropped_or_extra_collector_event_is_invalid(self):
        result = self.collect("test('fault',()=>assert.equal(1,2));")
        dropped = copy.deepcopy(result)
        dropped['results'].pop()
        self.assertEqual(classify(dropped, self.names), 'invalid')
        extra = copy.deepcopy(result)
        extra['results'].append(copy.deepcopy(extra['results'][0]))
        self.assertEqual(classify(extra, self.names), 'invalid')

    def test_catalog_has_23_unique_named_changes(self):
        faults = catalog()
        self.assertEqual(len(faults), 23)
        self.assertEqual(len({name for name, _, _ in faults}), len(faults))
        for name, before, after in faults:
            with self.subTest(name=name):
                self.assertNotEqual(before, after)
                self.assertTrue(before)


if __name__ == '__main__':
    unittest.main()
