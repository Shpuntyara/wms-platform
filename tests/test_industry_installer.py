"""Industry planning RPCs must be provisioned before API grants on update."""
import importlib.util
from pathlib import Path
import unittest
from unittest.mock import patch

ROOT=Path(__file__).resolve().parents[1]
spec=importlib.util.spec_from_file_location('industry_installer_target',ROOT/'deploy/linux/wmsctl.py')
wms=importlib.util.module_from_spec(spec);spec.loader.exec_module(wms)

class IndustryInstaller(unittest.TestCase):
    def test_existing_installation_loads_signed_planning_rpc_before_grants(self):
        identity='9115926d-9f22-44ab-840c-afcb3a2a2ac3'
        value=dict(instance_id=identity,api_password='disposable-fixture',worker_password='disposable-fixture',keycloak_password='disposable-fixture',request_key='disposable-fixture')
        statements=[]
        def sql(text,database='wms'):
            statements.append(text)
            if 'SELECT id::text FROM hardened_private.installation' in text:return identity
            if "SELECT encode(key,'base64')" in text:return value['request_key']
            return 'f'
        with patch.object(wms,'sql',side_effect=sql):wms.database_bootstrap(ROOT,value)
        profile=[i for i,text in enumerate(statements) if 'CREATE OR REPLACE FUNCTION hardened_api.work_order_command' in text]
        grants=[i for i,text in enumerate(statements) if 'GRANT EXECUTE ON FUNCTION hardened_api.work_orders(text)' in text]
        self.assertEqual(len(profile),1,'existing installation did not receive signed work-order RPCs')
        self.assertEqual(len(grants),1)
        self.assertLess(profile[0],grants[0])
        self.assertIn('hardened_api.work_order_command(uuid,text)',statements[grants[0]])

if __name__=='__main__':unittest.main()
