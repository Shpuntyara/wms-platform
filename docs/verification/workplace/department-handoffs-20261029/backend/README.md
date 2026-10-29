# Przekazania między działami: dowody backendu

Przekazanie jest dwuetapowe: materiał opuszcza dostępny stan nadawcy i trafia do własnego miejsca „w drodze”. Inny uprawniony pracownik działu docelowego potwierdza całą wysłaną ilość. Oba kroki zapisują zwykły MM z dotychczasowej serii numeracji. UUID towaru, SKU, właściciel, partia i numer seryjny pozostają bez zmian. Nazwy nadawcy i odbiorcy pochodzą z zarządzanej kartoteki pracowników i są zachowane w historii.

Pierwsze testy uruchomiono przed implementacją: 19 niepowodzeń API (nieistniejące endpointy) i 3 jawne niepowodzenia dostępności funkcji na prawdziwym PostgreSQL. Ten pierwszy wynik SQL nie jest dowodem błędu istniejącego księgowania ani zabicia mutanta. Zachowano surowe wyniki oraz dokładne źródła i sumy SHA-256.

Późniejsze rzeczywiste regresje zapisano przed poprawkami: brak wszystkich adresów źródłowych w kontekście działu; brak kolejki zależnej od bieżącego odbiorcy; możliwość użycia kursora w innym widoku; brak nowego profilu w aktualizatorze; ponowne nałożenie profilu RPC kończące się błędem; brak grantów w oficjalnym przygotowaniu bazy testowej. Bramka CI otrzymała również test, który wymaga wszystkich czterech nowych grup testów podpisanego profilu i odrzuca ich brak.

Końcowy lokalny przebieg na zamrożonej migracji 0014: API 22 PASS; PostgreSQL 25 PASS (w tym test prawdziwej przeglądarki), zero niepowodzeń i pominięć, race detector włączony. Oddzielny profil podpisanych wywołań: 29 kontroli SQL oraz wszystkie testy Go tego profilu bez niepowodzeń; nowe testy przekazań to 4 grupy / 33 PASS. Sprawdzono sześć rzeczywistych scenariuszy utraty potwierdzenia COMMIT dla zwykłego adaptera oraz sześć dla podpisanego adaptera: niewysłany COMMIT i dwa rodzaje utraconej odpowiedzi po trwałym zapisie. Każdy odzyskuje wynik przez ten sam identyfikator i treść komendy.

Test przeglądarkowy korzysta z dostarczanych plików aplikacji, prawdziwego HTTP i PostgreSQL. Nadawca i odbiorca mają różne tożsamości oraz tylko uprawnienia do stanów i przekazań. Zastąpiono wyłącznie zewnętrzny adapter OIDC syntetycznymi tożsamościami. Rzeczywiste odpowiedzi po COMMIT obu kroków zostały zgubione, a powtórzenia po przeładowaniu nie utworzyły dodatkowych stanów ani dokumentów. Zewnętrzne SSO oraz obie instalacje VM wymagają osobnych prób wdrożeniowych.

Osobny niezależny krytyk publikuje dowody mutacyjne, własności i rzeczywistych planów na 10 000 / 100 000 relacyjnie poprawnych rekordów. Takie rekordy do odczytu nie oznaczają wykonania 100 000 operacyjnych przekazań. W tym pakiecie nie przypisujemy sobie jego wyników ani pełnego końcowego CI.

`verification.json` opisuje zakres, źródła i sumy dowodów. W dwóch raportach JSON mapa sum źródeł została bezstratnie zapisana jako tablica par `path` / `sha256`, aby skaner sekretów nie mylił sumy pliku z kluczem API. Wyniki i wartości sum są niezmienione; zachowano sumę oryginalnego raportu, a jego dokładne bajty pozostają w `work/`. Logi testów i migawki źródeł zachowano bajt w bajt.

`migration-receipts.json` zawiera 14 potwierdzeń oficjalnego migratora; zweryfikowano zarówno źródło, jak i wykonany przepis każdej migracji. Opublikowane migracje 0001–0013 nie zostały zmienione. Historyczne pliki źródłowe mają dodatkowy przyrostek `.txt`, aby błędne wersje Go nie stały się wykonywalnymi pakietami w CI; ich bajty zachowano.

## Uzupełnienie po modernizacji składni testu Go

Oryginalny raport oraz sumy źródeł powyżej dotyczą bajtów sprzed końcowego `go fix`. Później autor wydania zastosował wyłącznie modernizację literału osadzonej struktury w `internal/api/handoffs_test.go`; kod produkcyjny API i migracja 0014 nie uległy przez tę zmianę modyfikacji. Zachowano oryginalne sumy zamiast przypisywania wcześniejszego wykonania nowym bajtom. Oddzielny ponowny przebieg całego pakietu API zakończył się sukcesem — `amendments/after-API.txt`. Końcowe pełne CI musi zostać uruchomione na nowej, zamrożonej rewizji.
