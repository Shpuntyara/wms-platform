import test from 'node:test';
import assert from 'node:assert/strict';
import fc from 'fast-check';
import {parseQuantity, remaining} from '../src/domain.js';

test('property: exact positive decimals round-trip and subtraction cancels for 1500 generated cases', () => {
  const micro = fc.bigInt({min: 1n, max: 999999999999999999999999n});
  const encode = n => `${n / 1000000n}.${String(n % 1000000n).padStart(6, '0')}`;
  fc.assert(fc.property(micro, n => {
    const canonical = parseQuantity(encode(n));
    assert.equal(remaining(canonical, encode(n)), '0');
    assert.equal(parseQuantity(canonical), canonical);
    assert.equal(parseQuantity(encode(n).replace('.', ',')), canonical);
  }), {numRuns: 1500, seed: 20261029});
});
