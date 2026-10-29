"""Numbering mutations cannot silently shrink or target missing behavior tests."""
import copy
from pathlib import Path
import re
import unittest

from numbering_mutations import load_numbering_mutations, mutate_numbering_function


class NumberingMutationCatalogTests(unittest.TestCase):
    def test_ten_distinct_faults_have_real_explicit_critic_tests(self):
        cases = load_numbering_mutations()
        self.assertEqual(len(cases), 10)
        self.assertEqual(len({case['name'] for case in cases}), 10)
        source = (Path(__file__).resolve().parent.parent / 'internal/postgres/documents_numbering_critic_integration_test.go').read_text(encoding='utf8')
        tests = re.findall(r'^func (Test\w+)\(t \*testing.T\)', source, re.MULTILINE)
        for case in cases:
            with self.subTest(case=case['name']):
                self.assertTrue(any(re.search(case['pattern'], test) for test in tests))
                self.assertEqual(case['domain'], 'document_numbering')
                self.assertEqual(case['kind'], 'sql_function')

    def test_missing_or_duplicate_campaign_is_rejected(self):
        cases = load_numbering_mutations()
        for change in ([], cases[:-1], cases + [cases[0]], cases[:-1] + [cases[0]]):
            with self.subTest(names=[case['name'] for case in change]), self.assertRaises(ValueError):
                load_numbering_mutations(change)

    def test_invalid_or_nonbehavioral_mutations_are_rejected(self):
        for change in ({'before': ''}, {'after': ''}, {'after': None}, {'kind': 'shell'}, {'domain': 'inventory'}, {'package': './...'}, {'signature': 'warehouse.x();DROP DATABASE production;--'}, {'pattern': ''}, {'pattern': '^TestMissingNumberingBehavior$'}, {'pattern': '^TestNumberingCriticMissingBehavior$'}, {'pattern': '['}, {'before': 'unchanged', 'after': 'unchanged'}):
            cases = copy.deepcopy(load_numbering_mutations())
            cases[0].update(change)
            with self.subTest(change=change), self.assertRaises(ValueError):
                load_numbering_mutations(cases)

    def test_builder_changes_exactly_one_site(self):
        case = load_numbering_mutations()[0]
        source = 'prefix\n' + case['before'] + '\nsuffix'
        self.assertEqual(mutate_numbering_function(source, case), 'prefix\n'+case['after']+'\nsuffix')
        for invalid in ('unrelated SQL', source+source):
            with self.subTest(source=invalid), self.assertRaises(ValueError):
                mutate_numbering_function(invalid, case)


if __name__ == '__main__':
    unittest.main()
