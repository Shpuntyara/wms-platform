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

- **Wydaj towar**: wybierz ilość i wpisz odbiorcę — firmę, dział albo osobę. Zapisz WZ, przekaż do realizacji, a następnie otwórz wydanie w trybie magazynowym. Wybierz **Towar do realizacji** i **Miejsce źródłowe** z list, wpisz **Ilość rzeczywista** i potwierdź wydanie. Po wykonaniu pozycji zakończ dokument. System kontroluje rezerwację i dostępny stan.
- **Przenieś towar**: zapisz MM i przekaż do realizacji. Wybierz towar, miejsce źródłowe i miejsce docelowe z list, wpisz ilość i potwierdź przesunięcie. MM zmienia miejsce towaru, zachowując jego łączną ilość, właściciela, partię i numer seryjny.
- **Historia**: pokazuje wykonane dokumenty, odbiorcę WZ i powiązany numer zamówienia.

Numery PZ, MM i WZ nadaje system po zapisaniu. Przy utracie odpowiedzi użyj **Sprawdź wynik** — polecenie zachowuje ten sam identyfikator. Nie twórz nowego dokumentu w zastępstwie niepotwierdzonej operacji.

## Praca bez kodów kreskowych

Domyślnie używaj **Wybierz z listy**. Towar rozpoznasz po nazwie, a miejsce po oznaczeniu i opisie. Nie musisz znać ani wpisywać kodu towaru lub adresu. Przy PZ wybierz **Miejsce przyjęcia**. Przy MM wybierz **Miejsce źródłowe** i **Miejsce docelowe**. Listy rozróżniają stany i partie; nie łączą ich w jedną pozycję tylko dlatego, że mają tę samą nazwę.

Przycisk **Skaner / wpisz kod** udostępnia dodatkowy sposób pracy dla stanowisk ze skanerem. Skanowanie nie jest warunkiem przyjęcia, wydania ani przesunięcia.

Artykuł zachowuje ten sam kod podczas kolejnych przesunięć. Przekazanie go ze składu na stanowisko spawania, a potem lakierowania nie wymaga nowej etykiety. Każdy taki obszar trzeba wcześniej skonfigurować jako osobne miejsce magazynowe. MM rejestruje zmianę miejsca; nie potwierdza wykonania spawania ani lakierowania. WZ zapisuje wydanie odbiorcy i zmniejsza stan magazynowy.

Nowy wyrób, np. szafa złożona z paneli, śrub i zamków, jest innym towarem niż jego materiały. Zwykłego przekazania tych samych materiałów między miejscami nie należy zapisywać jako tworzenia nowego wyrobu.

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
