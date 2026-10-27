"""Integration checks against an explicitly selected disposable local database.

Uses psql and Python's standard library. Never creates, drops or migrates a DB.
"""
import argparse
import concurrent.futures
import json
import os
from pathlib import Path
import subprocess
import time
import uuid

parser = argparse.ArgumentParser()
parser.add_argument('--psql', required=True)
parser.add_argument('--port', type=int, required=True)
parser.add_argument('--database', required=True)
parser.add_argument('--admin', required=True)
parser.add_argument('--report', required=True)
args = parser.parse_args()
if not args.database.startswith('wms_kernel_test'):
    raise SystemExit('Refusing a database without the wms_kernel_test prefix')

OWNER, RUNTIME, MODULE = 'wms_test_owner', 'wms_test_runtime', 'wms_test_module'
prefix = uuid.uuid4().hex[:10]
results = []


def q(value):
    if value is None:
        return 'NULL'
    return "'" + str(value).replace("'", "''") + "'"


def uid():
    return str(uuid.uuid4())


def run(sql, role=RUNTIME, app='wms_kernel_test'):
    env = dict(os.environ, PGAPPNAME=app, PGCLIENTENCODING='UTF8')
    statement = (f'SET ROLE {role}; ' if role else '') + sql
    result = subprocess.run([args.psql, '-X', '-qAt', '-h', '127.0.0.1', '-p', str(args.port),
                             '-U', args.admin, '-d', args.database, '-v', 'ON_ERROR_STOP=1', '-c', statement],
                            capture_output=True, encoding='utf-8', env=env, timeout=20)
    if result.returncode:
        raise RuntimeError(result.stderr.strip())
    return result.stdout.strip()


def scalar(sql, role=OWNER):
    return run(sql, role)


def call(name, *values):
    return json.loads(run(f'SELECT {name}(' + ','.join(q(v) for v in values) + ')'))


def fails(action, contains):
    try:
        action()
    except RuntimeError as error:
        assert contains.lower() in str(error).lower(), str(error)
        return
    raise AssertionError('Expected rejection: ' + contains)


def check(name, action):
    try:
        action()
        results.append({'name': name, 'status': 'passed'})
    except Exception as error:
        results.append({'name': name, 'status': 'failed', 'error': str(error)})


org, owner, wh, actor, actor2 = [uid() for _ in range(5)]
locations, positions = [uid() for _ in range(3)], [uid() for _ in range(3)]
seed = f"""
INSERT INTO platform.organizations VALUES ({q(org)}, {q(prefix)}, 'Integration test');
INSERT INTO platform.actors VALUES ({q(actor)}, 'https://example.invalid', {q(prefix+'-a')}),
 ({q(actor2)}, 'https://example.invalid', {q(prefix+'-b')});
INSERT INTO catalog.stock_owners VALUES ({q(owner)}, {q(org)}, {q(prefix)});
INSERT INTO catalog.units VALUES ('ea','count') ON CONFLICT DO NOTHING;
INSERT INTO warehouse.warehouses(id,organization_id,code) VALUES ({q(wh)}, {q(org)}, {q(prefix)});
"""
for i in range(3):
    seed += f"INSERT INTO warehouse.locations(id,organization_id,warehouse_id,code,kind) VALUES ({q(locations[i])},{q(org)},{q(wh)},{q(prefix+str(i))},'storage');"
    seed += f"INSERT INTO warehouse.positions(id,organization_id,warehouse_id,location_id) VALUES ({q(positions[i])},{q(org)},{q(wh)},{q(locations[i])});"
run(seed, OWNER)


def item(tracking='none', scale=0):
    value = uid()
    run(f"INSERT INTO catalog.items(id,organization_id,sku,base_unit,quantity_scale,tracking) VALUES ({q(value)},{q(org)},{q(value)},'ea',{scale},{q(tracking)})", OWNER)
    return value


