import test from 'node:test';
import assert from 'node:assert/strict';
import {DemoWarehouse} from '../src/demo.js';
function fixture() {
  const entries=new Map(),storage={getItem:key=>entries.get(key)??null,setItem:(key,value)=>entries.set(key,value)};
  const demo=new DemoWarehouse(storage),state=demo.state,actor=state.workspace.actor_id;
  state.handoff_context={memberships:[{department_id:'target'}]};
  state.handoffs=[{id:'history',state:'accepted',sender_actor_id:'other',destination_department_id:'target'},...['first','second'].map(id=>({id,state:'sent',sender_actor_id:'other',destination_department_id:'target'})),{id:'own',state:'sent',sender_actor_id:actor,destination_department_id:'target'}];
  storage.setItem('wms-training-v2',JSON.stringify(state));
  return demo;
}
test('training incoming queue filters before its bounded page and rejects a cursor from another view',async()=>{
  const demo=fixture(),first=await demo.request('/api/v1/handoffs?view=incoming&limit=1');
  assert.deepEqual(first.data.map(row=>row.id),['first']);assert.ok(first.pagination.next_cursor);
  const cursor=encodeURIComponent(first.pagination.next_cursor);
  const next=await demo.request(`/api/v1/handoffs?view=incoming&limit=1&cursor=${cursor}`);
  assert.deepEqual(next.data.map(row=>row.id),['second']);assert.equal(next.pagination.next_cursor,null);
  await assert.rejects(demo.request(`/api/v1/handoffs?view=outgoing&limit=1&cursor=${cursor}`),error=>error.status===400);
});
test('training queue rejects an unknown view instead of silently returning all records',async()=>{
  await assert.rejects(fixture().request('/api/v1/handoffs?view=unknown'),error=>error.status===400);
});
