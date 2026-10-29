import test from 'node:test';
import assert from 'node:assert/strict';
import {DemoWarehouse} from '../src/demo.js';

const memory = () => {
  const values = new Map();
  return {getItem: key => values.get(key) ?? null, setItem: (key, value) => values.set(key, value)};
};
function fixture() {
  const storage = memory();
  let instant = '2026-06-01T12:00:00Z';
  const demo = new DemoWarehouse(storage, {now: () => new Date(instant)});
  const state = structuredClone(demo.state);
  const warehouse = state.workspace.catalog.warehouses[0];
  warehouse.timezone = 'Europe/Warsaw';
  const second = {...warehouse, id: '22222222-2222-4222-8222-000000000099', name: 'Drugi magazyn', timezone: 'America/New_York'};
  state.workspace.catalog.warehouses.push(second);
  state.workspace.scopes.push({...state.workspace.scopes[0], warehouse_id: second.id});
  storage.setItem('wms-training-v2', JSON.stringify(state));
  const body = (kind = 'receipt', warehouseId = warehouse.id) => ({organization_id: warehouse.organization_id,
    warehouse_id: warehouseId, owner_id: state.workspace.catalog.owners[0].id, kind,
    lines: [{item_id: state.workspace.catalog.items[0].id, quantity: '4'}]});
  return {storage, demo: new DemoWarehouse(storage, {now: () => new Date(instant)}), body, warehouse, second,
    at: value => {instant = value;}};
}
async function create(demo, body, key) {
  let result;
  try {result = await demo.request('/api/v1/documents', body, key);}
  catch (error) {assert.fail(`A valid document must save: ${error.message}`);}
  const doc = (await demo.request('/api/v1/documents/' + result.data.document_id)).data;
  assert.ok(doc, 'a confirmed create must be readable');
  assert.match(String(doc.number), /^(PZ|MM)\/\d{4}\/\d{6,}$/, 'the assigned number must be available through GET');
  return {result, doc};
}

test('demo assigns a PZ number after save without manual number or stock movement', async () => {
  const {demo, body} = fixture();
  const before = (await demo.request('/api/v1/stock-balances')).data;
  const {doc} = await create(demo, body(), 'receipt-1');
  assert.equal(doc.number, 'PZ/2026/000001');
  assert.equal(doc.state, 'draft');
  assert.equal(doc.version, 1);
  assert.equal(doc.lines[0].fulfilled_qty, '0');
  assert.deepEqual((await demo.request('/api/v1/stock-balances')).data, before);
});

test('demo numbers increment independently for PZ and MM', async () => {
  const {demo, body} = fixture();
  assert.equal((await create(demo, body(), 'receipt-1')).doc.number, 'PZ/2026/000001');
  assert.equal((await create(demo, body('transfer'), 'transfer-1')).doc.number, 'MM/2026/000001');
  assert.equal((await create(demo, body(), 'receipt-2')).doc.number, 'PZ/2026/000002');
  assert.equal((await create(demo, body('transfer'), 'transfer-2')).doc.number, 'MM/2026/000002');
});

test('demo replays the same command after year rollover without consuming a number', async () => {
  const {demo, body, at} = fixture();
  const request = body();
  const first = await create(demo, request, 'same-command');
  at('2027-06-01T12:00:00Z');
  assert.deepEqual(await demo.request('/api/v1/documents', request, 'same-command'), first.result);
  assert.equal((await demo.request('/api/v1/documents/' + first.doc.id)).data.number, 'PZ/2026/000001');
  assert.equal((await create(demo, body(), 'new-year')).doc.number, 'PZ/2027/000001');
  await assert.rejects(demo.request('/api/v1/documents', {...request, lines: [{...request.lines[0], quantity: '5'}]}, 'same-command'), {status: 409});
  assert.equal((await create(demo, body(), 'next-command')).doc.number, 'PZ/2027/000002');
});

