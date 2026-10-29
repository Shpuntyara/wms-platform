"""Characterization of the native handoff runner's strict event classifier."""
import unittest

from verify_handoff_mutations import catalog, classify


class HandoffMutationClassifierTests(unittest.TestCase):
    names = ['TestHandoff/parcel', 'TestHandoff']

    def events(self, failed=False, message='HANDOFF_ASSERTION: custody differs'):
        action = 'fail' if failed else 'pass'
        return [dict(Action='output', Test='TestHandoff/parcel', Output=message if failed else 'ok'),
                dict(Action=action, Test='TestHandoff/parcel'), dict(Action=action, Test='TestHandoff')]

    def test_green_is_survived(self):
        self.assertEqual(classify(0, self.events(), self.names), 'survived')

    def test_real_leaf_assertion_with_parent_failure_kills(self):
        self.assertEqual(classify(1, self.events(True), self.names), 'killed')

    def test_unmarked_fixture_failure_is_invalid(self):
        self.assertEqual(classify(1, self.events(True, 'ERROR: relation does not exist'), self.names), 'invalid')

    def test_parent_only_assertion_does_not_cover_leaf_failure(self):
        events = self.events(True, 'setup failed')
        events.append(dict(Action='output', Test='TestHandoff', Output='HANDOFF_ASSERTION: summary'))
        self.assertEqual(classify(1, events, self.names), 'invalid')

    def test_feature_absence_is_invalid_even_with_assertion(self):
        self.assertEqual(classify(1, self.events(True, 'HANDOFF_ASSERTION: HANDOFF_FEATURE_UNAVAILABLE'), self.names), 'invalid')

    def test_timeout_is_invalid_even_with_assertion(self):
        events = self.events(True)
        events.append(dict(Action='output', Output='panic: test timed out after 90s'))
        self.assertEqual(classify(1, events, self.names), 'invalid')

    def test_panic_is_invalid(self):
        events = self.events(True)
        events.append(dict(Action='output', Output='panic: nil pointer'))
        self.assertEqual(classify(1, events, self.names), 'invalid')

    def test_build_failure_is_invalid(self):
        events = self.events(True)+[dict(Action='build-fail')]
        self.assertEqual(classify(1, events, self.names), 'invalid')

    def test_missing_test_is_invalid(self):
        self.assertEqual(classify(0, self.events()[:-1], self.names), 'invalid')

    def test_unexpected_extra_test_is_invalid(self):
        self.assertEqual(classify(0, self.events()+[dict(Action='pass',Test='TestOther')], self.names), 'invalid')

    def test_skipped_test_is_invalid(self):
        events = self.events()
        events[1]['Action'] = 'skip'
        self.assertEqual(classify(0, events, self.names), 'invalid')

    def test_no_test_is_invalid(self):
        self.assertEqual(classify(0, [], []), 'invalid')

    def test_process_failure_without_test_failure_is_invalid(self):
        self.assertEqual(classify(1, self.events(), self.names), 'invalid')

    def test_success_exit_with_failed_events_is_invalid(self):
        self.assertEqual(classify(0, self.events(True), self.names), 'invalid')

    def test_one_unmarked_failed_sibling_is_invalid(self):
        events = self.events(True)+[dict(Action='fail', Test='TestHandoff/other')]
        self.assertEqual(classify(1, events, self.names+['TestHandoff/other']), 'invalid')

    def test_catalog_has_22_unique_changes_and_named_behavior_tests(self):
        rows = catalog()
        self.assertEqual(len(rows), 22)
        self.assertEqual(len({row['name'] for row in rows}), 22)
        for row in rows:
            self.assertNotEqual(row['before'], row['after'])
            self.assertTrue(row['test'].startswith('TestDepartmentHandoffsCritic'))
            self.assertTrue(row['signature'].endswith(')'))


if __name__ == '__main__':
    unittest.main()
