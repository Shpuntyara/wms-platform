import {test,expect} from '@playwright/test';

async function seedIndustry(page) {
  await page.goto('/app/?demo=1');
  await page.evaluate(async()=>{
    const {DemoWarehouse}=await import('/app/src/demo.js');
    const state=new DemoWarehouse(localStorage).state;
    state.workspace.catalog.warehouses[0].name='METALFORM · Magazyn materiałów';
    state.workspace.catalog.positions[1].code='A1';
    state.workspace.catalog.positions[1].name='Regał A, poziom 1';
    state.workspace.catalog.positions[2].code='B1';
    state.workspace.catalog.positions[2].name='Regał B, poziom 1';
    const base=state.workspace.catalog.items[0];
    const product={...base,id:'33333333-3333-4333-8333-333333333333',sku:'SZAFA-1800',name:'Szafa metalowa 1800 × 800 × 400 mm'};
    state.workspace.catalog.items.push(product);
    state.workspace.catalog.recipes=[{id:'44444444-4444-4444-8444-444444444444',organization_id:base.organization_id,warehouse_id:state.workspace.scopes[0].warehouse_id,product_item_id:product.id,name:'Szafa metalowa · standard',components:[{item_id:base.id,quantity:'16'}],steps:['Spawanie','Szlifowanie','Lakierowanie','Montaż']}];
    localStorage.setItem('wms-training-v2',JSON.stringify(state));
  });
  await page.reload();
}

test('stock opens first and remains visible for the warehouse employee with shelf and units',async({page})=>{
  await seedIndustry(page);
  await expect(page.getByRole('heading',{name:'Stany według adresów',exact:true})).toBeVisible();
  await page.getByRole('button',{name:'Magazyn',exact:true}).click();
  await expect(page.getByRole('heading',{name:'Stany według adresów',exact:true})).toBeVisible();
  const search=page.getByLabel('Szukaj towaru lub miejsca', {exact:true});
  await search.pressSequentially('A1');
  await expect(search).toHaveValue('A1');
  await expect(page.getByRole('row').filter({hasText:'BOLT-M8'})).toContainText('Regał A, poziom 1');
  await expect(page.getByRole('row').filter({hasText:'BOLT-M8'})).toContainText('szt.');
  await search.fill('nakretka');
  await expect(page.getByRole('row').filter({hasText:'BOLT-M8'})).toHaveCount(0);
  await expect(page.getByRole('row').filter({hasText:'NUT-M8'})).toHaveCount(1);
});

test('warehouse employee issues WZ to a named recipient from stock, reserving then posting exactly once',async({page})=>{
  await seedIndustry(page);
  await page.getByRole('button',{name:'Magazyn',exact:true}).click();
  const row=page.getByRole('row').filter({hasText:'BOLT-M8'}).filter({hasText:'A1'});
  await row.getByRole('button',{name:'Wydaj towar',exact:true}).click();
  await expect(page.getByRole('textbox',{name:'Numer dokumentu',exact:true})).toHaveCount(0);
  await page.getByLabel('Ilość planowana',{exact:true}).fill('2');
  await page.getByRole('button',{name:'Zapisz dokument',exact:true}).click();
  await expect(page.getByRole('alert')).toContainText('odbiorc');
  await page.getByLabel('Odbiorca',{exact:true}).fill('MONTER Sp. z o.o. · Jan Kowalski');
  await page.getByLabel('Numer zamówienia',{exact:true}).fill('Zamówienie #1');
  await page.getByRole('button',{name:'Zapisz dokument',exact:true}).click();
  await page.getByRole('button',{name:'Przekaż do realizacji',exact:true}).click();
  await page.getByLabel('Kod towaru',{exact:true}).fill('BOLT-M8');
  await page.getByLabel('Kod towaru',{exact:true}).press('Enter');
  await page.getByLabel('Adres źródłowy',{exact:true}).fill('A1');
  await page.getByLabel('Adres źródłowy',{exact:true}).press('Enter');
  await page.getByLabel('Ilość rzeczywista',{exact:true}).fill('2');
  await page.getByRole('button',{name:'Wydaj towar',exact:true}).click();
  await expect(page.getByRole('status')).toContainText('Towar wydany');
  await page.getByRole('button',{name:'Zakończ wydanie (WZ)',exact:true}).click();
  await expect(page.getByRole('heading',{name:'Dokument zrealizowany',exact:true})).toBeVisible();
  const state=await page.evaluate(()=>JSON.parse(localStorage.getItem('wms-training-v2')));
  const doc=state.documents.find(doc=>doc.kind==='shipment');
  expect(doc.number).toMatch(/^WZ\/\d{4}\/000001$/);
  expect(doc.recipient).toBe('MONTER Sp. z o.o. · Jan Kowalski');
  expect(doc.order_reference).toBe('Zamówienie #1');
  expect(doc.state).toBe('closed');
  expect(doc.lines[0].fulfilled_qty).toBe('2');
  expect(doc.lines[0].reserved_qty).toBe('0');
  const balance=state.balances.find(b=>b.item_id===doc.lines[0].item_id&&b.position_id===state.workspace.catalog.positions[1].id);
  expect(balance.on_hand).toBe('22');
  expect(Object.values(state.commands).filter(c=>JSON.parse(c.body).kind==='issue')).toHaveLength(1);
  await page.getByRole('button',{name:'Historia',exact:true}).click();
  await page.getByRole('button',{name:doc.number,exact:true}).click();
  await expect(page.getByRole('complementary',{name:'Wybrany dokument',exact:true})).toContainText(doc.recipient);
  await expect(page.getByRole('complementary',{name:'Wybrany dokument',exact:true})).toContainText('Zamówienie #1');
});

