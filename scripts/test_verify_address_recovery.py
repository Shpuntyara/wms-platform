import copy
import unittest
from pathlib import Path
import sys
sys.path.insert(0,str(Path(__file__).parent))
from verify_address_recovery import validate_report


class AddressMutationAccountingTests(unittest.TestCase):
    def good(self):
        names=['leaf_address_not_revalidated','leaf_key_regenerated','previous_address_mismatch_accepted',
               'old_origin_resumed_before_cutover','actor_snapshot_binding_removed']
        return {'baseline_passed':True,'source_unchanged':True,
                'mutants':[{'id':name,'status':'killed'} for name in names]}

    def test_complete_independent_behavioral_kills_pass(self):
        self.assertTrue(validate_report(self.good()))

    def test_survivor_invalid_missing_duplicate_or_changed_source_never_passes(self):
        for alteration in ['survivor','invalid','missing','duplicate','baseline','source']:
            with self.subTest(alteration=alteration):
                report=copy.deepcopy(self.good())
                if alteration=='survivor':report['mutants'][0]['status']='survived'
                if alteration=='invalid':report['mutants'][0]['status']='invalid'
                if alteration=='missing':report['mutants'].pop()
                if alteration=='duplicate':report['mutants'][-1]=report['mutants'][0]
                if alteration=='baseline':report['baseline_passed']=False
                if alteration=='source':report['source_unchanged']=False
                with self.assertRaises(ValueError):validate_report(report)
