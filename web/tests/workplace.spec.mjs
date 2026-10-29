import {test, expect} from '@playwright/test';

test('bureau saves a PZ without choosing a number, then explicitly hands it to the warehouse', async ({page}) => {
  await page.goto('/app/?demo=1');
  await page.getByRole('button', {name: /^Nowe przyjęcie/}).click();
  await expect(page.getByRole('textbox', {name: 'Numer dokumentu', exact: true})).toHaveCount(0);
  await expect(page.getByText('Numer zostanie nadany automatycznie po zapisaniu', {exact:true})).toBeVisible();
  await expect(page.getByRole('dialog', {name:'Nowe przyjęcie (PZ)', exact:true})).toBeVisible();
  await page.getByLabel('Ilość planowana', {exact: true}).fill('4');
  await page.getByRole('button', {name: 'Zapisz dokument', exact: true}).click();
  const number = page.getByRole('complementary', {name:'Wybrany dokument'}).getByRole('heading', {level:2});
  await expect(number).toHaveText(/^PZ\/\d{4}\/000001$/);
  await expect(page.getByRole('heading', {name:'Przyjęcie towaru (PZ)', exact:true})).toBeVisible();
  await expect(page.getByRole('complementary', {name:'Wybrany dokument'})).toContainText('Do przygotowania');
  await expect(page.getByRole('button', {name: 'Rozpocznij przyjmowanie', exact: true})).toHaveCount(0);
  const saved = await page.evaluate(() => {
    const state = JSON.parse(localStorage.getItem('wms-training-v2'));
    const command = Object.values(state.commands).find(entry => entry.path === '/api/v1/documents');
    return {body:JSON.parse(command.body), document:state.documents.find(doc => doc.id === command.result.data.document_id)};
  });
  expect(saved.body).not.toHaveProperty('number');
  expect(saved.document.number).toBe(await number.textContent());
  expect(saved.document.state).toBe('draft');
  expect(saved.document.lines[0].fulfilled_qty).toBe('0');
  await page.getByRole('button', {name: 'Przekaż do realizacji', exact: true}).click();
  await expect(page.getByRole('button', {name: 'Rozpocznij przyjmowanie', exact: true})).toBeVisible();
  await expect(page.getByRole('complementary', {name:'Wybrany dokument'})).toContainText('Do realizacji');
});

test('a completed PZ prepares an automatically numbered MM without changing the source plan', async ({page}) => {
  await page.goto('/app/?demo=1');
  await page.getByRole('button', {name:'PZ-2026-0139', exact:true}).click();
  await page.getByRole('button', {name:'Przygotuj przesunięcie (MM)', exact:true}).click();
  await expect(page.getByRole('dialog', {name:'Nowe przesunięcie (MM)', exact:true})).toBeVisible();
  await expect(page.getByRole('textbox', {name:'Numer dokumentu', exact:true})).toHaveCount(0);
  await expect(page.getByText('Numer zostanie nadany automatycznie po zapisaniu', {exact:true})).toBeVisible();
  await expect(page.getByLabel('Ilość planowana', {exact:true}).nth(0)).toHaveValue('48');
  await expect(page.getByLabel('Ilość planowana', {exact:true}).nth(1)).toHaveValue('96');
  await page.getByRole('button', {name:'Zapisz dokument', exact:true}).click();
  await expect(page.getByRole('complementary', {name:'Wybrany dokument'}).getByRole('heading', {level:2})).toHaveText(/^MM\/\d{4}\/000001$/);
  const saved = await page.evaluate(() => {
    const state = JSON.parse(localStorage.getItem('wms-training-v2'));
    const command = Object.values(state.commands).find(entry => entry.path === '/api/v1/documents');
    return {body:JSON.parse(command.body), source:state.documents.find(doc => doc.number === 'PZ-2026-0139'), target:state.documents.find(doc => doc.id === command.result.data.document_id)};
  });
  expect(saved.body).not.toHaveProperty('number');
  expect(saved.target.kind).toBe('transfer');
  expect(saved.target.state).toBe('draft');
  expect(saved.target.lines.map(line => line.requested_qty)).toEqual(saved.source.lines.map(line => line.fulfilled_qty));
  expect(saved.source.state).toBe('closed');
});

test('terminal blocks wrong scan and receives one item with keyboard', async ({page}) => {
  await page.goto('/app/?demo=1');
  await page.getByRole('button', {name: 'PZ-2026-0142', exact: true}).click();
  await page.getByRole('button', {name: 'Rozpocznij przyjmowanie', exact: true}).click();
  await page.getByLabel('Kod towaru', {exact: true}).fill('WRONG');
  await page.getByLabel('Kod towaru', {exact: true}).press('Enter');
  await expect(page.getByRole('alert')).toContainText('Nie znaleziono kodu');
  await page.getByLabel('Kod towaru', {exact: true}).fill('BOLT-M8');
  await page.getByLabel('Kod towaru', {exact: true}).press('Enter');
  await page.getByLabel('Adres przyjęcia', {exact: true}).fill('PRZYJECIE-01');
  await page.getByLabel('Adres przyjęcia', {exact: true}).press('Enter');
  await page.getByLabel('Ilość rzeczywista', {exact: true}).fill('1');
  await page.getByRole('button', {name: 'Przyjmij towar', exact: true}).click();
  await expect(page.getByRole('status')).toContainText('Towar przyjęty');
});

test('worker screen reflows at TSD width and scanning controls remain touch-sized', async ({page}) => {
  await page.setViewportSize({width: 360, height: 740});
  await page.goto('/app/?demo=1');
  await page.getByRole('button', {name: 'PZ-2026-0142', exact: true}).click();
  await page.getByRole('button', {name: 'Rozpocznij przyjmowanie', exact: true}).click();
  await expect(page.getByLabel('Kod towaru', {exact: true})).toBeVisible();
  const size = await page.getByLabel('Kod towaru', {exact: true}).boundingBox();
  expect(size.height).toBeGreaterThanOrEqual(48);
  expect(await page.evaluate(() => document.documentElement.scrollWidth <= innerWidth)).toBe(true);
});
