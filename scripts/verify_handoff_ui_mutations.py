"""Run bounded handoff UI-helper faults using complete real Node assertion events."""
import argparse
import hashlib
import json
from pathlib import Path
import subprocess
import tempfile

from verify_selection_mutations import NODE_HARNESS as SELECTION_HARNESS, classify

NODE_HARNESS = SELECTION_HARNESS.replace(
    '/Selection helper .* must exist before/',
    '/(?:Selection|Handoff) helper .* must exist before/')
TESTS = ['tests/handoff-domain.test.mjs']
EXPECTED_TESTS = 8


def catalog():
    return [
        ('send_capability_ignored', '!context?.can_send || ', ''),
        ('send_employee_missing_allowed', '!context.employee?.actor_id || ', ''),
        ('organization_scope_ignored', 'row.organization_id === scope.organization_id && ', ''),
        ('warehouse_scope_ignored', ' && row.warehouse_id === scope.warehouse_id', ''),
        ('item_organization_ignored', 'item?.organization_id !== scope?.organization_id || ', ''),
        ('item_identity_ignored', 'item.id !== balance.item_id || ', ''),
        ('source_transit_stock_allowed', " || balance.stock_state !== 'available'", ''),
        ('source_zero_available_offered', "try {parseQuantity(formatQuantity(balance.available_qty),item.quantity_scale);} catch {return [];}", ''),
        ('api_fixed_six_normalization_removed', 'parseQuantity(formatQuantity(balance.available_qty),item.quantity_scale)', 'parseQuantity(balance.available_qty,item.quantity_scale)'),
        ('mapped_secondary_source_hidden', ' || (Array.isArray(row.position_ids)&&row.position_ids.includes(positionID))', ''),
        ('ambiguous_source_mapping_chooses_first', 'matches.length === 1?matches[0]:null', 'matches.length > 0?matches[0]:null'),
        ('configured_route_direction_reversed', 'context.routes.some(route=>route.source_department_id === source.id && route.destination_department_id === department.id)', 'context.routes.some(route=>route.destination_department_id === source.id && route.source_department_id === department.id)'),
        ('source_membership_ignored', '!member(context,source.id) || ', ''),
        ('unconfigured_route_allowed', 'context.routes.some(route=>route.source_department_id === source.id && route.destination_department_id === department.id)', 'true'),
        ('disabled_destination_allowed', ' && row.enabled === true', ''),
        ('quantity_availability_cap_removed', 'remaining(balance.available_qty,amount);', ''),
        ('quantity_availability_float_rounding', 'remaining(balance.available_qty,amount);', "if(Number(amount)>Number(balance.available_qty))throw new Error('Insufficient stock');"),
        ('fractional_pieces_allowed', 'parseQuantity(quantity,item.quantity_scale)', 'parseQuantity(quantity,6)'),
        ('accept_capability_ignored', 'context?.can_accept && ', ''),
        ('sender_can_accept_own_handoff', 'context.employee.actor_id !== handoff.sender_actor_id && ', ''),
        ('receiving_membership_ignored', 'member(context,handoff.destination_department_id) && ', ''),
        ('already_accepted_handoff_accepted_again', "handoff.state === 'sent' && ", ''),
        ('accept_scope_ignored', 'scoped(handoff,scope)', 'true'),
        ('wrong_expected_version', 'expected_version:args.handoff.version', 'expected_version:args.handoff.version+1'),
        ('send_posts_client_identity', 'return {...scope,source_balance_id:balance.id', 'return {...scope,sender_actor_id:context.employee.actor_id,source_balance_id:balance.id'),
        ('accept_posts_quantity', 'return {...args.scope,expected_version:', 'return {...args.scope,quantity:args.handoff.quantity,expected_version:'),
    ]


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--root', type=Path, default=Path(__file__).resolve().parents[1])
    parser.add_argument('--node', default='node')
    parser.add_argument('--output', type=Path, default=Path('work/handoff-ui-mutations.json'))
    args = parser.parse_args()
    root = args.root.resolve()
    source = root / 'web/src/handoff.js'
    inputs = [source, root / 'web/src/domain.js', *(root / 'web' / path for path in TESTS), Path(__file__).resolve(), Path(__file__).with_name('verify_selection_mutations.py').resolve()]
    originals = {path: path.read_bytes() for path in inputs}
    receipts = [{'path': path.relative_to(root).as_posix(), 'sha256': hashlib.sha256(data).hexdigest(), 'sha256_lf': hashlib.sha256(data.replace(b'\r\n', b'\n')).hexdigest()} for path, data in originals.items()]
    content = source.read_text(encoding='utf8')
    report = dict(passed=False, scope='Reviewed handoff UI selection, exact quantity and request-shape faults; not a whole-application mutation score.', sources=receipts, tests=TESTS, mutations=[], killed=0, survived=0, invalid=0, skipped=0)
    parent = root / 'web/work'
    parent.mkdir(exist_ok=True)
    with tempfile.TemporaryDirectory(prefix='handoff-ui-mutations-', dir=parent) as temporary:
        isolated = Path(temporary).resolve()
        isolated.relative_to(parent.resolve())
        (isolated / 'src').mkdir()
        (isolated / 'tests').mkdir()
        (isolated / 'package.json').write_text('{"type":"module"}\n', encoding='utf8')
        for path in inputs[:3]:
            (isolated / path.relative_to(root / 'web')).write_bytes(originals[path])
        harness = isolated / 'handoff-events.mjs'
        harness.write_text(NODE_HARNESS, encoding='utf8')
        target = isolated / 'src/handoff.js'

        def run():
            try:
                result = subprocess.run([args.node, str(harness), *TESTS], cwd=isolated, capture_output=True, text=True, encoding='utf8', timeout=45)
            except subprocess.TimeoutExpired:
                return dict(runner_error='timeout')
            if result.returncode:
                return dict(runner_error='process_failure', exit_code=result.returncode, stderr=result.stderr[-4000:])
            try:
                return json.loads(result.stdout)
            except ValueError:
                return dict(runner_error='invalid_event_report', stderr=result.stderr[-4000:])

        baseline = run()
        names = sorted(row['name'] for row in baseline.get('results', []))
        status = classify(baseline, names) if baseline.get('tests') == EXPECTED_TESTS else 'invalid'
        report['baseline'] = dict(status=status, **baseline)
        if status != 'survived':
            report['invalid'] += 1
        else:
            for name, before, after in catalog():
                if content.count(before) != 1:
                    record = dict(name=name, status='invalid', reason='reviewed_source_anchor_changed', matches=content.count(before))
                else:
                    changed = content.replace(before, after, 1)
                    target.write_text(changed, encoding='utf8', newline='\n')
                    try:
                        result = run()
                        outcome = classify(result, names)
                        record = dict(name=name, status=outcome, mutant_sha256=hashlib.sha256(changed.encode()).hexdigest(), **result)
                    finally:
                        target.write_bytes(originals[source])
                report['mutations'].append(record)
                report[record['status']] += 1
                print(json.dumps(dict(name=name, status=record['status'], failed=record.get('failed', 0))), flush=True)
    report['source_unchanged'] = all(path.read_bytes() == data for path, data in originals.items())
    report['passed'] = report['source_unchanged'] and len(report['mutations']) == len(catalog()) and report['killed'] == len(catalog()) and report['survived'] == report['invalid'] == 0
    args.output.parent.mkdir(parents=True, exist_ok=True)
    args.output.write_text(json.dumps(report, ensure_ascii=False, indent=2) + '\n', encoding='utf8')
    print(json.dumps({key: report[key] for key in ['passed', 'killed', 'survived', 'invalid', 'skipped', 'source_unchanged']}))
    return 0 if report['passed'] else 1


if __name__ == '__main__':
    raise SystemExit(main())
