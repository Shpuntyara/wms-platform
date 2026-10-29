import Keycloak from '../vendor/keycloak.js';
import {parseQuantity, remaining, matchScan, movementIntent, completed, formatQuantity as qty} from './domain.js';
import {CommandJournal} from './journal.js';
import {DemoWarehouse} from './demo.js';
import {errorMessage} from './messages.js';

const root = document.getElementById('app');
const e = value => String(value ?? '').replace(/[&<>"']/g, char => ({'&':'&amp;','<':'&lt;','>':'&gt;','"':'&quot;',"'":'&#39;'}[char]));
const paths = {box:'m3 7 9-5 9 5v10l-9 5-9-5V7Zm0 0 9 5 9-5M12 12v10M7.5 4.5l9 5',receive:'M12 3v12m-4-4 4 4 4-4M4 15v6h16v-6',move:'M3 7h17m-4-4 4 4-4 4M21 17H4m4-4-4 4 4 4',grid:'M3 3h7v7H3V3Zm11 0h7v7h-7V3ZM3 14h7v7H3v-7Zm11 0h7v7h-7v-7Z',history:'M3 10a9 9 0 1 1 1 7M3 4v6h6m3-4v6l4 2',search:'M16 16l5 5M17 10a7 7 0 1 1-14 0 7 7 0 0 1 14 0',plus:'M12 4v16M4 12h16',arrow:'M4 12h16m-6-6 6 6-6 6',scan:'M8 3H3v5m13-5h5v5M3 16v5h5m13-5v5h-5M7 7v10m4-10v10m3-10v10m3-10v10',check:'m5 12 4 4L20 5',refresh:'M20 8a8 8 0 1 0 0 8m0-13v5h-5',close:'m6 6 12 12M6 18 18 6'};
const icon = name => `<svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.7" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true"><path d="${paths[name] || paths.box}"/></svg>`;
const states = {draft:'Do przygotowania', active:'Do realizacji', closed:'Zrealizowany', cancelled:'Anulowany'};
const kinds = {receipt:'Przyjęcie towaru (PZ)', transfer:'Przesunięcie towaru (MM)', shipment:'Wydanie', adjustment:'Korekta'};
const S = {demo:new URLSearchParams(location.search).get('demo') === '1', mode:'bureau', section:'receipt', filter:'all', search:'', documents:[], balances:[], selected:null, busy:false, message:'', error:'', work:null};
let request, identity, workspace, journal;
const named = (group,id,fallback='Brak konfiguracji') => workspace?.catalog[group]?.find(row => row.id === id)?.name ?? fallback;
const itemById = id => workspace.catalog.items.find(row => row.id === id);
const positionById = id => workspace.catalog.positions.find(row => row.id === id);
const scoped = () => workspace.scopes.find(row => row.organization_id === S.scope?.organization_id && row.warehouse_id === S.scope?.warehouse_id);
const can = (permission,owner) => scoped()?.grants.some(grant => grant.permission === permission && (!owner || !grant.owner_id || grant.owner_id === owner));
const query = () => new URLSearchParams(S.scope).toString();
const badge = doc => `<span class="badge ${e(doc.state)}"><span aria-hidden="true">${doc.state === 'closed' ? '✓' : '●'}</span> ${e(states[doc.state] || doc.state)}</span>`;
const done = doc => doc.lines.filter(line => remaining(line.requested_qty,line.fulfilled_qty) === '0').length;
function pending() {
  try {return journal?.pending();} catch (err) {S.error = err.message; return {broken:true};}
}
function setScope(scope) {
  S.scope = {organization_id:scope.organization_id, warehouse_id:scope.warehouse_id};
  journal = new CommandJournal({storage:localStorage, namespace:`${S.demo?'training-v2':'live'}|${workspace.actor_id}|${scope.organization_id}|${scope.warehouse_id}`, request});
  S.selected = null; S.work = null;
}
async function list(resource,cursor) {
  const params = new URLSearchParams({...S.scope,limit:'100'});
  if (cursor) params.set('cursor',cursor);
  return request(`/api/v1/${resource}?${params}`);
}
async function refresh(append=false) {
  const page = await list('documents',append?S.cursor:null);
  S.documents = append ? [...S.documents,...page.data.filter(row => !S.documents.some(old => old.id === row.id))] : page.data;
  S.cursor = page.pagination?.next_cursor;
  if (can('stock.read')) {const stock = await list('stock-balances'); S.balances = stock.data; S.stockCursor = stock.pagination?.next_cursor;}
  if (S.selected) S.selected = (await request(`/api/v1/documents/${S.selected.id}?${query()}`)).data;
}
async function selectDocument(id) {
  S.selected = (await request(`/api/v1/documents/${id}?${query()}`)).data;
  S.work = null; S.error = ''; S.message = '';
  sessionStorage.setItem(`wms-open:${workspace.actor_id}`,id);
  render();
}
function resetWork() {
  const line = S.selected?.lines.find(row => remaining(row.requested_qty,row.fulfilled_qty) !== '0');
  S.work = line ? {line,item:itemById(line.item_id),stage:'item'} : null;
}
function render() {
  if (!workspace) return;
  const blocked = pending();
  document.body.classList.toggle('worker-mode',S.mode === 'worker');
  root.innerHTML = `<a class="skip-link" href="#main">Przejdź do obszaru roboczego</a>${S.demo?'<div class="training-bar">Magazyn szkoleniowy <span>Dane przykładowe · tylko w tej przeglądarce</span><a href="/app/">Do instalacji roboczej</a></div>':''}
  <header class="topbar"><a class="brand" href="/app/${S.demo?'?demo=1':''}">${icon('box')}<span>MAGAZYN<small>stanowisko pracy</small></span></a><div class="warehouse"><span class="eyebrow">MAGAZYN</span><label class="visually-hidden" for="warehouse">Magazyn roboczy</label><select id="warehouse" ${blocked||S.busy?'disabled':''}>${workspace.scopes.filter(row => row.grants.some(g => g.permission === 'document.read')).map(row => `<option value="${e(row.organization_id+'|'+row.warehouse_id)}" ${row.warehouse_id === S.scope?.warehouse_id && row.organization_id === S.scope?.organization_id?'selected':''}>${e(named('warehouses',row.warehouse_id,'Magazyn bez nazwy'))}</option>`).join('')}</select></div><nav class="mode-switch" aria-label="Stanowisko pracy"><button data-mode="bureau" aria-pressed="${S.mode === 'bureau'}">Biuro</button><button data-mode="worker" aria-pressed="${S.mode === 'worker'}">${icon('scan')} Magazyn</button></nav><div class="user"><span class="avatar">${S.demo?'S':'P'}</span><span>${S.demo?'Zmiana szkoleniowa':e(identity?.tokenParsed?.preferred_username || 'Pracownik')}<small>${S.demo?'Przykład lokalny':'Operator magazynu'}</small></span>${!S.demo?'<button class="text-button" data-action="logout">Wyloguj</button>':''}</div></header>
  <div class="app-shell"><aside class="sidebar"><span class="eyebrow sidebar-label">OPERACJE</span><nav aria-label="Operacje magazynowe">${[['receipt','receive','Przyjęcia (PZ)'],['transfer','move','Przesunięcia (MM)'],['stock','grid','Stany'],['history','history','Historia']].map(([section,graphic,label]) => `<button class="nav-item ${S.section === section?'selected':''}" data-section="${section}" ${section === 'stock'&&!can('stock.read')?'disabled':''}>${icon(graphic)}<span>${label}</span>${['receipt','transfer'].includes(section)?`<span class="nav-count">${S.documents.filter(doc => doc.kind === section && doc.state === 'active').length}</span>`:''}</button>`).join('')}</nav><div class="sidebar-bottom"><span class="connection-dot"></span>${S.demo?'Tryb szkoleniowy':'Instalacja robocza'}<small>${e(named('warehouses',S.scope?.warehouse_id))}</small></div></aside>
  <main id="main" tabindex="-1">${blocked?`<div class="pending"><div><strong>Operacja oczekuje na potwierdzenie</strong><p>Nie otrzymano odpowiedzi. Sprawdź wynik przed następną operacją.</p></div><button class="button warning" data-action="retry" ${S.busy||blocked.broken?'disabled':''}>${icon('refresh')} Sprawdź wynik</button></div>`:''}${S.error?`<div class="notice error" role="alert">${e(S.error)}</div>`:''}<div class="notice success ${S.message?'':'empty'}" role="status">${e(S.message)}</div>${!S.scope?'<div class="empty-state"><h1>Brak dostępnych magazynów</h1><p>Kierownik musi nadać Ci dostęp do dokumentów magazynu.</p></div>':S.mode === 'worker'?worker():bureau()}</main></div><dialog id="create-dialog" aria-labelledby="create-title"></dialog>`;
  bind();
}
function bureau() {
  if (S.section === 'stock') return stockView();
  const history = S.section === 'history';
  const kind = S.section === 'transfer'?'transfer':'receipt';
  const all = S.documents.filter(doc => history?['closed','cancelled'].includes(doc.state):doc.kind === kind);
  const docs = all.filter(doc => (S.filter === 'all'||doc.state === S.filter)&&doc.number.toLocaleLowerCase('pl').includes(S.search.toLocaleLowerCase('pl')));
  return `<div class="page-heading"><div><div class="breadcrumb">Operacje magazynowe <span>/</span> ${history?'Historia':kinds[kind]}</div><h1>${history?'Historia dokumentów':kinds[kind]}<span class="heading-count" aria-hidden="true">${all.length}</span></h1><p>${history?'Zrealizowane i anulowane dokumenty.':kind === 'receipt'?'Przyjmij towar, sprawdź ilość i przekaż go do składowania.':'Przesuń przyjęty towar ze strefy przyjęć pod adres składowania. To etap rozmieszczenia towaru.'}</p></div><div class="heading-actions"><button class="button" data-action="refresh" ${S.busy?'disabled':''}>${icon('refresh')} Odśwież</button>${!history&&can('document.create')?`<button class="button primary" data-action="create" ${S.busy||pending()?'disabled':''}>${icon('plus')} ${kind === 'receipt'?'Nowe przyjęcie (PZ)':'Nowe przesunięcie (MM)'}</button>`:''}</div></div>
  <div class="workbench ${S.selected?'has-detail':''}"><section class="document-panel" aria-label="Lista dokumentów"><div class="list-toolbar"><div class="filters" aria-label="Status dokumentów">${[['all','Wszystkie'],['active','Do realizacji'],['draft','Do przygotowania'],['closed','Zrealizowane']].filter(([key]) => !history||['all','closed'].includes(key)).map(([key,label]) => `<button data-filter="${key}" aria-pressed="${S.filter === key}">${label}${key === 'active'?`<span>${all.filter(doc => doc.state === key).length}</span>`:''}</button>`).join('')}</div><div class="search">${icon('search')}<label class="visually-hidden" for="search">Szukaj w załadowanych dokumentach</label><input id="search" type="search" placeholder="Znajdź dokument" value="${e(S.search)}"></div></div><div class="table-scroll"><table><thead><tr><th>Dokument</th><th>Status</th><th>Właściciel towaru</th><th class="numeric">Pozycji</th><th>Wykonanie</th><th><span class="visually-hidden">Otwórz</span></th></tr></thead><tbody>${docs.map(doc => `<tr class="${S.selected?.id === doc.id?'current':''}"><td><button class="document-link" data-document="${e(doc.id)}">${e(doc.number)}</button><small>${e(kinds[doc.kind])}</small></td><td>${badge(doc)}</td><td>${e(named('owners',doc.owner_id))}</td><td class="numeric mono">${doc.lines.length}</td><td><div class="completion"><progress max="${doc.lines.length||1}" value="${done(doc)}" aria-label="Wykonane pozycje"></progress><span>${done(doc)}/${doc.lines.length}</span></div></td><td class="row-arrow" aria-hidden="true">→</td></tr>`).join('')||'<tr><td colspan="6" class="table-empty">Brak dokumentów. Utwórz dokument lub zmień filtr.</td></tr>'}</tbody></table></div><div class="list-footer"><span>Widoczne ${docs.length} z ${all.length} załadowanych</span>${S.cursor?'<button class="text-button" data-action="more">Załaduj więcej</button>':'<span>Szukanie w załadowanych</span>'}</div></section>${S.selected?detail():`<aside class="detail-placeholder">${icon('receive')}<h2>Zacznij od dokumentu</h2><p>Otwórz dokument, aby zobaczyć towary i rzeczywistą ilość.</p><div class="process-note"><strong>Przyjęcie (PZ) → Przesunięcie (MM)</strong><p>Potwierdź towar w strefie przyjęć. Potem przygotuj przesunięcie (MM), aby przenieść go pod adres składowania.</p></div></aside>`}</div>`;
}
function detail() {
  const doc = S.selected;
  return `<aside class="detail-panel" aria-label="Wybrany dokument"><div class="detail-heading"><span class="eyebrow">${e(kinds[doc.kind])}</span><button class="icon-button" data-action="deselect" aria-label="Zamknij kartę dokumentu">${icon('close')}</button></div><h2>${e(doc.number)}</h2>${badge(doc)}<dl class="document-meta"><div><dt>Właściciel</dt><dd>${e(named('owners',doc.owner_id))}</dd></div><div><dt>Wykonane pozycje</dt><dd>${done(doc)} z ${doc.lines.length}</dd></div></dl><div class="detail-section-heading"><h3>Towary w dokumencie</h3><span>${doc.lines.length}</span></div><div class="line-list">${doc.lines.map(line => `<div class="item-line"><span class="item-icon">${icon('box')}</span><div><strong>${e(itemById(line.item_id)?.name||'Brak nazwy towaru')}</strong><small class="mono">${e(itemById(line.item_id)?.sku||'Brak kodu')}</small><div class="line-quantity"><b>${e(qty(line.fulfilled_qty))}</b><span> z ${e(qty(line.requested_qty))} ${e(itemById(line.item_id)?.unit||'')}</span>${remaining(line.requested_qty,line.fulfilled_qty) === '0'?`<span class="quantity-check">✓ ${doc.kind === 'transfer'?'Przesunięto':'Przyjęto'}</span>`:''}</div></div></div>`).join('')}</div><div class="detail-actions">${documentActions(doc)}</div><p class="detail-footnote">Ilość zmienia się po potwierdzeniu operacji magazynowej.</p></aside>`;
}
function documentActions(doc) {
  const blocked = S.busy||S.needsRefresh||pending();
  if (doc.state === 'draft'&&can('document.transition',doc.owner_id)) return `<button class="button primary full" data-action="activate" ${blocked?'disabled':''}>Przekaż do realizacji ${icon('arrow')}</button><p class="hint">Dokument jest zapisany, ale magazyn nie może jeszcze przyjmować ani przesuwać towaru. Sprawdź plan i przekaż go do realizacji. Zapisanych pozycji nie można edytować.</p>`;
  if (doc.state === 'active') return `${can('movement.create',doc.owner_id)&&['receipt','transfer'].includes(doc.kind)?`<button class="button primary full" data-action="terminal" ${blocked?'disabled':''}>${icon('scan')} ${doc.kind === 'receipt'?'Rozpocznij przyjmowanie':'Rozpocznij przesunięcie'}</button>`:''}${completed(doc)&&can('document.transition',doc.owner_id)?`<button class="button primary full" data-action="close-document" ${blocked?'disabled':''}>${icon('check')} Zakończ ${doc.kind === 'receipt'?'przyjęcie (PZ)':'przesunięcie (MM)'}</button>`:'<p class="hint">Aby zakończyć, wykonaj wszystkie pozycje dokumentu.</p>'}`;
  if (doc.state === 'closed'&&doc.kind === 'receipt'&&can('document.create',doc.owner_id)) return `<button class="button primary full" data-action="putaway" ${blocked?'disabled':''}>Przygotuj przesunięcie (MM) ${icon('arrow')}</button><p class="hint">Utwórz osobny dokument, aby przenieść przyjęty towar ze strefy przyjęć pod adres składowania.</p>`;
  return '';
}
function stockView() {
  return `<div class="page-heading"><div><div class="breadcrumb">Operacje magazynowe <span>/</span> Stany</div><h1>Stany według adresów</h1><p>Ilość fizyczna i dostępna w wybranym magazynie.</p></div><button class="button" data-action="refresh">${icon('refresh')} Odśwież</button></div><section class="document-panel"><div class="table-scroll"><table><thead><tr><th>Towar</th><th>Adres</th><th>Właściciel</th><th class="numeric">Stan fizyczny</th><th class="numeric">Zarezerwowane</th><th class="numeric">Dostępne</th></tr></thead><tbody>${S.balances.map(balance => `<tr><td><strong>${e(itemById(balance.item_id)?.name||'Brak konfiguracji towaru')}</strong><small class="mono">${e(itemById(balance.item_id)?.sku)}</small></td><td class="mono">${e(positionById(balance.position_id)?.code||'Brak konfiguracji adresu')}</td><td>${e(named('owners',balance.owner_id))}</td><td class="numeric mono">${e(qty(balance.on_hand))}</td><td class="numeric mono">${e(qty(balance.reserved_qty))}</td><td class="numeric mono">${e(qty(balance.available_qty))}</td></tr>`).join('')||'<tr><td colspan="6" class="table-empty">Brak stanów w dostępnym zakresie.</td></tr>'}</tbody></table></div><div class="list-footer"><span>Załadowane pozycje: ${S.balances.length}</span>${S.stockCursor?'<button class="text-button" data-action="more-stock">Załaduj więcej</button>':''}</div></section>`;
}
function worker() {
  const doc = S.selected;
  if (!doc||!['receipt','transfer'].includes(doc.kind)) return `<div class="terminal-container"><div class="terminal-heading"><span class="eyebrow">PRACA W MAGAZYNIE</span><h1>Wybierz dokument</h1><p>Przyjęcia (PZ) i przesunięcia (MM) przekazane do realizacji w Twoim magazynie.</p></div><div class="task-list">${S.documents.filter(row => row.state === 'active'&&['receipt','transfer'].includes(row.kind)&&can('movement.create',row.owner_id)).map(row => `<button class="task-button" data-worker-document="${e(row.id)}"><span>${icon(row.kind === 'receipt'?'receive':'move')}<strong>${e(row.number)}</strong><small>${e(kinds[row.kind])} · ${row.lines.length} pozycji</small></span>${icon('arrow')}</button>`).join('')||'<p class="empty-state">Brak dokumentów do realizacji. Biuro musi najpierw przekazać dokument do realizacji.</p>'}${S.cursor?'<button class="button" data-action="more">Załaduj więcej</button>':''}</div></div>`;
  if (!S.work) resetWork();
  const work = S.work, receive = doc.kind === 'receipt';
  if (doc.state !== 'active'||!work) return `<div class="terminal-container"><button class="text-button" data-action="back-tasks">← Do dokumentów</button><div class="terminal-complete">${icon('check')}<h1>${doc.state === 'closed'?'Dokument zrealizowany':'Wszystkie towary potwierdzone'}</h1><p>${e(doc.number)} · ${done(doc)} z ${doc.lines.length} pozycji</p>${documentActions(doc)}</div></div>`;
  if (!work.item) return '<div class="terminal-container"><h1>Skonfiguruj towar</h1><p>Brakuje kodu lub dokładności towaru. Skontaktuj się z kierownikiem zmiany.</p><button class="button" data-mode="bureau">Wróć do biura</button></div>';
  const stages = receive?['Towar','Adres','Ilość']:['Towar','Źródło','Cel','Ilość'];
  const index = work.stage === 'item'?0:work.stage === 'source'?1:work.stage === 'target'?(receive?1:2):stages.length-1;
  const label = work.stage === 'item'?'Kod towaru':work.stage === 'source'?'Adres źródłowy':work.stage === 'target'?(receive?'Adres przyjęcia':'Adres docelowy'):'Ilość rzeczywista';
  const heading = work.stage === 'item'?'Zeskanuj towar':work.stage === 'source'?'Zeskanuj adres źródłowy':work.stage === 'target'?(receive?'Zeskanuj adres przyjęcia':'Zeskanuj adres składowania'):'Potwierdź ilość';
  return `<div class="terminal-container"><div class="terminal-document"><button class="text-button" data-action="back-tasks">← Dokumenty</button><span class="mono">${e(doc.number)}</span>${badge(doc)}</div><div class="terminal-heading"><span class="eyebrow">${receive?'PRZYJĘCIE TOWARU (PZ)':'PRZESUNIĘCIE TOWARU (MM)'}</span><h1>${heading}</h1></div><ol class="steps">${stages.map((stage,n) => `<li class="${n === index?'current':n < index?'done':''}" ${n === index?'aria-current="step"':''}><span>${n < index?'✓':n+1}</span>${stage}</li>`).join('')}</ol><section class="scan-card"><div class="terminal-item"><span class="item-icon">${icon('box')}</span><div><span class="eyebrow">POZYCJA ${doc.lines.findIndex(line => line.id === work.line.id)+1} Z ${doc.lines.length}</span><h2>${e(work.item.name)}</h2><p class="mono">${e(work.item.sku)}</p></div></div><div class="terminal-quantity"><span>Pozostało ${receive?'do przyjęcia':'do przesunięcia'}</span><strong>${e(qty(remaining(work.line.requested_qty,work.line.fulfilled_qty)))} <small>${e(work.item.unit)}</small></strong></div>${work.sourceBalance?`<div class="scan-confirmed">✓ Źródło: <b class="mono">${e(positionById(work.sourceBalance.position_id)?.code)}</b></div>`:''}${work.targetBalance?`<div class="scan-confirmed">✓ ${receive?'Przyjęcia':'Cel'}: <b class="mono">${e(positionById(work.targetBalance.position_id)?.code)}</b></div>`:''}<form id="scan-form"><label for="scan-input">${label}</label><div class="scan-input-wrap">${icon(work.stage === 'quantity'?'box':'scan')}<input id="scan-input" name="scan" autocomplete="off" ${work.stage === 'quantity'?'inputmode="decimal"':''} aria-describedby="scan-hint" ${S.busy||pending()?'disabled':''}></div><p id="scan-hint" class="hint">${work.stage === 'quantity'?'Wpisz przeliczoną ilość. Część dziesiętną możesz oddzielić przecinkiem.':'Skaner z klawiszem Enter lub ręczne wpisanie dokładnego kodu.'}</p><button class="button primary full" ${S.busy||pending()?'disabled':''}>${work.stage === 'quantity'?(receive?'Przyjmij towar':'Przesuń towar'):'Dalej'} ${icon(work.stage === 'quantity'?'check':'arrow')}</button></form>${index > 0?'<button class="text-button" data-action="restart-scan">Skanuj ponownie</button>':''}</section><div class="terminal-footer"><span>Wykonane pozycje: <b>${done(doc)} / ${doc.lines.length}</b></span><button class="text-button" data-mode="bureau">Do dokumentu w biurze</button></div></div>`;
}
function bind() {
  root.querySelectorAll('[data-mode]').forEach(button => button.onclick = () => {S.mode = button.dataset.mode; S.error = ''; render(); focusScan();});
  root.querySelectorAll('[data-section]').forEach(button => button.onclick = () => {S.section = button.dataset.section; S.filter = 'all'; S.search = ''; S.selected = null; render();});
  root.querySelectorAll('[data-filter]').forEach(button => button.onclick = () => {S.filter = button.dataset.filter; render();});
  root.querySelectorAll('[data-document]').forEach(button => button.onclick = () => run(() => selectDocument(button.dataset.document)));
  root.querySelectorAll('[data-worker-document]').forEach(button => button.onclick = () => run(async () => {await selectDocument(button.dataset.workerDocument); resetWork(); render(); focusScan();}));
  root.querySelectorAll('[data-action]').forEach(button => button.onclick = () => action(button.dataset.action));
  const search = root.querySelector('#search');
  if (search) search.oninput = () => {S.search = search.value; const start = search.selectionStart, end = search.selectionEnd; render(); const input = root.querySelector('#search'); input.focus(); if (start !== null) input.setSelectionRange(start,end);};
  root.querySelector('#warehouse').onchange = event => run(async () => {const [org,wh] = event.target.value.split('|'); setScope(workspace.scopes.find(row => row.organization_id === org&&row.warehouse_id === wh)); await refresh(); render();});
  const form = root.querySelector('#scan-form');
  if (form) form.onsubmit = event => {event.preventDefault(); const value = root.querySelector('#scan-input').value; run(() => scanInput(value));};
}
function focusScan() {requestAnimationFrame(() => root.querySelector('#scan-input')?.focus());}
async function run(fn) {
  if (S.busy) return;
  S.error = ''; S.message = '';
  try {await fn();} catch (err) {S.error = problem(err); render(); focusScan();}
}
function problem(err) {
  return errorMessage(err,Boolean(pending()));
}
async function write(path,body,message) {
  if (S.busy) return;
  if (S.needsRefresh) throw new Error('Najpierw odśwież dokument po potwierdzonej operacji');
  S.busy = true; render();
  try {
    const send = () => journal.submit(path,body);
    const result = navigator.locks?await navigator.locks.request('wms:'+journal.namespace,send):await send();
    await afterConfirmation(result,message);
    return result;
  } finally {S.busy = false; render(); focusScan();}
}
async function afterConfirmation(result,message) {
  S.message = message;
  try {
    await refresh();
    if (result.data.document_id && S.selected?.id !== result.data.document_id) S.selected = (await request(`/api/v1/documents/${result.data.document_id}?${query()}`)).data;
    resetWork(); S.error = ''; S.needsRefresh = false;
  } catch {
    S.needsRefresh = true;
    S.error = 'Operacja potwierdzona. Nie udało się odświeżyć dokumentu. Wybierz „Odśwież”.';
  }
}
function action(name) {
  if (name === 'create'||name === 'putaway') return openCreate(name === 'putaway');
  if (name === 'terminal') {S.mode = 'worker'; resetWork(); render(); focusScan(); return;}
  if (name === 'deselect'||name === 'back-tasks') {S.selected = null; S.work = null; render(); return;}
  if (name === 'restart-scan') {resetWork(); S.error = ''; render(); focusScan(); return;}
  if (name === 'logout') {identity.logout({redirectUri:location.origin+'/app/'}); return;}
  run(async () => {
    if (name === 'refresh'||name === 'more') {await refresh(name === 'more'); S.needsRefresh = false; resetWork(); S.message = 'Dane odświeżone'; render();}
    if (name === 'more-stock') {const page = await list('stock-balances',S.stockCursor); S.balances.push(...page.data); S.stockCursor = page.pagination?.next_cursor; render();}
    if (name === 'activate'||name === 'close-document') {const doc = S.selected; await write(`/api/v1/documents/${doc.id}/transitions`,{...S.scope,expected_document_version:doc.version,state:name === 'activate'?'active':'closed'},name === 'activate'?'Dokument przekazany do realizacji':'Dokument zrealizowany');}
    if (name === 'retry') {
      S.busy = true; render();
      try {const result = await journal.retry(); await afterConfirmation(result,'Operacja potwierdzona. Możesz kontynuować pracę.');}
      finally {S.busy = false; render(); focusScan();}
    }
  });
}
async function scanInput(value) {
  if (S.needsRefresh) throw new Error('Najpierw odśwież dokument po potwierdzonej operacji');
  if (S.scanning) return;
  S.scanning = true;
  try {await advanceScan(value);} finally {S.scanning = false;}
}
async function advanceScan(value) {
  const work = S.work;
  if (work.stage === 'item') {matchScan(value,[work.item]); work.stage = S.selected.kind === 'receipt'?'target':'source';}
  else if (work.stage === 'source'||work.stage === 'target') {
    const position = matchScan(value,workspace.catalog.positions.filter(row => row.organization_id === S.scope.organization_id&&row.warehouse_id === S.scope.warehouse_id),'code');
    if (work.stage === 'target' && S.selected.kind === 'receipt' && position.kind !== 'receiving') throw new Error('Wybierz adres strefy przyjęć');
    if (work.stage === 'target' && S.selected.kind === 'transfer' && position.kind !== 'storage') throw new Error('Wybierz adres składowania');
    const params = new URLSearchParams({...S.scope,item_id:work.item.id,position_id:position.id,owner_id:S.selected.owner_id,limit:'200'});
    const page = await request(`/api/v1/stock-balances?${params}`);
    const balances = page.data.filter(row => row.item_id === work.item.id&&row.position_id === position.id&&row.owner_id === S.selected.owner_id);
    if (balances.length !== 1||page.pagination?.next_cursor) throw new Error(balances.length?'Pod adresem jest kilka partii lub stanów towaru. Poproś kierownika o wskazanie właściwej pozycji.':'Towar nie jest przypisany do tego adresu. Sprawdź adres lub skontaktuj się z kierownikiem.');
    work[work.stage === 'source'?'sourceBalance':'targetBalance'] = balances[0];
    work.stage = work.stage === 'source'?'target':'quantity';
  } else {
    const body = movementIntent({document:S.selected,line:work.line,item:work.item,sourceBalance:work.sourceBalance,targetBalance:work.targetBalance,scope:S.scope,quantity:value,kind:S.selected.kind});
    await write('/api/v1/movements',body,S.selected.kind === 'receipt'?'Towar przyjęty':'Towar przesunięty'); return;
  }
  S.error = ''; render(); focusScan();
}
function openCreate(fromReceipt=false) {
  if (pending()||S.busy||S.needsRefresh) return;
  const dialog = root.querySelector('#create-dialog'), kind = fromReceipt||S.section === 'transfer'?'transfer':'receipt';
  const owners = workspace.catalog.owners.filter(owner => owner.organization_id === S.scope.organization_id&&can('document.create',owner.id));
  const items = workspace.catalog.items.filter(item => item.organization_id === S.scope.organization_id);
  if (!owners.length||!items.length) {S.error = 'Kartoteka towarów lub właścicieli nie jest skonfigurowana. Skontaktuj się z administratorem.'; render(); return;}
  if (fromReceipt && S.selected.lines.some(line => !items.some(item => item.id === line.item_id))) {S.error = 'Brak kartoteki towaru z dokumentu. Poproś administratora o uzupełnienie danych.'; render(); return;}
  if (fromReceipt && !owners.some(owner => owner.id === S.selected.owner_id)) {S.error = 'Brak uprawnień do właściciela towaru z dokumentu.'; render(); return;}
  const lines = fromReceipt?S.selected.lines.map(line => ({item_id:line.item_id,quantity:line.fulfilled_qty})):[{item_id:items[0].id,quantity:''}];
  let sequence = lines.length;
  const row = (line,n) => `<div class="create-line"><div><label for="item-${n}">Towar</label><select id="item-${n}" name="item">${items.map(item => `<option value="${e(item.id)}" ${line.item_id === item.id?'selected':''}>${e(item.sku)} · ${e(item.name)}</option>`).join('')}</select></div><div><label for="quantity-${n}">Ilość planowana</label><input id="quantity-${n}" name="quantity" inputmode="decimal" autocomplete="off" value="${e(line.quantity?qty(line.quantity):'')}"></div><button type="button" class="icon-button" data-remove-line aria-label="Usuń pozycję">${icon('close')}</button></div>`;
  dialog.innerHTML = `<form id="create-form" novalidate><div class="modal-heading"><div><span class="eyebrow">NOWY DOKUMENT</span><h2 id="create-title">${kind === 'receipt'?'Nowe przyjęcie (PZ)':'Nowe przesunięcie (MM)'}</h2></div><button type="button" class="icon-button" id="close-dialog" aria-label="Zamknij formularz">${icon('close')}</button></div>${fromReceipt?`<p class="source-note">Na podstawie przyjęcia ${e(S.selected.number)}. Przesunięcie (MM) przeniesie towar ze strefy przyjęć pod adres składowania.</p>`:''}<p class="hint">Numer zostanie nadany automatycznie po zapisaniu</p><div class="form-grid"><div><label for="owner">Właściciel towaru</label><select id="owner" name="owner">${owners.map(owner => `<option value="${e(owner.id)}" ${fromReceipt&&owner.id === S.selected.owner_id?'selected':''}>${e(owner.name)}</option>`).join('')}</select></div></div><div class="detail-section-heading"><h3>Planowane towary</h3><span>do 200 pozycji</span></div><div id="create-lines">${lines.map(row).join('')}</div><button type="button" class="text-button" id="add-line">${icon('plus')} Dodaj towar</button><div id="form-error" role="alert"></div><div class="modal-footer"><p>Zapisz dokument, sprawdź plan i przekaż go do realizacji. Zapis nie zmienia stanów magazynowych.</p><button class="button primary" type="submit">Zapisz dokument ${icon('arrow')}</button></div></form>`;
  dialog.querySelector('#close-dialog').onclick = () => dialog.close();
  dialog.onclick = event => {const button = event.target.closest('[data-remove-line]'); if (button&&dialog.querySelectorAll('.create-line').length > 1) button.closest('.create-line').remove();};
  dialog.querySelector('#add-line').onclick = () => {if (dialog.querySelectorAll('.create-line').length < 200) dialog.querySelector('#create-lines').insertAdjacentHTML('beforeend',row({item_id:items[0].id,quantity:''},sequence++));};
  dialog.querySelector('#create-form').onsubmit = event => {
    event.preventDefault();
    try {
      const inputLines = [...dialog.querySelectorAll('.create-line')].map(container => {const item = items.find(item => item.id === container.querySelector('[name=item]').value), field = container.querySelector('[name=quantity]'); try {return {item_id:item.id,quantity:parseQuantity(field.value,item.quantity_scale)};} catch (err) {field.setAttribute('aria-invalid','true'); field.focus(); throw err;}});
      const body = {...S.scope,owner_id:dialog.querySelector('[name=owner]').value,kind,lines:inputLines};
      dialog.close(); S.section = kind; S.filter = 'all'; S.search = '';
      run(() => write('/api/v1/documents',body,'Dokument zapisany. Sprawdź plan i przekaż go do realizacji.'));
    } catch (err) {dialog.querySelector('#form-error').textContent = err.message;}
  };
  dialog.showModal();
}
async function start() {
  try {
    if (S.demo) {const demo = new DemoWarehouse(localStorage); request = demo.request.bind(demo);}
    else {
      const response = await fetch('/app/config.json',{cache:'no-store'});
      if (!response.ok) throw new Error('Nie udało się wczytać ustawień logowania');
      const config = await response.json();
      if (!config.oidc?.url||!config.oidc?.realm||!config.oidc?.clientId) {root.innerHTML = `<main class="login-screen"><div class="login-mark">${icon('box')}</div><span class="eyebrow">STANOWISKO PRACY WMS</span><h1>Twój magazyn.<br>Wszystko na swoim miejscu.</h1><p>Przyjęcia (PZ) i przesunięcia (MM) dla biura i pracowników magazynu.</p><div class="notice">Logowanie nie jest jeszcze skonfigurowane. Administrator musi podłączyć konta pracowników i kartoteki.</div><a class="button primary" href="/app/?demo=1">Otwórz magazyn szkoleniowy ${icon('arrow')}</a><small>Operacje szkoleniowe nie zmieniają stanów magazynu.</small></main>`; return;}
      identity = new Keycloak(config.oidc);
      try {await identity.init({onLoad:'login-required',pkceMethod:'S256',locale:'pl',checkLoginIframe:false,redirectUri:location.origin+'/app/'});}
      catch {throw new Error('Nie udało się zalogować. Sprawdź połączenie i spróbuj ponownie.');}
      request = async (path,body,key) => {
        try {await identity.updateToken(30);} catch {throw Object.assign(new Error('Sesja wygasła. Zaloguj się ponownie.'),{status:401});}
        const headers = {Authorization:'Bearer '+identity.token};
        if (body) {headers['Content-Type']='application/json'; headers['Idempotency-Key']=key;}
        const response = await fetch(path,{method:body?'POST':'GET',headers,body:body?JSON.stringify(body):undefined,cache:'no-store',redirect:'error'});
        const result = await response.json();
        if (!response.ok) throw Object.assign(new Error(result.detail||'Serwer odrzucił operację'),{status:response.status,code:result.code});
        return result;
      };
    }
    workspace = (await request('/api/v1/workspace')).data;
    const first = workspace.scopes.find(scope => scope.grants.some(grant => grant.permission === 'document.read'));
    if (first) {setScope(first); await refresh();}
    const saved = sessionStorage.getItem(`wms-open:${workspace.actor_id}`);
    if (saved&&S.scope) {try {S.selected = (await request(`/api/v1/documents/${saved}?${query()}`)).data;} catch {}}
    render();
  } catch (err) {root.innerHTML = `<main class="login-screen"><h1>Nie udało się otworzyć magazynu</h1><div class="notice error" role="alert">${e(problem(err))}</div><a class="button primary" href="/app/">Zaloguj się ponownie</a><a href="/app/?demo=1">Otwórz magazyn szkoleniowy</a></main>`;}
}
start();
