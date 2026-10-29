import {test, expect, firefox} from '@playwright/test';
import {readFile, writeFile, mkdir, mkdtemp, copyFile, lstat} from 'node:fs/promises';
import {randomUUID} from 'node:crypto';
import path from 'node:path';

const input = JSON.parse(await readFile(process.env.WMS_INSTALLATION_BROWSER_INPUT, 'utf8'));
if (!Array.isArray(input.installations) || input.installations.length !== 2) {
  throw new Error('Exactly two explicitly identified WMS installations are required');
}
if (!/^[A-Za-z0-9-]{1,40}$/.test(input.run_id) || input.installations.some(site => !/^[A-Za-z0-9-]{1,30}$/.test(site.name))) {
  throw new Error('Installation verification requires safe bounded run and site identifiers');
}
if (!path.isAbsolute(process.env.WMS_INSTALLATION_PROFILE_ROOT) || !path.isAbsolute(process.env.WMS_INSTALLATION_NSS_DIR)) {
  throw new Error('Scoped browser profiles and trust databases require explicit absolute directories');
}
const protectedPasswords = input.installations.flatMap(site => Object.values(site.users).map(user => user.password));
if (new Set(protectedPasswords).size !== 4 || protectedPasswords.some(value => typeof value !== 'string' || value.length < 16)) {
  throw new Error('Installation employees require four independent protected passwords');
}

function assertOrigin(site) {
  const origin = new URL(site.origin);
  if (origin.protocol !== 'https:' || origin.username || origin.password || origin.search || origin.hash || origin.pathname !== '/') {
    throw new Error('Installation origin must be an exact credential-free HTTPS origin');
  }
  return origin.origin;
}

async function login(site, role) {
  const profileRoot = process.env.WMS_INSTALLATION_PROFILE_ROOT;
  await mkdir(profileRoot,{recursive:true});
  const profile = await mkdtemp(path.join(profileRoot, `${site.name}-${role}-`));
  for (const name of ['cert9.db','key4.db']) {
    const source = path.join(process.env.WMS_INSTALLATION_NSS_DIR,name);
    const info = await lstat(source);
    if (!info.isFile() || info.isSymbolicLink()) throw new Error('Scoped browser trust requires regular NSS database files');
    await copyFile(source,path.join(profile,name));
  }
  const context = await firefox.launchPersistentContext(profile,{
    headless:true, ignoreHTTPSErrors:false, locale:'pl-PL', viewport:{width:1440,height:1000},
    firefoxUserPrefs:{
      'security.enterprise_roots.enabled':false,
      'security.certerrors.mitm.auto_enable_enterprise_roots':false,
      'signon.rememberSignons':false,
      'browser.cache.disk.enable':false,
    },
  });
  const page = await context.newPage();
  const signal = {authorizationCode:false, pkceS256:false, exactRedirect:false, tokenReceived:false};
  let accessToken;
  page.on('request', request => {
    const url = new URL(request.url());
    if (url.origin === site.origin && url.pathname.endsWith('/protocol/openid-connect/auth')) {
      signal.authorizationCode ||= url.searchParams.get('response_type') === 'code';
      signal.pkceS256 ||= url.searchParams.get('code_challenge_method') === 'S256';
      signal.exactRedirect ||= url.searchParams.get('redirect_uri') === site.origin + '/app/';
    }
  });
  page.on('response', async response => {
    const url = new URL(response.url());
    if (url.origin !== site.origin || !url.pathname.endsWith('/protocol/openid-connect/token') || response.status() !== 200) return;
    try {
      const body = await response.json();
      if (typeof body.access_token === 'string') {accessToken = body.access_token; signal.tokenReceived = true;}
    } catch {/* Login assertions below give a static, secret-free diagnostic. */}
  });
  try {
    await page.goto(site.origin + '/app/');
    await expect(page.locator('html')).toHaveAttribute('lang', /^pl(?:-PL)?$/i);
    await page.getByLabel(/Nazwa użytkownika|Adres e-mail|Login/i).fill(site.users[role].username);
    await page.getByLabel('Hasło', {exact:true}).fill(site.users[role].password);
    await page.getByRole('button', {name:'Zaloguj się', exact:true}).click();
    await expect(page.getByRole('heading', {name:'Przyjęcia', exact:true})).toBeVisible();
    await expect.poll(() => signal.tokenReceived).toBe(true);
  } catch {
    await context.close();
    throw new Error(`Real Polish Keycloak login failed for ${site.name}/${role}; credential details suppressed`);
  }
  expect(signal).toEqual({authorizationCode:true, pkceS256:true, exactRedirect:true, tokenReceived:true});
  expect(new URL(page.url()).pathname).toBe('/app/');
  await expect(page.getByText('Magazyn szkoleniowy', {exact:true})).toHaveCount(0);
  await expect(page.locator('body')).not.toContainText(/[\p{Script=Cyrillic}]/u);
  try {
    await page.setViewportSize({width:1024,height:900});
    await expect(page.getByRole('button',{name:'Wyloguj',exact:true})).toBeVisible();
    await page.setViewportSize({width:1440,height:1000});
  } catch {
    await context.close();
    throw new Error(`Logout is inaccessible at shared desktop width for ${site.name}/${role}`);
  }
  return {context, page, signal, token:() => accessToken};
}

