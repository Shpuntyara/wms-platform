"""Focused address-recovery behavior mutations in isolated source trees."""
import argparse
import hashlib
import json
from pathlib import Path
import shutil
import subprocess
import sys
import tempfile

sys.path.insert(0,str(Path(__file__).parent))
from verify_workplace_setup import classify, HARNESS

MUTANTS = [
    ('leaf_address_not_revalidated', '    valid = False\n',
     "    valid = (directory / 'server.crt').exists()\n",
     'test_actual_leaf_certificate_changes_ip_without_rotating_keys_or_ca'),
    ('leaf_key_regenerated', "key = ['-key', directory / 'server.key']",
     "key = ['-newkey', 'rsa:3072', '-nodes', '-keyout', directory / 'server.key']",
     'test_actual_leaf_certificate_changes_ip_without_rotating_keys_or_ca'),
    ('previous_address_mismatch_accepted', "or getattr(args, 'previous_address', None) != identity['address']", '',
     'test_unapproved_or_mismatched_previous_address_still_refuses_before_mutation'),
    ('old_origin_resumed_before_cutover', 'backup(value, resume=False) if change else backup(value)', 'backup(value)',
     'test_explicit_address_update_uses_verified_backup_and_waits_for_the_new_issuer'),
    ('actor_snapshot_binding_removed', 'if observed not in (old, new):', 'if False:',
     'test_recorded_actor_snapshot_cannot_adopt_changed_subjects_or_extra_actors'),
    ('configured_workplace_address_change_accepted', 'if old_address != new_address and configured:', 'if False:',
     'test_configured_workplace_address_change_refuses_before_any_mutation'),
]


def validate_report(report):
    names = [case.get('id') for case in report.get('mutants',[])]
    expected = {case[0] for case in MUTANTS}
    if (not report.get('baseline_passed') or not report.get('source_unchanged')
            or len(names) != len(expected) or set(names) != expected
            or any(case.get('status') != 'killed' for case in report['mutants'])):
        raise ValueError('Address recovery requires the complete unchanged-source behavioral mutation set')
    return True


def execute(stage, tests, expected):
    try:
        done = subprocess.run([sys.executable,'-c',HARNESS,json.dumps(tests)],cwd=stage,
            text=True,encoding='utf8',capture_output=True,timeout=60)
        result = json.loads(done.stdout)
    except (subprocess.TimeoutExpired,ValueError):
        return {'status':'invalid','reason':'timeout or malformed execution report'}
    result['status'] = classify(done.returncode,result,expected)
    return result


def main():
    parser=argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--output',type=Path,default=Path('work/address-recovery-mutations.json'))
    args=parser.parse_args()
    root=Path(__file__).resolve().parents[1]
    files=['deploy/linux/wmsctl.py','scripts/test_address_recovery.py']
    before={name:hashlib.sha256((root/name).read_bytes()).hexdigest() for name in files}
    source=(root/files[0]).read_text(encoding='utf8')
    report={'schema':1,'source_sha256':before,'mutants':[]}
    with tempfile.TemporaryDirectory(prefix='wms-address-mutants-') as temporary:
        stage=Path(temporary)
        for name in files:
            (stage/name).parent.mkdir(parents=True,exist_ok=True)
            shutil.copyfile(root/name,stage/name)
        (stage/'scripts/__init__.py').touch()
        baseline=execute(stage,['scripts.test_address_recovery.AddressRecoveryTests'],11)
        report['baseline']=baseline
        report['baseline_passed']=baseline['status']=='survived'
        if report['baseline_passed']:
            for name,needle,replacement,test in MUTANTS:
                if source.count(needle)!=1:
                    result={'status':'invalid','reason':'mutation anchor changed'}
                else:
                    (stage/files[0]).write_text(source.replace(needle,replacement,1),encoding='utf8')
                    result=execute(stage,['scripts.test_address_recovery.AddressRecoveryTests.'+test],1)
                result['id']=name
                report['mutants'].append(result)
                print(name+': '+result['status'],flush=True)
    report['source_unchanged']=before=={name:hashlib.sha256((root/name).read_bytes()).hexdigest() for name in files}
    for status in ('killed','survived','invalid'):
        report[status]=sum(case['status']==status for case in report['mutants'])
    try:
        report['passed']=validate_report(report)
    except ValueError:
        report['passed']=False
    args.output.parent.mkdir(parents=True,exist_ok=True)
    args.output.write_text(json.dumps(report,indent=2)+'\n',encoding='utf8')
    print(json.dumps({key:report[key] for key in ('passed','killed','survived','invalid','source_unchanged')}))
    raise SystemExit(0 if report['passed'] else 1)


if __name__=='__main__':
    main()
