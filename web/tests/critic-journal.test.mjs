import test from 'node:test';
import assert from 'node:assert/strict';
import {CommandJournal} from '../src/journal.js';

function fixture(request) {
  const entries = new Map();
  const storage = {getItem: key => entries.get(key) ?? null, setItem: (key, value) => entries.set(key, value), removeItem: key => entries.delete(key)};
  return {storage, namespace: 'actor|org|warehouse', request, uuid: () => 'fixed-command'};
}

test('critic: a forbidden retry cannot disprove a previously unconfirmed commit', async () => {
  for (const status of [400, 403, 404, 409, 413, 415, 422]) {
    let calls = 0;
    const options = fixture(async () => {
      if (++calls === 1) throw new TypeError('reply lost after possible commit');
      throw Object.assign(new Error('authorization or ingress rejection on retry'), {status});
    });
    const journal = new CommandJournal(options);
    await assert.rejects(journal.submit('/api/v1/movements', {quantity: '1', expected_document_version: 2}));
    const captured = journal.pending();
    const reloaded = new CommandJournal(options);
    await assert.rejects(reloaded.retry());
    assert.deepEqual(reloaded.pending(), captured, `retry status ${status} destroyed original intent`);
    await assert.rejects(reloaded.submit('/api/v1/movements', {quantity: '1'}));
    assert.equal(calls, 2);
  }
});

test('critic: reading a pending intent cannot mutate the saved request', async () => {
  const options = fixture(async () => {throw new TypeError('offline');});
  const journal = new CommandJournal(options);
  await assert.rejects(journal.submit('/api/v1/documents', {number: 'IN-01', lines: [{quantity: '1'}]}));
  journal.pending().body.lines[0].quantity = '99';
  assert.equal(journal.pending().body.lines[0].quantity, '1');
});

test('critic: caller mutation while a request waits cannot alter the recorded write', async () => {
  let release;
  const body = {quantity: '1'};
  const options = fixture(async (path, sentBody) => {
    await new Promise(resolve => {release = resolve;});
    assert.equal(sentBody.quantity, '1');
    return {data: {ok: true}};
  });
  const journal = new CommandJournal(options);
  const sending = journal.submit('/api/v1/movements', body);
  body.quantity = '99';
  release();
  await sending;
});
