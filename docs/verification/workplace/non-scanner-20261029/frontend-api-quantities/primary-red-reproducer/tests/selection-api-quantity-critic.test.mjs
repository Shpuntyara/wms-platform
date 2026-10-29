import test from 'node:test';
import assert from 'node:assert/strict';
import {balanceChoices} from '../src/selection.js';

function apiFixture(kind='transfer',scale=0) {
  const scope={organization_id:'org',warehouse_id:'wh'};
  const item={id:'item',organization_id:'org',sku:'SR-M8',quantity_scale:scale};
  const line={id:'line',item_id:item.id,requested_qty:'3.000000',fulfilled_qty:'0.000000',reserved_qty:'0.000000'};
  const document={id:'document',...scope,owner_id:'owner',kind,state:'active',version:2,lines:[line]};
  const positions=[{id:'A1',...scope,code:'A1',kind:'storage'},{id:'B1',...scope,code:'B1',kind:'storage'},{id:'PZ',...scope,code:'PRZYJECIE-01',kind:'receiving'}];
  const source={id:'source',...scope,owner_id:'owner',item_id:item.id,position_id:'A1',stock_state:'available',lot_id:null,serial_id:null,on_hand:'24.000000',reserved_qty:'0.000000',available_qty:'24.000000'};
  const target={...source,id:'target',position_id:kind==='receipt'?'PZ':'B1',on_hand:'0.000000',available_qty:'0.000000'};
  return {scope,item,line,document,positions,source,target,balances:[source,target],reservations:[]};
}

for(const [name,kind,scale,quantity] of [
  ['MM pieces','transfer',0,'24.000000'],
  ['WZ pieces','shipment',0,'24.000000'],
  ['MM millimetre-scale goods','transfer',3,'1.234000'],
]) test(`real API quantities: ${name} remain selectable with PostgreSQL six-place serialization`,()=>{
  const f=apiFixture(kind,scale);
  f.source.available_qty=quantity;f.source.on_hand=quantity;
  assert.deepEqual(balanceChoices({...f,role:'source'}).map(row=>row.id),['source']);
});

test('real API quantities: WZ fully reserved for this line remains selectable',()=>{
  const f=apiFixture('shipment');
  f.source.available_qty='0.000000';f.source.reserved_qty='2.000000';f.line.reserved_qty='2.000000';
  f.reservations=[{id:'reservation',...f.scope,owner_id:'owner',item_id:'item',document_line_id:'line',balance_id:'source',active_qty:'2.000000'}];
  assert.deepEqual(balanceChoices({...f,role:'source'}).map(row=>row.id),['source']);
});

for(const kind of ['receipt','transfer']) test(`real API quantities: ${kind} cannot target an already occupied serial balance`,()=>{
  const f=apiFixture(kind);
  f.source.serial_id=f.target.serial_id='SERIAL-1';
  f.target.on_hand='1.000000';f.target.available_qty='1.000000';
  assert.deepEqual(balanceChoices({...f,sourceBalance:f.source,role:'target'}),[]);
});

test('real API quantities: normalization must not admit fractional pieces',()=>{
  const f=apiFixture('transfer',0);f.source.available_qty='0.500000';f.source.on_hand='0.500000';
  assert.deepEqual(balanceChoices({...f,role:'source'}),[]);
});
