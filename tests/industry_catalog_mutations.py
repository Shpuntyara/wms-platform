"""Run bounded real-behavior fixture mutations without changing source files."""
import argparse
import hashlib
import importlib.util
import io
import json
import os
from pathlib import Path
import subprocess
import sys
import tempfile
import time
import unittest

ROOT=Path(__file__).resolve().parents[1]
SOURCE=ROOT/'deploy/linux/industry_catalog.py'
TESTS=ROOT/'tests/test_industry_catalog.py'
CASES={
 'wrong_customer_profile':("company = 'METALFORM' if profile == 'metal' else 'PAPIERNIK'","company = 'PAPIERNIK' if profile == 'metal' else 'METALFORM'",['IndustryCatalogContract.test_metal_catalog_is_a_factory_with_dimensions_and_production_recipe']),
 'shelf_level_off_by_one':("name='Regał '+letter+' · poziom '+str(level))","name='Regał '+letter+' · poziom '+str(level+1))",['IndustryCatalogContract.test_storage_codes_mean_section_and_level']),
 'bill_of_materials_quantity_replaced_by_one':('quantity=str(qty)) for key,qty in components]','quantity=\'1\') for key,qty in components]',['IndustryCatalogContract.test_ten_cabinets_have_independent_material_requirements_and_visible_shortages']),
 'stale_origin_not_validated':('workplace.validate_state(state,legacy)','pass # fault: stale origin permitted',['IndustryCatalogContract.test_apply_rejects_stale_origin_before_any_database_change']),
 'foreign_workspace_not_validated':("if workspace.get('oidc')!=legacy['workspace']['oidc']:","if False:",['IndustryCatalogContract.test_apply_rejects_foreign_workspace_before_any_database_change']),
 'conflicting_catalog_overlooked':('elif old != row:','elif False:',['IndustryCatalogContract.test_merge_refuses_conflicting_catalog_instead_of_relabelling_other_data']),
 'movement_replay_command_randomized':("uid('command/receipt/'+str(index))","str(uuid.uuid4())",['IndustryCatalogContract.test_plan_properties_for_many_scopes_and_both_profiles']),
 'small_stock_leftovers_omitted':('for sku, _, code, quantity in products if quantity > 0]','for sku, _, code, quantity in products if quantity > 150]',['IndustryCatalogContract.test_known_materials_and_leftovers_have_concrete_stock_places']),
 'opening_privileges_not_removed':("'DELETE FROM platform.actor_grants WHERE actor_id='","'DELETE FROM platform.actor_grants WHERE false AND actor_id='",['IndustryCatalogPostgres.test_actual_postgres_opening_replay_preserves_later_activity_and_no_fixture_grants']),
 'opening_transaction_removed':("'BEGIN; SET LOCAL standard_conforming_strings=on; SET LOCAL lock_timeout=\\'2s\\'; SET LOCAL statement_timeout=\\'30s\\';'","'SET standard_conforming_strings=on; SET lock_timeout=\\'2s\\'; SET statement_timeout=\\'30s\\';'",['IndustryCatalogPostgres.test_actual_postgres_interrupted_opening_rolls_back_all_previous_lines_and_retries']),
 'sample_orders_not_persisted':("for order in plan['sample_orders']:",'for order in []:',['IndustryCatalogPostgres.test_actual_postgres_sample_plans_persist_once_and_match_fixture_requirements']),
 'sample_bom_not_scaled_with_order':("int(component['quantity'])*int(lines[0]['quantity'])","int(component['quantity'])",['IndustryCatalogContract.test_example_orders_are_scope_bound_and_compute_real_materials']),
 'legacy_items_not_archived':("row['archived']=True","row['archived']=False",['IndustryCatalogContract.test_only_the_two_known_legacy_items_are_archived_without_data_changes']),
 'legacy_archival_matches_sku_without_identity':("legacy_items.get(row['id'])==row['sku']","row['sku'] in legacy_items.values()",['IndustryCatalogContract.test_unknown_uuid_with_legacy_sku_is_not_silently_archived']),
}


