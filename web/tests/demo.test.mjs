import test from 'node:test';
import assert from 'node:assert/strict';
import {DemoWarehouse} from '../src/demo.js';
const memory = () => {
  const map = new Map();
  return {getItem: k => map.get(k) ?? null, setItem: (k,v) => map.set(k,v), removeItem: k => map.delete(k)};
};
test('training mode receives the same command once, persists and exposes configured human names', async () => {
  const storage = memory();
  const demo = new DemoWarehouse(storage);
  const ws = (await demo.request('/api/v1/workspace')).data;
  assert.ok(ws, 'workspace must expose a catalog for training');
  assert.ok(ws.catalog.items[0].name);
  const documents = (await demo.request('/api/v1/documents')).data;
  const doc = documents.find(d => d.kind === 'receipt' && d.state === 'active');
  const balances = (await demo.request('/api/v1/stock-balances')).data;
  const balance = balances.find(b => b.item_id === doc.lines[0].item_id && b.owner_id === doc.owner_id);
  const body = {organization_id: doc.organization_id, warehouse_id: doc.warehouse_id, kind: 'receipt', document_line_id: doc.lines[0].id, expected_document_version: doc.version, target_balance_id: balance.id, quantity: '1'};
  const first = await demo.request('/api/v1/movements', body, 'one-key');
  assert.deepEqual(await demo.request('/api/v1/movements', body, 'one-key'), first);
  const reloaded = new DemoWarehouse(storage);
  const actual = (await reloaded.request('/api/v1/documents/' + doc.id)).data;
  assert.equal(actual.lines[0].fulfilled_qty, '1');
  assert.equal(actual.version, doc.version + 1);
  await assert.rejects(reloaded.request('/api/v1/movements', {...body, quantity: '2'}, 'one-key'), {status: 409});
});
