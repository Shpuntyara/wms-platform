export function errorMessage(error, unconfirmed = false) {
  if (unconfirmed) return 'Nie udało się potwierdzić operacji. Wybierz „Sprawdź wynik”.';
  const messages = {
    handoff_managed_stock:'Materiał należy do przekazania między działami. Otwórz przekazanie, aby sprawdzić jego stan.',
    handoff_managed_position:'To miejsce należy do działu produkcyjnego. Użyj przekazania między działami.',
    handoff_version_conflict:'Przekazanie zmienił inny pracownik. Odśwież dane i sprawdź przyjęcie.',
    handoff_already_accepted:'Przekazanie zostało już przyjęte. Odśwież dane, aby zobaczyć potwierdzenie.',
    handoff_unavailable:'Przekazania między działami są chwilowo niedostępne. Odśwież dane i spróbuj ponownie.',
    invalid_handoff_view:'Nie udało się otworzyć wybranego widoku przekazań. Odśwież dane.',
    invalid_document_recipient:'Wpisz odbiorcę wydania: firmę, dział lub osobę odbierającą towar.',
    invalid_work_order:'Sprawdź odbiorcę, towary i ilości w zamówieniu.',
    document_version_conflict:'Dokument zmienił inny pracownik. Odśwież go i sprawdź ilość.',
    forbidden:'Nie masz uprawnień do tej operacji. Skontaktuj się z kierownikiem zmiany.',
    unauthenticated:'Sesja wygasła. Zaloguj się ponownie.',
    insufficient_available_stock:'Pod adresem nie ma wystarczającej ilości dostępnego towaru.',
    document_incomplete:'Nie wykonano wszystkich pozycji. Sprawdź dokument.',
    document_not_active:'Dokument nie jest w toku. Odśwież dokument.',
    stock_scope_mismatch:'Wybrany towar lub adres nie należy do tego dokumentu.',
    transfer_tracking_mismatch:'Partia, numer seryjny lub stan towaru są niezgodne.',
  };
  if (messages[error?.code]) return messages[error.code];
  if (error?.status === 401) return 'Sesja wygasła. Zaloguj się ponownie.';
  if (error?.status >= 500 || error instanceof TypeError) return 'Nie udało się połączyć z serwerem. Wybierz „Odśwież” i spróbuj ponownie.';
  if (error?.status) return 'Serwer odrzucił operację. Odśwież dokument i sprawdź dane lub skontaktuj się z kierownikiem.';
  return error?.message || 'Nie udało się wykonać operacji. Spróbuj ponownie.';
}