test('demo series are isolated by warehouse and use each warehouse local calendar year', async () => {
  const {demo, body, at, warehouse, second} = fixture();
  at('2026-12-31T23:30:00Z');
  const polish = (await create(demo, body('receipt', warehouse.id), 'polish')).doc;
  const american = (await create(demo, body('receipt', second.id), 'american')).doc;
  assert.equal(polish.number, 'PZ/2027/000001');
  assert.equal(american.number, 'PZ/2026/000001');
  assert.equal(american.warehouse_id, second.id);
  at('2027-06-01T12:00:00Z');
  assert.equal((await create(demo, body('receipt', second.id), 'american-new-year')).doc.number, 'PZ/2027/000001');
  assert.equal((await create(demo, body('receipt', warehouse.id), 'polish-next')).doc.number, 'PZ/2027/000002');
});

test('demo reload retains a series and resumes after an existing automatic number', async () => {
  const {storage, body, warehouse} = fixture();
  const state = JSON.parse(storage.getItem('wms-training-v2'));
  state.documents.push({...state.documents[0], id: 'existing-auto', warehouse_id: warehouse.id, number: 'PZ/2026/000017'});
  storage.setItem('wms-training-v2', JSON.stringify(state));
  const first = new DemoWarehouse(storage, {now: () => new Date('2026-06-01T00:00:00Z')});
  assert.equal((await create(first, body(), 'resume')).doc.number, 'PZ/2026/000018');
  const reloaded = new DemoWarehouse(storage, {now: () => new Date('2026-06-01T00:00:00Z')});
  assert.equal((await create(reloaded, body(), 'resume-next')).doc.number, 'PZ/2026/000019');
});

test('demo adapters opened before either save do not overwrite or duplicate document numbers', async () => {
  const {storage, demo, body} = fixture();
  const other = new DemoWarehouse(storage, {now: () => new Date('2026-06-01T00:00:00Z')});
  const a = (await create(demo, body(), 'tab-a')).doc;
  const b = (await create(other, body(), 'tab-b')).doc;
  assert.equal(a.number, 'PZ/2026/000001');
  assert.equal(b.number, 'PZ/2026/000002');
  assert.ok((await demo.request('/api/v1/documents')).data.some(doc => doc.id === b.id));
});

test('demo storage failure cannot consume a document number or expose an unsaved document', async () => {
  const {storage, demo, body} = fixture();
  const before = storage.getItem('wms-training-v2');
  const save = storage.setItem;
  storage.setItem = () => {throw new Error('Storage is full');};
  await assert.rejects(demo.request('/api/v1/documents', body(), 'retry-save'), /Storage is full/);
  assert.equal(storage.getItem('wms-training-v2'), before);
  assert.equal(demo.state.documents.length, JSON.parse(before).documents.length);
  storage.setItem = save;
  assert.equal((await create(demo, body(), 'retry-save')).doc.number, 'PZ/2026/000001');
});

test('demo year falls back to the Polish calendar for older catalog data without timezone', async () => {
  const {storage, body} = fixture();
  const state = JSON.parse(storage.getItem('wms-training-v2'));
  delete state.workspace.catalog.warehouses[0].timezone;
  storage.setItem('wms-training-v2', JSON.stringify(state));
  const demo = new DemoWarehouse(storage, {now: () => new Date('2026-12-31T23:30:00Z')});
  assert.equal((await create(demo, body(), 'legacy-timezone')).doc.number, 'PZ/2027/000001');
});

test('demo preserves exact successive serials beyond the JavaScript safe integer range', async () => {
  const {storage, demo, body} = fixture();
  const state = JSON.parse(storage.getItem('wms-training-v2'));
  state.documents.push({...state.documents[0], id:'large-canonical-number', number:'PZ/2026/9007199254740992'});
  storage.setItem('wms-training-v2',JSON.stringify(state));
  assert.equal((await create(demo,body(),'large-next')).doc.number,'PZ/2026/9007199254740993');
  assert.equal((await create(demo,body(),'large-following')).doc.number,'PZ/2026/9007199254740994');
});

test('demo treats the empty legacy number as automatic allocation, matching the backend', async () => {
  const {demo, body} = fixture();
  assert.equal((await create(demo,{...body(),number:''},'empty-number')).doc.number,'PZ/2026/000001');
  assert.equal((await create(demo,body(),'after-empty')).doc.number,'PZ/2026/000002');
});
