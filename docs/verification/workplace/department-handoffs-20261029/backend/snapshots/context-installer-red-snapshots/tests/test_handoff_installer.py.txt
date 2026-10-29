"""Existing installations receive the additive custody RPC and exact grants."""
from pathlib import Path
import unittest
from unittest.mock import patch
from test_industry_installer import wms

ROOT=Path(__file__).resolve().parents[1]

class HandoffInstaller(unittest.TestCase):
    def test_existing_installation_installs_signed_custody_rpc_before_grants_on_each_update(self):
        identity='9115926d-9f22-44ab-840c-afcb3a2a2ac3'
        value=dict(instance_id=identity,api_password='synthetic',worker_password='synthetic',keycloak_password='synthetic',request_key='synthetic')
        def execute_bootstrap():
            statements=[]
            def sql(text,database='wms'):
                statements.append(text)
                if 'SELECT id::text FROM hardened_private.installation' in text:return identity
                if "SELECT encode(key,'base64')" in text:return value['request_key']
                return 'f'
            with patch.object(wms,'sql',side_effect=sql):wms.database_bootstrap(ROOT,value)
            profiles=[i for i,text in enumerate(statements) if 'FUNCTION hardened_api.handoff_command(' in text and 'CREATE' in text]
            grants=[i for i,text in enumerate(statements) if 'GRANT EXECUTE ON FUNCTION hardened_api.handoff_context()' in text]
            self.assertEqual(len(profiles),1,'existing installation lacks signed handoff commands')
            self.assertEqual(len(grants),1,'existing installation lacks precise signed handoff grants')
            self.assertLess(profiles[0],grants[0])
            self.assertIn('SET ROLE wms_owner;',statements[profiles[0]])
            self.assertIn('hardened_api.handoff_command(uuid,uuid,text)',statements[grants[0]])
            self.assertNotIn('GRANT ALL',statements[grants[0]])
        execute_bootstrap()
        execute_bootstrap()

if __name__=='__main__':unittest.main()
