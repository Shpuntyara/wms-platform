"""The official disposable backend harness must grant the new native RPCs."""
from pathlib import Path
import runpy
import unittest
from unittest.mock import patch

ROOT=Path(__file__).resolve().parents[1]

class HandoffTestBootstrap(unittest.TestCase):
    def test_official_backend_preparation_includes_precise_handoff_grants(self):
        commands=[]
        def run(argv,**kwargs):commands.append(argv)
        with patch('sys.argv',['prepare-test-db.py','--psql','synthetic-psql','--port','5432']),patch('subprocess.run',side_effect=run),patch('subprocess.check_output',return_value=b'42'):
            runpy.run_path(str(ROOT/'scripts/prepare-test-db.py'),run_name='__main__')
        grants=[arg for argv in commands for arg in argv if arg.endswith('grants.psql')]
        self.assertIn(str(ROOT/'db/bootstrap/handoffs-backend-grants.psql'),grants,'native handoff behavior tests would run without their runtime entry points')
        self.assertEqual(grants.count(str(ROOT/'db/bootstrap/handoffs-backend-grants.psql')),1)

if __name__=='__main__':unittest.main()
