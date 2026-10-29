export function errorMessage(error, unconfirmed = false) {
  if (unconfirmed) return 'Nie udało się potwierdzić operacji. Wybierz „Sprawdź wynik”.';
  const messages = {
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
