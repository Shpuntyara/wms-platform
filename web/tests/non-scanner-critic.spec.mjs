import {test,expect} from '@playwright/test';

const ID = n => `77777777-7777-4777-8777-${String(n).padStart(12,'0')}`;
async function instrumentTrainingRequests(page,{loseMMReply=false}={}) {
  // Install before start() binds the adapter method. Delegate every operation
  // to the actual training adapter; only record writes or lose one reply.
  await page.route('**/app/src/demo.js',async route=>{
    const response=await route.fetch(),source=await response.text();
    await route.fulfill({response,body:source+`
      const criticOriginalRequest=DemoWarehouse.prototype.request;
      DemoWarehouse.prototype.request=async function(path,body,command) {
        const known=command&&this.state.commands[command];
        if(body&&localStorage.getItem('critic-instrument-writes')==='1') {
          const attempts=JSON.parse(localStorage.getItem('critic-write-attempts')||'[]');
          attempts.push({path,body,command});
          localStorage.setItem('critic-write-attempts',JSON.stringify(attempts));
        }
        const result=await criticOriginalRequest.call(this,path,body,command);
        if(${Boolean(loseMMReply)}&&!known&&path==='/api/v1/movements'&&body?.kind==='transfer') throw new TypeError('Synthetic response lost after committed MM');
        return result;
      };
    `});
  });
}
async function seedChoiceTask(page,{kind='transfer',duplicateLines=false,lots=false,ownReserved=false}={}) {
  await page.goto('/app/?demo=1');
  const seeded=await page.evaluate(async options=>{
    const {DemoWarehouse}=await import('/app/src/demo.js');
    const state=new DemoWarehouse(localStorage).state;
    const id=n=>`77777777-7777-4777-8777-${String(n).padStart(12,'0')}`;
    const scope={organization_id:state.workspace.scopes[0].organization_id,warehouse_id:state.workspace.scopes[0].warehouse_id};
    const owner=state.workspace.catalog.owners[0].id;
    const first={...state.workspace.catalog.items[0],name:'Notes czerwony A5',sku:'NOT-A5-RED'};
    const second={...state.workspace.catalog.items[1],name:'Notes czerwony A5',sku:'NOT-A5-RED-PREMIUM'};
    state.workspace.catalog.items=[first,second];
    state.workspace.catalog.positions=[{id:id(20),...scope,code:'PRZYJECIE-01',name:'Strefa przyjęć',kind:'receiving'},
      {id:id(21),...scope,code:'A1',name:'Regał A · poziom 1',kind:'storage'},
      {id:id(22),...scope,code:'B1',name:'Regał B · poziom 1',kind:'storage'},
      {id:id(23),...scope,code:'WYDANIE-01',name:'Strefa wydań',kind:'shipping'}];
    const lines=[{id:id(40),item_id:first.id,requested_qty:'4',fulfilled_qty:'0',reserved_qty:options.ownReserved?'2':'0'}];
    if(options.duplicateLines) lines.push({...lines[0],id:id(41),reserved_qty:'0'});
    const document={id:id(30),...scope,owner_id:owner,kind:options.kind,number:`${options.kind==='receipt'?'PZ':options.kind==='shipment'?'WZ':'MM'}/KRYTYK/001`,state:'active',version:2,recipient:'Firma odbierająca Sp. z o.o.',lines};
    state.documents=[document];state.orders=[];state.commands={};state.reservations=[];
    const base={...scope,owner_id:owner,item_id:first.id,stock_state:'available',lot_id:null,serial_id:null,version:1,reserved_qty:'0'};
    const sources=[{...base,id:id(100),position_id:id(21),on_hand:options.ownReserved?'2':'6',available_qty:options.ownReserved?'0':'6',reserved_qty:options.ownReserved?'2':'0'}];
    const targets=[{...base,id:id(101),position_id:id(22),on_hand:'0',available_qty:'0'},
      {...base,id:id(102),position_id:id(20),on_hand:'0',available_qty:'0'},
      {...base,id:id(103),position_id:id(23),on_hand:'0',available_qty:'0'}];
    if(options.lots) {
      sources[0].lot_id='PARTIA-A';targets[0].lot_id='PARTIA-A';
      sources.push({...sources[0],id:id(110),lot_id:'PARTIA-B',on_hand:'5',available_qty:'5'});
      targets.push({...targets[0],id:id(111),lot_id:'PARTIA-B'});
    }
    state.balances=[...sources,...targets];
    if(options.ownReserved) state.reservations=[{id:id(50),...scope,owner_id:owner,item_id:first.id,document_line_id:lines[0].id,balance_id:sources[0].id,quantity:'2',active_qty:'2'}];
    localStorage.setItem('wms-training-v2',JSON.stringify(state));
    return {documentID:document.id,number:document.number,lineID:lines[0].id,secondLineID:lines[1]?.id,sourceID:sources[0].id,targetID:targets[0].id,receivingID:targets[1].id};
  },{kind,duplicateLines,lots,ownReserved});
  await page.reload();
  await page.getByRole('button',{name:kind==='receipt'?'Przyjęcia (PZ)':kind==='shipment'?'Wydania (WZ)':'Przesunięcia (MM)',exact:true}).click();
  await page.getByRole('button',{name:seeded.number,exact:true}).click();
  await page.getByRole('button',{name:kind==='receipt'?'Rozpocznij przyjmowanie':kind==='shipment'?'Rozpocznij wydanie':'Rozpocznij przesunięcie',exact:true}).click();
  await expect(page.getByLabel('Towar do realizacji',{exact:true})).toHaveCount(1);
  return seeded;
}

