"""A missing handoff helper is a broken campaign, never a mutation kill."""
import json
from pathlib import Path
import subprocess
import tempfile
import unittest

from verify_handoff_ui_mutations import NODE_HARNESS, classify


class HandoffMutationAccountingTests(unittest.TestCase):
    def test_actual_missing_handoff_helper_assertion_is_invalid(self):
        with tempfile.TemporaryDirectory(prefix='handoff-classifier-') as directory:
            base=Path(directory)
            (base/'events.mjs').write_text(NODE_HARNESS,encoding='utf8')
            (base/'missing.test.mjs').write_text("import test from 'node:test';import assert from 'node:assert/strict';test('missing helper',()=>assert.equal(typeof undefined,'function','Handoff helper handoffDestinations must exist before department transfers are safe'));",encoding='utf8')
            result=subprocess.run(['node',str(base/'events.mjs'),'missing.test.mjs'],cwd=base,capture_output=True,text=True,encoding='utf8',timeout=15)
            self.assertEqual(result.returncode,0,result.stderr)
            events=json.loads(result.stdout)
            self.assertTrue(events['results'][0]['assertion'])
            self.assertTrue(events['results'][0]['unavailable_helper'])
            self.assertEqual(classify(events,['missing helper']),'invalid')


if __name__=='__main__':
    unittest.main()
