import {test,expect} from '@playwright/test';

// Synthetic training data exercises the real UI and journal. PostgreSQL is
// covered separately by live-workflow.spec.mjs and the installation probe.
async function prepare(page,kind='receipt',twoLines=false) {
  await page.goto('/app/?demo=1');
  return page.evaluate(async({kind,twoLines})=>{
    const {DemoWarehouse}=await import('/app/src/demo.js');
    const state=new DemoWarehouse(localStorage).state;
    state.workspace.catalog.positions[1].code='A1';
    state.workspace.catalog.positions[1].name='Montaż · regał A, poziom 1';
    state.workspace.catalog.positions[2].code='B1';
    state.workspace.catalog.positions[2].name='Pakownia · regał B, poziom 1';
    const doc=state.documents.find(row=>row.number==='PZ-2026-0142');
    doc.kind=kind;
    if(kind!=='receipt') {
      doc.number=kind==='transfer'?'MM-WYBOR':'WZ-WYBOR';
      doc.lines=doc.lines.slice(0,twoLines?2:1);
      doc.lines.forEach(line=>line.requested_qty='3');
    }
    if(kind==='shipment') {doc.recipient='Firma Żółć Sp. z o.o.';doc.order_reference='ZAM/KLIENT/7';}
    localStorage.setItem('wms-training-v2',JSON.stringify(state));
    sessionStorage.setItem(`wms-open:${state.workspace.actor_id}`,doc.id);
    sessionStorage.setItem(`wms-mode:${state.workspace.actor_id}`,'worker');
    return {doc:doc.id,lines:doc.lines.map(row=>row.id),item:doc.lines[0].item_id,
      receiving:state.balances.find(row=>row.item_id===doc.lines[0].item_id&&row.position_id===state.workspace.catalog.positions[0].id).id,
      secondReceiving:state.balances.find(row=>row.item_id===doc.lines[1]?.item_id&&row.position_id===state.workspace.catalog.positions[0].id)?.id,
      source:state.balances.find(row=>row.item_id===doc.lines[0].item_id&&row.position_id===state.workspace.catalog.positions[1].id).id,
      target:state.balances.find(row=>row.item_id===doc.lines[0].item_id&&row.position_id===state.workspace.catalog.positions[2].id).id};
  },{kind,twoLines});
}
const snapshot=page=>page.evaluate(()=>JSON.parse(localStorage.getItem('wms-training-v2')));
async function selectMove(page,ids,quantity='3') {
  await page.getByLabel('Towar do realizacji',{exact:true}).selectOption(ids.lines[0]);
  await page.getByLabel('Miejsce źródłowe',{exact:true}).selectOption(ids.source);
  await page.getByLabel('Miejsce docelowe',{exact:true}).selectOption(ids.target);
  await page.getByLabel('Ilość rzeczywista',{exact:true}).fill(quantity);
}

test('PZ receives the chosen second line using names and a receiving list without exact codes',async({page})=>{
  const ids=await prepare(page);await page.reload();
  await expect(page.getByLabel('Towar do realizacji',{exact:true})).toBeVisible();
  await expect(page.getByLabel('Kod towaru',{exact:true})).toHaveCount(0);
  await page.getByLabel('Towar do realizacji',{exact:true}).selectOption(ids.lines[1]);
  await expect(page.getByLabel('Miejsce przyjęcia',{exact:true}).locator('option')).not.toContainText(['A1','B1']);
  await page.getByLabel('Miejsce przyjęcia',{exact:true}).selectOption(ids.secondReceiving);
  await page.getByLabel('Ilość rzeczywista',{exact:true}).fill('2');
  await page.getByLabel('Ilość rzeczywista',{exact:true}).press('Enter');
  await expect(page.getByRole('status')).toContainText('Towar przyjęty');
  const state=await snapshot(page),doc=state.documents.find(row=>row.id===ids.doc);
  expect(doc.lines.map(row=>row.fulfilled_qty)).toEqual(['0','2']);
  expect(state.balances.find(row=>row.id===ids.secondReceiving).on_hand).toBe('2');
});

