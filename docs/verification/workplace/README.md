# Weryfikacja stanowiska pracy

Zachowano rzeczywiste wyjścia RED sprzed zmian implementacji, hasze testów/źródeł oraz późniejsze GREEN. `receipts.json` wiąże zapisane pliki z SHA-256. Hasze dotyczą surowych bajtów lokalnego środowiska Windows; normalizacja końców linii w Git może zmienić reprezentację. Historyczny RED nie jest przedstawiany jako wynik obecnej implementacji.

W pierwszym cyklu nowe moduły JavaScript miały wykonalne, celowo niepoprawne szkielety. Testy sukcesu nie przechodziły przez asercje zachowania; nie zaliczano brakującego importu jako RED. Późniejsze defekty krytyków odtworzono w istniejącej implementacji: nieaktualna pozycja dokumentu, utrata niepewnej komendy, podwójny Enter, niewłaściwa strefa, odczyt po potwierdzonym zapisie, kursor wyszukiwania i zastępowanie brakującego SKU.

Nieudane próby infrastrukturalne, w tym brak pobranej przeglądarki i brak modułu zewnętrznego adaptera, nie są dowodem TDD ani zabicia mutanta. W CI klasyfikatory odrzucają brak testów, pominięcia, błędy kompilacji/importu i timeouty.

Polecenia odtwarzania i granice pierwszego procesu opisuje [stanowisko pracy](../../workplace.md). Pełny CI trzeba sprawdzać dla konkretnego końcowego commita; lokalne GREEN nie zastępuje wyniku pipeline.