test('stock WZ rejects unavailable quantity before creating a document',async({page})=>{
  await seedIndustry(page);
  await page.getByRole('row').filter({hasText:'BOLT-M8'}).filter({hasText:'A1'}).getByRole('button',{name:'Wydaj towar',exact:true}).click();
  await page.getByLabel('Odbiorca',{exact:true}).fill('MONTER Sp. z o.o.');
  await page.getByLabel('Ilość planowana',{exact:true}).fill('25');
  await page.getByRole('button',{name:'Zapisz dokument',exact:true}).click();
  await expect(page.getByRole('alert')).toContainText('dostępn');
  expect(await page.evaluate(()=>Object.values(JSON.parse(localStorage.getItem('wms-training-v2')).commands))).toHaveLength(0);
});

test('bureau plans material demand and work stages without changing physical stock',async({page})=>{
  await seedIndustry(page);
  await page.getByRole('button',{name:'Zamówienia',exact:true}).click();
  await page.getByRole('button',{name:'Nowe zamówienie',exact:true}).click();
  await page.getByLabel('Firma lub odbiorca',{exact:true}).fill('BIUROMAX Sp. z o.o.');
  await page.getByLabel('Numer zamówienia',{exact:true}).fill('Zamówienie #1');
  await page.getByLabel('Sposób realizacji',{exact:true}).selectOption('44444444-4444-4444-8444-444444444444');
  await page.getByLabel('Ilość zamówiona',{exact:true}).fill('10');
  await expect(page.getByRole('dialog')).toContainText('160');
  await expect(page.getByRole('dialog')).toContainText('136');
  await expect(page.getByRole('dialog')).toContainText('Spawanie');
  await page.getByRole('button',{name:'Zapisz zamówienie',exact:true}).click();
  await expect(page.getByRole('heading',{name:'Zamówienie #1',exact:true})).toBeVisible();
  await expect(page.getByRole('main')).toContainText('BIUROMAX Sp. z o.o.');
  const state=await page.evaluate(()=>JSON.parse(localStorage.getItem('wms-training-v2')));
  expect(state.orders).toHaveLength(1);
  expect(state.orders[0].requirements).toEqual([{item_id:state.workspace.catalog.items[0].id,quantity:'160'}]);
  expect(state.balances.find(b=>b.position_id===state.workspace.catalog.positions[1].id&&b.item_id===state.workspace.catalog.items[0].id).on_hand).toBe('24');
  await page.getByRole('button',{name:'Przygotuj wydanie materiałów',exact:true}).click();
  await expect(page.getByLabel('Odbiorca',{exact:true})).toHaveValue('Produkcja');
  await expect(page.getByRole('dialog')).toContainText('BIUROMAX Sp. z o.o.');
  await expect(page.getByLabel('Numer zamówienia',{exact:true})).toHaveValue('Zamówienie #1');
  await expect(page.getByLabel('Ilość planowana',{exact:true})).toHaveValue('160');
});

