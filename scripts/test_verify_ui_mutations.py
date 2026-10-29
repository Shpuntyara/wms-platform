"""Mutation accounting must never turn an infrastructure error into a kill."""
import importlib.util
from pathlib import Path
import unittest

spec = importlib.util.spec_from_file_location('ui_mutations', Path(__file__).with_name('verify_ui_mutations.py'))
module = importlib.util.module_from_spec(spec)
spec.loader.exec_module(module)


class MutationAccounting(unittest.TestCase):
    def test_executed_assertion_is_a_kill(self):
        output = "TAP version 13\nnot ok 1 - rejects scope\n  code: 'ERR_ASSERTION'\n# tests 16\n# pass 15\n# fail 1\n# skipped 0\n# cancelled 0\n# todo 0\n"
        self.assertEqual(module.classify(1, output, 16), 'killed')

    def test_success_is_survived(self):
        output = 'TAP version 13\n# tests 16\n# pass 16\n# fail 0\n# skipped 0\n# cancelled 0\n# todo 0\n'
        self.assertEqual(module.classify(0, output, 16), 'survived')

    def test_incomplete_or_infrastructure_failure_is_invalid(self):
        cases = [
            ('module', "code: 'ERR_MODULE_NOT_FOUND'\n# tests 1\n# fail 1\n"),
            ('syntax', "SyntaxError: Unexpected token\n# tests 16\n# fail 1\n"),
            ('runtime type error', "TypeError: Cannot read properties\n# tests 16\n# fail 1\n"),
            ('skipped', "code: 'ERR_ASSERTION'\n# tests 16\n# pass 14\n# fail 1\n# skipped 1\n# cancelled 0\n# todo 0\n"),
            ('no tests', "code: 'ERR_ASSERTION'\n# tests 0\n# fail 0\n"),
            ('lost test', "code: 'ERR_ASSERTION'\n# tests 15\n# pass 14\n# fail 1\n# skipped 0\n# cancelled 0\n# todo 0\n"),
            ('unrelated failure mixed with assertion', "code: 'ERR_ASSERTION'\ncode: 'ERR_MODULE_NOT_FOUND'\n# tests 16\n# pass 14\n# fail 2\n# skipped 0\n# cancelled 0\n# todo 0\n"),
        ]
        for name, output in cases:
            with self.subTest(name=name):
                self.assertEqual(module.classify(1, output, 16), 'invalid')


if __name__ == '__main__':
    unittest.main()
