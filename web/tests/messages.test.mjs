import test from 'node:test';
import assert from 'node:assert/strict';
import {errorMessage} from '../src/messages.js';
test('unmapped API errors never expose English server detail to warehouse staff', () => {
  for (const status of [400,401,403,404,409,500,503]) {
    const result = errorMessage(Object.assign(new Error('The operation was not permitted in this scope'),{status,code:'unmapped'}));
    assert.ok(result.length > 10);
    assert.doesNotMatch(result,/The operation/);
    assert.doesNotMatch(result,/Sprawdź wynik/);
  }
});
test('a lost write gives recovery guidance, a failed read gives refresh guidance', () => {
  assert.match(errorMessage(new TypeError('network failed'),true),/Sprawdź wynik/);
  assert.match(errorMessage(new TypeError('network failed'),false),/Odśwież/);
});
test('specific Polish validation stays precise', () => {
  assert.equal(errorMessage(new Error('Ilość musi być większa od zera')),'Ilość musi być większa od zera');
  assert.match(errorMessage({code:'document_version_conflict',status:409}),/inny pracownik/);
});