def document(kind, quantity, item_id):
    doc, line = uid(), uid()
    run(f"""INSERT INTO warehouse.documents(id,organization_id,warehouse_id,owner_id,kind,number,state)
      VALUES ({q(doc)},{q(org)},{q(wh)},{q(owner)},{q(kind)},{q(doc)},'active');
      INSERT INTO warehouse.document_lines(id,document_id,organization_id,warehouse_id,owner_id,item_id,line_no,requested_qty)
      VALUES ({q(line)},{q(doc)},{q(org)},{q(wh)},{q(owner)},{q(item_id)},1,{q(quantity)});""", OWNER)
    return doc, line


def version(doc):
    return scalar(f'SELECT version FROM warehouse.documents WHERE id={q(doc)}')


def balance(item_id, index=0, state='available', lot=None, serial=None):
    return run('SELECT inventory.ensure_balance(' + ','.join(q(x) for x in [org,wh,owner,item_id,positions[index],state,lot,serial]) + ')')


def post(doc, line, kind, source, target, quantity, reservation=None, command=None, expected=None, reason=None):
    return call('inventory.post_movement', command or uid(), actor, line, expected or version(doc),
                kind, source, target, quantity, reservation, reason)


def stock(quantity=5, tracking='none', scale=0):
    it = item(tracking, scale)
    lot = serial = None
    if tracking in ('lot', 'lot_serial'):
        lot = uid()
        run(f"INSERT INTO catalog.lots(id,organization_id,item_id,code) VALUES({q(lot)},{q(org)},{q(it)},{q(lot)})",OWNER)
    if tracking in ('serial', 'lot_serial'):
        serial = uid()
        run(f"INSERT INTO catalog.serials(id,organization_id,item_id,lot_id,code) VALUES({q(serial)},{q(org)},{q(it)},{q(lot)},{q(serial)})",OWNER)
    b = balance(it, lot=lot, serial=serial)
    d,l = document('receipt',quantity,it)
    post(d,l,'receipt',None,b,quantity)
    return it,b,lot,serial


def reserve(doc,line,b,quantity,command=None,expected=None):
    return call('inventory.reserve_stock',command or uid(),actor,line,expected or version(doc),b,quantity)


def state(b):
    return json.loads(scalar(f"SELECT json_build_array(on_hand,reserved_qty) FROM inventory.stock_balances WHERE id={q(b)}"))


def basic_receipt():
    _,b,_,_ = stock(5)
    assert state(b) == [5,0]
    assert scalar(f'SELECT available_qty FROM contracts_v1.stock_balances WHERE id={q(b)}',RUNTIME) == '5.000000'
check('Receipt posts balance, ledger, operation and outbox', basic_receipt)


def nullable_identity():
    it = item()
    a,b = balance(it),balance(it)
    assert a == b
check('NULL lot and serial do not create duplicate balance identities',nullable_identity)


def identity_race():
    it = item()
    with concurrent.futures.ThreadPoolExecutor(2) as pool:
        values = list(pool.map(lambda _: balance(it), range(2)))
    assert values[0] == values[1]
check('Concurrent balance identity creation returns one ID',identity_race)


def partial_release():
    it,b,_,_ = stock(5); d,l = document('shipment',5,it)
    r = reserve(d,l,b,4)['reservation_id']
    call('inventory.release_reservation',uid(),actor,r,version(d),2,'cancelled remainder')
    assert state(b)==[5,2]
    assert scalar(f'SELECT active_qty FROM inventory.reservations WHERE id={q(r)}')=='2.000000'
check('Partial reservation release preserves physical stock',partial_release)


def overreserve():
    it,b,_,_=stock(5); d,l=document('shipment',8,it)
    fails(lambda:reserve(d,l,b,6),'insufficient_available_stock')
    assert state(b)==[5,0]
check('Cannot reserve more than available stock',overreserve)


def overorder():
    it,b,_,_=stock(5); d,l=document('shipment',2,it)
    fails(lambda:reserve(d,l,b,3),'check constraint')
    assert state(b)==[5,0]
check('Cannot reserve beyond unfulfilled order quantity',overorder)


def idem():
    it,b,_,_=stock(5); d,l=document('shipment',5,it); c=uid(); v=version(d)
    a=reserve(d,l,b,3,c,v); z=reserve(d,l,b,3,c,v)
    assert a==z and state(b)==[5,3]
    assert scalar(f'SELECT count(*) FROM platform.outbox WHERE operation_id={q(c)}')=='1'
