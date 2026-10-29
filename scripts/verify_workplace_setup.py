"""Focused Python behavior mutations for the real-installation setup plan."""
import argparse
import hashlib
import json
from pathlib import Path
import shutil
import subprocess
import sys
import tempfile


def classify(code, result, expected):
    if result.get('tests') != expected or expected < 1 or result.get('errors') or result.get('skipped'):
        return 'invalid'
    failures = result.get('failures',[])
    if code == 1 and failures: return 'killed'
    if code == 0 and not failures: return 'survived'
    return 'invalid'


def catalog():
    return [
        ('wrong_installation_accepted','installation != expected or ','','test_identity_and_origin_drift_fail_before_a_plan_can_be_applied'),
        ('foreign_namespace_reused','namespace = uuid.UUID(installation)','namespace = uuid.NAMESPACE_DNS','test_generated_plans_keep_ids_unique_and_foreign_installations_disjoint'),
        ('http_origin','origin = \'https://\'+address','origin = \'http://\'+address','test_public_client_cannot_bypass_pkce_or_exact_origin'),
        ('confidential_browser_client',"'publicClient':True", "'publicClient':False",'test_public_client_cannot_bypass_pkce_or_exact_origin'),
        ('implicit_flow_enabled',"'implicitFlowEnabled':False", "'implicitFlowEnabled':True",'test_public_client_cannot_bypass_pkce_or_exact_origin'),
        ('password_grant_enabled',"'directAccessGrantsEnabled':False", "'directAccessGrantsEnabled':True",'test_public_client_cannot_bypass_pkce_or_exact_origin'),
        ('browser_service_account',"'serviceAccountsEnabled':False", "'serviceAccountsEnabled':True",'test_public_client_cannot_bypass_pkce_or_exact_origin'),
        ('pkce_downgrade',"'pkce.code.challenge.method':'S256'", "'pkce.code.challenge.method':'plain'",'test_public_client_cannot_bypass_pkce_or_exact_origin'),
        ('redirect_wildcard',"'redirectUris':[origin+'/app/']", "'redirectUris':[origin+'/*']",'test_public_client_cannot_bypass_pkce_or_exact_origin'),
        ('logout_wildcard',"'post.logout.redirect.uris':origin+'/app/'", "'post.logout.redirect.uris':origin+'/*'",'test_public_client_cannot_bypass_pkce_or_exact_origin'),
        ('bureau_can_post_movements',"'biuro': ('document.read', 'document.create', 'document.transition', 'stock.read')", "'biuro': ('document.read', 'document.create', 'document.transition', 'stock.read', 'movement.create')",'test_scope_and_passwords_are_not_in_public_plan'),
        ('worker_can_create_documents',"'magazynier': ('document.read', 'document.transition', 'stock.read', 'movement.create')", "'magazynier': ('document.read', 'document.transition', 'stock.read', 'movement.create', 'document.create')",'test_scope_and_passwords_are_not_in_public_plan'),
        ('database_installation_unchecked','if actual != expected_instance:', 'if False:', 'test_apply_needs_explicit_lab_and_checks_database_identity_before_mutation'),
        ('passwords_regenerated','if state is None:', 'if True:', 'test_managed_users_reuse_passwords_and_subjects_without_resetting'),
        ('profile_marker_missing','if desired_profile != profile:', 'if False:', 'test_managed_ownership_attribute_works_without_enabling_unmanaged_attributes'),
        ('profile_marker_user_editable',"'permissions':{'view':['admin'],'edit':['admin']}", "'permissions':{'view':['admin'],'edit':['admin','user']}", 'test_managed_ownership_attribute_works_without_enabling_unmanaged_attributes'),
        ('unowned_client_adopted',"if client.get('attributes',{}).get('wms.workplace.installation') != plan['instance_id']:", 'if False:', 'test_existing_unowned_user_or_client_refused_before_changes'),
        ('canonical_userinfo_false_omitted', ",'userinfo.token.claim':'false'", '', 'test_real_keycloak_audience_roundtrip_preserves_subjects_and_passwords'),
        ('audience_disclosure_or_mapper_drift_accepted', "if mappers != plan['client']['protocolMappers']:", 'if False:', 'test_audience_claim_or_additional_mapper_drift_is_not_canonicalization'),
    ]


