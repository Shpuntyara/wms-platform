"""The UI mutation gate must reject invalid executions and surviving mutants."""
import importlib.util
from pathlib import Path
import unittest


path = Path(__file__).resolve().parents[1] / 'tests/ui_backend_mutations.py'
spec = importlib.util.spec_from_file_location('ui_backend_mutations', path)
gate = importlib.util.module_from_spec(spec)
spec.loader.exec_module(gate)


def events(failure=False):
    names = sorted(gate.REQUIRED)
    rows = [{'Action': 'run', 'Package': 'ui', 'Test': name} for name in names]
    rows.extend({'Action': 'fail' if failure and i == 0 else 'pass',
                 'Package': 'ui', 'Test': name} for i, name in enumerate(names))
    rows.append({'Action': 'fail' if failure else 'pass', 'Package': 'ui'})
    return rows


class UIMutationClassificationTests(unittest.TestCase):
    def test_real_failed_test_kills_mutant(self):
        self.assertEqual('killed', gate.classify(events(True), 1))

    def test_survivor_is_not_killed(self):
        self.assertEqual('survived', gate.classify(events(), 0))

    def test_complete_baseline_is_green(self):
        self.assertEqual('green', gate.classify(events(), 0, baseline=True))

    def test_invalid_runs_are_not_kills(self):
        cases = {
            'empty output': ([], 1),
            'missing tests': ([{'Action': 'fail', 'Package': 'ui'}], 1),
            'build failure': (events(True) + [{'Action': 'build-fail', 'Package': 'api'}], 1),
            'another package did not build': (events(True) + [{'Action': 'fail', 'Package': 'api'}], 1),
            'skipped test': (events(True) + [{'Action': 'skip', 'Test': 'TestMissing', 'Package': 'ui'}], 1),
            'inconsistent zero exit': (events(True), 0),
        }
        for name, (output, returncode) in cases.items():
            with self.subTest(name=name):
                self.assertEqual('invalid', gate.classify(output, returncode))


if __name__ == '__main__':
    unittest.main()
