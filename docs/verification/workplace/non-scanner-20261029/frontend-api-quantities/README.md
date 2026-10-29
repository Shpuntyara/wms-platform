# Ilości z API: niezależne testy krytyczne

Wykryto i odtworzono błędy w prawdziwym kodzie interfejsu. PostgreSQL zwraca ilości z sześcioma miejscami po przecinku, np. `2.000000`. Pierwsza wersja wyboru bez skanera odrzucała takie poprawne wartości dla sztuk i innych jednostek z mniejszą precyzją. Nie rozpoznawała również zajętego miejsca dla numeru seryjnego. Ten sam wcześniejszy błąd parsowania występował w opcjonalnej ścieżce skanera przy już istniejącej rezerwacji WZ.

Zachowano rzeczywiste wyniki sprzed poprawek:

- Pierwsza wersja: 7 testów Node, 6 niepowodzeń, 1 sukces; zero pominięć.
- Poprawka pośrednia: 13 testów Node, 2 niepowodzenia dla pustych miejsc z numerem seryjnym, 11 sukcesów; zero pominięć. Blokowanie wszystkich miejsc nie stanowi poprawnego rozwiązania.
- Skaner: 2 testy przeglądarkowe, 1 niepowodzenie, 1 sukces. Zapisany kontekst strony pokazuje komunikat „Dopuszczalna liczba miejsc po przecinku: 0”. Interfejs nie dochodził do pola ilości. Jest to nieudana asercja widoczności wraz z dowodem komunikatu, a nie wynik testu mutacyjnego.

Po poprawkach: 13/13 testów Node i 2/2 testów przeglądarkowych, zero niepowodzeń i pominięć. Dwie właściwości generują razem 400 przypadków z ustalonymi ziarnami: dokładne ilości dla skal 0–6 oraz zakaz wyboru zajętego miejsca seryjnego. Oddzielne przykłady sprawdzają dozwolone puste miejsce, uszkodzoną wartość ilości, ułamki sztuk i własne rezerwacje dokumentu.

Wyniki i sumy źródeł zostały zapisane przy rzeczywistych uruchomieniach RED. Dołączone historyczne pliki źródłowe zrekonstruowano później, bez zmiany plików produkcyjnych, i porównano bajt w bajt z zapisanymi wcześniej SHA-256. Wszystkie 7 porównań jest zgodnych. Dotyczy to także zachowania CRLF starego `app.js`. Odtworzona pierwsza wersja została ponownie uruchomiona w osobnym katalogu: ponownie 6 niepowodzeń na 7 testów.

Polecenia z katalogu głównego repozytorium:

```powershell
node --test --test-reporter=tap web/tests/selection-api-quantity-critic.test.mjs
node --test --test-reporter=tap docs/verification/workplace/non-scanner-20261029/frontend-api-quantities/primary-red-reproducer/tests/selection-api-quantity-critic.test.mjs
```

Drugie polecenie celowo kończy się kodem 1, ponieważ uruchamia dokładną historyczną błędną wersję. Testy przeglądarkowe są stale włączone do głównej konfiguracji Playwright; po instalacji zależności i Chromium można uruchomić z `web/`:

```powershell
node node_modules/@playwright/test/cli.js test tests/api-quantity-browser-critic.spec.mjs
```

Granice dowodu: test przeglądarkowy używa kontrolowanego adaptera treningowego z reprezentacją ilości zgodną z rzeczywistym API, bez prawdziwego logowania OIDC i bez połączenia przeglądarki z PostgreSQL. Prawdziwy PostgreSQL, uprawnienia, tożsamość towaru oraz dokładne ilości w MM/WZ sprawdza osobny pakiet `../backend/`. Jego wyniki są testami charakteryzującymi istniejący backend, a nie dowodem RED. Weryfikacja całego CI na końcowej rewizji i próba na obu instalacjach pozostają osobnymi wymaganiami.

`verification.json` zawiera liczby przypadków oraz SHA-256 źródeł, historycznych migawek i wszystkich dołączonych raportów. Historyczne raporty skopiowano bez zmiany bajtów.