for (const fault of ['reservation','issue']) test(`critic: lost ${fault} reply survives reload and does not post twice`,async({page})=>{
  await page.route('**/app/src/demo.js',async route=>{
    const response=await route.fetch();
    const source=await response.text();
    await route.fulfill({response,body:source+`
      const industryOriginalRequest = DemoWarehouse.prototype.request;
      DemoWarehouse.prototype.request = async function(path,body,command) {
        const known = command && this.state.commands[command];
        const result = await industryOriginalRequest.call(this,path,body,command);
        if (!known && body && ${fault === 'reservation'?"path === '/api/v1/reservations'":"path === '/api/v1/movements' && body.kind === 'issue'"}) {
          localStorage.setItem('industry-lost-response','${fault}');
          throw new TypeError('Synthetic committed response loss');
        }
        return result;
      };
    `});
  });
  await seedIndustry(page);
  await page.getByRole('button',{name:'Magazyn',exact:true}).click();
  await page.getByRole('row').filter({hasText:'BOLT-M8'}).filter({hasText:'A1'}).getByRole('button',{name:'Wydaj towar',exact:true}).click();
  await page.getByLabel('Odbiorca',{exact:true}).fill('MONTER Sp. z o.o.');
  await page.getByLabel('Ilość planowana',{exact:true}).fill('2');
  await page.getByRole('button',{name:'Zapisz dokument',exact:true}).click();
  await page.getByRole('button',{name:'Przekaż do realizacji',exact:true}).click();
  const scan=async()=>{
    await page.getByLabel('Kod towaru',{exact:true}).fill('BOLT-M8');
    await page.getByLabel('Kod towaru',{exact:true}).press('Enter');
    await page.getByLabel('Adres źródłowy',{exact:true}).fill('A1');
    await page.getByLabel('Adres źródłowy',{exact:true}).press('Enter');
    await page.getByLabel('Ilość rzeczywista',{exact:true}).fill('2');
    await page.getByRole('button',{name:'Wydaj towar',exact:true}).click();
  };
  await scan();
  await expect(page.getByRole('button',{name:'Sprawdź wynik',exact:true})).toBeVisible();
  const pending=await page.evaluate(()=>Object.entries(localStorage).find(([key])=>key.startsWith('wms-command:'))?.[1]);
  expect(JSON.parse(pending).path).toBe(fault === 'reservation'?'/api/v1/reservations':'/api/v1/movements');
  await page.reload();
  await page.getByRole('button',{name:'Sprawdź wynik',exact:true}).click();
  await expect(page.getByRole('status')).toContainText('Operacja potwierdzona');
  if (fault === 'reservation') {
    await expect(page.getByLabel('Kod towaru',{exact:true})).toBeVisible();
    await scan();
  }
  await expect(page.getByRole('button',{name:'Zakończ wydanie (WZ)',exact:true})).toBeVisible();
  const state=await page.evaluate(()=>JSON.parse(localStorage.getItem('wms-training-v2')));
  const shipment=state.documents.find(doc=>doc.kind==='shipment');
  expect(shipment.lines[0].fulfilled_qty).toBe('2');
  expect(shipment.lines[0].reserved_qty).toBe('0');
  expect(state.reservations).toHaveLength(1);
  expect(Object.values(state.commands).filter(c=>JSON.parse(c.body).kind==='issue')).toHaveLength(1);
});

