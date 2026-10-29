"""Actual additive department setup against an explicitly disposable PostgreSQL."""
import copy
import json
import os
from pathlib import Path
import subprocess
import sys
import unittest
import urllib.parse
import uuid

ROOT=Path(__file__).resolve().parents[1]
sys.path.insert(0,str(ROOT/'deploy/linux'))
from department_workflow import build_plan,seed_sql,literal
from test_department_workflow import fixture

class DepartmentSetupPostgresTests(unittest.TestCase):
    @classmethod
    def setUpClass(cls):
        cls.dsn=os.environ.get('WMS_TEST_ADMIN_DSN','')
        parsed=urllib.parse.urlsplit(cls.dsn)
        if parsed.hostname not in ['127.0.0.1','localhost'] or parsed.username!='wms_test_admin' or not parsed.path.removeprefix('/').startswith('wms_kernel_test_'):
            raise RuntimeError('An explicitly disposable loopback WMS_TEST_ADMIN_DSN is required; this test never skips')
        cls.psql=os.environ.get('WMS_TEST_PSQL','psql')

    def sql(self,statement,*,success=True):
        result=subprocess.run([self.psql,'-X','-q','-At','-v','ON_ERROR_STOP=1','--dbname',self.dsn],input=statement,capture_output=True,text=True,encoding='utf8',env=dict(os.environ,PGCLIENTENCODING='UTF8'),timeout=30)
        if success:self.assertEqual(result.returncode,0,result.stderr)
        else:
            self.assertNotEqual(result.returncode,0,'Conflicting metadata was silently accepted')
            self.assertIn('department_configuration_drift',result.stderr,'A setup or syntax error is not a metadata conflict proof')
        return result.stdout.strip()

    def setUp(self):
        self.identity,self.users,self.workspace=copy.deepcopy(fixture())
        replacements={value:str(uuid.uuid4()) for value in [self.identity['instance_id'],*self.users['ids'].values(),*[p['id'] for p in self.workspace['positions']],self.workspace['items'][0]['id'],*[u['subject'] for u in self.users['users'].values()]]}
        def replace(value):
            if isinstance(value,str):return replacements.get(value,value)
            if isinstance(value,dict):return {key:replace(entry) for key,entry in value.items()}
            if isinstance(value,list):return [replace(entry) for entry in value]
            return value
        self.identity,self.users,self.workspace=replace(self.identity),replace(self.users),replace(self.workspace)
        self.plan=build_plan(self.identity,self.users,self.workspace,self.identity['instance_id'])
        ids=self.users['ids'];q=literal;org=ids['organization'];wh=ids['warehouse'];owner=ids['owner'];item=self.workspace['items'][0]['id']
        statements=["INSERT INTO platform.organizations(id,code,name) VALUES("+q(org)+','+q(org)+",'METALFORM');",
                    'INSERT INTO warehouse.warehouses(id,organization_id,code) VALUES('+q(wh)+','+q(org)+','+q(wh)+');',
                    'INSERT INTO catalog.stock_owners(id,organization_id,code) VALUES('+q(owner)+','+q(org)+",'own');",
                    "INSERT INTO catalog.units(code,dimension) VALUES('ea','count') ON CONFLICT DO NOTHING;",
                    'INSERT INTO catalog.items(id,organization_id,sku,base_unit,quantity_scale,tracking) VALUES('+q(item)+','+q(org)+",'SR-M8X40','ea',0,'none');"]
        for role in ['biuro','magazynier']:
            statements.append('INSERT INTO platform.actors(id,issuer,subject) VALUES('+q(ids['actor_'+role])+','+q(self.identity['issuer'])+','+q(self.users['users'][role]['subject'])+');')
        for position in self.workspace['positions']:
            location=str(uuid.uuid4())
            statements.extend(['INSERT INTO warehouse.locations(id,organization_id,warehouse_id,code,kind) VALUES('+','.join(q(v) for v in [location,org,wh,position['code'],'storage'])+');',
                               'INSERT INTO warehouse.positions(id,organization_id,warehouse_id,location_id) VALUES('+','.join(q(v) for v in [position['id'],org,wh,location])+');'])
        actor=ids['actor_magazynier'];self.balance=str(uuid.uuid4());document=str(uuid.uuid4());line=str(uuid.uuid4());command=str(uuid.uuid4())
        statements.extend(['INSERT INTO platform.actor_grants(actor_id,organization_id,warehouse_id,owner_id,permission) VALUES('+','.join(q(v) for v in [actor,org,wh,owner,'movement.create'])+');',
                           'INSERT INTO inventory.stock_balances(id,organization_id,warehouse_id,owner_id,item_id,position_id,stock_state) VALUES('+','.join(q(v) for v in [self.balance,org,wh,owner,item,self.workspace['positions'][0]['id'],'available'])+');',
                           'INSERT INTO warehouse.documents(id,organization_id,warehouse_id,owner_id,kind,number,state) VALUES('+','.join(q(v) for v in [document,org,wh,owner,'receipt','PZ/CONFIG/'+document,'active'])+');',
                           'INSERT INTO warehouse.document_lines(id,organization_id,warehouse_id,document_id,owner_id,item_id,line_no,requested_qty) VALUES('+','.join(q(v) for v in [line,org,wh,document,owner,item])+',1,10);',
                           'SELECT inventory.post_movement('+','.join(q(v) for v in [command,actor,line])+",1,'receipt',NULL,"+q(self.balance)+',10);'])
        self.sql('BEGIN;\n'+'\n'.join(statements)+'\nCOMMIT;')
        self.subjects={p['username']:str(uuid.uuid4()) for p in self.plan['employees']}

    def snapshot(self):
        org=literal(self.plan['organization_id'])
        return json.loads(self.sql("SELECT jsonb_build_object('stock',(SELECT jsonb_agg(to_jsonb(b) ORDER BY id) FROM inventory.stock_balances b WHERE organization_id="+org+"),'items',(SELECT jsonb_agg(to_jsonb(i) ORDER BY id) FROM catalog.items i WHERE organization_id="+org+"),'documents',(SELECT jsonb_agg(to_jsonb(d) ORDER BY id) FROM contracts_v1.document_details d WHERE organization_id="+org+"));"))

    def test_real_seed_is_additive_idempotent_and_preserves_positive_stock_documents_and_skus(self):
        before=self.snapshot();sql=seed_sql(self.plan,self.subjects)
        self.sql(sql);self.sql(sql)
        self.assertEqual(self.snapshot(),before)
        org=literal(self.plan['organization_id'])
        result=json.loads(self.sql("SELECT jsonb_build_object('departments',(SELECT count(*) FROM warehouse.departments WHERE organization_id="+org+"),'members',(SELECT count(*) FROM warehouse.department_members WHERE organization_id="+org+"),'positions',(SELECT count(*) FROM warehouse.department_positions WHERE organization_id="+org+"),'routes',(SELECT count(*) FROM warehouse.department_routes WHERE organization_id="+org+"));"))
        self.assertEqual(result,{'departments':5,'members':5,'positions':12,'routes':5})

    def test_existing_department_identity_drift_is_rejected_without_partial_stock_or_grant_changes(self):
        self.sql(seed_sql(self.plan,self.subjects))
        self.sql('UPDATE warehouse.departments SET name=\'Zmiana poza planem\' WHERE id='+literal(self.plan['departments'][1]['id'])+';')
        before=self.snapshot()
        self.sql(seed_sql(self.plan,self.subjects),success=False)
        self.assertEqual(self.snapshot(),before)

if __name__=='__main__':unittest.main()
