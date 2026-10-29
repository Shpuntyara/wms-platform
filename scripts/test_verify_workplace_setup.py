import importlib.util
from pathlib import Path
import unittest

spec = importlib.util.spec_from_file_location('verify_workplace_setup',Path(__file__).with_name('verify_workplace_setup.py'))
gate = importlib.util.module_from_spec(spec)
spec.loader.exec_module(gate)


class WorkplaceMutationAccountingTests(unittest.TestCase):
    def test_real_keycloak_audience_roundtrip_mutations_are_mandatory(self):
        names={case[0] for case in gate.catalog()}
        self.assertIn('canonical_userinfo_false_omitted',names)
        self.assertIn('audience_disclosure_or_mapper_drift_accepted',names)
        self.assertGreaterEqual(len(names),19)

    def test_assertion_kills_and_green_survives(self):
        self.assertEqual(gate.classify(1,{'tests':1,'failures':['test_behavior'],'errors':[],'skipped':[]},1),'killed')
        self.assertEqual(gate.classify(0,{'tests':1,'failures':[],'errors':[],'skipped':[]},1),'survived')

    def test_missing_skipped_import_error_and_inconsistent_reports_are_invalid(self):
        for code,result in [(1,{'tests':0,'failures':[],'errors':[],'skipped':[]}),
                            (0,{'tests':1,'failures':[],'errors':[],'skipped':['test']}),
                            (1,{'tests':1,'failures':[],'errors':['_FailedTest'],'skipped':[]}),
                            (1,{'tests':1,'failures':[],'errors':['TypeError'],'skipped':[]}),
                            (0,{'tests':1,'failures':['behavior'],'errors':[],'skipped':[]}),
                            (2,{'tests':1,'failures':['behavior'],'errors':[],'skipped':[]})]:
            with self.subTest(result=result):
                self.assertEqual(gate.classify(code,result,1),'invalid')


if __name__ == '__main__': unittest.main()