def child(source,selected):
    sys.path.insert(0,str(ROOT/'deploy/linux'))
    spec=importlib.util.spec_from_file_location('industry_catalog',source)
    module=importlib.util.module_from_spec(spec);sys.modules['industry_catalog']=module;spec.loader.exec_module(module)
    spec=importlib.util.spec_from_file_location('industry_catalog_checks',TESTS)
    checks=importlib.util.module_from_spec(spec);spec.loader.exec_module(checks)
    suite=unittest.defaultTestLoader.loadTestsFromModule(checks) if not selected else unittest.defaultTestLoader.loadTestsFromNames(selected,checks)
    stream=io.StringIO();result=unittest.TextTestRunner(stream=stream,verbosity=2).run(suite)
    failures=[dict(test=test.id(),detail=detail) for test,detail in result.failures]
    errors=[dict(test=test.id(),detail=detail) for test,detail in result.errors]
    payload=dict(tests=result.testsRun,failures=failures,errors=errors,skipped=[dict(test=test.id(),reason=why) for test,why in result.skipped],output=stream.getvalue())
    print(json.dumps(payload))
    return 0


def main():
    p=argparse.ArgumentParser(description=__doc__)
    p.add_argument('--output',type=Path,default=ROOT/'work/industry-catalog-mutations.json')
    p.add_argument('--child-source',type=Path)
    p.add_argument('--tests',nargs='*')
    args=p.parse_args()
    if args.child_source:
        return child(args.child_source,args.tests)
    original=SOURCE.read_bytes()
    hashes={str(path.relative_to(ROOT)).replace('\\','/'):hashlib.sha256(path.read_bytes()).hexdigest() for path in [SOURCE,TESTS,Path(__file__)]}
    results=[]
    args.output.parent.mkdir(parents=True,exist_ok=True)
    def run(path,selected):
        started=time.monotonic()
        try:
            command=[sys.executable,str(Path(__file__)), '--child-source',str(path)]
            if selected: command+=['--tests',*selected]
            result=subprocess.run(command,capture_output=True,text=True,encoding='utf8',timeout=120,cwd=ROOT)
            if result.returncode!=0:
                return dict(status='invalid',reason='harness_error',stderr=result.stderr[-4000:],seconds=round(time.monotonic()-started,3))
            evidence=json.loads(result.stdout)
            status='invalid' if evidence['errors'] or evidence['skipped'] or not evidence['tests'] else 'killed' if evidence['failures'] else 'survived'
            return dict(status=status,seconds=round(time.monotonic()-started,3),**evidence)
        except subprocess.TimeoutExpired:
            return dict(status='invalid',reason='timeout')
        except (ValueError,OSError) as error:
            return dict(status='invalid',reason=type(error).__name__)
    baseline=run(SOURCE,[])
    if baseline['status']=='survived':
        work=ROOT/'work/industry-catalog-mutation-overlays'
        work.mkdir(parents=True,exist_ok=True)
        with tempfile.TemporaryDirectory(prefix='run-',dir=work) as tmp:
            Path(tmp).resolve().relative_to(work.resolve())
            for name,(before,after,selected) in CASES.items():
                source=original.decode('utf8')
                if source.count(before)!=1:
                    value=dict(status='invalid',reason='source_anchor_count',count=source.count(before))
                else:
                    candidate=Path(tmp)/(name+'.py');candidate.write_text(source.replace(before,after,1),encoding='utf8',newline='\n')
                    value=run(candidate,selected)
                results.append(dict(name=name,**value))
                print(json.dumps(dict(name=name,status=value['status'],failures=len(value.get('failures',[])),errors=len(value.get('errors',[])),skipped=len(value.get('skipped',[])))),flush=True)
    unchanged=SOURCE.read_bytes()==original
    killed=sum(value['status']=='killed' for value in results)
    survived=sum(value['status']=='survived' for value in results)
    invalid=sum(value['status']=='invalid' for value in results)
    report=dict(passed=baseline['status']=='survived' and len(results)==len(CASES) and killed==len(CASES) and unchanged,
                scope='Selected non-equivalent catalog, opening receipt, replay and configuration faults; not a repository mutation score.',
                baseline=baseline,mutants=results,killed=killed,survived=survived,invalid=invalid,source_unchanged=unchanged,source_sha256=hashes)
    args.output.write_text(json.dumps(report,ensure_ascii=False,indent=2)+'\n',encoding='utf8')
    print(json.dumps(dict(passed=report['passed'],killed=killed,survived=survived,invalid=invalid,source_unchanged=unchanged)))
    return 0 if report['passed'] else 1


if __name__=='__main__':
    sys.exit(main())
