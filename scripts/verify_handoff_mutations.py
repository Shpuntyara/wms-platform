"""Isolated custody faults; only explicit behavior assertions can kill a mutant.

An owned, empty database is migrated by the official migrator. Each SQL fault
uses a fresh clone of that clean template. No shared test DB or migration source
is edited, and no installation/VM data is used.
"""
import argparse
import hashlib
import json
import os
from pathlib import Path
import re
import subprocess
import time
from urllib.parse import urlsplit
import uuid

from migrate_core import build_script
from mutation_runner import database_environment

ROOT = Path(__file__).resolve().parents[1]
PREFIX = 'TestDepartmentHandoffsCritic'


def case(name, signature, before, after, test):
    return dict(name=name, signature=signature, before=before, after=after,
                test=PREFIX+test)


def catalog():
    command = 'warehouse.command_handoff(uuid,uuid,uuid,jsonb)'
    member = 'warehouse.handoff_member(uuid,uuid,uuid,uuid)'
    authorize = 'warehouse.handoff_authorize(uuid,uuid,uuid,uuid,text)'
    movement = 'inventory.guard_handoff_movement()'
    balance = 'inventory.guard_handoff_balance()'
    return [
        case('owner_permission_ignored', authorize, '(owner_id IS NULL OR owner_id=p_owner)', 'true', 'OwnerRouteAndActorCannotBeSubstituted'),
        case('specific_permission_ignored', authorize, 'AND permission=p_permission', '', 'SelfAcceptanceAndRevocationBeforeReplay'),
        case('membership_revocation_ignored', member, ' AND m.enabled', '', 'SelfAcceptanceAndRevocationBeforeReplay'),
        case('employee_disabled_ignored', member, ' AND e.enabled', '', 'SelfAcceptanceAndRevocationBeforeReplay'),
        case('source_membership_ignored', command, 'NOT warehouse.handoff_member(p_actor,org,wh,source_dept.id)', 'false', 'OwnerRouteAndActorCannotBeSubstituted'),
        case('receiving_membership_ignored', command, 'NOT warehouse.handoff_member(p_actor,org,wh,h.destination_department_id)', 'false', 'AuthenticatedActorsAreActualWitnesses'),
        case('route_disabled_ignored', command, 'destination_department_id=dest_dept.id AND enabled)', 'destination_department_id=dest_dept.id)', 'OwnerRouteAndActorCannotBeSubstituted'),
        case('replay_payload_ignored', command, 'OR prior.request IS DISTINCT FROM request', 'OR false', 'SelfAcceptanceAndRevocationBeforeReplay'),
        case('replay_actor_ignored', command, 'prior.actor_id<>p_actor OR ', '', 'OwnerRouteAndActorCannotBeSubstituted'),
        case('quantity_float_rounding', command, "qty:=(p_input->>'quantity')::numeric", "qty:=(p_input->>'quantity')::double precision::numeric", 'TrackingAndLargeDecimalsStayExact'),
        case('production_generic_boundary_ignored', movement, "ELSIF source_dept.kind='production' OR target_dept.kind='production' THEN", 'ELSIF false THEN', 'LegacyCommandsCannotBypassManagedEndpoints'),
        case('production_department_identity_ignored', movement, 'source_dept.id IS DISTINCT FROM target_dept.id', 'false', 'LegacyCommandsCannotBypassManagedEndpoints'),
        case('same_department_membership_ignored', movement, 'NOT warehouse.handoff_member(op.actor_id,m.organization_id,m.warehouse_id,source_dept.id)', 'false', 'SameDepartmentMovementRequiresCurrentMember'),
        case('transit_capability_ignored', movement, 'IF managed THEN', 'IF false THEN', 'LegacyCommandsCannotBypassManagedEndpoints'),
        case('transit_stock_identity_ignored', balance, "IF transit IS DISTINCT FROM (NEW.stock_state='in_transit') THEN", 'IF false THEN', 'TransitIdentityAndReservationCannotBeForged'),
        case('production_reservation_allowed', balance, 'IF NEW.reserved_qty>0 AND EXISTS', 'IF false AND EXISTS', 'TransitIdentityAndReservationCannotBeForged'),
        case('handling_unit_managed_destination_allowed', 'warehouse.protect_managed_handling_unit()', 'IF EXISTS(SELECT 1 FROM warehouse.positions p WHERE p.location_id=NEW.location_id', 'IF false AND EXISTS(SELECT 1 FROM warehouse.positions p WHERE p.location_id=NEW.location_id', 'HandlingUnitCannotEnterManagedLocations'),
        case('ordinary_handling_unit_blocked', 'warehouse.protect_managed_handling_unit()', 'IF EXISTS(SELECT 1 FROM warehouse.positions p WHERE p.location_id=NEW.location_id', 'IF true OR EXISTS(SELECT 1 FROM warehouse.positions p WHERE p.location_id=NEW.location_id', 'OrdinaryHandlingUnitRemainsUsable'),
        case('acceptance_uses_transaction_start', command, 'accepted_at=clock_timestamp()', 'accepted_at=transaction_timestamp()', 'ServerChronologyUsesEventTime'),
        case('history_guard_omitted', 'warehouse.protect_handoff_history()', "BEGIN\n IF TG_OP='DELETE'", "BEGIN\n RETURN NEW;\n IF TG_OP='DELETE'", 'WitnessNamesAndHistoryAreImmutable'),
        case('send_inventory_post_omitted', command, "PERFORM inventory.post_movement(movement_command,p_actor,line,1,'transfer',source.id,transit_balance,qty);", 'NULL;', 'IndependentConservationHistories'),
        case('accept_inventory_post_omitted', command, "PERFORM inventory.post_movement(movement_command,p_actor,line,1,'transfer',h.transit_balance_id,h.destination_balance_id,h.quantity);", 'NULL;', 'IndependentConservationHistories'),
    ]


