import test from 'node:test';
import assert from 'node:assert/strict';
import {parseQuantity, remaining, matchScan, movementIntent, completed, formatQuantity} from '../src/domain.js';

const uuid = (n) => `11111111-1111-4111-8111-${String(n).padStart(12, '0')}`;
const scope = {organization_id: uuid(1), warehouse_id: uuid(2)};
const item = {id: uuid(3), sku: '000421', name: 'Крепёж', quantity_scale: 3, unit: 'kg'};
const line = {id: uuid(4), item_id: item.id, requested_qty: '10.000000', fulfilled_qty: '0.000000', reserved_qty: '0.000000'};
const document = {id: uuid(5), ...scope, owner_id: uuid(6), kind: 'receipt', state: 'active', version: 2, lines: [line]};
const targetBalance = {id: uuid(7), ...scope, owner_id: uuid(6), item_id: item.id, position_id: uuid(8), stock_state: 'available', lot_id: null, serial_id: null, on_hand: '0', reserved_qty: '0', available_qty: '0'};
const sourceBalance = {...targetBalance, id: uuid(9), position_id: uuid(10), on_hand: '8', available_qty: '5', reserved_qty: '3'};
const receipt = (changes = {}) => ({document, line, item, targetBalance, quantity: '1.250', scope, kind: 'receipt', ...changes});
const transfer = (changes = {}) => receipt({document: {...document, kind: 'transfer'}, sourceBalance, kind: 'transfer', ...changes});

test('critic: Russian scanner and keyboard input normalize without rounding', () => {
  assert.equal(parseQuantity(' 1,250 ', 3), '1.25');
  assert.equal(parseQuantity('999999999999999999.999999', 6), '999999999999999999.999999');
  assert.equal(parseQuantity('0.000001', 6), '0.000001');
});

test('critic: quantity rejects coercion, exponents, signs, zeros and excessive precision', () => {
  for (const value of [0.1, 1, null, undefined, {}, [], '', ' ', '0', '0,000', '-1', '+1', '1e2', 'NaN', 'Infinity', '0x10', '1,2.3', '1.0000001', '1000000000000000000', '01', '.5']) {
    assert.throws(() => parseQuantity(value, 6), undefined, String(value));
  }
  assert.throws(() => parseQuantity('1.001', 2));
  assert.throws(() => parseQuantity('1', -1));
  assert.throws(() => parseQuantity('1', 7));
});

test('critic: remaining keeps micro-units above Number precision', () => {
  assert.equal(remaining('100000000000000001.000001', '100000000000000000.999999'), '0.000002');
  assert.equal(remaining('0.3', '0.1'), '0.2');
  assert.equal(remaining('12.000000', '12'), '0');
  assert.throws(() => remaining('1', '2'));
});

test('critic: scan preserves significant zeros and refuses ambiguous or wrong codes', () => {
  assert.deepEqual(matchScan('000421\r\n', [item]), item);
  assert.throws(() => matchScan('421', [item]));
  assert.throws(() => matchScan('', [item]));
  assert.throws(() => matchScan('000421', [item, {...item, id: 'item-b'}]));
  const locations = [{id: 'a', code: 'A-01-02'}];
  assert.equal(matchScan('A-01-02', locations, 'code').id, 'a');
  assert.throws(() => matchScan('a-01-02', locations, 'code'));
});

test('critic: receiving intent preserves exact quantity and captured version', () => {
  assert.deepEqual(movementIntent(receipt()), {
    ...scope, document_line_id: line.id, expected_document_version: 2,
    kind: 'receipt', target_balance_id: targetBalance.id, quantity: '1.25',
  });
});

test('critic: zero available target is valid for receiving; missing target is not', () => {
  assert.equal(movementIntent(receipt()).target_balance_id, targetBalance.id);
  assert.throws(() => movementIntent(receipt({targetBalance: null})));
});

test('critic: receiving refuses over-fulfilment and unsupported item precision', () => {
  assert.throws(() => movementIntent(receipt({quantity: '10.001'})));
  assert.throws(() => movementIntent(receipt({quantity: '1.0001'})));
  assert.throws(() => movementIntent(receipt({line: {...line, fulfilled_qty: '10'}})));
});

test('critic: line must belong to captured document and item', () => {
  assert.throws(() => movementIntent(receipt({line: {...line, id: 'other-line'}})));
  assert.throws(() => movementIntent(receipt({line: {...line, item_id: 'other-item'}})));
  assert.throws(() => movementIntent(receipt({item: {...item, id: 'other-item'}})));
});

test('critic: a stale line object cannot borrow a freshly refreshed document version', () => {
  const refreshedLine = {...line, fulfilled_qty: '9.000000'};
  const refreshed = {...document, version: 3, lines: [refreshedLine]};
  assert.throws(() => movementIntent(receipt({document: refreshed, line, quantity: '2'})));
});

