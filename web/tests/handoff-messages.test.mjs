import test from 'node:test';
import assert from 'node:assert/strict';
import {errorMessage} from '../src/messages.js';
for(const [code,expected] of [
  ['handoff_managed_stock','Materiał należy do przekazania między działami. Otwórz przekazanie, aby sprawdzić jego stan.'],
  ['handoff_managed_position','To miejsce należy do działu produkcyjnego. Użyj przekazania między działami.'],
  ['handoff_version_conflict','Przekazanie zmienił inny pracownik. Odśwież dane i sprawdź przyjęcie.'],
  ['handoff_already_accepted','Przekazanie zostało już przyjęte. Odśwież dane, aby zobaczyć potwierdzenie.'],
  ['handoff_unavailable','Przekazania między działami są chwilowo niedostępne. Odśwież dane i spróbuj ponownie.'],
  ['invalid_handoff_view','Nie udało się otworzyć wybranego widoku przekazań. Odśwież dane.'],
]) test(`handoff problem ${code} explains the next action in Polish`,()=>{
  assert.equal(errorMessage({status:code==='handoff_unavailable'?503:409,code,message:'Internal English SQL detail'}),expected);
});
test('unknown handoff commit always asks for journal replay before any other action',()=>{
  assert.equal(errorMessage({status:503,code:'handoff_unavailable'},true),'Nie udało się potwierdzić operacji. Wybierz „Sprawdź wynik”.');
});
