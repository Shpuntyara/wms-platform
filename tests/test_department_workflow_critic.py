"""Independent negative provisioning tests; all identities and files are owned fixtures."""
import copy
from contextlib import contextmanager
import hashlib
import json
import os
from pathlib import Path
import stat
import unittest
import urllib.parse
from unittest.mock import patch

import test_department_workflow as primary
import test_department_workflow_postgres as native

workflow=primary.workflow

class DepartmentWorkflowProvisioningCritic(unittest.TestCase):
    setUp=primary.DepartmentProvisioningTests.setUp
    apply=primary.DepartmentProvisioningTests.apply

    def assert_no_writes(self):
        self.assertFalse(any(method in ('POST','PUT','DELETE') for method,_,_ in self.runtime.calls),'Rejected configuration mutated the identity provider')
        self.assertFalse(any('INSERT INTO' in statement for statement in self.runtime.statements),'Rejected configuration granted database rights')
        self.assertIsNone(self.runtime.published)

    def password_fingerprints(self):
        state=json.loads(self.staff.read_text(encoding='utf8'))
        return {name:hashlib.sha256(row['password'].encode()).hexdigest() for name,row in state['users'].items()}

    @contextmanager
    def owned_symlink(self,path,target):
        # Linux CI exercises the OS link. An unprivileged Windows test runner
        # models the same lstat/read boundary instead of skipping the assertion.
        try:
            path.symlink_to(target)
        except OSError as error:
            if os.name!='nt' or getattr(error,'winerror',None)!=1314:raise
            path.write_bytes(target.read_bytes())
            original_lstat,original_read=Path.lstat,Path.read_text
            def lstat(candidate,*args,**kwargs):
                info=original_lstat(candidate,*args,**kwargs)
                if candidate==path:
                    values=list(info);values[0]=stat.S_IFLNK|0o777;return os.stat_result(values)
                return info
            def read(candidate,*args,**kwargs):return original_read(target if candidate==path else candidate,*args,**kwargs)
            with patch.object(Path,'lstat',lstat),patch.object(Path,'read_text',read):yield
        else:
            yield

    def test_unknown_committed_keycloak_post_resumes_without_recreating_or_resetting_accounts(self):
        original=self.runtime.admin
        lost=[False]
        def admin(method,path,data=None):
            result=original(method,path,data)
            if method=='POST' and path=='/users' and not lost[0]:
                lost[0]=True
                raise ConnectionResetError('Synthetic reply loss after account creation')
            return result
        self.runtime.admin=admin
        with self.assertRaises(ConnectionResetError):self.apply()
        before=self.password_fingerprints()
        state=json.loads(self.staff.read_text(encoding='utf8'))
        self.assertTrue(state['users']['jan.kowalski']['creation_attempted'])
        self.assertIsNone(state['users']['jan.kowalski']['subject'])
        self.apply()
        self.assertTrue(self.password_fingerprints()==before,'Unknown create outcome changed saved credentials')
        created=[data['username'] for method,path,data in self.runtime.calls if method=='POST' and path=='/users']
        self.assertEqual(sorted(created),sorted(row[0] for row in workflow.EMPLOYEES))
        self.assertFalse(any('reset-password' in path for _,path,_ in self.runtime.calls))

    def test_confirmation_read_failure_resumes_owned_account_without_second_post(self):
        original=self.runtime.admin
        created=[False];lost=[False]
        def admin(method,path,data=None):
            if method=='GET' and path.startswith('/users?') and created[0] and not lost[0]:
                lost[0]=True;raise ConnectionResetError('Synthetic confirmation read failure')
            result=original(method,path,data)
            if method=='POST' and path=='/users':created[0]=True
            return result
        self.runtime.admin=admin
        with self.assertRaises(ConnectionResetError):self.apply()
        before=self.password_fingerprints();self.apply()
        posts=[data['username'] for method,path,data in self.runtime.calls if method=='POST' and path=='/users']
        self.assertEqual(len(posts),4);self.assertEqual(len(set(posts)),4)
        self.assertTrue(self.password_fingerprints()==before,'Confirmation recovery changed credentials')

    def assert_create_confirmation_refuses_drift(self,change):
        original=self.runtime.admin
        def admin(method,path,data=None):
            result=original(method,path,data)
            if method=='POST' and path=='/users':change(self.runtime.accounts[data['username']])
            return result
        self.runtime.admin=admin
        with self.assertRaises(ValueError,msg='A successful POST is not proof of the subsequently observed employee identity'):self.apply()
        self.assertFalse(any('INSERT INTO' in statement for statement in self.runtime.statements),'Unverified account received database rights')
        self.assertIsNone(self.runtime.published)

    def test_post_create_confirmation_rechecks_installation_ownership(self):
        self.assert_create_confirmation_refuses_drift(lambda account:account['attributes'].__setitem__('wms.workplace.installation',[primary.ident(999)]))

    def test_post_create_confirmation_rechecks_exact_username(self):
        self.assert_create_confirmation_refuses_drift(lambda account:account.__setitem__('username','unrelated.employee'))

    def test_post_create_confirmation_rechecks_enabled_identity(self):
        self.assert_create_confirmation_refuses_drift(lambda account:account.__setitem__('enabled',False))

    def test_post_create_confirmation_rechecks_named_employee(self):
        self.assert_create_confirmation_refuses_drift(lambda account:account.__setitem__('lastName','Inna osoba'))

    def test_duplicate_private_credentials_are_refused_without_remote_or_database_writes(self):
        self.apply();state=json.loads(self.staff.read_text(encoding='utf8'))
        state['users']['anna.zielinska']['password']=state['users']['jan.kowalski']['password']
        self.staff.write_text(json.dumps(state),encoding='utf8')
        before=hashlib.sha256(self.staff.read_bytes()).hexdigest()
        self.runtime.calls=[];self.runtime.statements=[];self.runtime.published=None
        with self.assertRaises(ValueError,msg='Corrupted duplicate credentials were silently trusted'):self.apply()
        self.assert_no_writes();self.assertEqual(hashlib.sha256(self.staff.read_bytes()).hexdigest(),before)

    def test_preflight_last_employee_conflict_prevents_first_account_creation(self):
        self.runtime.accounts['marek.wisniewski']={'id':primary.ident(999),'username':'marek.wisniewski','enabled':True,'attributes':{}}
        with self.assertRaises(ValueError):self.apply()
        self.assert_no_writes()

    def test_duplicate_keycloak_lookup_is_refused_before_creation(self):
        original=self.runtime.admin
        def admin(method,path,data=None):
            if method=='GET' and path.startswith('/users?'):
                username=urllib.parse.parse_qs(urllib.parse.urlsplit(path).query)['username'][0]
                if username=='marek.wisniewski':return 200,[{'id':primary.ident(501)},{'id':primary.ident(502)}]
            return original(method,path,data)
        self.runtime.admin=admin
        with self.assertRaises(ValueError):self.apply()
        self.assert_no_writes()

    def test_private_provisioning_marker_with_user_edit_permission_is_refused(self):
        self.runtime.profile={'attributes':[{'name':'wms.department.provision','displayName':'Identyfikator konfiguracji działów','multivalued':False,
            'permissions':{'view':['admin'],'edit':['admin','user']},'validations':{'length':{'min':36,'max':36}}}]}
        with self.assertRaises(ValueError):self.apply()
        self.assert_no_writes()

    def test_database_failure_preserves_previous_workplace_and_saved_new_account_credentials(self):
        old=hashlib.sha256(self.existing.read_bytes()).hexdigest()
        original=self.runtime.sql
        def sql(statement):
            if statement.startswith('BEGIN;'):raise ConnectionResetError('Synthetic database configuration failure')
            return original(statement)
        self.runtime.sql=sql
        with self.assertRaises(ConnectionResetError):self.apply()
        before=self.password_fingerprints();self.runtime.sql=original;self.runtime.calls=[];self.apply()
        self.assertTrue(self.password_fingerprints()==before,'Database recovery reset a new employee password')
        self.assertEqual(hashlib.sha256(self.existing.read_bytes()).hexdigest(),old,'Existing workplace credentials changed')
        self.assertFalse(any(method=='POST' for method,_,_ in self.runtime.calls))

    def test_private_credentials_symlink_is_refused_before_any_identity_mutation(self):
        target=self.base/'private-target.json';target.write_text('{}',encoding='utf8');target.chmod(0o600)
        before=target.read_bytes()
        with self.owned_symlink(self.staff,target),self.assertRaises(ValueError):self.apply()
        self.assert_no_writes();self.assertTrue(target.read_bytes()==before,'Private symlink target was modified')

    def test_workspace_symlink_is_refused_before_creating_accounts_or_sql_grants(self):
        target=self.base/'workspace-target.json';self.catalog.rename(target)
        before=hashlib.sha256(target.read_bytes()).hexdigest()
        with self.owned_symlink(self.catalog,target),self.assertRaises(ValueError,msg='Workspace symlink validation happened after external mutations'):self.apply()
        self.assert_no_writes();self.assertEqual(hashlib.sha256(target.read_bytes()).hexdigest(),before)

    def test_conflicting_existing_role_actor_ids_cannot_turn_bureau_into_warehouse_sender(self):
        users=copy.deepcopy(self.users);users['ids']['actor_magazynier']=users['ids']['actor_biuro']
        with self.assertRaises(ValueError,msg='Corrupted old-role identity would grant bureau warehouse custody rights'):
            workflow.build_plan(self.identity,users,self.workspace,self.identity['instance_id'])


