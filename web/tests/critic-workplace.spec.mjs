import {test, expect} from '@playwright/test';

test('critic: the Polish workplace declares its document language', async ({page}) => {
  await page.goto('/app/?demo=1');
  await expect(page.locator('html')).toHaveAttribute('lang', 'pl');
});

test('critic: bureau search keeps ordinary sequential typing in the right order', async ({page}) => {
  await page.goto('/app/?demo=1');
  const search = page.getByLabel('Szukaj w załadowanych dokumentach', {exact: true});
  await search.pressSequentially('PZ-2026-0142');
  await expect(search).toHaveValue('PZ-2026-0142');
  await expect(page.getByRole('button', {name: 'PZ-2026-0142', exact: true})).toBeVisible();
});

test('critic: editing the middle of a search preserves the caret after rerender', async ({page}) => {
  await page.goto('/app/?demo=1');
  const search = page.getByLabel('Szukaj w załadowanych dokumentach', {exact: true});
  await search.fill('PZ-2026');
  await search.press('Home');
  await search.press('ArrowRight');
  await search.pressSequentially('XY');
  await expect(search).toHaveValue('PXYZ-2026');
});

test('critic: an unmapped API error still gives the employee a Polish message', async ({page}) => {
  await page.route('**/app/config.json', route => route.fulfill({json: {
    oidc: {url: 'https://idp.invalid', realm: 'test', clientId: 'public-browser'},
  }}));
  await page.route('**/app/vendor/keycloak.js', route => route.fulfill({
    contentType: 'text/javascript', body: `export default class FakeIdentity {
      token = 'test-token'; tokenParsed = {preferred_username: 'Pracownik'};
      async init() { return true; } async updateToken() { return false; }
    }`,
  }));
  const englishDetail = 'A required dependency is unavailable.';
  await page.route('**/api/v1/workspace', route => route.fulfill({status: 503, json: {
    status: 503, code: 'upstream_unavailable', detail: englishDetail,
  }}));
  await page.goto('/app/');
  const alert = page.getByRole('alert');
  await expect(alert).toBeVisible();
  await expect(alert).not.toContainText(englishDetail);
  await expect(alert).toContainText(/[ąćęłńóśźż]|(Serwer|Operacja|Spróbuj)/);
});

test('critic: missing source SKU cannot silently become another item in putaway', async ({page}) => {
  await page.goto('/app/?demo=1');
  // A customer catalog can temporarily be incomplete. The receipt still
  // contains the original item; another catalog item must not replace it.
  await page.evaluate(async () => {
    const {DemoWarehouse} = await import('/app/src/demo.js');
    const state = new DemoWarehouse(localStorage).state;
    const source = state.documents.find(doc => doc.number === 'PZ-2026-0139');
    const omitted = source.lines[1].item_id;
    state.workspace.catalog.items = state.workspace.catalog.items.filter(item => item.id !== omitted);
    localStorage.setItem('wms-training-v2', JSON.stringify(state));
  });
  await page.reload();
  await page.getByRole('button', {name: 'PZ-2026-0139', exact: true}).click();
  await page.getByRole('button', {name: 'Utwórz rozmieszczenie', exact: true}).click();
  await expect(page.getByRole('dialog')).not.toBeVisible();
  await expect(page.getByRole('alert')).toHaveText('Brak kartoteki towaru z dokumentu. Poproś administratora o uzupełnienie danych.');
});
