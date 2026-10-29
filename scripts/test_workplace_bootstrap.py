import importlib.util
import json
import os
from pathlib import Path
import tempfile
import unittest
import uuid

ROOT = Path(__file__).resolve().parents[1]
spec = importlib.util.spec_from_file_location('workplace_bootstrap',ROOT/'deploy/linux/workplace_bootstrap.py')
bootstrap = importlib.util.module_from_spec(spec)
spec.loader.exec_module(bootstrap)


class FakeRuntime:
    def __init__(self):
        self.identity={'instance_id':str(uuid.uuid4()),'address':'192.0.2.12','issuer':'https://192.0.2.12/auth/realms/wms-test','audience':'wms-api-test'}
        self.steps=[]; self.exists=False; self.fail_bootstrap=False; self.fail_apply=False; self.fail_delete=False
        self.client=None; self.secret=None
    def sql(self,query): return self.identity['instance_id']
    def compose(self,*args): self.steps.append(args)
    def discovery(self): self.steps.append(('discovery',))
    def bootstrap(self,client,secret):
        self.steps.append(('bootstrap',)); self.client=client; self.secret=secret; self.exists=True
        if self.fail_bootstrap: raise ValueError('secret-bearing server failure')
    def client_exists(self,client): return self.exists
    def admin_token(self,client,secret):
        self.steps.append(('token',)); return {'access_token':'SECRET-TOKEN','refresh_token':'SECRET-REFRESH'}
    def cleanup_client(self,state,tokens):
        self.steps.append(('delete',))
        if self.fail_delete: raise ValueError('secret-bearing cleanup failure')
        self.exists=False
    def apply(self,path,expected):
        self.steps.append(('apply',))
        data=json.loads(Path(path).read_text())
        assert data['access_token']=='SECRET-TOKEN'
        if os.name=='posix': assert Path(path).stat().st_mode&0o777==0o600
        if self.fail_apply: raise ValueError('secret-bearing apply failure')
        return {'status':'configured','instance_id':expected}


class BootstrapTests(unittest.TestCase):
    def test_success_stops_bootstraps_restarts_verifies_and_removes_admin(self):
        runtime=FakeRuntime()
        with tempfile.TemporaryDirectory() as directory:
            state,token=Path(directory)/'state.json',Path(directory)/'token.json'
            result=bootstrap.provision(runtime,runtime.identity['instance_id'],lab=True,state_file=state,token_file=token)
            self.assertEqual(result.get('status'),'configured')
            self.assertEqual(runtime.steps[:4],[('stop','keycloak'),('bootstrap',),('up','-d','keycloak'),('discovery',)])
            self.assertIn(('delete',),runtime.steps)
            self.assertFalse(token.exists())
            saved=json.loads(state.read_text())
            self.assertEqual(saved['status'],'cleaned')
            self.assertNotIn('secret',saved)
            self.assertNotIn('SECRET',json.dumps(result))
    def test_bootstrap_failure_still_restarts_and_cleans_a_partially_created_client(self):
        runtime=FakeRuntime(); runtime.fail_bootstrap=True
        with tempfile.TemporaryDirectory() as directory,self.assertRaises(ValueError):
            bootstrap.provision(runtime,runtime.identity['instance_id'],lab=True,state_file=Path(directory)/'state.json',token_file=Path(directory)/'token.json')
        self.assertIn(('up','-d','keycloak'),runtime.steps)
        self.assertIn(('discovery',),runtime.steps)
        self.assertIn(('delete',),runtime.steps)
        self.assertNotIn(('apply',),runtime.steps)
    def test_apply_failure_still_deletes_admin_and_removes_token(self):
        runtime=FakeRuntime(); runtime.fail_apply=True
        with tempfile.TemporaryDirectory() as directory:
            token=Path(directory)/'token.json'
            with self.assertRaises(ValueError):
                bootstrap.provision(runtime,runtime.identity['instance_id'],lab=True,state_file=Path(directory)/'state.json',token_file=token)
            self.assertFalse(token.exists())
        self.assertIn(('delete',),runtime.steps)
    def test_cleanup_failure_retains_protected_recovery_state_and_blocks_another_admin(self):
        runtime=FakeRuntime(); runtime.fail_delete=True
        with tempfile.TemporaryDirectory() as directory:
            state,token=Path(directory)/'state.json',Path(directory)/'token.json'
            with self.assertRaises(ValueError):
                bootstrap.provision(runtime,runtime.identity['instance_id'],lab=True,state_file=state,token_file=token)
            saved=json.loads(state.read_text())
            self.assertEqual(saved['status'],'cleanup_failed')
            self.assertEqual(saved['secret'],runtime.secret)
            self.assertFalse(token.exists())
            before=list(runtime.steps)
            with self.assertRaises(ValueError):
                bootstrap.provision(runtime,runtime.identity['instance_id'],lab=True,state_file=state,token_file=token)
            self.assertEqual(runtime.steps,before)
    def test_password_is_only_in_environment_not_argv(self):
        command,env=bootstrap.bootstrap_command(Path('/etc/wms'),'wms-workplace-admin-'+uuid.uuid4().hex,'PRIVATE-ONLY-TEST-CREDENTIAL')
        self.assertNotIn('PRIVATE-ONLY-TEST-CREDENTIAL',' '.join(map(str,command)))
        self.assertEqual(env.get('WMS_WORKPLACE_ADMIN_SECRET'),'PRIVATE-ONLY-TEST-CREDENTIAL')
        self.assertIn('--client-secret:env=WMS_WORKPLACE_ADMIN_SECRET',command)
        self.assertIn('--no-prompt',command)
        self.assertIn('bootstrap-admin',command)
    def test_explicit_lab_and_same_installation_are_required_before_stopping(self):
        runtime=FakeRuntime()
        for expected,lab in [(runtime.identity['instance_id'],False),(str(uuid.uuid4()),True)]:
            with self.assertRaises(ValueError): bootstrap.provision(runtime,expected,lab=lab)
        self.assertEqual(runtime.steps,[])
    def test_revocation_failure_still_attempts_exact_client_delete(self):
        runtime=object.__new__(bootstrap.Runtime)
        client='wms-workplace-admin-'+uuid.uuid4().hex
        identifier=str(uuid.uuid4())
        calls=[]
        def http(method,path,**kwargs):
            calls.append((method,path))
            if method=='GET': return 200,[{'id':identifier,'clientId':client}]
            if method=='POST': return 500,{}
            return 204,{}
        runtime.http=http
        with self.assertRaises(ValueError):
            runtime.cleanup_client({'client_id':client,'secret':'PRIVATE-TEST'}, {'access_token':'PRIVATE-ACCESS','refresh_token':'PRIVATE-REFRESH'})
        self.assertIn(('DELETE','/auth/admin/realms/master/clients/'+identifier),calls)
    def test_cleanup_rejects_a_different_master_client_without_deleting(self):
        runtime=object.__new__(bootstrap.Runtime)
        calls=[]
        def http(method,path,**kwargs):
            calls.append(method)
            return 200,[{'id':str(uuid.uuid4()),'clientId':'other-admin'}]
        runtime.http=http
        with self.assertRaises(ValueError):
            runtime.cleanup_client({'client_id':'wms-workplace-admin-'+uuid.uuid4().hex,'secret':'PRIVATE-TEST'},{'access_token':'PRIVATE-ACCESS'})
        self.assertEqual(calls,['GET'])


if __name__=='__main__': unittest.main()
