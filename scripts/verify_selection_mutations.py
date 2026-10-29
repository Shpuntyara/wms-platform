"""Bounded selection-helper faults with actual Node assertion-event evidence."""
import argparse
import hashlib
import json
from pathlib import Path
import subprocess
import tempfile


TESTS=['tests/selection-critic.test.mjs','tests/selection-api-quantity-critic.test.mjs']
EXPECTED_TESTS=25
NODE_HARNESS=r'''
import {run} from 'node:test';
const results=[];
function causes(error) {
  const result=[],seen=new Set();
  while(error && !seen.has(error) && result.length<8) {
    seen.add(error);
    result.push({name:error.name||'',code:error.code||'',message:String(error.message||error),failureType:error.failureType||''});
    error=error.cause;
  }
  return result;
}
for await(const event of run({files:process.argv.slice(2),concurrency:false})) {
  if(event.type!=='test:pass'&&event.type!=='test:fail') continue;
  const data=event.data,chain=causes(data.details?.error);
  results.push({name:data.name,status:event.type==='test:pass'?'pass':'fail',skip:Boolean(data.skip),todo:Boolean(data.todo),
    assertion:chain.some(error=>error.code==='ERR_ASSERTION'),
    infrastructure_error:chain.some(error=>['SyntaxError','TypeError','ReferenceError'].includes(error.name)||['ERR_MODULE_NOT_FOUND','MODULE_NOT_FOUND','ERR_UNKNOWN_FILE_EXTENSION'].includes(error.code)),
    unavailable_helper:chain.some(error=>/Selection helper .* must exist before/.test(error.message)),errors:chain});
}
console.log(JSON.stringify({tests:results.length,passed:results.filter(row=>row.status==='pass').length,
  failed:results.filter(row=>row.status==='fail').length,skipped:results.filter(row=>row.skip).length,
  todo:results.filter(row=>row.todo).length,results}));
'''


def catalog():
    zero=r"const zero = value => typeof value === 'string' && /^0(?:\.0{1,6})?$/.test(value);"
    canonical=r"value.replace(/(\.\d*?[1-9])0+$/,'$1').replace(/\.0+$/,'')"
    return [
        ('inactive_document_allowed', " || document.state !== 'active'", ''),
        ('organization_scope_ignored', 'row.organization_id === scope.organization_id && ', ''),
        ('warehouse_scope_ignored', ' && row.warehouse_id === scope.warehouse_id', ''),
        ('completed_document_line_offered', "remaining(line.requested_qty,line.fulfilled_qty) !== '0'", 'true'),
        ('balance_owner_ignored', ' || balance.owner_id !== document.owner_id', ''),
        ('balance_item_ignored', ' || balance.item_id !== item.id', ''),
        ('nonavailable_stock_state_offered', " || balance.stock_state !== 'available'", ''),
        ('source_physical_quantity_used', 'positive(balance.available_qty,item.quantity_scale)', 'positive(balance.on_hand,item.quantity_scale)'),
        ('reservation_other_line_offered', 'reservation.document_line_id === line.id && ', ''),
        ('reservation_other_balance_offered', 'reservation.balance_id === balance.id && ', ''),
        ('reservation_other_owner_offered', 'reservation.owner_id === document.owner_id && ', ''),
        ('reservation_zero_quantity_offered', ' && positive(reservation.active_qty,item.quantity_scale)', ''),
        ('mm_borrows_wz_reservation', "document.kind === 'shipment' && reservations.some", 'reservations.some'),
        ('receipt_target_wrong_kind', "places[0].kind === 'receiving'", "places[0].kind !== 'receiving'"),
        ('mm_target_wrong_kind', "places[0].kind !== 'storage' || ", ''),
        ('mm_target_other_lot', '(balance.lot_id??null) !== (sourceBalance.lot_id??null) || ', ''),
        ('mm_target_other_serial', '(balance.serial_id??null) !== (sourceBalance.serial_id??null) || ', ''),
        ('empty_serial_target_never_offered', zero, 'const zero = value => false;'),
        ('occupied_or_malformed_serial_target_offered', zero, 'const zero = value => true;'),
        ('api_quantity_normalization_removed', 'parseQuantity(canonicalData(value),scale)', 'parseQuantity(value,scale)'),
        ('fractional_pieces_admitted', 'parseQuantity(canonicalData(value),scale)', 'parseQuantity(canonicalData(value),6)'),
        ('api_quantity_float_rounding', canonical, 'String(Number(value))'),
        ('missing_balance_identity_falls_through', 'if (!id || matches.length !== 1)', 'if (false)'),
    ]