test('critic: intent refuses inactive documents and document-kind mismatch', () => {
  for (const state of ['draft', 'closed', 'cancelled']) {
    assert.throws(() => movementIntent(receipt({document: {...document, state}})));
  }
  assert.throws(() => movementIntent(receipt({document: {...document, kind: 'shipment'}})));
  assert.throws(() => movementIntent(receipt({kind: 'issue'})));
});

test('critic: version cannot silently round or accept invalid optimistic lock', () => {
  for (const version of [0, -1, NaN, 1.5, Number.MAX_SAFE_INTEGER + 1, '2']) {
    assert.throws(() => movementIntent(receipt({document: {...document, version}})));
  }
});

test('critic: receiving cannot cross organization, warehouse or owner', () => {
  for (const field of ['organization_id', 'warehouse_id', 'owner_id']) {
    assert.throws(() => movementIntent(receipt({targetBalance: {...targetBalance, [field]: 'foreign'}})), undefined, field);
  }
  for (const field of ['organization_id', 'warehouse_id']) {
    assert.throws(() => movementIntent(receipt({scope: {...scope, [field]: 'foreign'}})), undefined, field);
  }
  assert.throws(() => movementIntent(receipt({targetBalance: {...targetBalance, item_id: 'foreign'}})));
});

test('critic: putaway maps both balances and exact document line', () => {
  assert.deepEqual(movementIntent(transfer()), {
    ...scope, document_line_id: line.id, expected_document_version: 2,
    kind: 'transfer', source_balance_id: sourceBalance.id,
    target_balance_id: targetBalance.id, quantity: '1.25',
  });
});

test('critic: putaway uses available stock, never physical on-hand with reservations', () => {
  assert.equal(movementIntent(transfer({quantity: '5'})).quantity, '5');
  assert.throws(() => movementIntent(transfer({quantity: '5.001'})));
  assert.throws(() => movementIntent(transfer({sourceBalance: null})));
  assert.throws(() => movementIntent(transfer({sourceBalance: {...sourceBalance, available_qty: '0'}})));
});

test('critic: putaway refuses same position, foreign scope, owner and item', () => {
  assert.throws(() => movementIntent(transfer({targetBalance: {...targetBalance, position_id: sourceBalance.position_id}})));
  for (const field of ['organization_id', 'warehouse_id', 'owner_id', 'item_id']) {
    assert.throws(() => movementIntent(transfer({sourceBalance: {...sourceBalance, [field]: 'foreign'}})), undefined, field);
  }
});

test('critic: putaway cannot launder lot, serial or stock-state identity', () => {
  for (const field of ['lot_id', 'serial_id', 'stock_state']) {
    assert.throws(() => movementIntent(transfer({targetBalance: {...targetBalance, [field]: 'foreign'}})), undefined, field);
  }
});

test('critic: serial operations accept one unit and refuse a multi-unit scan count', () => {
  const serialTarget = {...targetBalance, serial_id: uuid(11)};
  const serialItem = {...item, quantity_scale: 0};
  assert.equal(movementIntent(receipt({item: serialItem, targetBalance: serialTarget, quantity: '1'})).quantity, '1');
  assert.throws(() => movementIntent(receipt({item: serialItem, targetBalance: serialTarget, quantity: '2'})));
});

test('critic: closing requires every line fulfilled and every reservation released', () => {
  const done = {...line, fulfilled_qty: '10'};
  assert.equal(completed({...document, lines: [done]}), true);
  assert.equal(completed({...document, lines: []}), false);
  assert.equal(completed({...document, lines: [done, line]}), false);
  assert.equal(completed({...document, lines: [{...done, reserved_qty: '0.000001'}]}), false);
  assert.equal(completed({...document, lines: [{...done, fulfilled_qty: '9.999999'}]}), false);
});

test('critic: displayed quantity retains every significant digit and Russian decimal separator', () => {
  assert.equal(formatQuantity('999999999999999999.999999'), '999999999999999999,999999');
  assert.equal(formatQuantity('12.250000'), '12,25');
});

test('critic property: independently computed micro-unit differences remain exact', () => {
  let seed = 937451;
  for (let n = 0; n < 300; n++) {
    seed = (Math.imul(seed, 1664525) + 1013904223) >>> 0;
    const fulfilled = 900719925474099100000000n + BigInt(seed);
    const delta = BigInt(seed % 999999 + 1);
    const format = (units) => `${units / 1000000n}.${String(units % 1000000n).padStart(6, '0')}`;
    const expected = format(delta).replace(/0+$/, '').replace(/\.$/, '');
    assert.equal(remaining(format(fulfilled + delta), format(fulfilled)), expected);
  }
});
