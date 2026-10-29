# Przyjęcie i przesunięcie towaru

## Numery dokumentów

Nie wpisuj numeru ręcznie. Po wybraniu **Zapisz dokument** WMS nada numer:

- **PZ/2026/000001** — przyjęcie towaru do magazynu.
- **MM/2026/000001** — przesunięcie towaru między adresami w magazynie, na przykład ze strefy przyjęć na regał.

Każdy magazyn ma osobną serię PZ i MM na dany rok. Rok wynika ze strefy czasowej magazynu. Numer pozostaje przy dokumencie, także po anulowaniu. Dotychczasowe dokumenty zachowują swoje numery.

## Co oznaczają statusy

| Status | Co zrobić |
| --- | --- |
| **Do przygotowania** | Biuro sprawdza towary i planowane ilości. Dokument nie jest jeszcze przekazany do magazynu. |
| **Do realizacji** | Magazynier może potwierdzać rzeczywiście przyjęty lub przesunięty towar. |
| **Zrealizowany** | Wszystkie ilości są potwierdzone, dokument jest zamknięty. |
| **Anulowany** | Dokument został wycofany z realizacji. |

Samo zapisanie dokumentu lub przekazanie go do realizacji nie zmienia stanów magazynowych. Stan zmienia się po potwierdzeniu operacji przez magazyniera.

## Przyjęcie towaru — PZ

1. W biurze wybierz **Nowe przyjęcie (PZ)**.
2. Wybierz właściciela towaru, towary i planowane ilości. Numer nada system.
3. Wybierz **Zapisz dokument**. Sprawdź zapisane pozycje i nadany numer.
4. Gdy dokument jest gotowy, wybierz **Przekaż do realizacji**.
5. Magazynier otwiera dokument i wybiera **Rozpocznij przyjmowanie**.
6. Zeskanuj lub wpisz kod towaru, adres strefy przyjęć i rzeczywistą ilość. Potwierdź przyjęcie towaru.
7. Po wykonaniu wszystkich pozycji wybierz **Zakończ przyjęcie (PZ)**.

## Odłożenie towaru na regał — MM

1. Biuro otwiera zrealizowane PZ i wybiera **Przygotuj przesunięcie (MM)**.
2. Sprawdź towary i ilości, wybierz **Zapisz dokument**, a następnie **Przekaż do realizacji**. MM otrzyma własny numer.
3. Magazynier otwiera MM i wybiera **Rozpocznij przesunięcie**.
4. Zeskanuj towar, adres źródłowy i docelowy. Podaj rzeczywistą ilość i wybierz **Przesuń towar**.
5. Po wykonaniu wszystkich pozycji wybierz **Zakończ przesunięcie (MM)**.
6. Otwórz **Stany** i sprawdź ilość pod adresem docelowym.

Na stanowisku laboratoryjnym kod towaru to `BOLT-M8`, adres przyjęć to `PRZYJECIE-01`, a adres regału to `A-01-02`.

## Brak odpowiedzi po zapisie

Jeżeli połączenie przerwie się podczas zapisu, wybierz **Sprawdź wynik**. WMS ponowi tę samą operację i odszuka jej wynik. Nie twórz od razu drugiego dokumentu i nie czyść danych przeglądarki, dopóki wynik nie jest potwierdzony.

Na wspólnym komputerze wybierz **Wyloguj**, zanim kolejna osoba rozpocznie pracę. Przełącznik **Biuro / Magazyn** zmienia ekran, a nie zalogowaną osobę.