check('Identical retry returns original result despite stale expected version',idem)


def idem_changed():
    it,b,_,_=stock(5); d,l=document('shipment',5,it); c=uid(); v=version(d)
    reserve(d,l,b,3,c,v)
    fails(lambda:reserve(d,l,b,2,c,v),'idempotency_key_reused')
check('Same command ID with changed payload is rejected',idem_changed)


def idem_actor():
    it,b,_,_=stock(5); d,l=document('shipment',5,it); c=uid(); v=version(d)
    reserve(d,l,b,3,c,v)
    fails(lambda:call('inventory.reserve_stock',c,actor2,l,v,b,3),'idempotency_key_reused')
    assert run(f'SELECT platform.get_operation_result({q(c)},{q(actor2)},{q(org)},{q(wh)})')==''
check('Changed actor cannot replay or read another actor command result',idem_actor)


def stale_version():
    it,b,_,_=stock(5); d,l=document('shipment',5,it)
    reserve(d,l,b,1,expected=1)
    fails(lambda:reserve(d,l,b,1,expected=1),'document_version_conflict')
check('Stale document version is rejected for a new command',stale_version)


def free_transfer():
    it,a,_,_=stock(5); b=balance(it,1); d,l=document('transfer',3,it)
    post(d,l,'transfer',a,b,3)
    assert state(a)==[2,0] and state(b)==[3,0]
check('Free transfer debits and credits atomically',free_transfer)


def reserved_transfer():
    it,a,_,_=stock(5); b=balance(it,1); d,l=document('shipment',4,it)
    r=reserve(d,l,a,4)['reservation_id']
    moved=post(d,l,'transfer',a,b,3,r)
    assert moved['new_reservation_id'] and state(a)==[2,1] and state(b)==[3,3]
    assert scalar(f'SELECT fulfilled_qty FROM warehouse.document_lines WHERE id={q(l)}')=='0.000000'
    assert scalar(f'SELECT reserved_qty FROM warehouse.document_lines WHERE id={q(l)}')=='4.000000'
check('Picking relocates reservation without fulfilling the shipment',reserved_transfer)


def ship_reserved():
    it,b,_,_=stock(5); d,l=document('shipment',4,it); r=reserve(d,l,b,4)['reservation_id']
    post(d,l,'issue',b,None,3,r)
    assert state(b)==[2,1]
    assert scalar(f'SELECT fulfilled_qty FROM warehouse.document_lines WHERE id={q(l)}')=='3.000000'
check('Issue consumes reservation and increases fulfillment',ship_reserved)


def cannot_move_reserved():
    it,a,_,_=stock(5); b=balance(it,1); d,l=document('shipment',5,it); reserve(d,l,a,4)
    td,tl=document('transfer',2,it)
    fails(lambda:post(td,tl,'transfer',a,b,2),'insufficient_available_stock')
check('Free transfer cannot steal reserved stock',cannot_move_reserved)


def rollback_late():
    it,a,_,_=stock(5); b=balance(it,1); d,l=document('transfer',2,it); c=uid()
    fails(lambda:post(d,l,'transfer',a,b,3,command=c),'check constraint')
    assert state(a)==[5,0] and state(b)==[0,0]
    assert scalar(f'SELECT count(*) FROM platform.operations WHERE id={q(c)}')=='0'
    assert scalar(f'SELECT count(*) FROM platform.outbox WHERE operation_id={q(c)}')=='0'
check('Failure after balance changes rolls back all effects',rollback_late)


def fractional():
    it,b,_,_=stock('1.125',scale=3); d,l=document('shipment','1.125',it)
    reserve(d,l,b,'0.125')
    assert state(b)==[1.125,0.125]
    fails(lambda:reserve(d,l,b,'0.0001'),'invalid_quantity')
check('Exact fractional quantities obey item scale without silent rounding',fractional)

for value in ['0','-1','NaN','Infinity','0.0000001']:
    def invalid_quantity(v=value):
        it,b,_,_=stock(2); d,l=document('shipment',2,it)
        fails(lambda:reserve(d,l,b,v),'invalid_quantity')
    check('Reject quantity '+value,invalid_quantity)


