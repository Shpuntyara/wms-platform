"""Industrial fixtures are additive, scoped and replay real opening receipts."""
import copy
import concurrent.futures
from decimal import Decimal
import hashlib
import importlib.util
import json
import os
from pathlib import Path
import random
import re
import subprocess
import sys
import unittest
import urllib.parse
import uuid

ROOT = Path(__file__).resolve().parents[1]
sys.path.insert(0, str(ROOT / 'deploy/linux'))
sys.path.insert(0, str(ROOT / 'scripts'))
import workplace
import industry_catalog as industry
from migrate_core import build_script


def fixture(instance=None):
    instance = str(instance or uuid.uuid4())
    identity = dict(instance_id=instance, address='192.0.2.10',
                    issuer='https://192.0.2.10/auth/realms/wms-' + instance[:8], audience='wms-test')
    legacy = workplace.build_plan(identity, instance)
    scope = {key + '_id': legacy['ids'][key] for key in ['organization', 'warehouse', 'owner']}
    scope.update(instance_id=instance, position_receiving_id=legacy['ids']['position_receiving'],
                 location_receiving_id=legacy['ids']['location_receiving'])
    return scope, legacy


class IndustryCatalogContract(unittest.TestCase):
    def application_inputs(self):
        scope, legacy = fixture()
        identity=dict(instance_id=scope['instance_id'],address='192.0.2.10',issuer=legacy['issuer'],audience=legacy['audience'])
        state=dict(schema=1,instance_id=scope['instance_id'],origin=legacy['origin'],ids=copy.deepcopy(legacy['ids']),
                   users={name:dict(username=name,password='not-a-secret-fixture-password',subject=str(uuid.uuid4())) for name in ['biuro','magazynier']})
        return identity,state,copy.deepcopy(legacy['workspace'])

    def test_apply_rejects_stale_origin_before_any_database_change(self):
        identity,state,workspace=self.application_inputs()
        state['origin']='https://192.0.2.11'
        runtime=RecordingRuntime()
        with self.assertRaises(ValueError):
            industry.apply(runtime,'metal',identity,state,workspace,identity['instance_id'])
        self.assertEqual(runtime.calls,[])

    def test_apply_rejects_foreign_workspace_before_any_database_change(self):
        identity,state,workspace=self.application_inputs()
        workspace['oidc']['realm']='different-realm'
        runtime=RecordingRuntime()
        with self.assertRaises(ValueError):
            industry.apply(runtime,'metal',identity,state,workspace,identity['instance_id'])
        self.assertEqual(runtime.calls,[])

    def test_apply_rejects_changed_private_scope_before_any_database_change(self):
        identity,state,workspace=self.application_inputs()
        state['ids']['organization']=str(uuid.uuid4())
        runtime=RecordingRuntime()
        with self.assertRaises(ValueError):
            industry.apply(runtime,'metal',identity,state,workspace,identity['instance_id'])
        self.assertEqual(runtime.calls,[])

    def test_metal_catalog_is_a_factory_with_dimensions_and_production_recipe(self):
        scope, _ = fixture()
        plan = industry.build_plan('metal', scope)
        self.assertEqual(plan['company'], 'METALFORM')
        self.assertGreaterEqual(len(plan['workspace']['items']), 20)
        names = ' '.join(item['name'] for item in plan['workspace']['items'])
        for word in ['Śruba M6', 'Śruba M8', 'Śruba M10', 'Nit', 'Formatka aluminiowa', 'Szafa metalowa']:
            self.assertIn(word, names)
        self.assertRegex(names, r'500 × 300 × 2 mm')
        self.assertTrue(any(recipe['steps'] == ['Spawanie', 'Szlifowanie', 'Lakierowanie', 'Montaż']
                            for recipe in plan['workspace']['recipes']))

    def test_stationery_has_color_size_year_and_leftover_composition(self):
        scope, _ = fixture()
        plan = industry.build_plan('stationery', scope)
        self.assertEqual(plan['company'], 'PAPIERNIK')
        self.assertGreaterEqual(len(plan['workspace']['items']), 20)
        names = ' '.join(item['name'] for item in plan['workspace']['items'])
        for word in ['Notes 2027 czerwony A5', 'Notes 2027 biały A6', 'Notes 2026 czerwony A5',
                     'Wkład do notesu A5', 'Kalendarz 2027']:
            self.assertIn(word, names)
        recipe = next(row for row in plan['workspace']['recipes'] if '2026' in row['name'])
        by_id = {row['id']: row for row in plan['workspace']['items']}
        component_names = [by_id[row['item_id']]['name'] for row in recipe['components']]
        self.assertTrue(any('2026' in name for name in component_names))
        self.assertTrue(any('Wkład' in name for name in component_names))

    def test_ten_cabinets_have_independent_material_requirements_and_visible_shortages(self):
        scope,_=fixture()
        plan=industry.build_plan('metal',scope)
        by_id={item['id']:item['sku'] for item in plan['workspace']['items']}
        recipe=next(r for r in plan['workspace']['recipes'] if by_id[r['product_item_id']]=='SZAFA-1800')
        required={by_id[c['item_id']]:Decimal(c['quantity'])*10 for c in recipe['components']}
        available={by_id[row['item_id']]:Decimal(row['quantity']) for row in plan['opening']}
        for sku,need,shortage in [('SR-M8X40',240,0),('NAK-M8',240,100),('POD-M8',480,0),
                                  ('ST-1800X800X15',20,2),('ZAM-SZAFA',10,2),('LAK-RAL7035',10,1)]:
            self.assertEqual(required[sku],Decimal(need),sku)
            self.assertEqual(max(Decimal(0),required[sku]-available.get(sku,Decimal(0))),Decimal(shortage),sku)

    def test_known_materials_and_leftovers_have_concrete_stock_places(self):
        expected={'metal':{'SR-M6X20':('A1','1200'),'AL-500X300X2':('B1','70'),
                           'ST-1800X800X15':('B2','18'),'ZAM-SZAFA':('A4','8')},
                  'stationery':{'NOT-27-CZ-A5':('A1','240'),'NOT-26-CZ-A5':('A4','37'),
                                'WKL-A5-KR':('B2','350'),'KAL-27-SC-A2':('B4','70')}}
        for profile,want in expected.items():
            scope,_=fixture()
            plan=industry.build_plan(profile,scope)
            sku_by_id={row['id']:row['sku'] for row in plan['workspace']['items']}
            position_by_id={row['id']:row['code'] for row in plan['workspace']['positions']}
            actual={sku_by_id[row['item_id']]:(position_by_id[row['position_id']],row['quantity']) for row in plan['opening']}
            for sku,place_quantity in want.items():
                self.assertEqual(actual.get(sku),place_quantity,sku)

    def test_example_orders_are_scope_bound_and_compute_real_materials(self):
        for profile in ['metal','stationery']:
            scope,_=fixture()
            plan=industry.build_plan(profile,scope)
            examples=plan.get('sample_orders',[])
            self.assertEqual(len(examples),3,'Each factory needs visible real example orders')
            self.assertEqual(len({row['command_id'] for row in examples}),3)
            by_id={row['id']:row['sku'] for row in plan['workspace']['items']}
            for index,example in enumerate(examples,1):
                snapshot=example['snapshot']
                self.assertEqual(snapshot['reference'],'ZAM/PRZYKLAD/'+str(index).zfill(3))
                for field in ['organization_id','warehouse_id','owner_id']:
                    self.assertEqual(snapshot[field],scope[field])
                    self.assertEqual(snapshot['request'][field],scope[field])
                self.assertTrue(snapshot['customer'])
                self.assertTrue(snapshot['lines'])
                self.assertTrue(snapshot['requirements'])
                self.assertEqual(snapshot['request']['lines'],snapshot['lines'])
                self.assertTrue(all(row['item_id'] in by_id and Decimal(row['quantity'])>0 for row in snapshot['requirements']))
            if profile=='metal':
                first=examples[0]['snapshot']
                self.assertIn('ALFA',first['customer'])
                self.assertEqual(first['lines'][0]['quantity'],'10')
                self.assertEqual(by_id[first['lines'][0]['item_id']],'SZAFA-1800')
                required={by_id[row['item_id']]:row['quantity'] for row in first['requirements']}
                self.assertEqual(required['SR-M8X40'],'240')
                self.assertEqual(required['ST-1800X800X15'],'20')
                self.assertEqual(first['steps'],['Spawanie','Szlifowanie','Lakierowanie','Montaż'])
            else:
                dispatch=examples[0]['snapshot']
                self.assertGreaterEqual(len(dispatch['lines']),4)
                leftovers=examples[1]['snapshot']
                required={by_id[row['item_id']] for row in leftovers['requirements']}
                self.assertIn('NOT-26-CZ-A5',required)
                self.assertIn('WKL-A5-KR',required)

    def test_storage_codes_mean_section_and_level(self):
        for profile in ['metal', 'stationery']:
            scope, _ = fixture()
            positions = industry.build_plan(profile, scope)['workspace']['positions']
            by_code = {row['code']: row for row in positions}
            self.assertEqual(set(by_code), {letter + str(level) for letter in 'AB' for level in range(1, 5)}
                             | {'PRZYJECIE-01', 'WYDANIE-01'})
            self.assertEqual(by_code['PRZYJECIE-01']['id'], scope['position_receiving_id'])
            self.assertEqual(by_code['B4']['name'], 'Regał B · poziom 4')

    def test_merge_keeps_historical_labels_and_rows_except_selected_company_labels(self):
        scope, legacy = fixture()
        before = copy.deepcopy(legacy['workspace'])
        plan = industry.build_plan('metal', scope)
        merged = industry.merge_workspace(before, plan)
        self.assertEqual(before, legacy['workspace'])
        self.assertEqual(merged['oidc'], before['oidc'])
        self.assertEqual(merged['warehouses'][0]['name'], 'METALFORM · magazyn materiałów i wyrobów')
        by_id = {row['id']: row for row in merged['items']}
        for row in before['items']:
            self.assertEqual(by_id[row['id']], dict(row,archived=True))
        self.assertIn('A-01-02', {row['code'] for row in merged['positions']})
        self.assertEqual(len(merged['items']), len(before['items']) + len(plan['workspace']['items']))
        self.assertEqual(industry.merge_workspace(merged, plan), merged)

    def test_only_the_two_known_legacy_items_are_archived_without_data_changes(self):
        scope,legacy=fixture()
        before=copy.deepcopy(legacy['workspace'])
        custom=dict(before['items'][0],id=str(uuid.uuid4()),sku='SR-WLASNA',name='Śruba klienta')
        before['items'].append(custom)
        merged=industry.merge_workspace(before,industry.build_plan('stationery',scope))
        by_id={row['id']:row for row in merged['items']}
        self.assertEqual(by_id[custom['id']],custom)
        for original in legacy['workspace']['items']:
            self.assertIs(by_id[original['id']].get('archived'),True)
            self.assertEqual({key:value for key,value in by_id[original['id']].items() if key!='archived'},original)
        self.assertFalse(any(row.get('archived') for row in industry.build_plan('stationery',scope)['workspace']['items']))

    def test_unknown_uuid_with_legacy_sku_is_not_silently_archived(self):
        scope,legacy=fixture()
        current=copy.deepcopy(legacy['workspace'])
        current['items'][0]['id']=str(uuid.uuid4())
        unknown=copy.deepcopy(current['items'][0])
        merged=industry.merge_workspace(current,industry.build_plan('metal',scope))
        actual=next(row for row in merged['items'] if row['id']==unknown['id'])
        self.assertEqual(actual,unknown,'Matching an old SKU alone must not archive another product')

    def test_merge_refuses_conflicting_catalog_instead_of_relabelling_other_data(self):
        scope, legacy = fixture()
        plan = industry.build_plan('metal', scope)
        corrupt = copy.deepcopy(legacy['workspace'])
        corrupt['items'].append(dict(plan['workspace']['items'][0], sku='OTHER-SKU'))
        with self.assertRaises(ValueError):
            industry.merge_workspace(corrupt, plan)

    def test_seed_is_transactional_audited_and_does_not_set_stock_amounts(self):
        scope, _ = fixture()
        sql = industry.seed_sql(industry.build_plan('metal', scope))
        self.assertTrue(sql.startswith('BEGIN;'))
        self.assertTrue(sql.rstrip().endswith('COMMIT;'))
        self.assertIn('warehouse.command_document', sql)
        self.assertIn('inventory.post_movement', sql)
        self.assertNotRegex(sql, r'(?is)UPDATE\s+inventory\.stock_balances\s+SET\s+on_hand')
        self.assertNotIn('DISABLE TRIGGER', sql)
        self.assertIn('hardened_private.installation', sql)
        self.assertIn('DELETE FROM platform.actor_grants', sql)

    def test_plan_properties_for_many_scopes_and_both_profiles(self):
        rng = random.Random(20261029)
        all_product_ids = set()
        for _ in range(24):
            scope, _ = fixture(uuid.UUID(int=rng.getrandbits(128)))
            for profile in ['metal', 'stationery']:
                plan = industry.build_plan(profile, scope)
                self.assertEqual(plan, industry.build_plan(profile, scope))
                ids = {row['id'] for row in plan['workspace']['items']}
                self.assertFalse(ids & all_product_ids)
                all_product_ids.update(ids)
                self.assertTrue(all(row['organization_id'] == scope['organization_id'] for row in plan['workspace']['items']))
                self.assertTrue(all(row['warehouse_id'] == scope['warehouse_id'] for row in plan['workspace']['positions']))
                self.assertEqual(len(plan['balances']), len(ids) * 10)
                for opening in plan['opening']:
                    self.assertIn(opening['item_id'], ids)
                    self.assertRegex(opening['quantity'], r'^[1-9][0-9]*$')
                for recipe in plan['workspace']['recipes']:
                    self.assertIn(recipe['product_item_id'], ids)
                    self.assertEqual(len({c['item_id'] for c in recipe['components']}), len(recipe['components']))
                    for component in recipe['components']:
                        self.assertIn(component['item_id'], ids)
                        self.assertRegex(component['quantity'], r'^[1-9][0-9]*$')
                self.assertTrue(plan['opening'])

    def test_unknown_profile_or_bad_scope_rejected(self):
        scope, _ = fixture()
        with self.assertRaises(ValueError):
            industry.build_plan('other', scope)
        for field in scope:
            with self.subTest(field=field), self.assertRaises(ValueError):
                industry.build_plan('metal', dict(scope, **{field: 'not-an-id'}))