test('critic: all stock pages participate in name search',async({page})=>{
  await page.route('**/app/src/demo.js',async route=>{
    const response=await route.fetch(),source=await response.text();
    await route.fulfill({response,body:source+`
      const industryStockRequest=DemoWarehouse.prototype.request;
      DemoWarehouse.prototype.request=async function(path,body,command) {
        const result=await industryStockRequest.call(this,path,body,command);
        if (!body && path.split('?')[0] === '/api/v1/stock-balances') {
          const offset=Number(new URLSearchParams(path.split('?')[1]).get('cursor')||0);
          return {data:result.data.slice(offset,offset+2),pagination:{next_cursor:offset+2<result.data.length?String(offset+2):null}};
        }
        return result;
      };
    `});
  });
  await seedIndustry(page);
  await page.getByLabel('Szukaj towaru lub miejsca',{exact:true}).fill('Przewód');
  await expect(page.getByRole('row').filter({hasText:'CABLE-25'})).toContainText('24');
  await expect(page.getByRole('row').filter({hasText:'CABLE-25'})).toHaveCount(1);
});

test('worker moves stock on an MM while PZ creation remains unavailable',async({page})=>{
  await seedIndustry(page);
  await page.evaluate(()=>{
    const state=JSON.parse(localStorage.getItem('wms-training-v2'));
    state.workspace.scopes[0].grants=state.workspace.scopes[0].grants.filter(g=>g.permission!=='document.create');
    localStorage.setItem('wms-training-v2',JSON.stringify(state));
  });
  await page.reload();
  await page.getByRole('button',{name:'Magazyn',exact:true}).click();
  await page.getByRole('row').filter({hasText:'BOLT-M8'}).filter({hasText:'A1'}).getByRole('button',{name:'Przenieś towar',exact:true}).click();
  await page.getByLabel('Ilość planowana',{exact:true}).fill('3');
  await page.getByRole('button',{name:'Zapisz dokument',exact:true}).click();
  await page.getByRole('button',{name:'Przekaż do realizacji',exact:true}).click();
  for(const [label,value] of [['Kod towaru','BOLT-M8'],['Adres źródłowy','A1'],['Adres docelowy','B1']]) {
    await page.getByLabel(label,{exact:true}).fill(value); await page.getByLabel(label,{exact:true}).press('Enter');
  }
  await page.getByLabel('Ilość rzeczywista',{exact:true}).fill('3');
  await page.getByRole('button',{name:'Przesuń towar',exact:true}).click();
  await page.getByRole('button',{name:'Zakończ przesunięcie (MM)',exact:true}).click();
  await expect(page.getByRole('heading',{name:'Dokument zrealizowany',exact:true})).toBeVisible();
  const state=await page.evaluate(()=>JSON.parse(localStorage.getItem('wms-training-v2')));
  const doc=state.documents.find(doc=>doc.kind==='transfer');
  expect(doc.state).toBe('closed'); expect(doc.number).toMatch(/^MM\/\d{4}\/000001$/);
  const balances=state.balances.filter(row=>row.item_id===doc.lines[0].item_id);
  expect(balances.find(row=>row.position_id===state.workspace.catalog.positions[1].id).on_hand).toBe('21');
  expect(balances.find(row=>row.position_id===state.workspace.catalog.positions[2].id).on_hand).toBe('3');
  await page.getByRole('button',{name:'Biuro',exact:true}).click();
  await page.getByRole('button',{name:'Przyjęcia (PZ)',exact:true}).click();
  await expect(page.getByRole('button',{name:'Nowe przyjęcie (PZ)',exact:true})).toHaveCount(0);
  await page.getByRole('button',{name:'PZ-2026-0139',exact:true}).click();
  await expect(page.getByRole('button',{name:'Przygotuj przesunięcie (MM)',exact:true})).toBeVisible();
});

