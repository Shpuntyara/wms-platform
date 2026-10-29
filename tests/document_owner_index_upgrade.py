"""Actual catalog/upgrade checks in owned disposable PostgreSQL databases."""
import argparse
import hashlib
import json
from pathlib import Path
import re
import subprocess
import sys
import time
import uuid


ROOT = Path(__file__).resolve().parents[1]
NEW_INDEX = 'documents_owner_warehouse_page_idx'
CATALOG_SQL = """SELECT jsonb_build_object(
 'old_exists',to_regclass('warehouse.documents_owner_idx') IS NOT NULL,
 'replacement',(SELECT jsonb_build_object(
  'table',i.indrelid::regclass::text,'method',am.amname,
  'valid',i.indisvalid,'ready',i.indisready,'live',i.indislive,
  'unique',i.indisunique,'primary',i.indisprimary,'exclusion',i.indisexclusion,
  'key_count',i.indnkeyatts,'attribute_count',i.indnatts,
  'predicate',pg_get_expr(i.indpred,i.indrelid),'expressions',pg_get_expr(i.indexprs,i.indrelid),
  'keys',ARRAY(SELECT pg_get_indexdef(c.oid,n,true) FROM generate_series(1,i.indnkeyatts) n),
  'options',i.indoption::smallint[],'collations',ARRAY(SELECT x::bigint FROM unnest(i.indcollation::oid[]) x),
  'opclasses',ARRAY(SELECT opc.opcnamespace::regnamespace::text||'.'||opc.opcname
   FROM unnest(i.indclass::oid[]) WITH ORDINALITY k(oid,n)
   JOIN pg_opclass opc ON opc.oid=k.oid ORDER BY k.n))
 FROM pg_class c JOIN pg_index i ON i.indexrelid=c.oid
 JOIN pg_am am ON am.oid=c.relam
 WHERE c.oid=to_regclass('warehouse.documents_owner_warehouse_page_idx')));"""
EXPECTED = dict(table='warehouse.documents',method='btree',valid=True,ready=True,live=True,
                unique=False,primary=False,exclusion=False,key_count=4,attribute_count=4,
                predicate=None,expressions=None,
                keys=['organization_id','owner_id','warehouse_id','id'],
                options=[0,0,0,0],collations=[0,0,0,0],opclasses=['pg_catalog.uuid_ops']*4)


