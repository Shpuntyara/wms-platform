"""Offline behavior tests; real Keycloak/PostgreSQL/browser proof is a separate lab run."""
import hashlib
import importlib.util
import json
import os
from pathlib import Path
import tempfile
import unittest
import uuid

SOURCE = Path(__file__).resolve().parents[1] / 'deploy/linux/workplace.py'
spec = importlib.util.spec_from_file_location('workplace_setup', SOURCE)
workplace = importlib.util.module_from_spec(spec)
spec.loader.exec_module(workplace)


def identity(instance=None):
    return {'instance_id': instance or str(uuid.uuid4()), 'address': '192.0.2.12',
            'issuer': 'https://192.0.2.12/auth/realms/wms-test', 'audience': 'wms-api-test'}


class Runtime:
    def __init__(self, value):
        self.identity = value
        self.realm = {'realm': 'wms-test', 'enabled': True, 'registrationAllowed': False}
        self.clients, self.users, self.calls, self.sql_calls = [], [], [], []
        self.publish_calls = []
        self.wrong_instance = False
        self.strict_profile = False
        self.profile = {'attributes':[{'name':'email','required':{'roles':['user']}}], 'groups':[], 'unmanagedAttributePolicy':'DISABLED'}

    def sql(self, statement):
        self.sql_calls.append(statement)
        if 'SELECT id::text FROM hardened_private.installation' in statement:
            return str(uuid.uuid4()) if self.wrong_instance else self.identity['instance_id']
        return ''

    def publish_workspace(self, path, config):
        self.publish_calls.append((path, config))
        Path(path).write_text(json.dumps(config), encoding='utf8')

    def admin(self, method, path, data=None):
        self.calls.append((method, path, data))
        if path == '' and method == 'GET':
            return 200, dict(self.realm)
        if path == '' and method == 'PUT':
            self.realm.update(data)
            return 204, {}
        if path.startswith('/clients?'):
            return 200, [dict(client) for client in self.clients]
        if path == '/clients' and method == 'POST':
            self.clients.append(dict(data, id=str(uuid.uuid4())))
            return 201, {}
        if path.startswith('/clients/') and method == 'GET':
            return 200, next(dict(c) for c in self.clients if c['id'] == path.rsplit('/',1)[1])
        if path.startswith('/users?'):
            username = path.split('username=')[1].split('&')[0]
            return 200, [dict(user) for user in self.users if user['username'] == username]
        if path == '/users' and method == 'POST':
            if self.strict_profile:
                managed = {entry['name'] for entry in self.profile['attributes']}
                data = {**data,'attributes':{key:value for key,value in data['attributes'].items() if key in managed}}
            self.users.append(dict(data, id=str(uuid.uuid4())))
            return 201, {}
        if path == '/users/profile' and method == 'GET':
            return 200, json.loads(json.dumps(self.profile))
        if path == '/users/profile' and method == 'PUT':
            self.profile = json.loads(json.dumps(data))
            return 200, self.profile
        raise AssertionError((method, path))