test('critic: material and finished-goods WZ inherit the actual order owner',async({page})=>{
  await seedIndustry(page);
  const ownerB='55555555-5555-4555-8555-555555555555';
  await page.evaluate(owner=>{
    const state=JSON.parse(localStorage.getItem('wms-training-v2'));
    state.workspace.catalog.owners.push({...state.workspace.catalog.owners[0],id:owner,name:'Właściciel B'});
    localStorage.setItem('wms-training-v2',JSON.stringify(state));
  },ownerB);
  await page.reload();
  await page.getByRole('button',{name:'Zamówienia',exact:true}).click();
  await page.getByRole('button',{name:'Nowe zamówienie',exact:true}).click();
  await page.getByLabel('Firma lub odbiorca',{exact:true}).fill('Odbiorca B');
  await page.getByLabel('Numer zamówienia',{exact:true}).fill('ZAM-B');
  await page.getByLabel('Właściciel towaru',{exact:true}).selectOption(ownerB);
  await page.getByLabel('Sposób realizacji',{exact:true}).selectOption('44444444-4444-4444-8444-444444444444');
  await page.getByLabel('Ilość zamówiona',{exact:true}).fill('1');
  await page.getByRole('button',{name:'Zapisz zamówienie',exact:true}).click();
  await expect(page.getByRole('heading',{name:'ZAM-B',exact:true})).toBeVisible();
  for (const action of ['Przygotuj wydanie materiałów','Przygotuj wydanie towaru']) {
    await page.getByRole('button',{name:action,exact:true}).click();
    await expect(page.getByLabel('Właściciel towaru',{exact:true})).toHaveValue(ownerB);
    await page.getByRole('button',{name:'Zapisz dokument',exact:true}).click();
    await expect(page.getByRole('complementary',{name:'Wybrany dokument',exact:true})).toContainText('Właściciel B');
    await page.getByRole('button',{name:'Zamówienia',exact:true}).click();
  }
  const shipments=await page.evaluate(()=>JSON.parse(localStorage.getItem('wms-training-v2')).documents.filter(doc=>doc.kind==='shipment'));
  expect(shipments).toHaveLength(2);
  expect(shipments.every(doc=>doc.owner_id===ownerB)).toBe(true);
});

test('critic: recipient accepts 200 Polish characters and blocks 201 before sending',async({page})=>{
  await seedIndustry(page);
  const open=()=>page.getByRole('row').filter({hasText:'BOLT-M8'}).filter({hasText:'A1'}).getByRole('button',{name:'Wydaj towar',exact:true}).click();
  await open();
  await expect(page.getByLabel('Odbiorca',{exact:true})).toHaveAttribute('maxlength','200');
  await page.getByLabel('Odbiorca',{exact:true}).fill('Ż'.repeat(200));
  await page.getByLabel('Ilość planowana',{exact:true}).fill('1');
  await page.getByRole('button',{name:'Zapisz dokument',exact:true}).click();
  await expect(page.getByRole('complementary',{name:'Wybrany dokument',exact:true})).toContainText('Ż'.repeat(200));
  await page.getByRole('button',{name:'Stany',exact:true}).click();
  await open();
  await page.getByLabel('Odbiorca',{exact:true}).evaluate(input=>input.removeAttribute('maxlength'));
  await page.getByLabel('Odbiorca',{exact:true}).fill('Ż'.repeat(201));
  await page.getByLabel('Ilość planowana',{exact:true}).fill('1');
  await page.getByRole('button',{name:'Zapisz dokument',exact:true}).click();
  await expect(page.getByRole('alert')).toContainText('najwyżej 200 znaków');
  const shipments=await page.evaluate(()=>JSON.parse(localStorage.getItem('wms-training-v2')).documents.filter(doc=>doc.kind==='shipment'));
  expect(shipments).toHaveLength(1);
});

