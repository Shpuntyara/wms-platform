import test from 'node:test';
import assert from 'node:assert/strict';
import {readFileSync, readdirSync} from 'node:fs';
test('Polish is the product language, including errors and training data', () => {
  const html = readFileSync(new URL('../index.html',import.meta.url),'utf8');
  assert.match(html, /<html lang="pl">/);
  assert.match(readFileSync(new URL('../src/app.js',import.meta.url),'utf8'), /locale:'pl'/);
  for (const name of readdirSync(new URL('../src/',import.meta.url)).filter(name => name.endsWith('.js'))) {
    assert.doesNotMatch(readFileSync(new URL('../src/'+name,import.meta.url),'utf8'), /[\p{Script=Cyrillic}]/u, name);
  }
});