class WorkplaceTests(unittest.TestCase):
    def test_plan_is_stable_installation_bound_and_has_complete_polish_catalog(self):
        value = identity()
        plan = workplace.build_plan(value, value['instance_id'])
        self.assertIn('instance_id', plan)
        self.assertEqual(plan['instance_id'], value['instance_id'])
        self.assertEqual(plan, workplace.build_plan(value, value['instance_id']))
        catalog = plan['workspace']
        self.assertEqual(len(catalog['items']), 2)
        self.assertEqual({row['kind'] for row in catalog['positions']}, {'receiving','storage'})
        self.assertEqual(len(plan['balances']), 4)
        self.assertIn('Śruba', json.dumps(catalog, ensure_ascii=False))
        self.assertEqual(plan['workspace_path'], '/etc/wms-workplace/workspace.json')
        different = identity()
        other = workplace.build_plan(different, different['instance_id'])
        self.assertNotEqual(plan['workspace']['organizations'][0]['id'], other['workspace']['organizations'][0]['id'])

    def test_identity_and_origin_drift_fail_before_a_plan_can_be_applied(self):
        value = identity()
        for changed in ({**value,'instance_id':str(uuid.uuid4())}, {**value,'issuer':'https://other.invalid/auth/realms/test'},
                        {**value,'address':'127.0.0.1'}, {**value,'issuer':'http://192.0.2.12/auth/realms/test'},
                        {**value,'issuer':value['issuer']+'?token=secret'}, {**value,'audience':''}):
            with self.subTest(changed=changed), self.assertRaises(ValueError):
                workplace.build_plan(changed, value['instance_id'])

    def test_public_client_cannot_bypass_pkce_or_exact_origin(self):
        value = identity()
        plan = workplace.build_plan(value, value['instance_id'])
        self.assertIn('client', plan)
        client = plan['client']
        self.assertTrue(client['publicClient'])
        self.assertTrue(client['standardFlowEnabled'])
        self.assertFalse(client['implicitFlowEnabled'])
        self.assertFalse(client['directAccessGrantsEnabled'])
        self.assertFalse(client['serviceAccountsEnabled'])
        self.assertEqual(client['redirectUris'], ['https://192.0.2.12/app/'])
        self.assertEqual(client['webOrigins'], ['https://192.0.2.12'])
        self.assertEqual(client['attributes']['pkce.code.challenge.method'], 'S256')
        self.assertEqual(client['attributes'].get('post.logout.redirect.uris'), 'https://192.0.2.12/app/')
        self.assertNotIn('secret', client)
        self.assertEqual(client['protocolMappers'][0]['config']['included.client.audience'], value['audience'])

    def test_scope_and_passwords_are_not_in_public_plan(self):
        value = identity()
        plan = workplace.build_plan(value, value['instance_id'])
        self.assertIn('permissions', plan)
        self.assertEqual(plan['permissions']['biuro'], list(workplace.PERMISSIONS['biuro']))
        self.assertNotIn('movement.create', plan['permissions']['biuro'])
        self.assertNotIn('document.create', plan['permissions']['magazynier'])
        self.assertIn('movement.create', plan['permissions']['magazynier'])
        self.assertNotIn('password', json.dumps(plan).lower())

    def test_sql_is_transactional_own_installation_and_never_resets_quantities(self):
        value = identity()
        plan = workplace.build_plan(value, value['instance_id'])
        sql = workplace.seed_sql(plan, {'biuro':str(uuid.uuid4()),'magazynier':str(uuid.uuid4())})
        self.assertIn('BEGIN;', sql)
        self.assertIn('COMMIT;', sql)
        self.assertIn('pg_advisory_xact_lock', sql)
        self.assertIn('hardened_private.installation', sql)
        self.assertIn('Nieoczekiwane dane', sql)
        self.assertIn('ON CONFLICT DO NOTHING', sql)
        self.assertIn('platform.actor_grants', sql)
        self.assertNotIn('DO UPDATE', sql)
        self.assertNotIn('SET on_hand', sql)
        self.assertNotIn('DELETE FROM', sql)
        self.assertNotIn('password', sql.lower())

    def test_apply_needs_explicit_lab_and_checks_database_identity_before_mutation(self):
        runtime = Runtime(identity())
        with self.assertRaises(ValueError):
            workplace.apply(runtime, runtime.identity['instance_id'])
        self.assertEqual(runtime.calls, [])
        runtime.wrong_instance = True
        with tempfile.TemporaryDirectory() as temp, self.assertRaises(ValueError):
            workplace.apply(runtime, runtime.identity['instance_id'], lab=True,
                            credentials_file=Path(temp)/'users.json', workspace_file=Path(temp)/'workspace.json')
        self.assertFalse(any(method != 'GET' for method,_,_ in runtime.calls))

    def test_managed_users_reuse_passwords_and_subjects_without_resetting(self):
        runtime = Runtime(identity())
        with tempfile.TemporaryDirectory() as temp:
            credentials = Path(temp)/'users.json'
            workspace = Path(temp)/'workspace.json'
            first = workplace.apply(runtime, runtime.identity['instance_id'], lab=True,
                                    credentials_file=credentials, workspace_file=workspace)
            self.assertTrue(credentials.exists())
            before = credentials.read_bytes()
            secret = json.loads(before)
            self.assertEqual(set(secret['users']), {'biuro','magazynier'})
            self.assertNotEqual(secret['users']['biuro']['password'], secret['users']['magazynier']['password'])
            self.assertGreaterEqual(min(len(user['password']) for user in secret['users'].values()), 24)
            for user in secret['users'].values():
                uuid.UUID(user['subject'])
                self.assertNotIn(user['password'], json.dumps(first))
                self.assertNotIn(user['password'], workspace.read_text())
            posts_before = sum(method == 'POST' for method,_,_ in runtime.calls)
            second = workplace.apply(runtime, runtime.identity['instance_id'], lab=True,
                                     credentials_file=credentials, workspace_file=workspace)
            self.assertEqual(credentials.read_bytes(), before)
            self.assertEqual(sum(method == 'POST' for method,_,_ in runtime.calls), posts_before)
            self.assertEqual(first['subjects'], second['subjects'])
            self.assertEqual(runtime.realm['defaultLocale'], 'pl')
            self.assertIn('pl', runtime.realm['supportedLocales'])

    def test_existing_unowned_user_or_client_refused_before_changes(self):
        for category in ['users','clients']:
            runtime = Runtime(identity())
            candidate = {'id':str(uuid.uuid4()),'username':'biuro','clientId':'wms-browser','attributes':{}}
            if category == 'clients':
                candidate.update(workplace.build_plan(runtime.identity,runtime.identity['instance_id'])['client'])
                candidate['attributes'] = {key:value for key,value in candidate['attributes'].items() if key != 'wms.workplace.installation'}
            getattr(runtime, category).append(candidate)
            with tempfile.TemporaryDirectory() as temp, self.assertRaises(ValueError):
                workplace.apply(runtime, runtime.identity['instance_id'],lab=True,
                                credentials_file=Path(temp)/'users.json',workspace_file=Path(temp)/'workspace.json')
            self.assertFalse(any(method != 'GET' for method,_,_ in runtime.calls))

    def test_protected_receipt_roundtrips_and_symlinks_are_refused(self):
        with tempfile.TemporaryDirectory() as temp:
            path = Path(temp)/'private.json'
            workplace.write_protected(path, {'marker':'value'})
            self.assertEqual(workplace.read_protected(path), {'marker':'value'})
            if os.name == 'posix':
                self.assertEqual(path.stat().st_mode & 0o777, 0o600)
                path.chmod(0o644)
                with self.assertRaises(ValueError): workplace.read_protected(path)

    def test_owned_client_security_drift_is_rejected_without_mutation(self):
        runtime = Runtime(identity())
        with tempfile.TemporaryDirectory() as temp:
            credentials, workspace = Path(temp)/'users.json', Path(temp)/'workspace.json'
            workplace.apply(runtime,runtime.identity['instance_id'],lab=True,credentials_file=credentials,workspace_file=workspace)
            for field,invalid in [('publicClient',False),('redirectUris',['https://192.0.2.12/*']),('directAccessGrantsEnabled',True)]:
                original = runtime.clients[0][field]
                runtime.clients[0][field] = invalid
                before = len(runtime.calls)
                with self.assertRaises(ValueError):
                    workplace.apply(runtime,runtime.identity['instance_id'],lab=True,credentials_file=credentials,workspace_file=workspace)
                self.assertFalse(any(method != 'GET' for method,_,_ in runtime.calls[before:]))
                runtime.clients[0][field] = original

    def test_failed_user_creation_recovers_saved_passwords_without_reset(self):
        runtime = Runtime(identity())
        original = runtime.admin
        fail_once = True
        def admin(method,path,data=None):
            nonlocal fail_once
            if method == 'POST' and path == '/users' and data['username'] == 'magazynier' and fail_once:
                fail_once = False
                raise ValueError('Simulated unavailable identity provider')
            return original(method,path,data)
        runtime.admin = admin
        with tempfile.TemporaryDirectory() as temp:
            credentials, workspace = Path(temp)/'users.json',Path(temp)/'workspace.json'
            with self.assertRaises(ValueError):
                workplace.apply(runtime,runtime.identity['instance_id'],lab=True,credentials_file=credentials,workspace_file=workspace)
            first = json.loads(credentials.read_text())
            self.assertIsNotNone(first['users']['biuro']['subject'])
            self.assertIsNone(first['users']['magazynier']['subject'])
            self.assertFalse(workspace.exists())
            workplace.apply(runtime,runtime.identity['instance_id'],lab=True,credentials_file=credentials,workspace_file=workspace)
            second = json.loads(credentials.read_text())
            self.assertEqual([u['password'] for u in first['users'].values()], [u['password'] for u in second['users'].values()])

    def test_generated_plans_keep_ids_unique_and_foreign_installations_disjoint(self):
        seen = set()
        for number in range(100):
            value = identity(str(uuid.uuid5(uuid.NAMESPACE_DNS,'workplace-property-'+str(number))))
            plan = workplace.build_plan(value, value['instance_id'])
            self.assertIn('ids', plan)
            ids = set(plan['ids'].values())
            self.assertEqual(len(ids), len(plan['ids']))
            self.assertTrue(seen.isdisjoint(ids))
            seen.update(ids)

    def test_managed_ownership_attribute_works_without_enabling_unmanaged_attributes(self):
        runtime = Runtime(identity())
        runtime.strict_profile = True
        email = json.loads(json.dumps(runtime.profile['attributes'][0]))
        with tempfile.TemporaryDirectory() as temp:
            try:
                workplace.apply(runtime,runtime.identity['instance_id'],lab=True,
                                credentials_file=Path(temp)/'users.json',workspace_file=Path(temp)/'workspace.json')
            except ValueError as error:
                self.fail('Managed ownership attribute was not configured before user creation: '+str(error))
        marker = next(attr for attr in runtime.profile['attributes'] if attr['name']=='wms.workplace.installation')
        self.assertEqual(marker['permissions'], {'view':['admin'],'edit':['admin']})
        self.assertEqual(runtime.profile['unmanagedAttributePolicy'],'DISABLED')
        self.assertEqual(runtime.profile['attributes'][0],email)


if __name__ == '__main__':
    unittest.main()