test('native lists retain keyboard focus through selection and submit with Tab and Enter',async({page})=>{
  const ids=await prepare(page);await page.reload();
  const line=page.getByLabel('Towar do realizacji',{exact:true});
  await line.focus();await page.keyboard.press('Home');await page.keyboard.press('ArrowDown');
  await expect(line).toHaveValue(ids.lines[0]);await expect(line).toBeFocused();
  await page.keyboard.press('Tab');
  const target=page.getByLabel('Miejsce przyjęcia',{exact:true});
  await expect(target).toBeFocused();
  await page.keyboard.press('Home');await page.keyboard.press('ArrowDown');
  await expect(target).toHaveValue(ids.receiving);await expect(target).toBeFocused();
  await page.keyboard.press('Tab');
  await expect(page.getByLabel('Ilość rzeczywista',{exact:true})).toBeFocused();
  await page.keyboard.type('1');await page.keyboard.press('Enter');
  await expect(page.getByRole('status')).toContainText('Towar przyjęty');
  expect((await snapshot(page)).documents.find(row=>row.id===ids.doc).lines[0].fulfilled_qty).toBe('1');
});

test('no-code WZ freezes choices after a committed lost issue reply and replays the exact command after reload',async({page})=>{
  await page.route('**/app/src/demo.js',async route=>{
    const response=await route.fetch(),source=await response.text();
    await route.fulfill({response,body:source+`
      const noCodeOriginal=DemoWarehouse.prototype.request;
      DemoWarehouse.prototype.request=async function(path,body,key) {
        if(body?.kind==='issue') {const attempts=JSON.parse(localStorage.getItem('no-code-attempts')||'[]');attempts.push({path,body,key});localStorage.setItem('no-code-attempts',JSON.stringify(attempts));}
        const result=await noCodeOriginal.call(this,path,body,key);
        if(body?.kind==='issue'&&!localStorage.getItem('no-code-dropped')) {localStorage.setItem('no-code-dropped','1');throw new TypeError('Synthetic committed response loss');}
        return result;
      };
    `});
  });
  const ids=await prepare(page,'shipment');await page.reload();
  await page.getByLabel('Towar do realizacji',{exact:true}).selectOption(ids.lines[0]);
  await page.getByLabel('Miejsce źródłowe',{exact:true}).selectOption(ids.source);
  await page.getByLabel('Ilość rzeczywista',{exact:true}).fill('1');
  await page.getByRole('button',{name:'Wydaj towar',exact:true}).click();
  await expect(page.getByRole('button',{name:'Sprawdź wynik',exact:true})).toBeVisible();
  for(const label of ['Towar do realizacji','Miejsce źródłowe','Ilość rzeczywista']) await expect(page.getByLabel(label,{exact:true})).toBeDisabled();
  await expect(page.getByRole('button',{name:'Skaner / wpisz kod',exact:true})).toBeDisabled();
  await page.reload();
  await expect(page.getByLabel('Towar do realizacji',{exact:true})).toBeDisabled();
  await page.getByRole('button',{name:'Sprawdź wynik',exact:true}).click();
  await expect(page.getByRole('status')).toContainText('Operacja potwierdzona');
  const attempts=await page.evaluate(()=>JSON.parse(localStorage.getItem('no-code-attempts')));
  expect(attempts).toHaveLength(2);expect(attempts[1]).toEqual(attempts[0]);
  const state=await snapshot(page);
  expect(state.documents.find(row=>row.id===ids.doc).lines[0]).toMatchObject({fulfilled_qty:'1',reserved_qty:'0'});
  expect(state.balances.find(row=>row.id===ids.source).on_hand).toBe('23');
  expect(state.reservations).toHaveLength(1);
  expect(Object.values(state.commands).filter(row=>JSON.parse(row.body).kind==='issue')).toHaveLength(1);
});