def quarantine():
    it=item(); b=balance(it,state='quarantine'); d,l=document('receipt',2,it); post(d,l,'receipt',None,b,2)
    sd,sl=document('shipment',2,it)
    fails(lambda:reserve(sd,sl,b,1),'stock_state_not_allocatable')
    assert scalar(f'SELECT available_qty FROM contracts_v1.stock_balances WHERE id={q(b)}',RUNTIME)=='0'
check('Quarantine stock is visible but not allocatable',quarantine)


def serial_identity():
    it,a,lot,s=stock(1,tracking='serial'); b=balance(it,1,serial=s)
    d,l=document('receipt',1,it)
    fails(lambda:post(d,l,'receipt',None,b,1),'stock_serial_occupancy_uq')
    td,tl=document('transfer',1,it); post(td,tl,'transfer',a,b,1)
    assert state(a)==[0,0] and state(b)==[1,0]
check('A serial has one live location and can be transferred',serial_identity)


def tracking_required():
    it=item('lot')
    fails(lambda:balance(it),'tracking_identity_mismatch')
check('Lot-tracked item requires a lot',tracking_required)


def lot_serial_mismatch():
    it,_,lot,s=stock(1,'lot_serial'); other=uid()
    run(f'INSERT INTO catalog.lots(id,organization_id,item_id,code) VALUES({q(other)},{q(org)},{q(it)},{q(other)})',OWNER)
    fails(lambda:balance(it,1,lot=other,serial=s),'serial_lot_mismatch')
check('Serial cannot be paired with a different lot',lot_serial_mismatch)


def scope_fk():
    it=item(); other_org=uid()
    run(f'INSERT INTO platform.organizations VALUES({q(other_org)},{q(other_org)},\'Other\')',OWNER)
    fails(lambda:run('SELECT inventory.ensure_balance('+','.join(q(x) for x in [other_org,wh,owner,it,positions[0],'available',None,None])+')'),'foreign key constraint')
check('Composite foreign keys reject cross-organization stock identity',scope_fk)


def item_mismatch():
    it,b,_,_=stock(5); other=item(); d,l=document('shipment',5,other)
    fails(lambda:reserve(d,l,b,1),'stock_scope_mismatch')
check('Reservation cannot use another item',item_mismatch)


def incomplete():
    fails(lambda:run(f"INSERT INTO platform.operations(id,actor_id,organization_id,warehouse_id,operation_type,request) VALUES({q(uid())},{q(actor)},{q(org)},{q(wh)},'fault','{{}}')",OWNER),'incomplete_operation_cannot_commit')
check('Incomplete operation cannot commit',incomplete)


def immutable():
    fails(lambda:run('UPDATE inventory.movement_lines SET quantity=quantity WHERE id=(SELECT min(id) FROM inventory.movement_lines)',OWNER),'append_only_history')
check('Posted movement history rejects updates',immutable)


def runtime_permissions():
    fails(lambda:run('UPDATE inventory.stock_balances SET on_hand=0'),'permission denied')
    fails(lambda:run('SELECT inventory.assert_quantity(NULL,1)'),'permission denied')
    fails(lambda:run('SELECT * FROM platform.operations'),'permission denied')
check('Runtime cannot write balances or invoke private helpers',runtime_permissions)


def module_permissions():
    assert scalar('SELECT count(*) FROM contracts_v1.stock_balances',MODULE)
    fails(lambda:run('UPDATE inventory.stock_balances SET on_hand=0',MODULE),'permission denied')
    fails(lambda:run('SELECT inventory.ensure_balance(NULL,NULL,NULL,NULL,NULL,NULL)',MODULE),'permission denied')
check('Module read role cannot call posting functions or mutate core',module_permissions)


