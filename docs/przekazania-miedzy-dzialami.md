# Przekazania materiałów między działami

W METALFORM działa trasa:

**Magazyn → Spawanie → Szlifowanie → Lakiernia → Montaż → Magazyn**.

Adresy A1–A4 i B1–B4 należą do magazynu. Każdy dział produkcyjny ma osobne miejsce. Końcowe przekazanie do magazynu pozwala później wydać materiał zwykłym WZ. Przekazanie zachowuje ten sam towar, właściciela, partię i numer seryjny. Nie drukuj nowej etykiety po zmianie działu.

## Wyślij materiał

1. Zaloguj się na swoje konto. W **Stanach** znajdź towar i jego rzeczywiste miejsce.
2. Wybierz **Przekaż do działu**. Lista pokazuje dział dostępny na skonfigurowanej trasie.
3. Wpisz **Ilość do przekazania** i wybierz **Wyślij do działu**.
4. Materiał otrzyma status **W przekazaniu**. Nie jest dostępny do ponownej wysyłki, wydania ani rezerwacji. W szczegółach widać trasę, ilość, wysyłającego i czas wysłania.

Nie wpisujesz nazwiska wysyłającego. System zapisuje pracownika przypisanego do zalogowanego konta. Wysyłać można z miejsc własnego działu; zwykły MM nie zastępuje przekazania między działami.

## Przyjmij materiał

1. Pracownik działu docelowego loguje się na własne konto.
2. W **Przekazaniach między działami** wybiera **Do przyjęcia**. Lista oczekujących jest pobierana z serwera; stare zakończone przekazania nie zasłaniają bieżącej kolejki.
3. Sprawdza towar, partię lub numer seryjny i całą przekazaną ilość.
4. Wybiera **Potwierdź przyjęcie**. Materiał pojawia się w miejscu działu docelowego. Historia zapisuje pracownika i czas przyjęcia.

Przyjęcie dotyczy całej wysłanej ilości. Nie potwierdzaj niezgodnej dostawy. Wysyłający nie może przyjąć własnego przekazania, również jeśli ma dostęp do obu działów. Skaner nie jest wymagany.

## Gdzie jest materiał i kto go przyjął

**Stany** pokazują bieżące ilości w magazynie i działach oraz materiał w przekazaniu. **Wysłane** i **Wszystkie** pozwalają przeglądać historię. Szczegóły zakończonej operacji mówią **Przyjęto w** — opisują potwierdzone zdarzenie; późniejsze przekazanie może już przenieść ten sam materiał dalej.

Po utracie odpowiedzi wybierz **Sprawdź wynik**. System ponawia dokładnie to samo polecenie, również po odświeżeniu strony. Nie twórz zastępczej wysyłki ani przyjęcia dla niepotwierdzonej operacji.

## Przykładowe konta osobowe do sprawdzenia trasy

| Konto | Pracownik przykładowy | Dział |
|---|---|---|
| `magazynier` | Konto magazyniera | Magazyn |
| `jan.kowalski` | Jan Kowalski | Spawanie |
| `anna.zielinska` | Anna Zielińska | Szlifowanie |
| `piotr.nowak` | Piotr Nowak | Lakiernia |
| `marek.wisniewski` | Marek Wiśniewski | Montaż |

Hasła są przekazywane wyłącznie w prywatnym pliku dostępu. Nowi operatorzy mają odczyt stanów i obsługę przekazań. Nie otrzymują ogólnych praw do wystawiania dokumentów lub księgowania ruchów. `biuro` widzi przekazania i historię; przyjęcie potwierdza właściwy pracownik działu.

To przykładowi pracownicy laboratoryjnego zakładu. W rzeczywistej pracy każdy pracownik używa własnej tożsamości. Nazwiska w historii są zachowywane z chwili operacji.

Przekazanie rejestruje przemieszczenie materiału. Nie zużywa go w produkcji, nie tworzy nowego SKU i nie potwierdza powstania nowego wyrobu. Instalacja PAPIERNIK zachowuje swój proces; nie dostaje działów metalowych.