def classify(result,expected_names):
    if result.get('runner_error'):
        return 'invalid'
    events=result.get('results',[])
    names=sorted(row['name'] for row in events)
    if not events or names!=expected_names or result['tests']!=len(expected_names) or result['skipped'] or result['todo']:
        return 'invalid'
    failures=[row for row in events if row['status']=='fail']
    if any(row['infrastructure_error'] or row['unavailable_helper'] or not row['assertion'] for row in failures):
        return 'invalid'
    return 'killed' if failures else 'survived'


def main():
    parser=argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--root',type=Path,default=Path(__file__).resolve().parents[1])
    parser.add_argument('--node',default='node')
    parser.add_argument('--output',type=Path,default=Path('work/selection-mutations.json'))
    args=parser.parse_args();root=args.root.resolve()
    source=root/'web/src/selection.js'
    paths=[source,root/'web/src/domain.js',*(root/'web'/name for name in TESTS),Path(__file__).resolve()]
    originals={path:path.read_bytes() for path in paths}
    hashes={path.relative_to(root).as_posix():hashlib.sha256(data).hexdigest() for path,data in originals.items()}
    lf_hashes={path.relative_to(root).as_posix():hashlib.sha256(data.replace(b'\r\n',b'\n')).hexdigest() for path,data in originals.items()}
    content=source.read_text(encoding='utf8')
    report=dict(passed=False,scope='Selected non-equivalent line, scope, stock, tracking, reservation and API quantity selection faults; not a whole-UI mutation score.',
                source_sha256=hashes,source_sha256_lf=lf_hashes,tests=TESTS,mutations=[],killed=0,survived=0,invalid=0,skipped=0)
    parent=root/'web/work';parent.mkdir(exist_ok=True)
    with tempfile.TemporaryDirectory(prefix='selection-mutations-',dir=parent) as temporary:
        isolated=Path(temporary).resolve();isolated.relative_to(parent.resolve())
        (isolated/'src').mkdir();(isolated/'tests').mkdir()
        (isolated/'package.json').write_text('{"type":"module"}\n',encoding='utf8')
        for path in paths[:-1]:
            target=isolated/path.relative_to(root/'web');target.write_bytes(originals[path])
        harness=isolated/'selection-test-events.mjs'
        harness.write_text(NODE_HARNESS,encoding='utf8')
        target=isolated/'src/selection.js'
        def run():
            try:
                value=subprocess.run([args.node,str(harness),*TESTS],cwd=isolated,capture_output=True,text=True,encoding='utf8',timeout=45)
            except subprocess.TimeoutExpired:
                return dict(runner_error='timeout')
            if value.returncode!=0:
                return dict(runner_error='process_failure',exit_code=value.returncode,stderr=value.stderr[-4000:])
            try:
                return json.loads(value.stdout)
            except ValueError:
                return dict(runner_error='invalid_event_report',stderr=value.stderr[-4000:])
        baseline=run()
        expected_names=sorted(row['name'] for row in baseline.get('results',[]))
        baseline_status=classify(baseline,expected_names) if baseline.get('tests')==EXPECTED_TESTS else 'invalid'
        report['baseline']=dict(status=baseline_status,**baseline)
        if baseline_status!='survived':
            report['invalid']+=1
        else:
            for name,before,after in catalog():
                if content.count(before)!=1:
                    record=dict(name=name,status='invalid',reason='reviewed_source_anchor_changed',matches=content.count(before))
                else:
                    changed=content.replace(before,after,1)
                    target.write_text(changed,encoding='utf8',newline='\n')
                    try:
                        result=run()
                        status=classify(result,expected_names)
                        record=dict(name=name,status=status,source='web/src/selection.js',mutant_sha256=hashlib.sha256(changed.encode()).hexdigest(),**result)
                    finally:
                        target.write_bytes(originals[source])
                report['mutations'].append(record);report[record['status']]+=1
                print(json.dumps(dict(name=name,status=record['status'],failed=record.get('failed',0))),flush=True)
    unchanged=all(path.read_bytes()==data for path,data in originals.items())
    report['source_unchanged']=unchanged
    report['passed']=unchanged and len(report['mutations'])==len(catalog()) and report['killed']==len(catalog()) and report['survived']==report['invalid']==0
    args.output.parent.mkdir(parents=True,exist_ok=True)
    args.output.write_text(json.dumps(report,ensure_ascii=False,indent=2)+'\n',encoding='utf8')
    print(json.dumps({key:report[key] for key in ['passed','killed','survived','invalid','skipped','source_unchanged']}))
    return 0 if report['passed'] else 1


if __name__=='__main__':
    raise SystemExit(main())
