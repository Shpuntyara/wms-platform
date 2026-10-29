import {test, expect} from '@playwright/test';
import {writeFile} from 'node:fs/promises';

// The identity provider is the only replaced dependency. UI, HTTP handlers and
// all document/stock operations use the real Go/PostgreSQL fixture.
test('bureau receipt and scanner putaway survive a lost committed response', async ({page}) => {
  const actor = process.env.WMS_UI_LIVE_ACTOR || 'placeholder-actor';
  await page.route('**/app/vendor/keycloak.js', route => route.fulfill({
    contentType: 'text/javascript',
    body: `export default class TestIdentityProvider {
      authenticated = true;
      token = 'valid';
      tokenParsed = {sub: ${JSON.stringify(actor)}, preferred_username: 'Operator testowy'};
      async init() { return true; }
      async updateToken() { return false; }
      async login() { return true; }
      async logout() { this.authenticated = false; }
    }`,
  }));

  const documentNumber = process.env.WMS_UI_LIVE_NUMBER || 'PZ-TEST-TDD';
  await page.goto('/app/');
  const login = page.getByRole('button', {name: 'Zaloguj się do WMS', exact: true});
  const createReceipt = page.getByRole('button', {name: 'Nowe przyjęcie', exact: true});
  await expect(login.or(createReceipt)).toBeVisible();
  if (await login.isVisible()) await login.click();
  await expect(createReceipt).toBeVisible();
  await page.setViewportSize({width:1024,height:900});
  await expect(page.getByRole('button',{name:'Wyloguj',exact:true})).toBeVisible();
  await page.setViewportSize({width:1440,height:1000});
  await createReceipt.click();
  await page.getByLabel('Numer dokumentu', {exact: true}).fill(documentNumber);
  await page.getByLabel('Właściciel towaru', {exact: true}).selectOption(process.env.WMS_UI_LIVE_OWNER);
  await page.getByLabel('Towar', {exact: true}).selectOption(process.env.WMS_UI_LIVE_ITEM);
  await page.getByLabel('Ilość planowana', {exact: true}).fill('4');
  await page.getByRole('button', {name: 'Utwórz dokument', exact: true}).click();
  await page.getByRole('button', {name: 'Rozpocznij pracę', exact: true}).click();
  await expect(page.getByRole('button', {name: 'Otwórz na stanowisku magazynowym', exact: true})).toBeEnabled();
  await page.screenshot({path: test.info().outputPath('bureau.png'), fullPage: true});
  await page.getByRole('button', {name: 'Otwórz na stanowisku magazynowym', exact: true}).click();
  await page.getByLabel('Kod towaru', {exact: true}).fill('WRONG-SKU');
  await page.getByLabel('Kod towaru', {exact: true}).press('Enter');
  await expect(page.getByLabel('Kod towaru', {exact: true})).toBeVisible();
  await expect(page.getByRole('alert')).toBeVisible();
  await page.getByLabel('Kod towaru', {exact: true}).fill('BOLT-M8');
  await page.getByLabel('Kod towaru', {exact: true}).press('Enter');
  // A valid storage address is still the wrong zone for this receiving step.
  await page.getByLabel('Adres przyjęcia', {exact: true}).fill('A-01-02');
  await page.getByLabel('Adres przyjęcia', {exact: true}).press('Enter');
  await expect(page.getByLabel('Adres przyjęcia', {exact: true})).toBeVisible();
  await expect(page.getByLabel('Ilość rzeczywista', {exact: true})).toHaveCount(0);
  await page.getByLabel('Adres przyjęcia', {exact: true}).fill('PRZYJECIE-01');
  await page.getByLabel('Adres przyjęcia', {exact: true}).press('Enter');
  await page.getByLabel('Ilość rzeczywista', {exact: true}).fill('4');
  await page.screenshot({path: test.info().outputPath('worker.png'), fullPage: true});
  await page.setViewportSize({width: 390, height: 844});
  await page.screenshot({path: test.info().outputPath('worker-mobile.png'), fullPage: true});
  await page.setViewportSize({width: 1440, height: 1000});

  const receiptRequests = [];
  let droppedResponse = false;
  await page.route('**/api/v1/movements', async route => {
    const request = route.request();
    if (request.method() !== 'POST') return route.continue();
    const body = request.postDataJSON();
    if (body.kind !== 'receipt') return route.continue();
    receiptRequests.push({path: new URL(request.url()).pathname,
      key: request.headers()['idempotency-key'], body});
    if (!droppedResponse) {
      const committed = await route.fetch();
      expect(committed.status()).toBe(201);
      droppedResponse = true;
      // Deliberately lose the response only AFTER the kernel has committed.
      return route.abort('failed');
    }
    return route.continue();
  });

  await page.getByRole('button', {name: 'Przyjmij towar', exact: true}).click();
  await expect(page.getByRole('button', {name: 'Sprawdź wynik', exact: true})).toBeVisible();
  // Recovery must survive a real page reload, not only component state.
  await page.reload();
  await page.getByRole('button', {name: 'Sprawdź wynik', exact: true}).click();
  await page.getByRole('button', {name: 'Zakończ przyjęcie', exact: true}).click();
  expect(receiptRequests).toHaveLength(2);
  expect(receiptRequests[1]).toEqual(receiptRequests[0]);
  expect(receiptRequests[0].key).toMatch(/^[0-9a-f-]{36}$/i);
  expect(receiptRequests[0].body.quantity).toBe('4');
  expect(receiptRequests[0].body.expected_document_version).toBe(2);
  await writeFile(test.info().outputPath('api-replay.json'), JSON.stringify({receiptRequests}, null, 2));

  await page.getByRole('button', {name: 'Utwórz rozmieszczenie', exact: true}).click();
  await page.getByLabel('Numer dokumentu', {exact: true}).fill(documentNumber + '-RZ');
  await page.getByRole('button', {name: 'Utwórz dokument', exact: true}).click();
  let activationCommitted = false;
  let readFaultInjected = false;
  await page.route('**/api/v1/documents/*/transitions', async route => {
    if (activationCommitted || route.request().postDataJSON().state !== 'active') return route.continue();
    const response = await route.fetch();
    expect(response.status()).toBe(201);
    activationCommitted = true;
    await route.fulfill({response});
  });
  await page.route('**/api/v1/documents?**', route => {
    if (activationCommitted && !readFaultInjected) {
      readFaultInjected = true;
      return route.abort('failed');
    }
    return route.continue();
  });
  await page.getByRole('button', {name: 'Rozpocznij pracę', exact: true}).click();
  await expect.poll(() => readFaultInjected).toBe(true);
  // A delivered success followed by a failed read is a confirmed operation.
  await expect(page.getByRole('alert')).toBeVisible();
  await expect(page.getByRole('alert')).not.toContainText(/Sprawdź wynik/i);
  await expect(page.getByRole('button', {name: 'Sprawdź wynik', exact: true})).toHaveCount(0);
  await page.getByRole('button', {name: 'Odśwież', exact: true}).click();
  await page.getByRole('button', {name: 'Otwórz na stanowisku magazynowym', exact: true}).click();
  await page.getByLabel('Kod towaru', {exact: true}).fill('BOLT-M8');
  await page.getByLabel('Kod towaru', {exact: true}).press('Enter');
  await page.getByLabel('Adres źródłowy', {exact: true}).fill('PRZYJECIE-01');
  let releaseLookup;
  const delayedLookup = new Promise(resolve => { releaseLookup = resolve; });
  let sourceLookups = 0;
  let activeLookups = 0;
  const balancePattern = '**/api/v1/stock-balances?**';
  await page.route(balancePattern, async route => {
    sourceLookups++;
    activeLookups++;
    try {
      const response = await route.fetch();
      await delayedLookup;
      await route.fulfill({response});
    } finally { activeLookups--; }
  });
  await page.getByLabel('Adres źródłowy', {exact: true}).press('Enter');
  await expect.poll(() => sourceLookups).toBeGreaterThan(0);
  // A scanner can send Enter twice while the lookup is still in flight.
  await page.keyboard.press('Enter');
  releaseLookup();
  await expect.poll(() => activeLookups).toBe(0);
  await expect(page.getByLabel('Adres docelowy', {exact: true})).toBeVisible();
  await expect(page.getByLabel('Ilość rzeczywista', {exact: true})).toHaveCount(0);
  await page.getByLabel('Adres docelowy', {exact: true}).fill('PRZYJECIE-01');
  await page.getByLabel('Adres docelowy', {exact: true}).press('Enter');
  await expect(page.getByLabel('Adres docelowy', {exact: true})).toBeVisible();
  await expect(page.getByLabel('Ilość rzeczywista', {exact: true})).toHaveCount(0);
  await page.getByLabel('Adres docelowy', {exact: true}).fill('A-01-02');
  await page.getByLabel('Adres docelowy', {exact: true}).press('Enter');
  await page.getByLabel('Ilość rzeczywista', {exact: true}).fill('4');
  await page.getByRole('button', {name: 'Odłóż towar', exact: true}).click();
  await page.getByRole('button', {name: 'Zakończ rozmieszczenie', exact: true}).click();
  await expect(page.getByRole('heading', {name: 'Dokument zakończony', exact: true})).toBeVisible();
});