def module_storage():
    d,l=document('shipment',1,item()); check_id=uid()
    run(f"GRANT USAGE ON SCHEMA mod_example_dispatch_quality TO {MODULE}; GRANT SELECT,INSERT ON mod_example_dispatch_quality.checks TO {MODULE}",OWNER)
    run(f"""INSERT INTO mod_example_dispatch_quality.checks(id,organization_id,warehouse_id,document_id,
      checked_content_revision,policy_revision,decision,reviewer_id,evidence_reference,expires_at)
      VALUES({q(check_id)},{q(org)},{q(wh)},{q(d)},1,1,'approved',{q(actor)},'test-evidence',clock_timestamp()+interval '1 hour')""",MODULE)
    fails(lambda:run(f'UPDATE mod_example_dispatch_quality.checks SET decision=\'rejected\' WHERE id={q(check_id)}',OWNER),'append_only_history')
check('Module writes its own storage while decisions remain append-only',module_storage)


def expiry_replay():
    it,b,_,_=stock(3); d,l=document('shipment',3,it); c=uid(); v=version(d)
    expiry=scalar("SELECT (clock_timestamp()+interval '1 hour')::text")
    a=call('inventory.reserve_stock',c,actor,l,v,b,2,expiry); r=a['reservation_id']
    run(f"UPDATE inventory.reservations SET expires_at=clock_timestamp()-interval '1 second' WHERE id={q(r)}",OWNER)
    z=call('inventory.reserve_stock',c,actor,l,v,b,2,expiry)
    assert a==z
    fails(lambda:post(d,l,'issue',b,None,1,r),'reservation_expired')
    call('inventory.release_reservation',uid(),actor,r,version(d),2,'expired')
    assert state(b)==[3,0]
check('Expired reservation blocks new issue, supports replay and explicit expiry release',expiry_replay)


def race(statements):
    def one(index_sql):
        index,sql=index_sql
        try:
            return {'ok':True,'result':run('BEGIN; '+sql+"; SELECT pg_sleep(0.3); COMMIT",app='wms_kernel_race_'+str(index))}
        except RuntimeError as error:
            return {'ok':False,'error':str(error)}
    with concurrent.futures.ThreadPoolExecutor(2) as pool:
        return list(pool.map(one,enumerate(statements)))


def reserve_race():
    it,b,_,_=stock(5); d1,l1=document('shipment',4,it); d2,l2=document('shipment',4,it)
    commands=[f'SELECT inventory.reserve_stock({q(uid())},{q(actor)},{q(l)},1,{q(b)},4)' for l in [l1,l2]]
    values=race(commands)
    assert sum(x['ok'] for x in values)==1,values
    assert 'insufficient_available_stock' in next(x['error'] for x in values if not x['ok'])
    assert state(b)==[5,4]
check('Concurrent reservations of the last stock cannot oversell',reserve_race)


def command_race():
    it,b,_,_=stock(5); d,l=document('shipment',4,it); c=uid()
    sql=f'SELECT inventory.reserve_stock({q(c)},{q(actor)},{q(l)},1,{q(b)},4)'
    values=race([sql,sql]); assert all(x['ok'] for x in values),values
    assert state(b)==[5,4]
    assert scalar(f'SELECT count(*) FROM inventory.reservation_events WHERE operation_id={q(c)}')=='1'
check('Concurrent identical commands post once',command_race)


def transfer_race():
    it,a,_,_=stock(5); b=balance(it,1); rd,rl=document('receipt',5,it); post(rd,rl,'receipt',None,b,5)
    d1,l1=document('transfer',2,it); d2,l2=document('transfer',2,it)
    commands=[f"SELECT inventory.post_movement({q(uid())},{q(actor)},{q(l)},1,'transfer',{q(src)},{q(dst)},2)" for l,src,dst in [(l1,a,b),(l2,b,a)]]
    values=race(commands); assert all(x['ok'] for x in values),values
    assert state(a)==[5,0] and state(b)==[5,0]
check('Opposite-direction transfers use consistent lock order',transfer_race)


def prepared_guard():
    it,a,_,_=stock(5); b=balance(it,1); d,l=document('transfer',2,it); c=uid()
    argv=','.join(q(x) for x in [c,actor,l,1,'transfer',a,b,2])
    run('BEGIN; SELECT inventory.prepare_movement('+argv+'); SELECT inventory.post_movement('+argv+'); COMMIT')
    assert state(a)==[3,0] and state(b)==[2,0]
    replay=json.loads(run('SELECT inventory.prepare_movement('+argv+')'))
    assert replay['replayed'] is True and replay['result']['operation_id']==c
