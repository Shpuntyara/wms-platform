import test from 'node:test';
import assert from 'node:assert/strict';
import * as domain from '../src/domain.js';

const scope = {organization_id:'org',warehouse_id:'warehouse'};
const document = {...scope,id:'document',owner_id:'owner',kind:'shipment',state:'active',version:2,lines:[{id:'line',item_id:'screw',requested_qty:'20',fulfilled_qty:'0',reserved_qty:'0'}]};
const item = {id:'screw',quantity_scale:0};
const balance = {...scope,id:'balance',owner_id:'owner',item_id:'screw',position_id:'A1',on_hand:'30',available_qty:'24',reserved_qty:'6'};
const requireFunction = name => {assert.equal(typeof domain[name],'function',`${name} must be available to the employee workflow`);return domain[name];};

test('material plan multiplies exact decimal recipe quantities and sums repeated components', () => {
  const demand=requireFunction('materialRequirements')({components:[{item_id:'plate',quantity:'1.125'},{item_id:'screw',quantity:'16'},{item_id:'screw',quantity:'2'}]},'10');
  assert.deepEqual(demand,[{item_id:'plate',quantity:'11.25'},{item_id:'screw',quantity:'180'}]);
});

test('material requirements reject a fraction smaller than one inventory micro-unit', () => {
  assert.throws(()=>requireFunction('materialRequirements')({components:[{item_id:'plate',quantity:'0.000001'}]},'0.1'),/dokładność|precyzj/);
});

test('material requirements preserve integer precision above Number.MAX_SAFE_INTEGER', () => {
  assert.deepEqual(requireFunction('materialRequirements')({components:[{item_id:'screw',quantity:'9007199254740993'}]},'2'),[{item_id:'screw',quantity:'18014398509481986'}]);
});

test('material requirements do not admit zero, negative quantities or malformed components', () => {
  const calculate=requireFunction('materialRequirements');
  for(const quantity of ['0','-1','1e3','1.0000001']) assert.throws(()=>calculate({components:[{item_id:'screw',quantity}]},'10'));
  assert.throws(()=>calculate({components:[]},'10'));
});

test('availability uses the correct warehouse and owner and sums only available quantities', () => {
  const rows=[balance,{...balance,id:'two',available_qty:'3'},{...balance,id:'forbidden',owner_id:'other',available_qty:'999'},{...balance,id:'remote',warehouse_id:'remote',available_qty:'999'}];
  assert.deepEqual(requireFunction('materialAvailability')([{item_id:'screw',quantity:'40'}],rows,{...scope,owner_id:'owner'}),[{item_id:'screw',quantity:'40',available_qty:'27',shortage_qty:'13'}]);
});

test('surplus materials show zero shortage rather than a negative need', () => {
  assert.deepEqual(requireFunction('materialAvailability')([{item_id:'screw',quantity:'4'}],[balance],{...scope,owner_id:'owner'}),[{item_id:'screw',quantity:'4',available_qty:'24',shortage_qty:'0'}]);
});

test('stock search matches Polish names, SKU and shelf code while preserving row identity', () => {
  const items=[{id:'screw',name:'Śruba M8 × 40, ocynkowana',sku:'SRUBA-M8'}];
  const positions=[{id:'A1',code:'A1',name:'Regał A, poziom 1'}];
  for(const term of ['ŚRUBA','sruba','M8','a1','poziom 1']) assert.deepEqual(requireFunction('findStock')([balance],items,positions,term),[balance]);
  assert.deepEqual(domain.findStock([balance],items,positions,'kalendarz'),[]);
});

test('critic: stock search folds accents in names independently of the SKU',()=>{
  const items=[{id:'screw',name:'Łącznik śrubowy',sku:'ART-001'}];
  assert.deepEqual(requireFunction('findStock')([balance],items,[{id:'A1',code:'A1'}],'lacznik srubowy'),[balance]);
});

test('shipment reservation validates remaining quantity and genuinely available source stock', () => {
  const reserve=requireFunction('reservationIntent');
  assert.deepEqual(reserve({document,line:document.lines[0],item,sourceBalance:balance,quantity:'2',scope}),{...scope,document_line_id:'line',expected_document_version:2,balance_id:'balance',quantity:'2'});
  assert.throws(()=>reserve({document,line:document.lines[0],item,sourceBalance:{...balance,available_qty:'1'},quantity:'2',scope}),/dostępn/);
  assert.throws(()=>reserve({document,line:document.lines[0],item,sourceBalance:balance,quantity:'21',scope}),/pozostał/);
  assert.throws(()=>reserve({document,line:document.lines[0],item,sourceBalance:{...balance,owner_id:'other'},quantity:'2',scope}),/właściciel|Adres/);
  const reserved={...document.lines[0],reserved_qty:'14'}, partial={...document,lines:[reserved]};
  assert.throws(()=>reserve({document:partial,line:reserved,item,sourceBalance:balance,quantity:'10',scope}),/zarezerwow/);
});

test('shipment issue requires its own active reservation and never another document or balance', () => {
  const issue=requireFunction('issueIntent');
  const line={...document.lines[0],reserved_qty:'2'};
  const doc={...document,version:3,lines:[line]};
  const reservation={id:'reservation',...scope,document_line_id:'line',balance_id:'balance',active_qty:'2',owner_id:'owner',item_id:'screw'};
  assert.deepEqual(issue({document:doc,line,item,sourceBalance:balance,reservation,quantity:'2',scope}),{...scope,document_line_id:'line',expected_document_version:3,kind:'issue',source_balance_id:'balance',reservation_id:'reservation',quantity:'2'});
  for(const altered of [{...reservation,document_line_id:'other'},{...reservation,balance_id:'other'},{...reservation,active_qty:'1'},{...reservation,owner_id:'other'}]) assert.throws(()=>issue({document:doc,line,item,sourceBalance:balance,reservation:altered,quantity:'2',scope}));
  assert.throws(()=>issue({document:doc,line,item,sourceBalance:{...balance,on_hand:'1'},reservation,quantity:'2',scope}),/wystarczając/);
});
