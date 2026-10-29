import test from 'node:test';
import assert from 'node:assert/strict';
import fc from 'fast-check';
import {DemoWarehouse} from '../src/demo.js';

test('property: demo numbering agrees with independent per-warehouse, kind and year counters including replays', async () => {
  await fc.assert(fc.asyncProperty(fc.array(fc.record({
    transfer: fc.boolean(), secondWarehouse: fc.boolean(), year: fc.integer({min:2026,max:2029}),
    replay: fc.integer({min:0,max:5}),
  }), {minLength:1,maxLength:60}), async operations => {
    const values = new Map();
    const storage = {getItem:key => values.get(key) ?? null, setItem:(key,value) => values.set(key,value)};
    let year = 2026;
    const seed = new DemoWarehouse(storage).state;
    const warehouse = seed.workspace.catalog.warehouses[0];
    const second = {...warehouse,id:'property-second-warehouse'};
    seed.workspace.catalog.warehouses.push(second);
    seed.workspace.scopes.push({...seed.workspace.scopes[0],warehouse_id:second.id});
    storage.setItem('wms-training-v2',JSON.stringify(seed));
    const demo = new DemoWarehouse(storage,{now:() => new Date(`${year}-06-01T12:00:00Z`)});
    const counters = new Map(), confirmed = [];
    for (const [index,operation] of operations.entries()) {
      year = operation.year;
      if (operation.replay === 0 && confirmed.length) {
        const prior = confirmed[index % confirmed.length];
        assert.deepEqual(await demo.request('/api/v1/documents',prior.body,prior.key),prior.result);
        const reread = (await demo.request('/api/v1/documents/'+prior.result.data.document_id)).data;
        assert.equal(reread.number,prior.number);
        continue;
      }
      const warehouseID = operation.secondWarehouse ? second.id : warehouse.id;
      const kind = operation.transfer ? 'transfer' : 'receipt';
      const bucket = `${warehouseID}|${kind}|${year}`;
      const next = (counters.get(bucket) || 0)+1;
      const prefix = operation.transfer ? 'MM' : 'PZ';
      const number = `${prefix}/${year}/${String(next).padStart(6,'0')}`;
      const body = {organization_id:warehouse.organization_id,warehouse_id:warehouseID,
        owner_id:seed.workspace.catalog.owners[0].id,kind,
        lines:[{item_id:seed.workspace.catalog.items[0].id,quantity:'1'}]};
      const key = `property-${index}`;
      const result = await demo.request('/api/v1/documents',body,key);
      const document = (await demo.request('/api/v1/documents/'+result.data.document_id)).data;
      assert.equal(document.number,number);
      assert.equal(document.warehouse_id,warehouseID);
      assert.equal(document.state,'draft');
      counters.set(bucket,next);
      confirmed.push({body,key,result,number});
    }
    const created = (await demo.request('/api/v1/documents')).data.filter(document => confirmed.some(entry => entry.result.data.document_id === document.id));
    assert.equal(created.length,confirmed.length);
    assert.equal(new Set(created.map(document => `${document.warehouse_id}|${document.kind}|${document.number}`)).size,created.length);
  }), {seed:20261029,numRuns:120});
});