async function chooseMove(page,{line=ID(40),source=ID(100),target=ID(101),quantity='1'}={}) {
  await page.getByLabel('Towar do realizacji',{exact:true}).selectOption(line);
  await page.getByLabel('Miejsce źródłowe',{exact:true}).selectOption(source);
  await page.getByLabel('Miejsce docelowe',{exact:true}).selectOption(target);
  await page.getByLabel('Ilość rzeczywista',{exact:true}).fill(quantity);
}

async function trainingState(page) {
  return page.evaluate(()=>JSON.parse(localStorage.getItem('wms-training-v2')));
}

test('critic non-scanner: repeated product lines remain independently selectable in PZ',async({page})=>{
  const task=await seedChoiceTask(page,{kind:'receipt',duplicateLines:true});
  const selector=page.getByLabel('Towar do realizacji',{exact:true});
  const labels=await selector.locator('option').allTextContents();
  expect(labels.filter(text=>text.includes('Notes czerwony A5'))).toHaveLength(2);
  expect(new Set(labels).size).toBe(labels.length);
  await selector.selectOption(task.secondLineID);
  await page.getByLabel('Miejsce przyjęcia',{exact:true}).selectOption(task.receivingID);
  await page.getByLabel('Ilość rzeczywista',{exact:true}).fill('1');
  await page.getByRole('button',{name:'Przyjmij towar',exact:true}).click();
  await expect(page.getByRole('status')).toContainText('Towar przyjęty');
  const state=await trainingState(page),document=state.documents[0];
  expect(document.lines[0].fulfilled_qty).toBe('0');
  expect(document.lines[1].fulfilled_qty).toBe('1');
  const writes=Object.values(state.commands).map(command=>JSON.parse(command.body));
  expect(writes).toHaveLength(1);
  expect(writes[0].document_line_id).toBe(task.secondLineID);
});

test('critic non-scanner: same-address lots preserve the explicitly selected MM balance identities',async({page})=>{
  await seedChoiceTask(page,{lots:true});
  await page.getByLabel('Towar do realizacji',{exact:true}).selectOption(ID(40));
  const source=page.getByLabel('Miejsce źródłowe',{exact:true});
  const labels=await source.locator('option').allTextContents();
  expect(labels.some(text=>text.includes('PARTIA-A'))).toBe(true);
  expect(labels.some(text=>text.includes('PARTIA-B'))).toBe(true);
  await source.selectOption(ID(110));
  const targets=await page.getByLabel('Miejsce docelowe',{exact:true}).locator('option').evaluateAll(options=>options.map(option=>option.value));
  expect(targets).toContain(ID(111));
  expect(targets).not.toContain(ID(101));
  await page.getByLabel('Miejsce docelowe',{exact:true}).selectOption(ID(111));
  await page.getByLabel('Ilość rzeczywista',{exact:true}).fill('2');
  await page.getByRole('button',{name:'Przesuń towar',exact:true}).click();
  await expect(page.getByRole('status')).toContainText('Towar przesunięty');
  const state=await trainingState(page),movement=JSON.parse(Object.values(state.commands)[0].body);
  expect(movement.source_balance_id).toBe(ID(110));expect(movement.target_balance_id).toBe(ID(111));
  expect(state.balances.find(row=>row.id===ID(100)).on_hand).toBe('6');
  expect(state.balances.find(row=>row.id===ID(110)).on_hand).toBe('3');
  expect(state.balances.find(row=>row.id===ID(111)).on_hand).toBe('2');
});