test('critic: customer accepts 200 Polish characters and blocks 201 before sending',async({page})=>{
  await seedIndustry(page);
  await page.getByRole('button',{name:'Zamówienia',exact:true}).click();
  const open=()=>page.getByRole('button',{name:'Nowe zamówienie',exact:true}).click();
  await open();
  await expect(page.getByLabel('Firma lub odbiorca',{exact:true})).toHaveAttribute('maxlength','200');
  await page.getByLabel('Firma lub odbiorca',{exact:true}).fill('Ł'.repeat(200));
  await page.getByLabel('Numer zamówienia',{exact:true}).fill('DŁUGA-1');
  await page.getByLabel('Sposób realizacji',{exact:true}).selectOption('44444444-4444-4444-8444-444444444444');
  await page.getByLabel('Ilość zamówiona',{exact:true}).fill('1');
  await page.getByRole('button',{name:'Zapisz zamówienie',exact:true}).click();
  await expect(page.getByRole('heading',{name:'DŁUGA-1',exact:true})).toBeVisible();
  await page.getByRole('button',{name:'Przygotuj wydanie materiałów',exact:true}).click();
  await expect(page.getByLabel('Odbiorca',{exact:true})).toHaveValue('Produkcja');
  await expect(page.getByRole('dialog')).toContainText('Ł'.repeat(200));
  await page.getByRole('button',{name:'Zamknij formularz',exact:true}).click();
  await page.getByRole('button',{name:'Przygotuj wydanie towaru',exact:true}).click();
  await expect(page.getByLabel('Odbiorca',{exact:true})).toHaveValue('Ł'.repeat(200));
  await page.getByRole('button',{name:'Zamknij formularz',exact:true}).click();
  await open();
  await page.getByLabel('Firma lub odbiorca',{exact:true}).evaluate(input=>input.removeAttribute('maxlength'));
  await page.getByLabel('Firma lub odbiorca',{exact:true}).fill('Ł'.repeat(201));
  await page.getByLabel('Numer zamówienia',{exact:true}).fill('DŁUGA-2');
  await page.getByLabel('Ilość zamówiona',{exact:true}).fill('1');
  await page.getByRole('button',{name:'Zapisz zamówienie',exact:true}).click();
  await expect(page.getByRole('alert')).toContainText('najwyżej 200 znaków');
  const orders=await page.evaluate(()=>JSON.parse(localStorage.getItem('wms-training-v2')).orders);
  expect(orders).toHaveLength(1);
});

test('critic: partial reservation is visible and issued before reserving the remaining plan',async({page})=>{
  await seedIndustry(page);
  await page.evaluate(async()=>{
    const {DemoWarehouse}=await import('/app/src/demo.js');
    const demo=new DemoWarehouse(localStorage),state=demo.state;
    const scope=state.workspace.scopes[0],owner=state.workspace.catalog.owners[0].id,item=state.workspace.catalog.items[0].id;
    const created=await demo.request('/api/v1/documents',{organization_id:scope.organization_id,warehouse_id:scope.warehouse_id,owner_id:owner,kind:'shipment',recipient:'MONTER',lines:[{item_id:item,quantity:'10'}]},crypto.randomUUID());
    const documentID=created.data.document_id;
    await demo.request(`/api/v1/documents/${documentID}/transitions`,{organization_id:scope.organization_id,warehouse_id:scope.warehouse_id,expected_document_version:1,state:'active'},crypto.randomUUID());
    const line=demo.state.documents.find(doc=>doc.id===documentID).lines[0];
    const balance=demo.state.balances.find(row=>row.item_id===item&&row.position_id===state.workspace.catalog.positions[1].id);
    await demo.request('/api/v1/reservations',{organization_id:scope.organization_id,warehouse_id:scope.warehouse_id,document_line_id:line.id,expected_document_version:2,balance_id:balance.id,quantity:'2'},crypto.randomUUID());
    sessionStorage.setItem(`wms-open:${state.workspace.actor_id}`,documentID);
    sessionStorage.setItem(`wms-mode:${state.workspace.actor_id}`,'worker');
  });
  await page.reload();
  const scanSource=async()=>{
    await page.getByLabel('Kod towaru',{exact:true}).fill('BOLT-M8');
    await page.getByLabel('Kod towaru',{exact:true}).press('Enter');
    await page.getByLabel('Adres źródłowy',{exact:true}).fill('A1');
    await page.getByLabel('Adres źródłowy',{exact:true}).press('Enter');
  };
  await scanSource();
  await expect(page.getByLabel('Ilość rzeczywista',{exact:true})).toHaveValue('2');
  await expect(page.getByLabel('Ilość rzeczywista',{exact:true})).toHaveAttribute('max','2');
  await expect(page.getByText('Zarezerwowane pod tym adresem: 2 szt.',{exact:true})).toBeVisible();
  await page.getByLabel('Ilość rzeczywista',{exact:true}).fill('3');
  await page.getByRole('button',{name:'Wydaj towar',exact:true}).click();
  await expect(page.getByRole('alert')).toContainText('Najpierw wydaj z aktywnej rezerwacji: 2 szt.');
  await page.getByLabel('Ilość rzeczywista',{exact:true}).fill('2');
  await page.getByRole('button',{name:'Wydaj towar',exact:true}).click();
  await expect(page.getByRole('status')).toContainText('Towar wydany');
  await scanSource();
  await page.getByLabel('Ilość rzeczywista',{exact:true}).fill('8');
  await page.getByRole('button',{name:'Wydaj towar',exact:true}).click();
  await expect(page.getByRole('button',{name:'Zakończ wydanie (WZ)',exact:true})).toBeVisible();
  const state=await page.evaluate(()=>JSON.parse(localStorage.getItem('wms-training-v2')));
  const doc=state.documents.find(doc=>doc.kind==='shipment');
  expect(doc.lines[0].fulfilled_qty).toBe('10'); expect(doc.lines[0].reserved_qty).toBe('0');
  expect(state.reservations.map(reservation=>reservation.quantity)).toEqual(['2','8']);
  expect(state.reservations.every(reservation=>reservation.active_qty==='0')).toBe(true);
});

