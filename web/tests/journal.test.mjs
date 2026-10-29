import {test} from 'node:test';
import assert from 'node:assert/strict';
import {CommandJournal} from '../src/journal.js';

function fixture(request) {
  const entries = new Map();
  const storage = {getItem: k => entries.get(k) ?? null, setItem: (k,v) => entries.set(k,v), removeItem: k => entries.delete(k)};
  let ids = 0;
  const options = {storage, namespace: 'actor|org|warehouse', request, uuid: () => `command-${++ids}`};
  return {options, journal: new CommandJournal(options)};
}

test('a lost reply survives reload and retries the exact original request', async () => {
  const sent = [];
  const {journal, options} = fixture(async (path, body, id) => {
    sent.push({path, body: structuredClone(body), id});
    if (sent.length === 1) throw new TypeError('network lost after commit');
    return {data: {document_id: 'confirmed'}};
  });
  const body = {quantity: '0.000001', expected_document_version: 7};
  await assert.rejects(journal.submit('/api/v1/movements', body));
  body.quantity = '99';
  const reloaded = new CommandJournal(options);
  assert.equal(reloaded.pending().body.quantity, '0.000001');
  assert.deepEqual(await reloaded.retry(), {data: {document_id: 'confirmed'}});
  assert.deepEqual(sent[1], sent[0]);
  assert.equal(reloaded.pending(), null);
});

test('a second command and double click cannot replace an unresolved intent', async () => {
  let finish;
  let calls = 0;
  const {journal} = fixture(() => { calls++; return new Promise(resolve => {finish = resolve;}); });
  const first = journal.submit('/api/v1/documents', {number: 'IN-001'});
  await assert.rejects(journal.submit('/api/v1/documents', {number: 'IN-002'}));
  assert.equal(calls, 1);
  finish({data: {state: 'draft'}});
  await first;
  assert.equal(journal.pending(), null);
});

test('failure to persist stops a write before it reaches the server', async () => {
  let calls = 0;
  const {options} = fixture(async () => {calls++;});
  options.storage.setItem = () => { throw new Error('storage denied'); };
  const journal = new CommandJournal(options);
  await assert.rejects(journal.submit('/api/v1/documents', {}), /zapisać/);
  assert.equal(calls, 0);
});

test('a definitive version conflict clears the intent, a transport/server failure does not', async () => {
  for (const status of [400, 403, 404, 409]) {
    const {journal} = fixture(async () => {throw Object.assign(new Error('rejected'), {status});});
    await assert.rejects(journal.submit('/api/v1/documents/x/transitions', {state: 'active'}));
    assert.equal(journal.pending(), null);
  }
  for (const status of [401, 429, 500, 502, 503, 504]) {
    const {journal} = fixture(async () => {throw Object.assign(new Error('uncertain'), {status});});
    await assert.rejects(journal.submit('/api/v1/movements', {quantity: '1'}));
    assert.notEqual(journal.pending(), null);
    assert.equal(journal.pending().body.quantity, '1');
  }
});

test('recovery is scoped to employee, organization and warehouse', async () => {
  const {journal, options} = fixture(async () => {throw new TypeError('offline');});
  await assert.rejects(journal.submit('/api/v1/movements', {quantity: '2'}));
  assert.equal(new CommandJournal({...options, namespace: 'other|org|warehouse'}).pending(), null);
  assert.equal(new CommandJournal(options).pending().body.quantity, '2');
});
