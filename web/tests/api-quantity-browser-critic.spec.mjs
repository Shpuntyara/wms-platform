import {test,expect} from '@playwright/test';

async function apiShapedShipment(page) {
  await page.goto('/app/?demo=1');
  const seeded=await page.evaluate(async()=>{
    const {DemoWarehouse}=await import('/app/src/demo.js');
    const state=new DemoWarehouse(localStorage).state,doc=state.documents.find(row=>row.number==='PZ-2026-0142');
    doc.kind='shipment';doc.number='WZ/API-ILOSCI';doc.recipient='Firma Alfa';doc.lines=doc.lines.slice(0,1);doc.version=3;
    Object.assign(doc.lines[0],{requested_qty:'3.000000',fulfilled_qty:'0.000000',reserved_qty:'2.000000'});
    const source=state.balances.find(row=>row.item_id===doc.lines[0].item_id&&row.position_id===state.workspace.catalog.positions[1].id);
    Object.assign(source,{on_hand:'24.000000',available_qty:'22.000000',reserved_qty:'2.000000'});
    state.workspace.catalog.positions[1].code='A1';
    state.reservations=[{id:crypto.randomUUID(),organization_id:doc.organization_id,warehouse_id:doc.warehouse_id,owner_id:doc.owner_id,item_id:doc.lines[0].item_id,document_line_id:doc.lines[0].id,balance_id:source.id,quantity:'2.000000',active_qty:'2.000000'}];
    state.commands={};localStorage.setItem('wms-training-v2',JSON.stringify(state));
    sessionStorage.setItem(`wms-open:${state.workspace.actor_id}`,doc.id);sessionStorage.setItem(`wms-mode:${state.workspace.actor_id}`,'worker');
    return {document:doc.id,line:doc.lines[0].id,source:source.id,sku:state.workspace.catalog.items.find(item=>item.id===doc.lines[0].item_id).sku};
  });
  await page.reload();
  return seeded;
}

test('API-shaped WZ reservation remains usable on the explicit optional scanner path',async({page})=>{
  const ids=await apiShapedShipment(page);
  await page.getByRole('button',{name:'Skaner / wpisz kod',exact:true}).click();
  await page.getByLabel('Kod towaru',{exact:true}).fill(ids.sku);
  await page.getByLabel('Kod towaru',{exact:true}).press('Enter');
  await page.getByLabel('Adres źródłowy',{exact:true}).fill('A1');
  await page.getByLabel('Adres źródłowy',{exact:true}).press('Enter');
  await expect(page.getByLabel('Ilość rzeczywista',{exact:true})).toBeVisible();
  await expect(page.getByLabel('Ilość rzeczywista',{exact:true})).toHaveValue('2');
  await expect(page.getByText('Zarezerwowane pod tym adresem: 2 szt.',{exact:false})).toBeVisible();
  await expect(page.getByRole('alert')).toHaveCount(0);
  const state=await page.evaluate(()=>JSON.parse(localStorage.getItem('wms-training-v2')));
  expect(Object.values(state.commands)).toHaveLength(0);
  expect(state.documents.find(row=>row.id===ids.document).lines[0].fulfilled_qty).toBe('0.000000');
});

test('API-shaped piece quantities populate the default no-code source selector',async({page})=>{
  const ids=await apiShapedShipment(page);
  await page.getByLabel('Towar do realizacji',{exact:true}).selectOption(ids.line);
  await expect(page.getByLabel('Miejsce źródłowe',{exact:true}).locator(`option[value="${ids.source}"]`)).toHaveCount(1);
  await page.getByLabel('Miejsce źródłowe',{exact:true}).selectOption(ids.source);
  await expect(page.getByLabel('Ilość rzeczywista',{exact:true})).toHaveValue('2');
  expect(Object.values(await page.evaluate(()=>JSON.parse(localStorage.getItem('wms-training-v2')).commands))).toHaveLength(0);
});
