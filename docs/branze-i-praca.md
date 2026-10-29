# Dwa przykładowe zakłady

Każdy zakład działa w osobnej instalacji WMS i osobnej bazie PostgreSQL.
Towary, firmy i zamówienia są przykładowe. Operacje zapisują rzeczywiste dokumenty i ruchy magazynowe.

| Instalacja | Asortyment | Plan pracy |
|---|---|---|
| METALFORM | Śruby, nakrętki, podkładki, nity, formatki aluminiowe, stalowe panele, zamki i szafy metalowe | Produkcja 10 szaf: potrzebne materiały oraz spawanie, szlifowanie, lakierowanie i montaż |
| PAPIERNIK | Notesy 2027 w różnych kolorach i rozmiarach, pozostałe notesy 2026, wkłady, kalendarze i opakowania | Wydanie dla firmy, odnowienie notesu 2026 nowym wkładem albo komplet prezentowy |

A1–A4 oznaczają sekcję regału A i poziomy 1–4. B1–B4 to sekcja B i poziomy 1–4.
`PRZYJECIE-01` jest strefą przyjęć, a `WYDANIE-01` strefą wydań.

## Magazynier

Po zalogowaniu jako `magazynier` otwiera się **Stany**. Każdy wiersz pokazuje towar, adres, jednostkę, stan fizyczny, rezerwację i ilość dostępną.
Wyszukaj nazwę, kod, kolor, rozmiar albo adres, np. `A1`.
Opcja **Pokaż puste miejsca** pozwala sprawdzić również skonfigurowane adresy z zerowym stanem.

- **Wydaj towar**: wybierz ilość i wpisz odbiorcę — firmę, dział albo osobę. Zapisz WZ, przekaż do realizacji, a następnie otwórz wydanie w trybie magazynowym. Potwierdź kod towaru, adres źródłowy i rzeczywistą ilość. Po wykonaniu pozycji zakończ wydanie. System kontroluje rezerwację i dostępny stan.
- **Przenieś towar**: zapisz MM, przekaż do realizacji i potwierdź towar, adres źródłowy, adres docelowy oraz ilość. Przesunięcie zmienia miejsce towaru, zachowując jego łączną ilość.
- **Historia**: pokazuje wykonane dokumenty, odbiorcę WZ i powiązany numer zamówienia.

Numery PZ, MM i WZ nadaje system po zapisaniu. Przy utracie odpowiedzi użyj **Sprawdź wynik** — polecenie zachowuje ten sam identyfikator. Nie twórz nowego dokumentu w zastępstwie niepotwierdzonej operacji.

## Biuro

Po zalogowaniu jako `biuro` widać te same stany oraz **Zamówienia i plan pracy**.
Wybierz firmę, towary i ilości. Dla produkcji lub kompletacji wybierz zapisany skład produktu.
System obliczy potrzebne ilości, porówna je z dostępnym stanem i pokaże braki oraz kolejność prac.

Z zamówienia można przygotować **wydanie materiałów** do produkcji lub **wydanie towaru** dla klienta.
WZ zachowuje właściciela towaru i numer zamówienia; odbiorcę można doprecyzować przed zapisem.
Biuro planuje dokumenty, a magazynier potwierdza rzeczywiste ruchy.

Plan nie rezerwuje materiałów, nie potwierdza spawania ani montażu i nie zwiększa stanu wyrobów gotowych.
Stan zmienia dopiero potwierdzona operacja magazynowa. Etapy produkcji są planem pracy, a nie rejestracją wykonania produkcji.

## Wcześniejsze próby

Dotychczasowe dokumenty i ilości pozostają w bazie. Dwa znane towary z poprzednich prób są oznaczone jako **Wcześniejsze próby**.
Domyślny widok pokazuje asortyment roboczy. Przełącznik **Pokaż wcześniejsze dane testowe** ujawnia zachowane pozycje; nie usuwa ani nie zmienia stanu magazynowego.
