# Charakterystyka backendu: wybór towaru bez skanera

Cztery testy główne i 21 podtestów przeszły na rzeczywistym PostgreSQL z detektorem wyścigów. To charakterystyka istniejącego backendu, a nie dowód RED dla nowego interfejsu.

Dwadzieścia stałych przesunięć MM obejmuje towary bez śledzenia, partie, numery seryjne i ich połączenie. Szesnaście generowanych historii wykonało 107 kolejnych MM. Każde przesunięcie powtórzono dwukrotnie po zamknięciu dokumentu. Niezależny model całkowitych mikrojednostek sprawdza ilości; porównanie katalogu pilnuje niezmienionych identyfikatorów i SKU.

Próby zamiany towaru, właściciela, partii lub numeru seryjnego oraz użycia źródła jako celu są odrzucane bez skutków. Wydanie WZ przez identyfikatory zachowuje te same uprawnienia, rezerwacje, kontrolę wersji i powtarzalność polecenia.

Plik `verification.json` rozdziela zakres dowodu, wyniki i skróty źródeł. `migration-receipts.json` zawiera sprawdzone skróty zastosowanych migracji. Test uwierzytelniania HTTP jest kontrolowany; ten zestaw nie zastępuje osobnych testów prawdziwego SSO ani końcowego CI.