def verify_catalog(value):
    if value.get('replacement') != EXPECTED:
        raise AssertionError('Owner lookup replacement must be a valid complete covering btree index')
    if value.get('old_exists') is not False:
        raise AssertionError('Old narrow owner index must be retired only after the replacement is valid')


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--psql',required=True)
    parser.add_argument('--port',required=True,type=int)
    parser.add_argument('--admin',default='wms_test_admin')
    parser.add_argument('--database',default='wms_kernel_test_backend')
    parser.add_argument('--owner-role',default='wms_test_owner')
    parser.add_argument('--catalog-only',action='store_true')
    parser.add_argument('--report',required=True)
    args = parser.parse_args()
    base = [args.psql,'-X','-qAt','-v','ON_ERROR_STOP=1','-h','127.0.0.1','-p',str(args.port),'-U',args.admin]
    def raw_sql(text,database):
        result = subprocess.run(base+['-d',database],input=text,text=True,capture_output=True)
        return result
    def sql(text,database=args.database):
        result = raw_sql(text,database)
        if result.returncode:
            raise RuntimeError('Disposable PostgreSQL probe failed')
        return result.stdout.strip()
    results = []
    cleanup = []
    def check(name,body):
        try:
            body()
            results.append(dict(name=name,status='pass'))
        except AssertionError as error:
            results.append(dict(name=name,status='fail',detail=str(error)))
        except Exception:
            results.append(dict(name=name,status='invalid',detail='Fixture/infrastructure failed; no behavioral pass claimed'))
    if args.catalog_only:
        check('complete_live_catalog_definition',lambda:verify_catalog(json.loads(sql(CATALOG_SQL))))
    else:
        sys.path.insert(0,str(ROOT/'scripts'))
        from migrate_core import build_script
        baseline = build_script(ROOT,through='0010')
        upgrade = build_script(ROOT)
        owner = '"'+args.owner_role.replace('"','""')+'"'
        own = 'SET ROLE '+owner+';\n'
        replacement = 'CREATE INDEX CONCURRENTLY '+NEW_INDEX+' ON warehouse.documents USING btree(organization_id,owner_id,warehouse_id,id);'
        old_drop = 'DROP INDEX CONCURRENTLY IF EXISTS warehouse.documents_owner_idx;'
        assert upgrade.count(old_drop)==1,'Expected exactly one owned old-index drop checkpoint'
        def create_database():
            database = 'wms_kernel_test_owner_index_'+uuid.uuid4().hex
            assert re.fullmatch('wms_kernel_test_owner_index_[a-f0-9]{32}',database)
            sql('CREATE DATABASE '+database+' OWNER '+owner+';','postgres')
            sql(own+baseline,database)
            sql(own+"""INSERT INTO platform.organizations(id,code,name) VALUES('00000000-0000-4000-8000-000000000123','index-test','index-test');
INSERT INTO warehouse.warehouses(id,organization_id,code) VALUES('00000000-0000-4000-8000-000000000124','00000000-0000-4000-8000-000000000123','wh');
INSERT INTO catalog.stock_owners(id,organization_id,code) VALUES('00000000-0000-4000-8000-000000000125','00000000-0000-4000-8000-000000000123','owner');
INSERT INTO warehouse.documents(id,organization_id,warehouse_id,owner_id,kind,number,state)
SELECT ('00000000-0000-4000-8000-'||lpad(g::text,12,'0'))::uuid,'00000000-0000-4000-8000-000000000123','00000000-0000-4000-8000-000000000124','00000000-0000-4000-8000-000000000125','receipt',g::text,'draft' FROM generate_series(126,127) g;""",database)
            return database
        def snapshot(database):
            return sql('SELECT jsonb_agg(to_jsonb(d) ORDER BY id)::text FROM warehouse.documents d;',database)
        def apply(database,must=True,script=upgrade,expected_error='Document owner seek replacement index is not valid and compatible; original index retained'):
            result = raw_sql(own+script,database)
            if must:
                assert result.returncode==0,'Valid owned migration/resume was rejected'
                verify_catalog(json.loads(sql(CATALOG_SQL,database)))
            else:
                assert result.returncode!=0,'Unsafe pre-existing index was accepted'
                assert expected_error in result.stderr,'Migration failed outside the intended catalog/checkpoint guard'
                value=json.loads(sql(CATALOG_SQL,database))
                assert value['old_exists'],'Migration dropped old index before validating replacement'
                assert sql("SELECT count(*) FROM wms_migrations.history WHERE name='0011_document_owner_seek.sql';",database)=='0','Failed DDL was recorded as completed'
        def scenario(body):
            database=create_database()
            before=snapshot(database)
            try:
                body(database)
                assert snapshot(database)==before,'Index upgrade changed document data'
            finally:
                # This exact generated name is the only database ever removed.
                assert re.fullmatch('wms_kernel_test_owner_index_[a-f0-9]{32}',database)
                sql('DROP DATABASE '+database+';','postgres')
                cleanup.append(database)
        def ordinary(database):
            apply(database)
            oid=sql("SELECT 'warehouse.documents_owner_warehouse_page_idx'::regclass::oid;",database)
            apply(database)
            assert sql("SELECT 'warehouse.documents_owner_warehouse_page_idx'::regclass::oid;",database)==oid,'Repeat migration rebuilt replacement index'
        check('upgrade_and_repeat_preserve_rows_and_owner_lookup_prefix',lambda:scenario(ordinary))
        def resume(database,already_dropped):
            sql(own+replacement,database)
            if already_dropped: sql(own+old_drop,database)
            ordinary(database)
        for already_dropped in (False,True):
            check('resume_valid_created_index_old_absent='+str(already_dropped),lambda flag=already_dropped:scenario(lambda db:resume(db,flag)))
        def failed_checkpoint(database):
            sentinel='owner_index_upgrade_intentional_checkpoint'
            fault=upgrade.replace(old_drop,"DO $$ BEGIN RAISE EXCEPTION '"+sentinel+"'; END $$;\n"+old_drop,1)
            apply(database,must=False,script=fault,expected_error=sentinel)
            assert json.loads(sql(CATALOG_SQL,database))['replacement']==EXPECTED,'DDL prefix did not retain its valid created index'
            ordinary(database)
        check('failed_after_create_before_drop_retries_without_data_loss',lambda:scenario(failed_checkpoint))
        wrong_definitions={
            'column_order':'USING btree(organization_id,warehouse_id,owner_id,id)',
            'descending':'USING btree(organization_id,owner_id,warehouse_id,id DESC)',
            'include':'USING btree(organization_id,owner_id,warehouse_id,id) INCLUDE(number)',
            'partial':"USING btree(organization_id,owner_id,warehouse_id,id) WHERE state='draft'",
            'access_method':'USING brin(organization_id,owner_id,warehouse_id,id)',
        }
        def wrong(database,definition):
            sql(own+'CREATE INDEX '+NEW_INDEX+' ON warehouse.documents '+definition+';',database)
            apply(database,must=False)
        for name,definition in wrong_definitions.items():
            check('wrong_existing_'+name+'_refused_before_old_drop',lambda definition=definition:scenario(lambda db:wrong(db,definition)))
        def wrong_old(database):
            sql(own+'DROP INDEX warehouse.documents_owner_idx; CREATE INDEX documents_owner_idx ON warehouse.documents(id);',database)
            apply(database,must=False,expected_error='Original document owner index has an unexpected contract; index retained')
        check('unexpected_old_index_name_contract_is_retained',lambda:scenario(wrong_old))
        def invalid_concurrent(database):
            blocker=subprocess.Popen(base+['-d',database],stdin=subprocess.PIPE,stdout=subprocess.PIPE,stderr=subprocess.PIPE,text=True)
            builder=None
            application='wms_index_probe_'+uuid.uuid4().hex
            try:
                blocker.stdin.write('BEGIN; LOCK TABLE warehouse.documents IN ROW EXCLUSIVE MODE; SELECT 1;\n')
                blocker.stdin.flush()
                assert blocker.stdout.readline().strip()=='1','Cannot establish owned index-build blocker'
                builder=subprocess.Popen(base+['-d',database],stdin=subprocess.PIPE,stdout=subprocess.PIPE,stderr=subprocess.PIPE,text=True)
                builder.stdin.write("SET application_name='"+application+"';\n"+own+replacement+'\n')
                builder.stdin.close()
                builder.stdin=None
                for _ in range(100):
                    state=sql("SELECT count(*) FROM pg_index WHERE indexrelid=to_regclass('warehouse.documents_owner_warehouse_page_idx') AND NOT indisvalid;",database)
                    if state=='1': break
                    time.sleep(.02)
                assert state=='1','Concurrent build did not reach its incomplete catalog checkpoint'
                assert sql("SELECT bool_and(pg_cancel_backend(pid)) FROM pg_stat_activity WHERE datname=current_database() AND application_name='"+application+"';",database)=='t','Cannot cancel owned concurrent index build'
                builder.communicate(timeout=10)
                assert builder.returncode!=0,'Index cancellation did not interrupt the build'
            finally:
                if builder and builder.poll() is None: builder.kill(); builder.communicate(timeout=10)
                blocker.communicate('ROLLBACK;\n',timeout=10)
            apply(database,must=False)
            # Explicit repair is bounded to the known owned invalid candidate;
            # the installer itself must refuse, rather than silently delete it.
            sql(own+'DROP INDEX CONCURRENTLY warehouse.'+NEW_INDEX+';',database)
            ordinary(database)
        check('cancelled_concurrent_build_refuses_until_explicit_owned_repair',lambda:scenario(invalid_concurrent))
    files=['tests/document_owner_index_upgrade.py','scripts/migrate_core.py','db/migrations/0011_document_owner_seek.sql']
    report = dict(passed=bool(results) and all(result['status']=='pass' for result in results),results=results,
                  cleanup_databases=len(cleanup),source_sha256={name:hashlib.sha256((ROOT/name).read_bytes()).hexdigest() for name in files if (ROOT/name).is_file()})
    path = ROOT/args.report
    path.parent.mkdir(parents=True,exist_ok=True)
    path.write_text(json.dumps(report,indent=2)+'\n',encoding='utf8')
    print(json.dumps(report))
    return 0 if report['passed'] else 1


if __name__ == '__main__':
    sys.exit(main())