for (const missing of ['item','owner']) test(`critic: a missing order ${missing} cannot fall back to the first catalog entry`,async({page})=>{
  await seedIndustry(page);
  await page.evaluate(async missing=>{
    const {DemoWarehouse}=await import('/app/src/demo.js');
    const state=new DemoWarehouse(localStorage).state,scope=state.workspace.scopes[0];
    const owner='55555555-5555-4555-8555-555555555555';
    state.workspace.catalog.owners.push({...state.workspace.catalog.owners[0],id:owner,name:'Właściciel B'});
    localStorage.setItem('wms-training-v2',JSON.stringify(state));
    const demo=new DemoWarehouse(localStorage),recipe=state.workspace.catalog.recipes[0];
    await demo.request('/api/v1/work-orders',{organization_id:scope.organization_id,warehouse_id:scope.warehouse_id,owner_id:owner,customer:'MONTER',reference:'PLAN-BRAK-KARTOTEKI',recipe_id:recipe.id,lines:[{item_id:recipe.product_item_id,quantity:'1'}]},crypto.randomUUID());
    const changed=demo.state;
    if (missing==='item') changed.workspace.catalog.items=changed.workspace.catalog.items.filter(item=>item.id!==recipe.components[0].item_id);
    else changed.workspace.catalog.owners=changed.workspace.catalog.owners.filter(entry=>entry.id!==owner);
    localStorage.setItem('wms-training-v2',JSON.stringify(changed));
  },missing);
  await page.reload();
  await page.getByRole('button',{name:'Zamówienia',exact:true}).click();
  await page.getByRole('button',{name:/PLAN-BRAK-KARTOTEKI/}).click();
  await page.getByRole('button',{name:'Przygotuj wydanie materiałów',exact:true}).click();
  await expect(page.getByRole('dialog')).not.toBeVisible();
  await expect(page.getByRole('alert')).toContainText(missing==='item'?'Brak kartoteki towaru z zamówienia':'Brak kartoteki właściciela z zamówienia');
  const shipments=await page.evaluate(()=>JSON.parse(localStorage.getItem('wms-training-v2')).documents.filter(doc=>doc.kind==='shipment'));
  expect(shipments).toHaveLength(0);
});

async function seedPaperWithLegacyStock(page) {
  await seedIndustry(page);
  const ids=await page.evaluate(()=>{
    const state=JSON.parse(localStorage.getItem('wms-training-v2'));
    const bolt=state.workspace.catalog.items.find(item=>item.sku==='BOLT-M8');
    const cable=state.workspace.catalog.items.find(item=>item.sku==='CABLE-25');
    const notes={...bolt,id:'66666666-6666-4666-8666-666666666666',sku:'NOTES-2027-CZERWONY-A5',name:'Notes 2027 Czerwony · A5',archived:false};
    bolt.archived=true; cable.archived=true;
    state.workspace.catalog.items=[bolt,cable,notes];
    state.workspace.catalog.warehouses[0].name='PAPIERNIK · Notesy i kalendarze';
    state.balances=state.balances.filter(balance=>balance.item_id===bolt.id||balance.item_id===cable.id);
    const source=state.balances.find(balance=>balance.item_id===bolt.id&&balance.on_hand==='24');
    state.balances.push({...source,id:'77777777-7777-4777-8777-777777777777',item_id:notes.id,on_hand:'52',available_qty:'52',reserved_qty:'0'});
    const historical=state.documents.find(doc=>doc.number==='PZ-2026-0139');
    historical.lines=historical.lines.filter(line=>line.item_id===bolt.id);
    localStorage.setItem('wms-training-v2',JSON.stringify(state));
    return {bolt:bolt.id,notes:notes.id};
  });
  await page.reload();
  return ids;
}

