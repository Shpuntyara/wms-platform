import test from 'node:test';
import assert from 'node:assert/strict';
import fc from 'fast-check';

let selection = {};
try {selection = await import('../src/selection.js');} catch (error) {
  if (error.code !== 'ERR_MODULE_NOT_FOUND') throw error;
}
const helper = name => {
  assert.equal(typeof selection[name],'function',`Selection helper ${name} must exist before the employee can safely choose a balance`);
  return selection[name];
};
const scope = {organization_id:'org',warehouse_id:'warehouse'};
function fixture(kind='transfer') {
  const item={id:'item',organization_id:'org',sku:'NOT-27-CZ-A5',name:'Notes 2027 czerwony A5',unit:'szt.',quantity_scale:0};
  const line={id:'line',item_id:item.id,requested_qty:'20',fulfilled_qty:'0',reserved_qty:'0'};
  const document={...scope,id:'document',owner_id:'owner',kind,state:'active',version:3,lines:[line]};
  const positions=[['source','A1','storage'],['target','B1','storage'],['receiving','PRZYJECIE-01','receiving'],['shipping','WYDANIE-01','shipping']].map(([id,code,kind])=>({id,code,kind,...scope}));
  const base={...scope,owner_id:'owner',item_id:item.id,stock_state:'available',lot_id:null,serial_id:null,version:1};
  const sourceBalance={...base,id:'balance-source',position_id:'source',on_hand:'12',reserved_qty:'2',available_qty:'10'};
  const targetBalance={...base,id:'balance-target',position_id:'target',on_hand:'0',reserved_qty:'0',available_qty:'0'};
  const receiving={...targetBalance,id:'balance-receiving',position_id:'receiving'};
  const shipping={...targetBalance,id:'balance-shipping',position_id:'shipping'};
  return {scope,document,line,item,items:[item],positions,balances:[sourceBalance,targetBalance,receiving,shipping],sourceBalance,targetBalance,reservations:[]};
}
const choices = (data,role) => helper('balanceChoices')({...data,role});
const resolve = data => helper('resolveSelection')({...data,lineID:data.line.id,sourceID:data.sourceBalance?.id,targetID:data.document.kind==='receipt'?'balance-receiving':data.document.kind==='transfer'?data.targetBalance?.id:undefined});

test('critic: duplicate item names and repeated product lines retain separate line identities',()=>{
  const data=fixture();
  const second={...data.item,id:'item-two',sku:'NOT-27-CZ-A5-ALT'};
  const duplicate={...data.line,id:'line-two'};
  const variant={...data.line,id:'line-three',item_id:second.id};
  const finished={...data.line,id:'line-finished',fulfilled_qty:'20'};
  const document={...data.document,lines:[data.line,duplicate,variant,finished]};
  const result=helper('lineChoices')(document,[data.item,second],scope);
  assert.deepEqual(result.map(row=>[row.line.id,row.item.id]),[['line','item'],['line-two','item'],['line-three','item-two']]);
});

test('critic: choices fail closed for inactive document, foreign warehouse and missing catalog item',()=>{
  const data=fixture();const select=helper('lineChoices');
  assert.deepEqual(select({...data.document,state:'closed'},data.items,scope),[]);
  assert.deepEqual(select(data.document,data.items,{...scope,warehouse_id:'other'}),[]);
  assert.deepEqual(select(data.document,[],scope),[]);
});

test('critic: source choices use available stock, owner and complete scope rather than physical stock',()=>{
  const data=fixture();
  const forbidden=[{...data.sourceBalance,id:'reserved-elsewhere',available_qty:'0',on_hand:'999'},
    {...data.sourceBalance,id:'other-owner',owner_id:'other'},
    {...data.sourceBalance,id:'other-item',item_id:'other'},
    {...data.sourceBalance,id:'other-wh',warehouse_id:'other'},
    {...data.sourceBalance,id:'other-org',organization_id:'other'},
    {...data.sourceBalance,id:'quarantine',stock_state:'quarantine'},
    {...data.sourceBalance,id:'unknown-state',stock_state:undefined}];
  assert.deepEqual(choices({...data,balances:[...forbidden,data.sourceBalance]},'source'),[data.sourceBalance]);
});

test('critic: WZ with zero free stock can resume only its own exact active reservation',()=>{
  const data=fixture('shipment');
  data.sourceBalance={...data.sourceBalance,on_hand:'2',reserved_qty:'2',available_qty:'0'};
  data.line.reserved_qty='2';
  data.balances=[data.sourceBalance];
  const own={...scope,id:'reservation',owner_id:'owner',item_id:'item',document_line_id:'line',balance_id:data.sourceBalance.id,active_qty:'2'};
  assert.deepEqual(choices({...data,reservations:[own]},'source'),[data.sourceBalance]);
  for(const [field,value] of Object.entries({organization_id:'other',warehouse_id:'other',owner_id:'other',item_id:'other',document_line_id:'other',balance_id:'other',active_qty:'0'})) {
    assert.deepEqual(choices({...data,reservations:[{...own,[field]:value}]},'source'),[],field);
  }
  assert.deepEqual(choices({...data,document:{...data.document,kind:'transfer'},reservations:[own]},'source'),[],'MM must not borrow WZ reservations');
});

test('critic: zero-quantity PZ target is selectable only in receiving positions',()=>{
  const data=fixture('receipt');
  assert.deepEqual(choices(data,'target').map(row=>row.id),['balance-receiving']);
  assert.deepEqual(choices(data,'source'),[]);
});