class IndustryCatalogPostgres(unittest.TestCase):
    @classmethod
    def setUpClass(cls):
        raw = os.environ.get('WMS_TEST_ADMIN_DSN')
        if not raw:
            raise RuntimeError('WMS_TEST_ADMIN_DSN is required: real seed/replay checks must not be skipped')
        dsn = urllib.parse.urlsplit(raw)
        if dsn.hostname not in ('localhost', '127.0.0.1') or dsn.username != 'wms_test_admin':
            raise RuntimeError('Only the owned disposable local PostgreSQL test administrator is permitted')
        cls.psql = os.environ.get('WMS_TEST_PSQL', 'psql')
        cls.base = [cls.psql, '-X', '-q', '-At', '-v', 'ON_ERROR_STOP=1', '-h', dsn.hostname,
                    '-p', str(dsn.port or 5432), '-U', dsn.username]
        cls.database = 'wms_industry_test_' + uuid.uuid4().hex
        cls.sql('CREATE DATABASE ' + cls.database + ' OWNER wms_test_owner;', database='postgres')
        try:
            cls.sql('SET ROLE wms_test_owner;\n' + build_script(ROOT, through='0013'))
            cls.sql('SET ROLE wms_test_owner; CREATE SCHEMA hardened_private; CREATE TABLE hardened_private.installation(singleton boolean PRIMARY KEY DEFAULT true CHECK(singleton),id uuid NOT NULL);')
        except Exception:
            cls.sql('DROP DATABASE ' + cls.database + ';', database='postgres')
            raise

    @classmethod
    def tearDownClass(cls):
        cls.sql('DROP DATABASE ' + cls.database + ';', database='postgres')

    @classmethod
    def sql(cls, statement, *, database=None, must=True):
        result = subprocess.run(cls.base + ['-d', database or cls.database], input=statement,
                                capture_output=True, text=True, encoding='utf8', timeout=60)
        if must and result.returncode:
            raise AssertionError(result.stderr[-4000:])
        if not must:
            return result
        return result.stdout.strip()

    def prepare(self, profile):
        scope, legacy = fixture()
        # Each test scenario owns one synthetic installation marker in its isolated DB.
        self.sql('TRUNCATE hardened_private.installation; INSERT INTO hardened_private.installation(id) VALUES(' + workplace.sql_literal(scope['instance_id']) + ');')
        base = workplace.seed_sql(legacy)
        self.sql('SET ROLE wms_test_owner;\n' + base)
        return scope, industry.build_plan(profile, scope)

    def stock(self, plan):
        return json.loads(self.sql("SELECT coalesce(jsonb_object_agg(id::text,jsonb_build_array(on_hand::text,reserved_qty::text,version)),'{}') FROM inventory.stock_balances WHERE organization_id=" + workplace.sql_literal(plan['scope']['organization_id']) + ';'))

    def test_actual_postgres_opening_replay_preserves_later_activity_and_no_fixture_grants(self):
        for profile in ['metal', 'stationery']:
            scope, plan = self.prepare(profile)
            self.sql('SET ROLE wms_test_owner;\n' + industry.seed_sql(plan))
            actual = self.stock(plan)
            self.assertGreater(len(actual), 200)
            for opening in plan['opening']:
                self.assertEqual(actual[opening['balance_id']][0], opening['quantity'] + '.000000')
            self.assertEqual(self.sql('SELECT count(*) FROM platform.actor_grants WHERE actor_id=' + workplace.sql_literal(plan['actor_id']) + ';'), '0')
            before = self.stock(plan)
            audit_count = self.sql('SELECT count(*) FROM inventory.movements WHERE organization_id=' + workplace.sql_literal(scope['organization_id']) + ';')
            self.sql('SET ROLE wms_test_owner;\n' + industry.seed_sql(plan))
            self.assertEqual(self.stock(plan), before)
            self.assertEqual(self.sql('SELECT count(*) FROM inventory.movements WHERE organization_id=' + workplace.sql_literal(scope['organization_id']) + ';'), audit_count)
            first = plan['opening'][0]
            # A real later transfer uses the kernel; replaying opening must not restore original quantities.
            target = next(b for b in plan['balances'] if b['item_id'] == first['item_id'] and b['position_id'] != first['position_id'])
            doc = str(uuid.uuid4()); line = str(uuid.uuid4()); cmd = str(uuid.uuid4())
            literal = workplace.sql_literal
            self.sql('SET ROLE wms_test_owner; BEGIN; INSERT INTO warehouse.documents(id,organization_id,warehouse_id,owner_id,kind,number,state) VALUES(' + ','.join(literal(v) for v in [doc,scope['organization_id'],scope['warehouse_id'],scope['owner_id'],'transfer','MM/TEST/'+doc,'active']) + '); INSERT INTO warehouse.document_lines(id,document_id,organization_id,warehouse_id,owner_id,item_id,line_no,requested_qty) VALUES(' + ','.join(literal(v) for v in [line,doc,scope['organization_id'],scope['warehouse_id'],scope['owner_id'],first['item_id']]) + ',1,1); SELECT inventory.post_movement(' + ','.join(literal(v) for v in [cmd,plan['actor_id'],line]) + ',1,\'transfer\',' + literal(first['balance_id']) + ',' + literal(target['id']) + ',1); COMMIT;')
            moved = self.stock(plan)
            self.assertNotEqual(moved, before)
            self.sql('SET ROLE wms_test_owner;\n' + industry.seed_sql(plan))
            self.assertEqual(self.stock(plan), moved)

    def test_actual_postgres_catalog_drift_rejects_atomic_seed(self):
        scope, plan = self.prepare('metal')
        self.assertGreaterEqual(len(plan['workspace']['items']), 4, 'Industrial seed must contain enough real catalog items to exercise drift')
        row = plan['workspace']['items'][3]
        literal = workplace.sql_literal
        self.sql('INSERT INTO catalog.items(id,organization_id,sku,base_unit,quantity_scale,tracking) VALUES(' + ','.join(literal(v) for v in [row['id'],scope['organization_id'],'CONFLICT','workplace-'+scope['instance_id']+'-szt']) + ',0,\'none\');')
        before = self.stock(plan)
        result = self.sql('SET ROLE wms_test_owner;\n' + industry.seed_sql(plan), must=False)
        self.assertNotEqual(result.returncode, 0)
        self.assertIn('Nieoczekiwane dane', result.stderr)
        self.assertEqual(self.stock(plan), before)
        self.assertEqual(self.sql('SELECT count(*) FROM catalog.items WHERE organization_id=' + literal(scope['organization_id']) + ';'), '3')

    def test_actual_postgres_interrupted_opening_rolls_back_all_previous_lines_and_retries(self):
        scope, plan=self.prepare('stationery')
        literal=workplace.sql_literal
        function='reject_industry_'+uuid.uuid4().hex
        third=plan['opening'][2]['item_id']
        self.sql('SET ROLE wms_test_owner; CREATE FUNCTION public.'+function+"() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN IF NEW.item_id="+literal(third)+"::uuid THEN RAISE EXCEPTION 'injected_opening_interruption'; END IF; RETURN NEW; END $$; CREATE TRIGGER "+function+' BEFORE INSERT ON inventory.movements FOR EACH ROW EXECUTE FUNCTION public.'+function+'();')
        before=self.stock(plan)
        try:
            result=self.sql('SET ROLE wms_test_owner;\n'+industry.seed_sql(plan),must=False)
            self.assertNotEqual(result.returncode,0)
            self.assertIn('injected_opening_interruption',result.stderr)
            self.assertEqual(self.stock(plan),before)
            self.assertEqual(self.sql('SELECT count(*) FROM inventory.movements WHERE organization_id='+literal(scope['organization_id'])+';'),'0')
            self.assertEqual(self.sql('SELECT count(*) FROM platform.operations WHERE organization_id='+literal(scope['organization_id'])+';'),'0')
            self.assertEqual(self.sql('SELECT count(*) FROM platform.actor_grants WHERE actor_id='+literal(plan['actor_id'])+';'),'0')
        finally:
            self.sql('DROP TRIGGER '+function+' ON inventory.movements; DROP FUNCTION public.'+function+'();')
        self.sql('SET ROLE wms_test_owner;\n'+industry.seed_sql(plan))
        self.assertEqual(self.stock(plan)[plan['opening'][0]['balance_id']][0],plan['opening'][0]['quantity']+'.000000')

    def test_actual_postgres_concurrent_seed_posts_one_opening(self):
        scope,plan=self.prepare('metal')
        statement='SET ROLE wms_test_owner;\n'+industry.seed_sql(plan)
        with concurrent.futures.ThreadPoolExecutor(max_workers=2) as pool:
            results=list(pool.map(lambda _:self.sql(statement),range(2)))
        self.assertEqual(len(results),2)
        literal=workplace.sql_literal
        self.assertEqual(self.sql('SELECT count(*) FROM inventory.movements WHERE organization_id='+literal(scope['organization_id'])+';'),str(len(plan['opening'])))
        self.assertEqual(self.sql('SELECT count(*) FROM warehouse.documents WHERE organization_id='+literal(scope['organization_id'])+';'),'1')
        self.assertEqual(self.sql('SELECT count(*) FROM platform.actor_grants WHERE actor_id='+literal(plan['actor_id'])+';'),'0')

    def test_actual_postgres_foreign_installation_and_fixture_privileges_fail_closed(self):
        scope,plan=self.prepare('metal')
        literal=workplace.sql_literal
        self.sql('UPDATE hardened_private.installation SET id='+literal(str(uuid.uuid4()))+';')
        denied=self.sql('SET ROLE wms_test_owner;\n'+industry.seed_sql(plan),must=False)
        self.assertNotEqual(denied.returncode,0)
        self.assertIn('Niezgodna instalacja',denied.stderr)
        self.sql('UPDATE hardened_private.installation SET id='+literal(scope['instance_id'])+';')
        self.sql("INSERT INTO platform.actors(id,issuer,subject) VALUES("+','.join(literal(v) for v in [plan['actor_id'],'wms-industry-fixture',scope['instance_id']+'/'+plan['profile']+'/v1'])+'); INSERT INTO platform.actor_grants(actor_id,organization_id,warehouse_id,owner_id,permission) VALUES('+','.join(literal(v) for v in [plan['actor_id'],scope['organization_id'],scope['warehouse_id'],scope['owner_id'],'document.create'])+');')
        denied=self.sql('SET ROLE wms_test_owner;\n'+industry.seed_sql(plan),must=False)
        self.assertNotEqual(denied.returncode,0)
        self.assertIn('Nieoczekiwane uprawnienia',denied.stderr)
        self.assertEqual(self.sql('SELECT count(*) FROM platform.actor_grants WHERE actor_id='+literal(plan['actor_id'])+';'),'1')
        self.assertEqual(self.sql('SELECT count(*) FROM inventory.movements WHERE organization_id='+literal(scope['organization_id'])+';'),'0')

    def test_actual_postgres_sample_plans_persist_once_and_match_fixture_requirements(self):
        for profile in ['metal','stationery']:
            scope,plan=self.prepare(profile)
            self.sql('SET ROLE wms_test_owner;\n'+industry.seed_sql(plan))
            rows=json.loads(self.sql("SELECT coalesce(jsonb_agg(to_jsonb(o) ORDER BY reference),'[]') FROM warehouse.work_orders o WHERE organization_id="+workplace.sql_literal(scope['organization_id'])+';'))
            self.assertEqual(len(rows),3,'Opening catalog must persist real orders, not UI-only placeholders')
            for actual,expected in zip(rows,plan['sample_orders']):
                for field in ['customer','reference','owner_id','lines','requirements','steps']:
                    self.assertEqual(actual[field],expected['snapshot'][field],field)
            stock=self.stock(plan)
            self.sql('SET ROLE wms_test_owner;\n'+industry.seed_sql(plan))
            replay=json.loads(self.sql("SELECT coalesce(jsonb_agg(to_jsonb(o) ORDER BY reference),'[]') FROM warehouse.work_orders o WHERE organization_id="+workplace.sql_literal(scope['organization_id'])+';'))
            self.assertEqual(replay,rows)
            self.assertEqual(self.stock(plan),stock,'Replaying plans must not change stock')


class RecordingRuntime:
    def __init__(self):
        self.calls=[]
    def sql(self,statement):
        self.calls.append(('sql',statement))
    def publish_workspace(self,path,config):
        self.calls.append(('workspace',str(path),config))


if __name__ == '__main__':
    unittest.main(verbosity=2)