test('paper workplace keeps earlier metal trials explicitly archived without changing stock',async({page})=>{
  await seedPaperWithLegacyStock(page);
  const before=await page.evaluate(()=>localStorage.getItem('wms-training-v2'));
  await expect(page.getByRole('row').filter({hasText:'Notes 2027 Czerwony'})).toContainText('52');
  await expect(page.getByRole('row').filter({hasText:'BOLT-M8'})).toHaveCount(0);
  await expect(page.getByRole('row').filter({hasText:'CABLE-25'})).toHaveCount(0);
  await expect(page.getByRole('region',{name:'Wcześniejsze dane testowe',exact:true})).toContainText('Ukryte pozycje: 2');
  await page.getByLabel('Pokaż wcześniejsze dane testowe',{exact:true}).check();
  for (const sku of ['BOLT-M8','CABLE-25']) {
    const row=page.getByRole('row').filter({hasText:sku});
    await expect(row).toContainText('24');
    await expect(row).toContainText('Wcześniejsze próby');
  }
  await page.getByLabel('Pokaż wcześniejsze dane testowe',{exact:true}).uncheck();
  await expect(page.getByRole('row').filter({hasText:'BOLT-M8'})).toHaveCount(0);
  expect(await page.evaluate(()=>localStorage.getItem('wms-training-v2'))).toBe(before);
});

test('new paper documents use working items while explicit historical placement preserves the archived SKU',async({page})=>{
  const ids=await seedPaperWithLegacyStock(page);
  const before=await page.evaluate(()=>localStorage.getItem('wms-training-v2'));
  await page.getByRole('button',{name:'Przyjęcia (PZ)',exact:true}).click();
  await page.getByRole('button',{name:'Nowe przyjęcie (PZ)',exact:true}).click();
  await expect(page.getByLabel('Towar',{exact:true}).locator('option')).toHaveCount(1);
  await expect(page.getByLabel('Towar',{exact:true})).toHaveValue(ids.notes);
  await page.getByRole('button',{name:'Zamknij formularz',exact:true}).click();
  await page.getByRole('button',{name:'Zamówienia',exact:true}).click();
  await page.getByRole('button',{name:'Nowe zamówienie',exact:true}).click();
  await expect(page.getByLabel('Zamówiony towar',{exact:true}).locator('option')).toHaveCount(1);
  await expect(page.getByLabel('Zamówiony towar',{exact:true})).toHaveValue(ids.notes);
  await page.getByRole('button',{name:'Zamknij formularz',exact:true}).click();
  await page.getByRole('button',{name:'Stany',exact:true}).click();
  await page.getByRole('row').filter({hasText:'Notes 2027 Czerwony'}).getByRole('button',{name:'Wydaj towar',exact:true}).click();
  await expect(page.getByLabel('Towar',{exact:true}).locator('option')).toHaveCount(1);
  await expect(page.getByLabel('Towar',{exact:true})).toHaveValue(ids.notes);
  await page.getByRole('button',{name:'Zamknij formularz',exact:true}).click();
  await page.getByRole('button',{name:'Historia',exact:true}).click();
  await page.getByRole('button',{name:'PZ-2026-0139',exact:true}).click();
  await expect(page.getByRole('complementary',{name:'Wybrany dokument',exact:true})).toContainText('Wcześniejsze próby');
  await page.getByRole('button',{name:'Przygotuj przesunięcie (MM)',exact:true}).click();
  await expect(page.getByLabel('Towar',{exact:true})).toHaveValue(ids.bolt);
  await expect(page.getByLabel('Towar',{exact:true}).locator('option:checked')).toContainText('Wcześniejsze próby');
  await page.getByRole('button',{name:'Zamknij formularz',exact:true}).click();
  expect(await page.evaluate(()=>localStorage.getItem('wms-training-v2'))).toBe(before);
});
