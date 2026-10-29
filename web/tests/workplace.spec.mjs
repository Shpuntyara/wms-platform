import {test, expect} from '@playwright/test';

test('bureau can create and start a receipt using names, not UUIDs', async ({page}) => {
  await page.goto('/app/?demo=1');
  await expect(page.getByRole('heading', {name: 'Przyjęcia', exact: true})).toBeVisible();
  await page.getByRole('button', {name: 'Nowe przyjęcie', exact: true}).click();
  await page.getByLabel('Numer dokumentu').fill('PZ-TEST-101');
  await page.getByLabel('Ilość planowana', {exact: true}).fill('4');
  await page.getByRole('button', {name: 'Utwórz dokument', exact: true}).click();
  await expect(page.getByRole('heading', {name: 'PZ-TEST-101', exact: true})).toBeVisible();
  await page.getByRole('button', {name: 'Rozpocznij pracę', exact: true}).click();
  await expect(page.getByRole('button', {name: 'Otwórz na stanowisku magazynowym', exact: true})).toBeVisible();
});

test('terminal blocks wrong scan and receives one item with keyboard', async ({page}) => {
  await page.goto('/app/?demo=1');
  await page.getByRole('button', {name: 'PZ-2026-0142', exact: true}).click();
  await page.getByRole('button', {name: 'Otwórz na stanowisku magazynowym', exact: true}).click();
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
  await page.getByRole('button', {name: 'Otwórz na stanowisku magazynowym', exact: true}).click();
  await expect(page.getByLabel('Kod towaru', {exact: true})).toBeVisible();
  const size = await page.getByLabel('Kod towaru', {exact: true}).boundingBox();
  expect(size.height).toBeGreaterThanOrEqual(48);
  expect(await page.evaluate(() => document.documentElement.scrollWidth <= innerWidth)).toBe(true);
});
