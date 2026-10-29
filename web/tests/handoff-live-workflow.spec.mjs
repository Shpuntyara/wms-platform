import {test,expect} from '@playwright/test';
import {writeFile} from 'node:fs/promises';

// Only the external identity adapter is replaced. Distinct authenticated actors,
// permissions, custody commands, transit balances and MM entries use real SQL.
test('different employees send and accept one exact handoff through real PostgreSQL despite both committed replies being lost',async({browser,baseURL})=>{
  const env=process.env,scope={organization_id:env.WMS_HANDOFF_LIVE_ORG,warehouse_id:env.WMS_HANDOFF_LIVE_WAREHOUSE};
  const sourceID=env.WMS_HANDOFF_LIVE_SOURCE,departmentID=env.WMS_HANDOFF_LIVE_DESTINATION_DEPARTMENT;
  const sessions=[],forbiddenReads=[];
  async function session(role,actor) {
    const context=await browser.newContext({baseURL});sessions.push(context);
    const page=await context.newPage();
    await page.route('**/app/vendor/keycloak.js',route=>route.fulfill({contentType:'text/javascript',body:`export default class FixtureIdentity {authenticated=true;token='valid-${role}';tokenParsed={sub:${JSON.stringify(actor)},preferred_username:'Pracownik ${role}'};async init(){return true;}async updateToken(){return false;}async login(){return true;}async logout(){this.authenticated=false;}}`}));
    page.on('request',request=>{const path=new URL(request.url()).pathname;if(path.startsWith('/api/v1/documents')||path.startsWith('/api/v1/work-orders'))forbiddenReads.push(path);});
    await page.goto('/app/');
    await expect(page.getByRole('heading',{name:'Stany według adresów',exact:true})).toBeVisible();
    await expect(page.getByRole('button',{name:'Nowe przyjęcie (PZ)',exact:true})).toHaveCount(0);
    const read=async path=>{
      const response=await context.request.get(path,{headers:{Authorization:`Bearer valid-${role}`}});
      expect(response.status()).toBe(200);return(await response.json()).data;
    };
    return{page,context,read};
  }
  async function loseFirstReply(page,path,expectedStatus) {
    const attempts=[];let lost=false;
    await page.route('**'+path,async route=>{
      if(route.request().method()!=='POST')return route.continue();
      const request=route.request();attempts.push({path:new URL(request.url()).pathname,key:request.headers()['idempotency-key'],body:request.postDataJSON()});
      if(!lost){const response=await route.fetch();expect(response.status()).toBe(expectedStatus);const result=(await response.json()).data;expect(Object.keys(result).sort()).toEqual(['handoff_id','operation_id','state','version']);lost=true;return route.abort('failed');}
      return route.continue();
    });
    return attempts;
  }
  try {
    const sender=await session('sender',env.WMS_HANDOFF_LIVE_SENDER);
    const before=await sender.read(`/api/v1/stock-balances?${new URLSearchParams({...scope,item_id:env.WMS_HANDOFF_LIVE_ITEM,owner_id:env.WMS_HANDOFF_LIVE_OWNER,limit:'200'})}`);
    expect(before.find(row=>row.id===sourceID)).toMatchObject({on_hand:'10.000000',available_qty:'10.000000'});
    const sendAttempts=await loseFirstReply(sender.page,'/api/v1/handoffs/send',201);
    await sender.page.getByRole('row').filter({hasText:'SR-M8'}).filter({hasText:'A1'}).getByRole('button',{name:'Przekaż do działu',exact:true}).click();
    await expect(sender.page.getByRole('dialog')).toContainText('Jan Kowalski');
    await sender.page.getByLabel('Dział docelowy',{exact:true}).selectOption(departmentID);
    await sender.page.getByLabel('Ilość do przekazania',{exact:true}).fill('1,125');
    await sender.page.getByRole('button',{name:'Wyślij do działu',exact:true}).click();
    await expect(sender.page.getByRole('button',{name:'Sprawdź wynik',exact:true})).toBeVisible();
    await sender.page.reload();await sender.page.getByRole('button',{name:'Sprawdź wynik',exact:true}).click();
    await expect(sender.page.getByRole('region',{name:'Wybrane przekazanie',exact:true})).toContainText('Oczekuje na przyjęcie');
    expect(sendAttempts).toHaveLength(2);expect(sendAttempts[1]).toEqual(sendAttempts[0]);
    expect(sendAttempts[0].body).toEqual({...scope,source_balance_id:sourceID,destination_department_id:departmentID,quantity:'1.125'});
    const sent=(await sender.read(`/api/v1/handoffs?${new URLSearchParams(scope)}`))[0];
    expect(sent).toMatchObject({quantity:'1.125000',state:'sent',sender_actor_id:env.WMS_HANDOFF_LIVE_SENDER,sender_name:'Jan Kowalski',receiver_actor_id:null});
    await expect(sender.page.getByRole('button',{name:'Potwierdź przyjęcie',exact:true})).toHaveCount(0);
    const inFlight=await sender.read(`/api/v1/stock-balances?${new URLSearchParams({...scope,item_id:env.WMS_HANDOFF_LIVE_ITEM,limit:'200'})}`);
    const transit=inFlight.find(row=>row.id===sent.transit_balance_id);
    expect(transit).toMatchObject({on_hand:'1.125000',stock_state:'in_transit'});
    expect(/^0(?:\.0{1,6})?$/.test(transit.available_qty)).toBe(true);
    const receiver=await session('receiver',env.WMS_HANDOFF_LIVE_RECEIVER);
    await receiver.page.getByRole('button',{name:'Przekazania między działami',exact:true}).click();
    const incoming=receiver.page.waitForResponse(response=>new URL(response.url()).pathname==='/api/v1/handoffs'&&new URL(response.url()).searchParams.get('view')==='incoming');
    await receiver.page.getByRole('button',{name:'Do przyjęcia',exact:true}).click();
    const incomingResponse=await incoming;expect(incomingResponse.status()).toBe(200);
    expect((await incomingResponse.json()).data.map(row=>row.id)).toEqual([sent.id]);
    await receiver.page.getByRole('button',{name:/^Otwórz przekazanie /}).first().click();
    const acceptAttempts=await loseFirstReply(receiver.page,`/api/v1/handoffs/${sent.id}/accept`,200);
    await receiver.page.getByRole('button',{name:'Potwierdź przyjęcie',exact:true}).click();
    await expect(receiver.page.getByRole('button',{name:'Sprawdź wynik',exact:true})).toBeVisible();
    await receiver.page.reload();await receiver.page.getByRole('button',{name:'Sprawdź wynik',exact:true}).click();
    await expect(receiver.page.getByRole('region',{name:'Wybrane przekazanie',exact:true})).toContainText('Anna Zielińska');
    const incomingAfter=receiver.page.waitForResponse(response=>new URL(response.url()).pathname==='/api/v1/handoffs'&&new URL(response.url()).searchParams.get('view')==='incoming');
    await receiver.page.getByRole('button',{name:'Do przyjęcia',exact:true}).click();
    const incomingAfterResponse=await incomingAfter;expect(incomingAfterResponse.status()).toBe(200);
    expect((await incomingAfterResponse.json()).data).toEqual([]);
    await expect(receiver.page.getByRole('button',{name:/^Otwórz przekazanie /})).toHaveCount(0);
    expect(acceptAttempts).toHaveLength(2);expect(acceptAttempts[1]).toEqual(acceptAttempts[0]);
    expect(acceptAttempts[0].body).toEqual({...scope,expected_version:1});
    const accepted=await receiver.read(`/api/v1/handoffs/${sent.id}?${new URLSearchParams(scope)}`);
    expect(accepted).toMatchObject({state:'accepted',version:2,sender_actor_id:env.WMS_HANDOFF_LIVE_SENDER,receiver_actor_id:env.WMS_HANDOFF_LIVE_RECEIVER,receiver_name:'Anna Zielińska',item_id:env.WMS_HANDOFF_LIVE_ITEM,quantity:'1.125000'});
    expect(Date.parse(accepted.accepted_at)).toBeGreaterThanOrEqual(Date.parse(accepted.sent_at));
    const after=await receiver.read(`/api/v1/stock-balances?${new URLSearchParams({...scope,item_id:env.WMS_HANDOFF_LIVE_ITEM,limit:'200'})}`);
    expect(after.find(row=>row.id===sourceID).on_hand).toBe('8.875000');
    expect(after.find(row=>row.id===accepted.destination_balance_id).on_hand).toBe('1.125000');
    expect(after.find(row=>row.id===accepted.transit_balance_id).on_hand).toBe('0.000000');
    expect(forbiddenReads).toEqual([]);
    await writeFile(test.info().outputPath('verified-handoff.json'),JSON.stringify({identityAdapterReplaced:true,httpAndPostgresReal:true,sendAttempts,acceptAttempts,handoff:accepted,forbiddenDocumentReads:forbiddenReads},null,2));
  } finally {for(const context of sessions)await context.close();}
});