async function api(session, origin, path, method = 'GET', data) {
  const headers = {Authorization:'Bearer '+session.token()};
  if (data) {headers['Content-Type']='application/json'; headers['Idempotency-Key']=randomUUID();}
  try {
    const response = await session.context.request.fetch(origin+path,{method,headers,data:data ? JSON.stringify(data) : undefined});
    const status = response.status();
    let body = {};
    try {body = await response.json();} catch {/* Status is still checked. */}
    return {status,body};
  } catch {throw new Error('Authenticated installation API probe failed; credential details suppressed');}
}

async function document(session, site, id, scope) {
  const result = await api(session,site.origin,`/api/v1/documents/${id}?${new URLSearchParams(scope)}`);
  expect(result.status).toBe(200);
  return result.body.data;
}

async function openDocument(page, number) {
  await page.getByRole('button', {name:number,exact:true}).click();
  await expect(page.getByRole('heading', {name:number,exact:true})).toBeVisible();
}

async function scan(page,label,value) {
  await page.getByLabel(label,{exact:true}).fill(value);
  await page.getByLabel(label,{exact:true}).press('Enter');
}

function units(value) {
  const [whole, fraction = ''] = value.split('.');
  return BigInt(whole) * 1000000n + BigInt(fraction.padEnd(6,'0'));
}