test('MM moves an existing item between named workplaces without creating a SKU or label',async({page})=>{
  const ids=await prepare(page,'transfer');await page.reload();
  await expect(page.getByLabel('Towar do realizacji',{exact:true})).toBeVisible();
  await expect(page.getByText('Kod towaru i partia pozostają bez zmian. Przesunięcie nie wymaga nowej etykiety.',{exact:true})).toBeVisible();
  await selectMove(page,ids);
  await page.getByRole('button',{name:'Przesuń towar',exact:true}).click();
  await page.getByRole('button',{name:'Zakończ przesunięcie (MM)',exact:true}).click();
  await expect(page.getByRole('heading',{name:'Dokument zrealizowany',exact:true})).toBeVisible();
  const state=await snapshot(page);
  expect(state.documents.find(row=>row.id===ids.doc).state).toBe('closed');
  expect(state.balances.find(row=>row.id===ids.source).on_hand).toBe('21');
  expect(state.balances.find(row=>row.id===ids.target).on_hand).toBe('3');
  expect(state.workspace.catalog.items).toHaveLength(4);
  expect(state.workspace.catalog.items.find(row=>row.id===ids.item).sku).toBe('BOLT-M8');
});

test('MM source lists accept PostgreSQL fixed-six numeric quantities for whole pieces',async({page})=>{
  const ids=await prepare(page,'transfer');
  await page.evaluate(()=>{
    const state=JSON.parse(localStorage.getItem('wms-training-v2'));
    for(const row of state.balances) for(const key of ['on_hand','reserved_qty','available_qty']) row[key]+='.000000';
    for(const doc of state.documents) for(const row of doc.lines) for(const key of ['requested_qty','fulfilled_qty','reserved_qty']) row[key]+='.000000';
    localStorage.setItem('wms-training-v2',JSON.stringify(state));
  });
  await page.reload();
  await page.getByLabel('Towar do realizacji',{exact:true}).selectOption(ids.lines[0]);
  await expect(page.getByLabel('Miejsce źródłowe',{exact:true}).locator(`option[value="${ids.source}"]`)).toHaveCount(1);
  await page.getByLabel('Miejsce źródłowe',{exact:true}).selectOption(ids.source);
  await page.getByLabel('Miejsce docelowe',{exact:true}).selectOption(ids.target);
  await page.getByLabel('Ilość rzeczywista',{exact:true}).fill('3');
  await page.getByRole('button',{name:'Przesuń towar',exact:true}).click();
  await expect(page.getByRole('status')).toContainText('Towar przesunięty');
});

test('WZ issues to its saved recipient from a labeled source without entering any codes',async({page})=>{
  const ids=await prepare(page,'shipment');await page.reload();
  await expect(page.getByLabel('Towar do realizacji',{exact:true})).toBeVisible();
  await expect(page.getByText('Firma Żółć Sp. z o.o.',{exact:false})).toBeVisible();
  await page.getByLabel('Towar do realizacji',{exact:true}).selectOption(ids.lines[0]);
  await page.getByLabel('Miejsce źródłowe',{exact:true}).selectOption(ids.source);
  await page.getByLabel('Ilość rzeczywista',{exact:true}).fill('3');
  await page.getByRole('button',{name:'Wydaj towar',exact:true}).click();
  await page.getByRole('button',{name:'Zakończ wydanie (WZ)',exact:true}).click();
  await expect(page.getByRole('heading',{name:'Dokument zrealizowany',exact:true})).toBeVisible();
  const state=await snapshot(page),doc=state.documents.find(row=>row.id===ids.doc);
  expect(doc).toMatchObject({state:'closed',recipient:'Firma Żółć Sp. z o.o.',order_reference:'ZAM/KLIENT/7'});
  expect(doc.lines[0]).toMatchObject({fulfilled_qty:'3',reserved_qty:'0'});
  expect(state.balances.find(row=>row.id===ids.source)).toMatchObject({on_hand:'21',reserved_qty:'0'});
  expect(Object.values(state.commands).filter(row=>JSON.parse(row.body).kind==='issue')).toHaveLength(1);
});