def classify(code, events, expected_names):
    completed = [e for e in events if e.get('Test') and e.get('Action') in ('pass', 'fail', 'skip')]
    names = sorted(e['Test'] for e in completed)
    broken = any(e.get('Action') == 'build-fail' or any(value in e.get('Output', '') for value in
                 ('panic:', '[build failed]', 'HANDOFF_FEATURE_UNAVAILABLE')) for e in events)
    if broken or names != sorted(expected_names) or not names or any(e['Action'] == 'skip' for e in completed):
        return 'invalid'
    failures = [e['Test'] for e in completed if e['Action'] == 'fail']
    leaves = [name for name in failures if not any(other.startswith(name+'/') for other in failures)]
    if any(not any(e.get('Test') == name and 'HANDOFF_ASSERTION:' in e.get('Output', '') for e in events) for name in leaves):
        return 'invalid'
    if code != 0 and leaves:
        return 'killed'
    if code == 0 and not failures:
        return 'survived'
    return 'invalid'


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--go', default='go')
    parser.add_argument('--psql', default='psql')
    parser.add_argument('--output', type=Path, default=Path('work/handoff-mutations.json'))
    args = parser.parse_args()
    _, psql_environment = database_environment(os.environ.get('WMS_TEST_ADMIN_DSN', ''))
    _, runtime_environment = database_environment(os.environ.get('WMS_TEST_RUNTIME_DSN', ''))
    if psql_environment['PGPORT'] != runtime_environment['PGPORT']:
        raise ValueError('Admin and runtime fixtures must use the same loopback server')
    cases = catalog()
    assert len(cases) == len({row['name'] for row in cases}) == 22
    token = uuid.uuid4().hex
    template = 'wms_kernel_test_handoff_' + token[:16]
    directory = ROOT/'work'/('handoff-mutations-'+token)
    directory.mkdir(parents=True)
    tracked = [ROOT/'db/migrations/0014_department_handoffs.sql', ROOT/'internal/postgres/department_handoffs_critic_integration_test.go', Path(__file__).resolve()]
    original_bytes = {path: path.read_bytes() for path in tracked}
    report = dict(passed=False, scope=__doc__, source_sha256_lf={path.relative_to(ROOT).as_posix(): hashlib.sha256(data.replace(b'\r\n',b'\n')).hexdigest() for path,data in original_bytes.items()}, mutations=[])
    created = []

    def psql(sql, database='postgres', variables=()):
        environment = dict(psql_environment, PGDATABASE=database)
        command = [args.psql, '-X', '-q', '-At', '-v', 'ON_ERROR_STOP=1']
        for key, value in variables:
            command += ['-v', key+'='+value]
        result = subprocess.run(command, input=sql, env=environment, capture_output=True, text=True, encoding='utf8', timeout=90)
        if result.returncode:
            (directory/'sql-error.txt').write_text(result.stderr, encoding='utf8')
            raise RuntimeError('SQL setup failed; see isolated work directory')
        return result.stdout

    def environment(database):
        result = os.environ.copy()
        for key in ('WMS_TEST_ADMIN_DSN', 'WMS_TEST_RUNTIME_DSN'):
            result[key] = urlsplit(result[key])._replace(path='/'+database).geturl()
        return result

    def run(pattern, database, target):
        target.mkdir()
        started = time.monotonic()
        try:
            value = subprocess.run([args.go, 'test', '-tags=integration', './internal/postgres', '-run', pattern, '-count=1', '-timeout=90s', '-json'], cwd=ROOT, env=environment(database), capture_output=True, text=True, encoding='utf8', timeout=105)
        except subprocess.TimeoutExpired:
            return 1, [], dict(status='invalid', reason='timeout')
        (target/'events.jsonl').write_text(value.stdout, encoding='utf8')
        (target/'stderr.txt').write_text(value.stderr, encoding='utf8')
        try:
            events = [json.loads(line) for line in value.stdout.splitlines() if line.strip()]
        except ValueError:
            return value.returncode, [], dict(status='invalid', reason='invalid_event_stream')
        names = sorted(e['Test'] for e in events if e.get('Test') and e.get('Action') in ('pass','fail','skip'))
        return value.returncode, events, dict(tests=names, duration_seconds=round(time.monotonic()-started,3))

    try:
        psql(f'CREATE DATABASE {template} OWNER wms_test_owner;')
        created.append(template)
        psql('SET ROLE wms_test_owner;\n'+build_script(ROOT), template)
        for name in ('grants.psql','backend-grants.psql','industry-backend-grants.psql','handoffs-backend-grants.psql'):
            psql('SET ROLE wms_test_owner;\n'+(ROOT/'db/bootstrap'/name).read_text(encoding='utf8'), template, [('runtime_role','wms_go_runtime'),('module_role','wms_go_runtime')])
        baseline_database = 'wms_kernel_test_handoff_' + uuid.uuid4().hex[:16]
        psql(f'CREATE DATABASE {baseline_database} WITH TEMPLATE {template} OWNER wms_test_owner;')
        created.append(baseline_database)
        pattern = '^('+ '|'.join(sorted({row['test'] for row in cases}))+')$'
        code, events, baseline = run(pattern, baseline_database, directory/'baseline')
        baseline['status'] = classify(code, events, baseline.get('tests', []))
        report['baseline'] = baseline
        if baseline['status'] != 'survived':
            raise RuntimeError('Unmutated custody baseline must pass every selected test without skips')
        for row in cases:
            target = directory/row['name']
            original = psql("SELECT pg_get_functiondef('"+row['signature']+"'::regprocedure);", template)
            if original.count(row['before']) != 1 or row['before'] == row['after']:
                report['mutations'].append(dict(name=row['name'], status='invalid', reason='reviewed_source_anchor_changed'))
                continue
            changed = original.replace(row['before'], row['after'], 1)
            database = 'wms_kernel_test_handoff_' + uuid.uuid4().hex[:16]
            psql(f'CREATE DATABASE {database} WITH TEMPLATE {template} OWNER wms_test_owner;')
            created.append(database)
            try:
                psql(changed, database)
                code, events, outcome = run('^'+row['test']+'$', database, target)
                expected = [name for name in baseline['tests'] if name == row['test'] or name.startswith(row['test']+'/')]
                status = classify(code, events, expected) if 'status' not in outcome else 'invalid'
                record = dict(name=row['name'], signature=row['signature'], status=status, tests=outcome.get('tests', []), failed_tests=[e['Test'] for e in events if e.get('Test') and e.get('Action')=='fail'], source_function_sha256_lf=hashlib.sha256(original.encode()).hexdigest(), mutant_sha256=hashlib.sha256(changed.encode()).hexdigest())
                (target/'mutant.sql').write_text(changed, encoding='utf8')
                report['mutations'].append(record)
                print(json.dumps(dict(name=row['name'],status=status)),flush=True)
            finally:
                psql(f'DROP DATABASE {database};')
                created.remove(database)
    finally:
        for database in reversed(created):
            if not re.fullmatch(r'wms_kernel_test_handoff_[a-f0-9]{16}',database):
                raise ValueError('Refusing to drop a non-owned database')
            psql(f'DROP DATABASE {database};')
        report.update(killed=sum(row['status']=='killed' for row in report['mutations']),survived=sum(row['status']=='survived' for row in report['mutations']),invalid=sum(row['status']=='invalid' for row in report['mutations']),skipped=0,source_unchanged=all(path.read_bytes()==data for path,data in original_bytes.items()))
        report['passed'] = len(report['mutations'])==22 and report['killed']==22 and report['survived']==report['invalid']==0 and report['source_unchanged']
        args.output.parent.mkdir(parents=True,exist_ok=True)
        args.output.write_text(json.dumps(report,ensure_ascii=False,indent=2)+'\n',encoding='utf8')
        print(json.dumps({key:report[key] for key in ('passed','killed','survived','invalid','skipped','source_unchanged')}))
    return 0 if report['passed'] else 1


if __name__ == '__main__':
    raise SystemExit(main())
