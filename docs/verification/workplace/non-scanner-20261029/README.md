# Przyjęcie, przesunięcie i wydanie bez skanera

Domyślny interfejs wybiera pozycję dokumentu oraz dokładny stan magazynowy z podpisanych list. MM zachowuje towar, SKU, właściciela, partię i numer seryjny. Skaner pozostaje osobnym, dobrowolnym sposobem wprowadzania danych.

## Dowody i granice

- [Pierwotny interfejs RED → GREEN](../../no-code-workplace/evidence.json): pięć scenariuszy najpierw nie przeszło na wersji `061a0a4964d07377f1383025e83a30ff38f35ce1`. Zachowano dokładny test, pierwszą implementację GREEN, skróty i surowe wyniki. Kolejne regresje obejmują utratę odpowiedzi, odświeżenie po potwierdzonym zapisie, klawiaturę i rzeczywisty format ilości PostgreSQL.
- [Backend](backend/README.md): charakterystyka istniejącej logiki na PostgreSQL z detektorem wyścigów. Nie jest przedstawiana jako RED nowego interfejsu. Model całkowitych mikrojednostek sprawdza kolejne MM i powtórzenia tych samych poleceń; katalog i cechy śledzenia pozostają niezmienione.
- [Niezależny krytyk wyboru i mutacje](../../no-code-workplace/selection-critic/manifest.json): oryginalny RED, poprawki warsztatu testowego oraz 23 poprawnie rozliczone mutacje. `scripts/verify_selection_mutations.py` wykonuje oba zestawy testów dla każdego mutanta. Osobne testy zbieracza zdarzeń zabraniają zaliczania błędów, timeoutów, brakujących przypadków i pominięć jako zabitych mutantów.
- [Rzeczywisty format ilości API](frontend-api-quantities/README.md): niezależne RED → GREEN, dokładne historyczne źródła oraz generowane przypadki dla wszystkich dokładności ilości.
- Niezależne testy `selection-critic.test.mjs`, `selection-api-quantity-critic.test.mjs`, `non-scanner-critic.spec.mjs` i `api-quantity-browser-critic.spec.mjs` sprawdzają zakres, partie, powtarzające się nazwy i pozycje, aktywne rezerwacje, stare dane i odtwarzanie poleceń.

Krytycy znaleźli rzeczywisty błąd: PostgreSQL zwraca ilości np. `24.000000`, również dla sztuk. Sprawdzanie ich jak ręcznie wpisanego ułamka ukrywało dostępny towar. Oddzielne testy RED wykryły też odrzucenie pustego celu z numerem seryjnym oraz ten sam problem formatu przy rezerwacji w opcjonalnym trybie skanera. Normalizacja usuwa tylko zera reprezentacji danych; nie dopuszcza ułamkowych sztuk.

`npm test` uruchamia również właściwości generowane, a `npm run test:browser` cały zestaw interfejsu. `TestBrowserReceiptAndPutawayAgainstPostgres` wykonuje operacje przez rzeczywiste HTTP i PostgreSQL; tylko zewnętrzny dostawca tożsamości jest zastąpiony. Test instalacyjny korzysta z prawdziwego SSO i zwykłej walidacji TLS w wydzielonym profilu NSS.

Żadna opublikowana migracja ani jej przypięty skrót nie została zmieniona. Końcowy CI musi obejmować dokładną wdrażaną rewizję, wszystkie wymagane kroki i pełne raporty mutacji. Osobne dowody wdrożenia porównują istniejące dokumenty, plany, katalog, klucze i stany z kopią sprzed aktualizacji; dane nie są ponownie inicjalizowane.
