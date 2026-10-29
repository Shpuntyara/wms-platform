# Konfiguracja działów — dowody testów

- `first-red.txt`: 10 rzeczywistych niepowodzeń przed istnieniem modułu; zachowany test i jego hash. `first-green.txt`: 10 PASS. Dodatni test wystawcy tożsamości dodano przed GREEN, dlatego nie przedstawiamy tego jako identycznego zestawu RED/GREEN.
- `provisioning-red.txt`: 6 FAIL (5 brakujących operacji provisioningu i błędny pusty wystawca); zachowano ówczesny moduł, testy i hashe. `provisioning-green.txt`: późniejszy GREEN.
- `operator-least-privilege-red.txt`: faktyczny nadmiarowy grant `document.read` dla nowych operatorów; `operator-least-privilege-green.txt`: po usunięciu grantu. Zachowany stan przed zmianą.
- `native-config-first.txt` zawiera 2 błędy przygotowania fixture (brak `line_no`), nie produktowy RED. Po poprawieniu fixture `native-config-fixture-corrected.txt` potwierdza 2 PASS na PostgreSQL. To charakterystyka natywnej konfiguracji, nie dowód RED.
- `installer-critic-green.txt`: cały końcowy zestaw 35 PASS na kandydacie 05. Niezależny krytyk zachowuje własne 10 FAIL i późniejsze 17/35 PASS w sąsiednim katalogu `critic/`.

Konfiguracja nie księguje zapasu ani dokumentów. Czterech nowych operatorów dostaje dokładnie `stock.read`, `handoff.read`, `handoff.send`, `handoff.accept` dla określonego właściciela. Istniejące hasła nie są resetowane; konta obce, zmienione lub z nadmiarowymi uprawnieniami powodują przerwanie.

`verification.json` wiąże dokładne bajty paragonów z SHA-256. Pełne CI i prawdziwe wdrożenie na końcowym commicie są osobnymi, późniejszymi etapami.
