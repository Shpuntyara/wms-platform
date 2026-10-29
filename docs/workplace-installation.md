# Stanowisko na dwóch niezależnych maszynach

Każda maszyna ma własną instalację WMS, bazę, realm, klucze i hasła pracowników. To wdrożenie laboratoryjne korzysta z prawdziwego PostgreSQL i rzeczywistego logowania Keycloak. Dane towarowe są przykładowe; nie jest to tryb `demo=1` przeglądarki.

[Instrukcja dla biura i magazyniera](praca-z-dokumentami.md) opisuje przyjęcie PZ i przesunięcie MM. Numery nadaje baza przy zapisie, w osobnych seriach dla magazynu, rodzaju i roku; formularze nie wymagają ręcznego numeru. Powtórzenie tej samej komendy zachowuje numer. Aktualizacja dodaje migrację `0010_document_numbering.sql` i nie zmienia numerów istniejących dokumentów.

Najpierw wykonaj zweryfikowaną aktualizację według [instrukcji instalatora](deployment-installer.md). Przed skonfigurowaniem kont pracowników odzyskanie zmienionego adresu DHCP wymaga jawnego `--previous-address <poprzedni-IP>`. Instalator zatrzymuje writerów, wykonuje pełne kopie WMS i Keycloak z odtworzeniem w osobnych bazach, zachowuje klucze i weryfikuje nowy certyfikat oraz issuer.

**Automatyczna zmiana adresu już skonfigurowanego stanowiska pracowników nie jest obsługiwana.** Instalator odmawia jej przed rozpakowaniem wydania, zatrzymaniem usług i zmianami danych; nie przepina częściowo issuer ani ustawień logowania. Użyj stałego adresu lub rezerwacji DHCP. Późniejsza zmiana wymaga uzgodnionej z operatorem konfiguracji SSO, adresów klienta Keycloak oraz plików stanowiska. Zwykła aktualizacja na dotychczasowym adresie pozostaje obsługiwana.

Na właściwej maszynie, jako root:

```text
python3 /opt/wms/current/deploy/linux/workplace_bootstrap.py apply --lab --expected-instance-id <UUID-instalacji-WMS>
```

Narzędzie wymaga jawnego potwierdzenia laboratoryjnego zakresu. Tworzy `wms-browser` z Authorization Code i wymaganym PKCE S256, dokładnymi adresami powrotu oraz wyłączonym implicit flow, direct grants i service accounts. Realm i strony logowania używają języka polskiego. Tymczasowe konto administracyjne powstaje oficjalnym poleceniem Keycloak, przy zatrzymanym serwerze, i jest usuwane po wykonaniu konfiguracji. Błąd czyszczenia blokuje tworzenie następnego administratora; służy do tego osobne działanie `cleanup`.

Powstają użytkownicy:

| Użytkownik | Zakres pracy |
| --- | --- |
| `biuro` | Tworzenie i otwieranie dokumentów, podgląd stanów; bez księgowania ruchów |
| `magazynier` | Podgląd dokumentów i stanów, przyjmowanie i rozmieszczanie towaru, zamykanie dokumentów; bez tworzenia dokumentów |

Prawo `document.transition` pozwala również rozpocząć przygotowany dokument; obecny kontrakt nie rozdziela rozpoczęcia i zamknięcia na osobne uprawnienia. Przełącznik Biuro/Magazyn zmienia ekran, a nie zalogowaną osobę. Aby zmienić użytkownika, wybierz **Wyloguj**.

W każdym magazynie są dwa towary: `BOLT-M8` (sztuki) i `CABLE-25` (metry, dokładność do trzech miejsc). Adres przyjęcia to `PRZYJECIE-01`, adres składowania — `A-01-02`. Powtórna konfiguracja nie zeruje stanów, nie resetuje haseł i nie przyjmuje obcych kont o tych samych nazwach.

Hasła są w root-only `/etc/wms/workplace-users.json`. Publiczne etykiety znajdują się w `/etc/wms-workplace/workspace.json`, czytelnym dla usługi WMS, a w API są zwracane dopiero po sprawdzeniu uprawnień. Kolejna aktualizacja instalatora wiąże plik przez `WMS_UI_CONFIG`; trzeba ją wykonać także po pierwszym utworzeniu kartoteki. Kopia konfiguracji obejmuje również ten zewnętrzny plik. Nie publikuj haseł ani profili przeglądarki w Git lub w materiale portfolio.

Na komputerze operatora test korzysta z osobnych profili Firefox z bazą NSS zawierającą CA tych dwóch maszyn. Nie zmienia magazynów certyfikatów Windows i nie wyłącza weryfikacji HTTPS. Adresy maszyn i kopię dostępu operator przechowuje poza repozytorium.

Automatyczny scenariusz przechodzi prawdziwe logowanie obu pracowników, przyjęcie, osobne rozmieszczenie i zamknięcie dokumentów. Sprawdza rzeczywiste 403 dla zakazanych czynności oraz 401 dla tokena z drugiej instalacji. Test zewnętrznych maszyn uruchamia się osobno przez `web/playwright.installation.config.mjs`; nie udaje uruchomienia prywatnych VM z runnera GitHub. Pełny CI nadal obejmuje testy jednostkowe, SQL, prawdziwy browser/PostgreSQL, property tests i mutation tests.