check('Prepare and post share a transaction; retry bypasses stale guard snapshot',prepared_guard)


def incomplete_prepare():
    it,a,_,_=stock(5); b=balance(it,1); d,l=document('transfer',2,it); c=uid()
    argv=','.join(q(x) for x in [c,actor,l,1,'transfer',a,b,2])
    fails(lambda:run('SELECT inventory.prepare_movement('+argv+')'),'incomplete_operation_cannot_commit')
    assert state(a)==[5,0] and state(b)==[0,0]
    assert scalar(f'SELECT count(*) FROM platform.operations WHERE id={q(c)}')=='0'
check('Prepared command cannot commit without posting',incomplete_prepare)


def denied_guard():
    it,a,_,_=stock(5); b=balance(it,1); d,l=document('transfer',2,it); c=uid()
    argv=','.join(q(x) for x in [c,actor,l,1,'transfer',a,b,2])
    fails(lambda:run('BEGIN; SELECT inventory.prepare_movement('+argv+
      "); DO $$ BEGIN RAISE EXCEPTION 'required_guard_denied'; END $$; COMMIT"),'required_guard_denied')
    assert state(a)==[5,0] and state(b)==[0,0]
    assert scalar(f'SELECT count(*) FROM platform.operations WHERE id={q(c)}')=='0'
check('Guard denial rolls back prepared operation before stock posting',denied_guard)


def event_contract():
    catalog_path=Path(__file__).resolve().parents[1]/'contracts'/'core-contract-catalog.example.json'
    required=set(json.loads(catalog_path.read_text(encoding='utf-8'))['event_contracts'][0]['envelope_required'])
    payload=json.loads(scalar('SELECT payload FROM platform.outbox ORDER BY created_at LIMIT 1'))
    assert required.issubset(payload),str(required-set(payload))
    assert scalar('''SELECT count(*) FROM platform.operations p LEFT JOIN platform.outbox o ON o.operation_id=p.id
      WHERE p.result IS NOT NULL AND o.id IS NULL''')=='0'
check('Outbox envelope follows published contract and accompanies every completed command',event_contract)


def reconciliation():
    sql="""WITH r AS (SELECT balance_id,sum(active_qty) q FROM inventory.reservations GROUP BY balance_id),
      d AS (SELECT target_balance_id b,quantity q FROM inventory.movement_lines WHERE target_balance_id IS NOT NULL
       UNION ALL SELECT source_balance_id,-quantity FROM inventory.movement_lines WHERE source_balance_id IS NOT NULL),
      m AS (SELECT b,sum(q) q FROM d GROUP BY b)
      SELECT count(*) FROM inventory.stock_balances b LEFT JOIN r ON r.balance_id=b.id LEFT JOIN m ON m.b=b.id
      WHERE b.reserved_qty<>coalesce(r.q,0) OR b.on_hand<>coalesce(m.q,0)"""
    assert scalar(sql)=='0'
    assert scalar('''WITH r AS(SELECT document_line_id,sum(active_qty) q FROM inventory.reservations GROUP BY document_line_id)
      SELECT count(*) FROM warehouse.document_lines l LEFT JOIN r ON r.document_line_id=l.id WHERE l.reserved_qty<>coalesce(r.q,0)''')=='0'
    assert scalar('SELECT count(*) FROM platform.operations WHERE result IS NULL')=='0'
check('All balances and document reservations reconcile with durable history',reconciliation)

report={'status':'passed' if all(x['status']=='passed' for x in results) else 'failed',
        'postgresql':scalar('SHOW server_version',None),'passed':sum(x['status']=='passed' for x in results),
        'failed':sum(x['status']=='failed' for x in results),'checks':results,
        'scope':'Isolated PostgreSQL integration checks; no load, HA, crash durability or application authorization verification.'}
Path(args.report).parent.mkdir(parents=True,exist_ok=True)
Path(args.report).write_text(json.dumps(report,ensure_ascii=False,indent=2)+'\n',encoding='utf-8')
print(json.dumps(report,ensure_ascii=False,indent=2))
raise SystemExit(0 if report['status']=='passed' else 1)