test('critic: MM target is a different storage position with identical lot serial and state',()=>{
  const data=fixture();
  data.sourceBalance={...data.sourceBalance,lot_id:'lot-A',serial_id:'serial-A',on_hand:'1',available_qty:'1',reserved_qty:'0'};
  data.targetBalance={...data.targetBalance,lot_id:'lot-A',serial_id:'serial-A'};
  const forbidden=[{...data.targetBalance,id:'same-place',position_id:'source'},
    {...data.targetBalance,id:'other-lot',lot_id:'lot-B'},
    {...data.targetBalance,id:'other-serial',serial_id:'serial-B'},
    {...data.targetBalance,id:'other-state',stock_state:'quarantine'},
    {...data.targetBalance,id:'shipping',position_id:'shipping'},
    {...data.targetBalance,id:'receiving',position_id:'receiving'}];
  data.balances=[data.sourceBalance,...forbidden,data.targetBalance];
  assert.deepEqual(choices(data,'target'),[data.targetBalance]);
});

test('critic: selecting one of several lots at the same named address never substitutes the first lot',()=>{
  const data=fixture();
  const otherSource={...data.sourceBalance,id:'source-lot-A',lot_id:'A'};
  data.sourceBalance={...data.sourceBalance,id:'source-lot-B',lot_id:'B'};
  const otherTarget={...data.targetBalance,id:'target-lot-A',lot_id:'A'};
  data.targetBalance={...data.targetBalance,id:'target-lot-B',lot_id:'B'};
  data.balances=[otherSource,otherTarget,data.sourceBalance,data.targetBalance];
  const selected=resolve(data);
  assert.strictEqual(selected.sourceBalance,data.sourceBalance);
  assert.strictEqual(selected.targetBalance,data.targetBalance);
});

test('critic: stale removed source ID is rejected even when another balance occupies the same address',()=>{
  const data=fixture();
  data.balances=[{...data.sourceBalance,id:'replacement'},data.targetBalance];
  const select=helper('resolveSelection');
  assert.throws(()=>select({...data,lineID:data.line.id,sourceID:data.sourceBalance.id,targetID:data.targetBalance.id}));
});

test('critic: missing or duplicate selected IDs must never fall back to the first visible option',()=>{
  const data=fixture();const select=helper('resolveSelection');
  const args={...data,lineID:data.line.id,sourceID:data.sourceBalance.id,targetID:data.targetBalance.id};
  for(const field of ['lineID','sourceID','targetID']) assert.throws(()=>select({...args,[field]:''}),field);
  assert.throws(()=>select({...args,document:{...data.document,lines:[data.line,{...data.line}]}}),'duplicate line ID');
  assert.throws(()=>select({...args,balances:[...data.balances,{...data.sourceBalance}]}),'duplicate balance ID');
});

test('critic property: randomized multi-owner source choices equal an independent identity and availability oracle',()=>{
  const select=helper('balanceChoices');
  fc.assert(fc.property(fc.array(fc.record({owner:fc.boolean(),org:fc.boolean(),warehouse:fc.boolean(),item:fc.boolean(),state:fc.boolean(),quantity:fc.bigInt({min:0n,max:999999999999999999n})}),{minLength:1,maxLength:60}),entries=>{
    const data=fixture();
    const balances=entries.map((row,index)=>({...data.sourceBalance,id:`balance-${index}`,owner_id:row.owner?'owner':'other',organization_id:row.org?'org':'other',warehouse_id:row.warehouse?'warehouse':'other',item_id:row.item?'item':'other',stock_state:row.state?'available':'damaged',available_qty:String(row.quantity),on_hand:String(row.quantity),reserved_qty:'0'}));
    const want=balances.filter((_,index)=>{const row=entries[index];return row.owner&&row.org&&row.warehouse&&row.item&&row.state&&row.quantity>0n;});
    assert.deepEqual(select({...data,balances,role:'source'}),want);
  }),{seed:20261031,numRuns:180});
});

test('critic property: shuffled same-address lots preserve explicit selected source and target identity',()=>{
  const select=helper('resolveSelection');
  fc.assert(fc.property(fc.integer({min:0,max:9}),fc.shuffledSubarray(Array.from({length:10},(_,index)=>index),{minLength:10,maxLength:10}),(chosen,order)=>{
    const data=fixture();
    const sources=order.map(index=>({...data.sourceBalance,id:`source-${index}`,lot_id:`lot-${index}`}));
    const targets=[...order].reverse().map(index=>({...data.targetBalance,id:`target-${index}`,lot_id:`lot-${index}`}));
    const snapshot=JSON.stringify({sources,targets});
    const result=select({...data,balances:[...sources,...targets],lineID:'line',sourceID:`source-${chosen}`,targetID:`target-${chosen}`});
    assert.equal(result.sourceBalance.id,`source-${chosen}`);
    assert.equal(result.targetBalance.id,`target-${chosen}`);
    assert.equal(JSON.stringify({sources,targets}),snapshot,'Selection helpers must not mutate available rows');
  }),{seed:20261101,numRuns:180});
});

test('critic property: completed lines are excluded without coalescing repeated product lines',()=>{
  const select=helper('lineChoices');
  fc.assert(fc.property(fc.array(fc.record({requested:fc.integer({min:1,max:10000}),complete:fc.boolean()}),{minLength:1,maxLength:80}),entries=>{
    const data=fixture();
    const lines=entries.map((row,index)=>({...data.line,id:`line-${index}`,requested_qty:String(row.requested),fulfilled_qty:row.complete?String(row.requested):'0'}));
    assert.deepEqual(select({...data.document,lines},data.items,scope).map(row=>row.line.id),lines.filter((_,index)=>!entries[index].complete).map(line=>line.id));
  }),{seed:20261102,numRuns:180});
});
