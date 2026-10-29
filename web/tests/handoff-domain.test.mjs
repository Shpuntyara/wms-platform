import test from 'node:test';
import assert from 'node:assert/strict';
import fc from 'fast-check';
let helpers={};
try {helpers=await import('../src/handoff.js');} catch(error) {if(error.code!=='ERR_MODULE_NOT_FOUND') throw error;}
const helper=name=>{assert.equal(typeof helpers[name],'function',`Handoff helper ${name} must exist before department transfers are safe`);return helpers[name];};
const scope={organization_id:'org',warehouse_id:'wh'};
function fixture() {
  const item={id:'item',organization_id:'org',quantity_scale:0};
  const balance={id:'source',...scope,owner_id:'owner',item_id:'item',position_id:'A1',stock_state:'available',available_qty:'24.000000',on_hand:'24.000000',lot_id:null,serial_id:null};
  const context={employee:{actor_id:'sender',display_name:'Marek Wójcik'},can_send:true,can_accept:true,
    memberships:[{department_id:'warehouse'}],departments:[{id:'warehouse',name:'Magazyn',position_id:'A1',enabled:true},{id:'welding',name:'Spawanie',position_id:'S1',enabled:true},{id:'polishing',name:'Szlifowanie',position_id:'P1',enabled:true}],
    routes:[{source_department_id:'warehouse',destination_department_id:'welding'}]};
  const handoff={id:'handoff',...scope,owner_id:'owner',item_id:'item',quantity:'2.000000',version:1,state:'sent',source_department_id:'warehouse',destination_department_id:'welding',sender_actor_id:'sender'};
  return {scope,item,balance,context,handoff};
}
test('handoff destinations are configured enabled routes for the authenticated source member only',()=>{
  const destinations=helper('handoffDestinations'),f=fixture();
  assert.deepEqual(destinations(f).map(row=>row.id),['welding']);
  assert.deepEqual(destinations({...f,context:{...f.context,memberships:[]}}),[]);
  assert.deepEqual(destinations({...f,balance:{...f.balance,stock_state:'in_transit'}}),[]);
  assert.deepEqual(destinations({...f,balance:{...f.balance,warehouse_id:'other'}}),[]);
  assert.deepEqual(destinations({...f,context:{...f.context,can_send:false}}),[]);
  assert.deepEqual(destinations({...f,context:{...f.context,employee:null}}),[]);
  assert.deepEqual(destinations({...f,item:{...f.item,organization_id:'foreign'}}),[]);
  assert.deepEqual(destinations({...f,context:{...f.context,departments:f.context.departments.map(row=>row.id==='warehouse'?{...row,enabled:false}:row)}}),[]);
  assert.deepEqual(destinations({...f,item:{...f.item,id:'other-item'}}),[]);
  assert.deepEqual(destinations({...f,balance:{...f.balance,available_qty:'0.000000'}}),[]);
  assert.deepEqual(destinations({...f,context:{...f.context,departments:[...f.context.departments,{id:'ambiguous',position_id:'A1',enabled:true}]}}),[]);
  f.context.departments[1].enabled=false;
  assert.deepEqual(destinations(f),[]);
});
test('handoff send intent keeps exact decimal quantity and excludes owner or typed identities',()=>{
  const send=helper('handoffSendIntent'),f=fixture();f.item.quantity_scale=3;
  let intent;assert.doesNotThrow(()=>{intent=send({...f,destinationID:'welding',quantity:'2,125'});});
  assert.deepEqual(intent,{...scope,source_balance_id:'source',destination_department_id:'welding',quantity:'2.125'});
});
test('all explicitly mapped warehouse positions can send along the department route',()=>{
  const destinations=helper('handoffDestinations'),f=fixture();
  f.context.departments[0].position_ids=['A1','B1'];f.balance.position_id='B1';
  assert.deepEqual(destinations(f).map(row=>row.id),['welding']);
});
test('handoff send rejects unavailable quantity, fractional pieces and an unconfigured destination',()=>{
  const send=helper('handoffSendIntent'),f=fixture();
  for(const change of [{quantity:'25'},{quantity:'0.5'},{destinationID:'polishing'},{balance:{...f.balance,stock_state:'in_transit'}},{balance:{...f.balance,organization_id:'other'}}]) {
    assert.throws(()=>send({...f,destinationID:'welding',quantity:'1',...change}));
  }
});
test('only a different authenticated receiving member can accept a sent handoff',()=>{
  const accept=helper('canAcceptHandoff'),f=fixture();
  const receiver={...f.context,employee:{actor_id:'receiver',display_name:'Anna Nowak'},memberships:[{department_id:'welding'}]};
  assert.equal(accept({...f,context:receiver}),true);
  assert.equal(accept({...f,context:{...receiver,employee:f.context.employee}}),false);
  assert.equal(accept({...f,context:{...receiver,memberships:[]}}),false);
  assert.equal(accept({...f,context:{...receiver,employee:null}}),false);
  assert.equal(accept({...f,context:{...receiver,can_accept:false}}),false);
  assert.equal(accept({...f,context:receiver,handoff:{...f.handoff,state:'accepted'}}),false);
  assert.equal(accept({...f,context:receiver,scope:{...scope,warehouse_id:'other'}}),false);
});
test('acceptance confirms the full existing handoff version without quantity or identity inputs',()=>{
  const accept=helper('handoffAcceptIntent'),f=fixture();
  f.context.employee={actor_id:'receiver',display_name:'Anna Nowak'};f.context.memberships=[{department_id:'welding'}];
  assert.deepEqual(accept(f),{...scope,expected_version:1});
});
test('handoff property: one micro-unit above availability never passes through floating point rounding',()=>{
  const send=helper('handoffSendIntent');
  const decimal=micros=>`${micros/1000000n}.${String(micros%1000000n).padStart(6,'0')}`;
  fc.assert(fc.property(fc.bigInt({min:1n,max:99999999999999999999999n}),available=>{
    const f=fixture();f.item.quantity_scale=6;f.balance.available_qty=decimal(available);
    let intent;assert.doesNotThrow(()=>{intent=send({...f,destinationID:'welding',quantity:decimal(available)});});
    assert.equal(intent.quantity,decimal(available).replace(/(\.\d*?)0+$/,'$1').replace(/\.$/,''));
    assert.throws(()=>send({...f,destinationID:'welding',quantity:decimal(available+1n)}));
  }),{seed:2026103001,numRuns:180});
});
test('handoff property: routes equal an independent scope membership and enabled-department oracle',()=>{
  const destinations=helper('handoffDestinations');
  const fields=['org','warehouse','itemOrg','member','employee','send','sourceEnabled','available'];
  fc.assert(fc.property(fc.constantFrom('ok',...fields),fc.bigInt({min:1n,max:999999999999999999n}),fc.array(fc.record({enabled:fc.boolean(),route:fc.boolean()}),{maxLength:25}),(fault,free,targets)=>{
    // Keep one valid target in every history. Each generated single denied
    // condition must therefore suppress a real route, not an empty fixture.
    const row={...Object.fromEntries(fields.map(field=>[field,field!==fault])),free,targets:[{enabled:true,route:true},...targets]};
    const f=fixture();f.balance.organization_id=row.org?'org':'foreign';f.balance.warehouse_id=row.warehouse?'wh':'foreign';f.balance.stock_state=row.available?'available':'in_transit';f.balance.available_qty=`${row.free}.000000`;
    f.item.organization_id=row.itemOrg?'org':'foreign';f.context.can_send=row.send;f.context.employee=row.employee?f.context.employee:null;f.context.memberships=row.member?[{department_id:'warehouse'}]:[];
    f.context.departments=[{id:'warehouse',position_id:'A1',position_ids:['A1','B1'],enabled:row.sourceEnabled},...row.targets.map((target,index)=>({id:`department-${index}`,position_id:`position-${index}`,enabled:target.enabled}))];
    f.context.routes=row.targets.flatMap((target,index)=>target.route?[{source_department_id:'warehouse',destination_department_id:`department-${index}`}]:[]);
    const sourceAllowed=row.org&&row.warehouse&&row.itemOrg&&row.member&&row.employee&&row.send&&row.sourceEnabled&&row.available;
    const expected=sourceAllowed?row.targets.flatMap((target,index)=>target.enabled&&target.route?[`department-${index}`]:[]):[];
    assert.deepEqual(destinations(f).map(department=>department.id),expected);
  }),{seed:2026103002,numRuns:220});
});