for (const rawSite of input.installations) {
  const site = {...rawSite,origin:assertOrigin(rawSite)};
  test(`${site.name}: real Polish employee login, receipt and putaway`, async () => {
    const sessions = [];
    const receiptNumber = `PZ-ODBIOR-${input.run_id}-${site.name}`;
    const putawayNumber = `MM-ODBIOR-${input.run_id}-${site.name}`;
    const quantity = site.catalog.quantity || '4';
    const expectedScope = {organization_id:site.catalog.organization_id,warehouse_id:site.catalog.warehouse_id};
    let receiptID, putawayID;
    let completed = false;
    try {
      const bureau = await login(site,'biuro'); sessions.push(bureau);
      const workspace = await api(bureau,site.origin,'/api/v1/workspace');
      expect(workspace.status).toBe(200);
      expect(workspace.body.data.scopes.some(scope => scope.organization_id === expectedScope.organization_id && scope.warehouse_id === expectedScope.warehouse_id)).toBe(true);
      expect(workspace.body.data.catalog.items.some(item => item.id === site.catalog.item_id && item.sku === site.catalog.sku)).toBe(true);
      const stockPath = `/api/v1/stock-balances?${new URLSearchParams({...expectedScope,item_id:site.catalog.item_id,owner_id:site.catalog.owner_id,limit:'200'})}`;
      const beforeStock = await api(bureau,site.origin,stockPath);
      expect(beforeStock.status).toBe(200);
      const beforeReceiving = beforeStock.body.data.find(balance => balance.id === site.catalog.receiving_balance_id);
      const beforeStorage = beforeStock.body.data.find(balance => balance.id === site.catalog.storage_balance_id);
      expect(Boolean(beforeReceiving && beforeStorage)).toBe(true);
      await bureau.page.getByRole('button',{name:'Nowe przyjęcie',exact:true}).click();
      await bureau.page.getByLabel('Numer dokumentu',{exact:true}).fill(receiptNumber);
      await bureau.page.getByLabel('Właściciel towaru',{exact:true}).selectOption(site.catalog.owner_id);
      await bureau.page.getByLabel('Towar',{exact:true}).selectOption(site.catalog.item_id);
      await bureau.page.getByLabel('Ilość planowana',{exact:true}).fill(quantity);
      const created = bureau.page.waitForResponse(response => new URL(response.url()).pathname === '/api/v1/documents' && response.request().method() === 'POST');
      await bureau.page.getByRole('button',{name:'Utwórz dokument',exact:true}).click();
      const createdResponse = await created;
      expect(createdResponse.status()).toBe(201);
      receiptID = (await createdResponse.json()).data.document_id;
      await bureau.page.getByRole('button',{name:'Rozpocznij pracę',exact:true}).click();
      await expect(bureau.page.getByRole('button',{name:'Rozpocznij pracę',exact:true})).toHaveCount(0);
      await expect(bureau.page.getByRole('button',{name:'Otwórz na stanowisku magazynowym',exact:true})).toHaveCount(0);
      await bureau.page.screenshot({path:test.info().outputPath('biuro-real-login.png'),fullPage:true});

      const worker = await login(site,'magazynier'); sessions.push(worker);
      const workerWorkspace = await api(worker,site.origin,'/api/v1/workspace');
      expect(workerWorkspace.status).toBe(200);
      expect(workerWorkspace.body.data.actor_id === workspace.body.data.actor_id).toBe(false);
      await expect(worker.page.getByRole('button',{name:'Nowe przyjęcie',exact:true})).toHaveCount(0);
      const deniedCreate = await api(worker,site.origin,'/api/v1/documents','POST',{...expectedScope,owner_id:site.catalog.owner_id,kind:'receipt',number:receiptNumber+'-DENIED',lines:[{item_id:site.catalog.item_id,quantity}]});
      expect(deniedCreate.status).toBe(403);
      const receipt = await document(worker,site,receiptID,expectedScope);
      const deniedMovement = await api(bureau,site.origin,'/api/v1/movements','POST',{...expectedScope,document_line_id:receipt.lines[0].id,expected_document_version:receipt.version,kind:'receipt',target_balance_id:site.catalog.receiving_balance_id,quantity});
      expect(deniedMovement.status).toBe(403);
      const foreign = input.installations.find(other => other.origin !== site.origin);
      expect((await api(worker,foreign.origin,'/api/v1/workspace')).status).toBe(401);
      const anonymous = await worker.context.request.get(site.origin+'/api/v1/workspace');
      expect(anonymous.status()).toBe(401);

      await openDocument(worker.page,receiptNumber);
      await worker.page.getByRole('button',{name:'Otwórz na stanowisku magazynowym',exact:true}).click();
      await scan(worker.page,'Kod towaru',site.catalog.sku);
      await scan(worker.page,'Adres przyjęcia',site.catalog.receiving_code);
      await worker.page.getByLabel('Ilość rzeczywista',{exact:true}).fill(quantity);
      await worker.page.screenshot({path:test.info().outputPath('magazynier-real-login.png'),fullPage:true});
      await worker.page.getByRole('button',{name:'Przyjmij towar',exact:true}).click();
      await worker.page.getByRole('button',{name:'Zakończ przyjęcie',exact:true}).click();
      await expect(worker.page.getByRole('heading',{name:'Dokument zakończony',exact:true})).toBeVisible();
      await expect(worker.page.getByRole('button',{name:'Utwórz rozmieszczenie',exact:true})).toHaveCount(0);

      await bureau.page.getByRole('button',{name:'Odśwież',exact:true}).click();
      await bureau.page.getByRole('button',{name:'Utwórz rozmieszczenie',exact:true}).click();
      await bureau.page.getByLabel('Numer dokumentu',{exact:true}).fill(putawayNumber);
      const putawayCreated = bureau.page.waitForResponse(response => new URL(response.url()).pathname === '/api/v1/documents' && response.request().method() === 'POST');
      await bureau.page.getByRole('button',{name:'Utwórz dokument',exact:true}).click();
      const placementResponse = await putawayCreated;
      expect(placementResponse.status()).toBe(201);
      putawayID = (await placementResponse.json()).data.document_id;
      await bureau.page.getByRole('button',{name:'Rozpocznij pracę',exact:true}).click();
      await expect(bureau.page.getByRole('button',{name:'Rozpocznij pracę',exact:true})).toHaveCount(0);

      await worker.page.reload();
      await openDocument(worker.page,putawayNumber);
      await worker.page.getByRole('button',{name:'Otwórz na stanowisku magazynowym',exact:true}).click();
      await scan(worker.page,'Kod towaru',site.catalog.sku);
      await scan(worker.page,'Adres źródłowy',site.catalog.receiving_code);
      await scan(worker.page,'Adres docelowy',site.catalog.storage_code);
      await worker.page.getByLabel('Ilość rzeczywista',{exact:true}).fill(quantity);
      await worker.page.getByRole('button',{name:'Odłóż towar',exact:true}).click();
      await worker.page.getByRole('button',{name:'Zakończ rozmieszczenie',exact:true}).click();
      await expect(worker.page.getByRole('heading',{name:'Dokument zakończony',exact:true})).toBeVisible();
      const finalReceipt = await document(worker,site,receiptID,expectedScope);
      const finalPutaway = await document(worker,site,putawayID,expectedScope);
      expect(finalReceipt.state).toBe('closed'); expect(finalPutaway.state).toBe('closed');
      const stock = await api(worker,site.origin,stockPath);
      expect(stock.status).toBe(200);
      const balances = stock.body.data.filter(balance => [site.catalog.receiving_balance_id,site.catalog.storage_balance_id].includes(balance.id));
      expect(balances).toHaveLength(2);
      const afterReceiving = balances.find(balance => balance.id === site.catalog.receiving_balance_id);
      const afterStorage = balances.find(balance => balance.id === site.catalog.storage_balance_id);
      expect(units(afterReceiving.on_hand)).toBe(units(beforeReceiving.on_hand));
      expect(units(afterStorage.on_hand)-units(beforeStorage.on_hand)).toBe(units(quantity));
      expect(units(afterReceiving.reserved_qty)).toBe(units(beforeReceiving.reserved_qty));
      expect(units(afterStorage.reserved_qty)).toBe(units(beforeStorage.reserved_qty));
      await worker.page.getByRole('button',{name:'Biuro',exact:true}).click();
      await worker.page.getByRole('button',{name:'Stany',exact:true}).click();
      await expect(worker.page.getByRole('heading',{name:'Stany według adresów',exact:true})).toBeVisible();
      await worker.page.screenshot({path:test.info().outputPath('stany-after-putaway.png'),fullPage:true});
      await writeFile(test.info().outputPath('verified-workflow.json'),JSON.stringify({
        installation:site.name,origin:site.origin,profile:'onprem-v1',identityAdapterReplaced:false,
        tlsValidation:true,receipt_id:receiptID,receipt_number:receiptNumber,putaway_id:putawayID,putaway_number:putawayNumber,
        quantity,roles:{bureau_create:true,bureau_movement_denied:deniedMovement.status,worker_create_denied:deniedCreate.status},
        auth:{bureau:bureau.signal,worker:worker.signal,foreign_installation_token_rejected:true,anonymous_rejected:true},
        documents:{receipt:finalReceipt,putaway:finalPutaway},before_balances:[beforeReceiving,beforeStorage],balances,
      },null,2));
      completed = true;
    } finally {
      let logoutFailed = false;
      for (const session of sessions) {
        try {
          await session.page.getByRole('button',{name:'Wyloguj',exact:true}).click();
          const confirm = session.page.getByRole('button',{name:'Wyloguj',exact:true});
          if (await confirm.isVisible()) await confirm.click();
          await expect(session.page.getByLabel('Hasło',{exact:true})).toBeVisible();
        } catch {logoutFailed = true;} finally {await session.context.close();}
      }
      if (completed && logoutFailed) throw new Error('The completed workflow failed to confirm real employee logout');
    }
  });
}
