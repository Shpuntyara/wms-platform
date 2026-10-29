import importlib.util
from pathlib import Path
import tempfile
import unittest
from unittest.mock import patch

ROOT = Path(__file__).resolve().parents[1]
spec = importlib.util.spec_from_file_location('workplace_binding',ROOT/'deploy/linux/wmsctl.py')
wms = importlib.util.module_from_spec(spec)
spec.loader.exec_module(wms)


class WorkplaceInstallerBinding(unittest.TestCase):
    def render(self, root, present):
        config = root/'config'
        catalog = root/'workspace/workspace.json'
        catalog.parent.mkdir(exist_ok=True)
        if present:
            catalog.write_text('{"organizations":[]}',encoding='utf8')
        value = wms.ensure_secrets(config/'secrets.json')
        def write(path,data,mode=0o600):
            if config in Path(path).parents:
                Path(path).parent.mkdir(parents=True,exist_ok=True)
                Path(path).write_text(data,encoding='utf8')
        with patch.object(wms,'CONFIG',config),patch.object(wms,'STATE',root/'state'), \
             patch.object(wms,'WORKSPACE_CONFIG',catalog,create=True), \
             patch.object(wms,'run',return_value='172.17.0.1'), \
             patch.object(wms,'private_write',side_effect=write):
            wms.configuration(value,'172.30.1.10')
            first = (config/'app.env').read_text()
            wms.configuration(value,'172.30.1.10')
        self.assertEqual(first,(config/'app.env').read_text())
        return first,catalog,value

    def test_an_existing_workplace_remains_bound_after_repeated_update(self):
        with tempfile.TemporaryDirectory() as temp:
            env,catalog,value = self.render(Path(temp),True)
            self.assertIn('WMS_UI_CONFIG='+str(catalog)+'\n',env)
            self.assertEqual(env.count('WMS_UI_CONFIG='),1)
            self.assertNotIn('workplace-users',env)
            self.assertIn('WMS_OIDC_AUDIENCE=wms-api-'+value['instance_id'][:8],env)

    def test_a_host_without_employee_catalog_can_start_before_setup(self):
        with tempfile.TemporaryDirectory() as temp:
            env,_,_ = self.render(Path(temp),False)
            self.assertNotIn('WMS_UI_CONFIG=',env)


if __name__=='__main__':
    unittest.main()