test('critic non-scanner: WZ resumes its own reservation even when no free quantity remains',async({page})=>{
  await seedChoiceTask(page,{kind:'shipment',ownReserved:true});
  await page.getByLabel('Towar do realizacji',{exact:true}).selectOption(ID(40));
  await page.getByLabel('Miejsce źródłowe',{exact:true}).selectOption(ID(100));
  await page.getByLabel('Ilość rzeczywista',{exact:true}).fill('2');
  await page.getByRole('button',{name:'Wydaj towar',exact:true}).click();
  await expect(page.getByRole('status')).toContainText('Towar wydany');
  const state=await trainingState(page),commands=Object.values(state.commands);
  expect(commands).toHaveLength(1);
  expect(commands[0].path).toBe('/api/v1/movements');
  expect(JSON.parse(commands[0].body).reservation_id).toBe(ID(50));
  expect(state.reservations[0].active_qty).toBe('0');
  expect(state.balances.find(row=>row.id===ID(100)).on_hand).toBe('0');
});

test('critic non-scanner: changing a document line clears source target and quantity',async({page})=>{
  const task=await seedChoiceTask(page,{duplicateLines:true});
  await chooseMove(page,{quantity:'2'});
  await page.getByLabel('Towar do realizacji',{exact:true}).selectOption(task.secondLineID);
  await expect(page.getByLabel('Miejsce źródłowe',{exact:true})).toHaveValue('');
  await expect(page.getByLabel('Miejsce docelowe',{exact:true})).toHaveValue('');
  await expect(page.getByLabel('Ilość rzeczywista',{exact:true})).toHaveValue('');
  expect(Object.values((await trainingState(page)).commands)).toHaveLength(0);
});

for(const fault of ['document-version','available-quantity']) test(`critic non-scanner: fresh ${fault} invalidates selection before a write attempt`,async({page})=>{
  await instrumentTrainingRequests(page);
  await seedChoiceTask(page);
  await chooseMove(page,{quantity:'2'});
  await page.evaluate(fault=>{
    localStorage.setItem('critic-instrument-writes','1');
    const state=JSON.parse(localStorage.getItem('wms-training-v2'));
    if(fault==='document-version') state.documents[0].version++;
    else {const source=state.balances.find(row=>row.id==='77777777-7777-4777-8777-000000000100');source.available_qty='0';source.reserved_qty=source.on_hand;}
    localStorage.setItem('wms-training-v2',JSON.stringify(state));
  },fault);
  await page.getByRole('button',{name:'Przesuń towar',exact:true}).click();
  await expect(page.getByRole('alert')).toContainText(/zmian|zmien|odśwież|dostępn|wyb[io]|właściw/i);
  expect(await page.evaluate(()=>JSON.parse(localStorage.getItem('critic-write-attempts')||'[]'))).toEqual([]);
  expect(Object.values((await trainingState(page)).commands)).toHaveLength(0);
});

test('critic non-scanner: lost MM reply locks selectors and replays the exact selected lot after reload',async({page})=>{
  await instrumentTrainingRequests(page,{loseMMReply:true});
  await seedChoiceTask(page,{lots:true});
  await chooseMove(page,{source:ID(110),target:ID(111),quantity:'2'});
  await page.getByRole('button',{name:'Przesuń towar',exact:true}).click();
  await expect(page.getByRole('button',{name:'Sprawdź wynik',exact:true})).toBeVisible();
  for(const label of ['Towar do realizacji','Miejsce źródłowe','Miejsce docelowe','Ilość rzeczywista']) await expect(page.getByLabel(label,{exact:true})).toBeDisabled();
  const pending=await page.evaluate(()=>Object.entries(localStorage).find(([key])=>key.startsWith('wms-command:'))?.[1]);
  const captured=JSON.parse(pending);
  expect(captured.body.source_balance_id).toBe(ID(110));expect(captured.body.target_balance_id).toBe(ID(111));
  await page.reload();
  await page.getByRole('button',{name:'Sprawdź wynik',exact:true}).click();
  await expect(page.getByRole('status')).toContainText('Operacja potwierdzona');
  const state=await trainingState(page),movement=state.commands[captured.id];
  expect(Object.values(state.commands)).toHaveLength(1);
  expect(JSON.parse(movement.body)).toEqual(captured.body);
  expect(state.documents[0].lines[0].fulfilled_qty).toBe('2');
  expect(state.balances.find(row=>row.id===ID(110)).on_hand).toBe('3');
});

test('critic non-scanner: write-attempt observer records a valid MM as a positive control',async({page})=>{
  await instrumentTrainingRequests(page);
  await seedChoiceTask(page);
  await chooseMove(page,{quantity:'1'});
  await page.evaluate(()=>localStorage.setItem('critic-instrument-writes','1'));
  await page.getByRole('button',{name:'Przesuń towar',exact:true}).click();
  await expect(page.getByRole('status')).toContainText('Towar przesunięty');
  const attempts=await page.evaluate(()=>JSON.parse(localStorage.getItem('critic-write-attempts')||'[]'));
  expect(attempts).toHaveLength(1);expect(attempts[0].path).toBe('/api/v1/movements');
  expect(attempts[0].body.source_balance_id).toBe(ID(100));expect(attempts[0].body.target_balance_id).toBe(ID(101));
});