HARNESS = '''import contextlib,io,json,unittest,sys
names=json.loads(sys.argv[1]); output=io.StringIO()
with contextlib.redirect_stdout(output),contextlib.redirect_stderr(output):
 suite=unittest.defaultTestLoader.loadTestsFromNames(names)
 result=unittest.TextTestRunner(stream=output,verbosity=2).run(suite)
print(json.dumps(dict(tests=result.testsRun,failures=[name.id() for name,_ in result.failures],errors=[name.id() for name,_ in result.errors],skipped=[name.id() for name,_ in result.skipped],output=output.getvalue())))
raise SystemExit(0 if result.wasSuccessful() else 1)
'''


def execute(stage, names):
    try:
        done = subprocess.run([sys.executable,'-c',HARNESS,json.dumps(names)],cwd=stage,capture_output=True,text=True,encoding='utf8',timeout=30)
        result = json.loads(done.stdout)
    except (subprocess.TimeoutExpired,ValueError):
        return {'status':'invalid','reason':'timeout or invalid test report'}
    result['status'] = classify(done.returncode,result,len(names))
    return result


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--output',type=Path,default=Path('work/workplace-setup-mutations.json'))
    args = parser.parse_args()
    root = Path(__file__).resolve().parents[1]
    files = ['deploy/linux/workplace.py','scripts/test_workplace_setup.py']
    before = {name:hashlib.sha256((root/name).read_bytes()).hexdigest() for name in files}
    source = (root/files[0]).read_text(encoding='utf8')
    report = {'schema':1,'source_sha256':before,'mutants':[]}
    with tempfile.TemporaryDirectory(prefix='wms-workplace-mutants-') as temporary:
        stage = Path(temporary)
        for name in files:
            (stage/name).parent.mkdir(parents=True,exist_ok=True)
            shutil.copyfile(root/name,stage/name)
        (stage/'scripts/__init__.py').touch()
        # The full class baseline guards against a missing/empty test suite.
        baseline = subprocess.run([sys.executable,'-m','unittest','scripts.test_workplace_setup'],cwd=stage,capture_output=True,text=True,encoding='utf8',timeout=30)
        report['baseline_passed'] = baseline.returncode == 0 and 'Ran 15 tests' in baseline.stderr and 'OK' in baseline.stderr
        if not report['baseline_passed']: raise RuntimeError('Workplace setup baseline is not fully green')
        for name,needle,replacement,test in catalog():
            if source.count(needle) != 1:
                result = {'status':'invalid','reason':'mutation anchor changed'}
            else:
                (stage/files[0]).write_text(source.replace(needle,replacement,1),encoding='utf8')
                result = execute(stage,['scripts.test_workplace_setup.WorkplaceTests.'+test])
            result['id'] = name
            report['mutants'].append(result)
            print(name+': '+result['status'],flush=True)
    report['source_unchanged'] = before == {name:hashlib.sha256((root/name).read_bytes()).hexdigest() for name in files}
    for status in ['killed','survived','invalid']:
        report[status] = sum(case['status']==status for case in report['mutants'])
    report['passed'] = report['baseline_passed'] and report['source_unchanged'] and report['killed']==len(catalog())
    args.output.parent.mkdir(parents=True,exist_ok=True)
    args.output.write_text(json.dumps(report,indent=2)+'\n',encoding='utf8')
    print(json.dumps({key:report[key] for key in ['passed','killed','survived','invalid','source_unchanged']}))
    raise SystemExit(0 if report['passed'] else 1)


if __name__ == '__main__': main()
