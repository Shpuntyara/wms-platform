# Instructions

- Following Playwright test failed.
- Explain why, be concise, respect Playwright best practices.
- Provide a snippet of code with the fix, if possible.

# Test info

- Name: api-quantity-browser-critic.spec.mjs >> API-shaped WZ reservation remains usable on the explicit optional scanner path
- Location: tests\api-quantity-browser-critic.spec.mjs:22:1

# Error details

```
Error: expect(locator).toBeVisible() failed

Locator: getByLabel('Ilość rzeczywista', { exact: true })
Expected: visible
Timeout: 5000ms
Error: element(s) not found

Call log:
  - Expect "toBeVisible" getByLabel('Ilość rzeczywista', { exact: true }) with timeout 5000ms
  - waiting for getByLabel('Ilość rzeczywista', { exact: true })

```

```yaml
- link "Przejdź do obszaru roboczego":
  - /url: "#main"
- text: Magazyn szkoleniowy Dane przykładowe · tylko w tej przeglądarce
- link "Do instalacji roboczej":
  - /url: /app/
- banner:
  - link "MAGAZYN stanowisko pracy":
    - /url: /app/?demo=1
  - text: MAGAZYN Magazyn roboczy
  - combobox "Magazyn roboczy":
    - option "Magazyn Północny" [selected]
  - navigation "Stanowisko pracy":
    - button "Biuro"
    - button "Magazyn" [pressed]
  - text: S Zmiana szkoleniowa Przykład lokalny
- complementary:
  - text: OPERACJE
  - navigation "Operacje magazynowe":
    - button "Stany"
    - button "Zamówienia"
    - button "Wydania (WZ)"
    - button "Przyjęcia (PZ)"
    - button "Przesunięcia (MM)"
    - button "Historia"
  - text: Tryb szkoleniowy Magazyn Północny
- main:
  - alert: "Dopuszczalna liczba miejsc po przecinku: 0"
  - status
  - button "← Dokumenty"
  - text: WZ/API-ILOSCI Do realizacji
  - group "Sposób wprowadzania":
    - button "Wybierz z listy"
    - button "Skaner / wpisz kod" [pressed]
  - text: WYDANIE TOWARU (WZ)
  - heading "Zeskanuj adres źródłowy" [level=1]
  - paragraph:
    - text: "Odbiorca:"
    - strong: Firma Alfa
  - list:
    - listitem: ✓ Towar
    - listitem: 2 Źródło
    - listitem: 3 Ilość
  - text: POZYCJA 1 Z 1
  - heading "Śruba M8 × 40, ocynkowana" [level=2]
  - paragraph: BOLT-M8
  - text: Pozostało do wydania
  - strong: 3 szt.
  - text: "✓ Źródło: A1 Adres źródłowy"
  - textbox "Adres źródłowy"
  - paragraph: Skaner z klawiszem Enter lub ręczne wpisanie dokładnego kodu.
  - button "Dalej"
  - button "Skanuj ponownie"
  - text: "Wykonane pozycje: 0 / 1"
  - button "Do dokumentu w biurze"
```

# Test source

```ts
  1  | import {test,expect} from '@playwright/test';
  2  | 
  3  | async function apiShapedShipment(page) {
  4  |   await page.goto('/app/?demo=1');
  5  |   const seeded=await page.evaluate(async()=>{
  6  |     const {DemoWarehouse}=await import('/app/src/demo.js');
  7  |     const state=new DemoWarehouse(localStorage).state,doc=state.documents.find(row=>row.number==='PZ-2026-0142');
  8  |     doc.kind='shipment';doc.number='WZ/API-ILOSCI';doc.recipient='Firma Alfa';doc.lines=doc.lines.slice(0,1);doc.version=3;
  9  |     Object.assign(doc.lines[0],{requested_qty:'3.000000',fulfilled_qty:'0.000000',reserved_qty:'2.000000'});
  10 |     const source=state.balances.find(row=>row.item_id===doc.lines[0].item_id&&row.position_id===state.workspace.catalog.positions[1].id);
  11 |     Object.assign(source,{on_hand:'24.000000',available_qty:'22.000000',reserved_qty:'2.000000'});
  12 |     state.workspace.catalog.positions[1].code='A1';
  13 |     state.reservations=[{id:crypto.randomUUID(),organization_id:doc.organization_id,warehouse_id:doc.warehouse_id,owner_id:doc.owner_id,item_id:doc.lines[0].item_id,document_line_id:doc.lines[0].id,balance_id:source.id,quantity:'2.000000',active_qty:'2.000000'}];
  14 |     state.commands={};localStorage.setItem('wms-training-v2',JSON.stringify(state));
  15 |     sessionStorage.setItem(`wms-open:${state.workspace.actor_id}`,doc.id);sessionStorage.setItem(`wms-mode:${state.workspace.actor_id}`,'worker');
  16 |     return {document:doc.id,line:doc.lines[0].id,source:source.id,sku:state.workspace.catalog.items.find(item=>item.id===doc.lines[0].item_id).sku};
  17 |   });
  18 |   await page.reload();
  19 |   return seeded;
  20 | }
  21 | 
  22 | test('API-shaped WZ reservation remains usable on the explicit optional scanner path',async({page})=>{
  23 |   const ids=await apiShapedShipment(page);
  24 |   await page.getByRole('button',{name:'Skaner / wpisz kod',exact:true}).click();
  25 |   await page.getByLabel('Kod towaru',{exact:true}).fill(ids.sku);
  26 |   await page.getByLabel('Kod towaru',{exact:true}).press('Enter');
  27 |   await page.getByLabel('Adres źródłowy',{exact:true}).fill('A1');
  28 |   await page.getByLabel('Adres źródłowy',{exact:true}).press('Enter');
> 29 |   await expect(page.getByLabel('Ilość rzeczywista',{exact:true})).toBeVisible();
     |                                                                   ^ Error: expect(locator).toBeVisible() failed
  30 |   await expect(page.getByLabel('Ilość rzeczywista',{exact:true})).toHaveValue('2');
  31 |   await expect(page.getByText('Zarezerwowane pod tym adresem: 2 szt.',{exact:false})).toBeVisible();
  32 |   await expect(page.getByRole('alert')).toHaveCount(0);
  33 |   const state=await page.evaluate(()=>JSON.parse(localStorage.getItem('wms-training-v2')));
  34 |   expect(Object.values(state.commands)).toHaveLength(0);
  35 |   expect(state.documents.find(row=>row.id===ids.document).lines[0].fulfilled_qty).toBe('0.000000');
  36 | });
  37 | 
  38 | test('API-shaped piece quantities populate the default no-code source selector',async({page})=>{
  39 |   const ids=await apiShapedShipment(page);
  40 |   await page.getByLabel('Towar do realizacji',{exact:true}).selectOption(ids.line);
  41 |   await expect(page.getByLabel('Miejsce źródłowe',{exact:true}).locator(`option[value="${ids.source}"]`)).toHaveCount(1);
  42 |   await page.getByLabel('Miejsce źródłowe',{exact:true}).selectOption(ids.source);
  43 |   await expect(page.getByLabel('Ilość rzeczywista',{exact:true})).toHaveValue('2');
  44 |   expect(Object.values(await page.evaluate(()=>JSON.parse(localStorage.getItem('wms-training-v2')).commands))).toHaveLength(0);
  45 | });
  46 | 
```