test('changing a line or a source clears dependent choices and counted quantity',async({page})=>{
  const ids=await prepare(page,'transfer',true);await page.reload();
  await expect(page.getByLabel('Towar do realizacji',{exact:true})).toBeVisible();
  await selectMove(page,ids,'1');
  await page.getByLabel('Towar do realizacji',{exact:true}).selectOption(ids.lines[1]);
  await expect(page.getByLabel('Miejsce źródłowe',{exact:true})).toHaveValue('');
  await expect(page.getByLabel('Miejsce docelowe',{exact:true})).toHaveValue('');
  await expect(page.getByLabel('Ilość rzeczywista',{exact:true})).toHaveValue('');
  await page.getByLabel('Towar do realizacji',{exact:true}).selectOption(ids.lines[0]);
  await selectMove(page,ids,'1');
  await page.getByLabel('Miejsce źródłowe',{exact:true}).selectOption('');
  await expect(page.getByLabel('Miejsce docelowe',{exact:true})).toHaveValue('');
  await expect(page.getByLabel('Ilość rzeczywista',{exact:true})).toHaveValue('');
  expect(Object.values((await snapshot(page)).commands)).toHaveLength(0);
});

test('confirmation rechecks current availability before any MM write',async({page})=>{
  const ids=await prepare(page,'transfer');await page.reload();
  await expect(page.getByLabel('Towar do realizacji',{exact:true})).toBeVisible();
  await selectMove(page,ids,'3');
  await page.evaluate(id=>{const s=JSON.parse(localStorage.getItem('wms-training-v2')),b=s.balances.find(row=>row.id===id);b.on_hand='1';b.available_qty='1';b.version++;localStorage.setItem('wms-training-v2',JSON.stringify(s));},ids.source);
  await page.getByRole('button',{name:'Przesuń towar',exact:true}).click();
  await expect(page.getByRole('alert')).toContainText(/dostępn|odśwież|zmieni/i);
  expect(Object.values((await snapshot(page)).commands)).toHaveLength(0);
});

test('a confirmed PZ with a failed refresh freezes choices and offers a read-only refresh without replaying the movement',async({page})=>{
  await page.route('**/app/src/demo.js',async route=>{
    const response=await route.fetch(),source=await response.text();
    await route.fulfill({response,body:source+`
      const confirmedOriginal=DemoWarehouse.prototype.request;
      DemoWarehouse.prototype.request=async function(path,body,key) {
        if(!body&&path.startsWith('/api/v1/documents?')&&localStorage.getItem('confirmed-read-fault')==='armed') {
          localStorage.setItem('confirmed-read-fault','spent');throw new TypeError('Synthetic read failure');
        }
        const result=await confirmedOriginal.call(this,path,body,key);
        if(body?.kind==='receipt') localStorage.setItem('confirmed-read-fault','armed');
        return result;
      };
    `});
  });
  const ids=await prepare(page);await page.reload();
  await page.getByLabel('Towar do realizacji',{exact:true}).selectOption(ids.lines[0]);
  await page.getByLabel('Miejsce przyjęcia',{exact:true}).selectOption(ids.receiving);
  await page.getByLabel('Ilość rzeczywista',{exact:true}).fill('1');
  await page.getByRole('button',{name:'Przyjmij towar',exact:true}).click();
  await expect(page.getByRole('alert')).toContainText('Operacja potwierdzona');
  await expect(page.getByLabel('Towar do realizacji',{exact:true})).toBeDisabled();
  await expect(page.getByRole('button',{name:'Sprawdź wynik',exact:true})).toHaveCount(0);
  await page.getByRole('button',{name:'Odśwież',exact:true}).click();
  await expect(page.getByLabel('Towar do realizacji',{exact:true})).toBeEnabled();
  expect(Object.values((await snapshot(page)).commands).filter(row=>JSON.parse(row.body).kind==='receipt')).toHaveLength(1);
  expect((await snapshot(page)).documents.find(row=>row.id===ids.doc).lines[0].fulfilled_qty).toBe('1');
});