class DepartmentWorkflowNativePrivilegeCritic(unittest.TestCase):
    @classmethod
    def setUpClass(cls):native.DepartmentSetupPostgresTests.setUpClass.__func__(cls)
    setUp=native.DepartmentSetupPostgresTests.setUp
    sql=native.DepartmentSetupPostgresTests.sql
    snapshot=native.DepartmentSetupPostgresTests.snapshot

    def assert_unexpected_employee_grant_is_rejected(self,permission,all_owners=False):
        self.sql(workflow.seed_sql(self.plan,self.subjects))
        person=self.plan['employees'][0];q=workflow.literal
        owner='NULL' if all_owners else q(self.plan['owner_id'])
        self.sql('INSERT INTO platform.actor_grants(actor_id,organization_id,warehouse_id,owner_id,permission) VALUES('+','.join([q(person['actor_id']),q(self.plan['organization_id']),q(self.plan['warehouse_id']),owner,q(permission)])+');')
        before=self.snapshot()
        self.sql(workflow.seed_sql(self.plan,self.subjects),success=False)
        self.assertEqual(self.snapshot(),before,'Rejected privilege drift changed stock, products or documents')

    def test_real_seed_refuses_generic_movement_grant_on_new_employee(self):
        self.assert_unexpected_employee_grant_is_rejected('movement.create')

    def test_real_seed_refuses_extra_document_read_grant_on_new_employee(self):
        self.assert_unexpected_employee_grant_is_rejected('document.read')

    def test_real_seed_refuses_wildcard_owner_handoff_grant_on_new_employee(self):
        self.assert_unexpected_employee_grant_is_rejected('handoff.send',all_owners=True)

if __name__=='__main__':unittest.main()
