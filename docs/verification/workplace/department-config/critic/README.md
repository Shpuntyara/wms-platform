# Niezależny krytyk konfiguratora działów

Przed poprawkami: 17 testów, 10 rzeczywistych niepowodzeń i 7 sukcesów, zero błędów przygotowania i pominięć. Po poprawkach autora: niezależnie 17/17 PASS; następnie cały zestaw konfiguratora 35/35 PASS na PostgreSQL z migracją 0014. Sumy źródeł nie zmieniły się podczas tego końcowego przebiegu. Krytyk zmieniał wyłącznie własny plik testów; poprawki modułu wykonał jego autor po zapisaniu RED.

Wykryte i odtworzone problemy:

- Trzy testy prawdziwego PostgreSQL: powtórna konfiguracja akceptowała dodatkowe `movement.create`, `document.read` albo `handoff.send` dla wszystkich właścicieli u nowego pracownika. Naprawa odrzuca taki dryf uprawnień, bez usuwania istniejących grantów.
- Cztery testy granicy dostawcy tożsamości: odczyt po udanym utworzeniu konta nie sprawdzał ponownie instalacji, nazwy użytkownika, aktywności i nazwiska, zanim nadawał prawa SQL. Teraz działa ta sama pełna weryfikacja przed utworzeniem i po nim.
- Uszkodzony prywatny zapis mógł zawierać identyczne hasła dwóch pracowników.
- Identyczny UUID starego konta biura i magazyniera mógł nadać biuru możliwość wysyłania materiału.
- Dowiązanie do pliku konfiguracji miejsc nie było odrzucane przed efektami w systemie tożsamości i bazie.

Siedem przypadków już przechodziło przed zmianą; są to testy charakteryzujące, nie dowód RED. Obejmują utratę odpowiedzi po utworzeniu konta w Keycloak, utratę późniejszego odczytu potwierdzającego, wznowienie bez drugiego POST i resetu hasła, konflikt ostatniego pracownika przed pierwszym POST, zduplikowany wynik wyszukiwania, ochronę atrybutu własności, prywatną ścieżkę oraz wznowienie po awarii konfiguracji bazy bez zmiany dotychczasowych danych dostępowych.

Zakres: wszystkie konta Keycloak są kontrolowanymi obiektami testowymi; nie wykonano wywołań zewnętrznego dostawcy ani zmian na VM. Pięć przypadków całego zestawu korzysta z prawdziwego PostgreSQL i osobnych UUID testowych, z dodatnim stanem i dokumentami zachowanymi przed/po konfiguracji. Żadne prawdziwe hasło nie trafia do raportu.

Na lokalnym Windows tworzenie dowiązań systemowych wymaga niedostępnego uprawnienia. Dwa przypadki dowiązań używają wtedy jawnego modelu granicy `lstat`/odczytu; na Linux CI test tworzy prawdziwe dowiązanie. Pierwszy lokalny przebieg z dwoma błędami Win1314 odrzucono jako błąd przygotowania — nie jest dowodem RED. Opublikowany `actual-red-v2.txt` nie zawiera błędów przygotowania. Nie jest to dowód działania uprawnień POSIX na Windows ani końcowego wdrożenia z prawdziwym SSO.

Historyczne źródła mają przyrostek `.txt` i sumy z chwili RED. Końcowy manifest rozróżnia testy niezależnego krytyka i cały zestaw; pełne CI i obie instalacje pozostają oddzielnymi wymaganiami wydania.
