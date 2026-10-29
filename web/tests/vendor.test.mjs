import test from 'node:test';
import assert from 'node:assert/strict';
import {existsSync, readFileSync} from 'node:fs';
test('vendored Keycloak includes its runtime imports and exact pinned bytes', () => {
  for (const name of ['keycloak.js','deprecations.js']) {
    const target = new URL('../vendor/'+name,import.meta.url);
    assert.ok(existsSync(target), `missing runtime module ${name}`);
    assert.deepEqual(readFileSync(target),readFileSync(new URL('../node_modules/keycloak-js/lib/'+name,import.meta.url)));
  }
});
