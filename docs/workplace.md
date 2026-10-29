# Stanowisko pracy WMS

Interfejs jest przeznaczony na polski rynek. Pod adresem `/app/` są dwa widoki tej samej instalacji: **Biuro** do przygotowania dokumentów oraz **Magazyn** do pracy przy komputerze ze skanerem lub klawiaturą.

Pierwszy obsługiwany proces: utworzenie przyjęcia → rozpoczęcie pracy → skan towaru i adresu przyjęcia → potwierdzenie rzeczywistej ilości → zakończenie przyjęcia → osobny dokument rozmieszczenia → skan adresów źródłowego i docelowego → odłożenie towaru. Plan dokumentu jest niezmienny po utworzeniu. Zakończenie wymaga wykonania wszystkich pozycji i braku rezerwacji.

Przyjęcie używa skonfigurowanych adresów `receiving`, a rozmieszczenie — docelowych adresów `storage`. Skaner powinien wysyłać kod i Enter. Obecny kod to SKU z kartoteki; nie ma jeszcze mapowania EAN ani aparatu. Adres i pozycja towaru muszą już istnieć. Gdy pod adresem jest kilka partii lub stanów, interfejs zatrzymuje operację i kieruje do kierownika; nie wybiera partii automatycznie.

## Uruchomienie

Pliki interfejsu są osadzone w binarnym pliku Go. Serwer produkcyjny nie potrzebuje Node.js ani pobierania zasobów z CDN. W istniejącej instalacji udostępnij `/app/` przez ten sam reverse proxy HTTPS co `/api/v1/`.

Ustaw `WMS_UI_CONFIG=/etc/wms/workspace.json`. Strukturę pliku pokazuje [przykład](../web/config.example.json). Kartoteki zawierają nazwy i istniejące identyfikatory organizacji, magazynów, właścicieli, towarów i adresów. Nie wpisuj do tego pliku haseł ani sekretów. Nazwy są zwracane dopiero po uwierzytelnieniu i filtrowane według rzeczywistych uprawnień pracownika. Wybrany magazyn dodatkowo ogranicza właścicieli dostępnych w formularzu. Ten pierwszy interfejs nie ma jeszcze edytora kartotek — administrator przygotowuje konfigurację.

W Keycloak utwórz publicznego klienta `wms-browser`, włącz Authorization Code oraz PKCE S256. Wyłącz client authentication, direct grants i service accounts. Ustaw dokładny redirect URI `https://<adres-instalacji>/app/`, dokładny Web Origin i mapper audience zgodny z `WMS_OIDC_AUDIENCE`. Issuer musi zgadzać się z backendem. Użytkowników powiąż z `platform.actors` przez issuer/subject i nadaj wymagane prawa organizacji, magazynu i właściciela. Interfejs nie przechowuje sekretu klienta ani tokenów w localStorage. [Dokumentacja adaptera Keycloak](https://www.keycloak.org/securing-apps/javascript-adapter).

W ustawieniach lokalizacji realm włącz język polski i ustaw go jako domyślny. Aplikacja wysyła `locale=pl`; strony logowania i wiadomości dostawcy tożsamości również powinny być po polsku.

Do procesu potrzebne są `document.read`, `document.create`, `document.transition`, `stock.read` i `movement.create` w odpowiednim zakresie. Przełącznik Biuro/Magazyn zmienia sposób pracy; nie nadaje uprawnień. API i SQL sprawdzają każdą operację niezależnie od widoczności przycisków.

Przy utracie odpowiedzi przeglądarka zachowuje oryginalne żądanie i identyfikator komendy w localStorage, oddzielnie dla pracownika i magazynu. **Sprawdź wynik** powtarza to samo żądanie. Nie czyść danych przeglądarki, dopóki wynik nie zostanie potwierdzony. Jeżeli po wysłaniu zmieniły się prawa pracownika, odrzucenie powtórzenia nie usuwa niepewnego wyniku. Kierownik musi uzgodnić stan operacji. Gdy zapis został potwierdzony, lecz odczyt dokumentu nie działa, UI prosi o **Odśwież** i blokuje kolejny zapis. Ilości są liczone na dokładnych jednostkach dziesiętnych, bez arytmetyki zmiennoprzecinkowej.

## Podgląd i sprawdzanie

`cd web && npm ci && npm start` udostępnia lokalny podgląd na `127.0.0.1:18744/app/?demo=1`. Tryb szkoleniowy jest wyraźnie oznaczony i działa wyłącznie na fikcyjnych danych w przeglądarce. Nigdy nie przełącza automatycznie na dane szkoleniowe po błędzie instalacji.

`npm test` sprawdza dokładne ilości, właściwości generowanych danych i ponawianie komend. `npm run test:browser` sprawdza polskie ekrany, tworzenie dokumentu, skanowanie, zwykłe pisanie w wyszukiwarce i układ ekranu. Potrzebna jest instalacja `npx playwright install chromium --only-shell`.

Test `TestBrowserReceiptAndPutawayAgainstPostgres` uruchamia dostarczone pliki UI oraz prawdziwy HTTP API i PostgreSQL. Zastępuje wyłącznie zewnętrzny adapter tożsamości testową tożsamością. Odrzuca rzeczywistą odpowiedź po COMMIT, odświeża stronę, sprawdza identyczne powtórzenie i niezależnie odczytuje końcowe stany SQL. Nie dowodzi prawdziwego logowania pracownika przez Keycloak w danej instalacji ani pracy na rzeczywistym skanerze; te próby pozostają częścią odbioru stanowiska u klienta.
