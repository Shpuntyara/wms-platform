const error = message => { throw new Error(message); };

function units(value) {
  if (typeof value !== 'string' || !/^(0|[1-9][0-9]{0,17})(\.[0-9]{1,6})?$/.test(value)) error('Nieprawidłowa ilość');
  const [whole, fraction = ''] = value.split('.');
  return BigInt(whole) * 1000000n + BigInt(fraction.padEnd(6, '0'));
}

function decimal(value) {
  const fraction = String(value % 1000000n).padStart(6, '0').replace(/0+$/, '');
  return `${value / 1000000n}${fraction ? '.' + fraction : ''}`;
}

export function parseQuantity(input, scale = 6) {
  if (typeof input !== 'string' || !Number.isInteger(scale) || scale < 0 || scale > 6) error('Nieprawidłowa dokładność towaru');
  const value = input.trim().replace(',', '.');
  if ((value.split('.')[1]?.length ?? 0) > scale) error(`Dopuszczalna liczba miejsc po przecinku: ${scale}`);
  const amount = units(value);
  if (amount === 0n) error('Ilość musi być większa od zera');
  return decimal(amount);
}

export function remaining(requested, fulfilled) {
  const value = units(requested) - units(fulfilled);
  if (value < 0n) error('Wykonanie przekracza plan. Odśwież dokument');
  return decimal(value);
}

export function matchScan(scan, candidates, key = 'sku') {
  if (typeof scan !== 'string' || !scan.trim()) error('Zeskanuj kod lub wpisz go ręcznie');
  const matches = candidates.filter(candidate => candidate[key] === scan.trim());
  if (matches.length !== 1) error(matches.length ? 'Kod jest niejednoznaczny. Sprawdź kartotekę' : 'Nie znaleziono kodu w tym zadaniu');
  return matches[0];
}

export function movementIntent({document, line, item, targetBalance, sourceBalance, quantity, scope, kind}) {
  if (!document || document.state !== 'active' || !['receipt', 'transfer'].includes(kind) || document.kind !== kind) error('Dokument nie jest otwarty dla tej operacji');
  if (!Number.isSafeInteger(document.version) || document.version < 1) error('Odśwież wersję dokumentu');
  if (!line || !item || line.item_id !== item.id || !document.lines.some(entry => entry.id === line.id && entry.item_id === item.id)) error('Towar nie należy do dokumentu');
  const capturedLine = document.lines.find(entry => entry.id === line.id);
  if (['requested_qty', 'fulfilled_qty', 'reserved_qty'].some(field => capturedLine[field] !== line[field])) error('Pozycja uległa zmianie. Odśwież dokument');
  for (const field of ['organization_id', 'warehouse_id']) {
    if (scope[field] !== document[field]) error('Wybrano inny magazyn');
  }
  const amount = parseQuantity(quantity, item.quantity_scale);
  if (units(amount) > units(remaining(line.requested_qty, line.fulfilled_qty))) error('Ilość przekracza pozostałą ilość dokumentu');
  const checkBalance = balance => {
    if (!balance || ['organization_id', 'warehouse_id', 'owner_id'].some(field => balance[field] !== document[field]) || balance.item_id !== item.id) error('Adres nie pasuje do towaru lub właściciela');
    if (balance.serial_id && units(amount) > 1000000n) error('Towar z numerem seryjnym przyjmuj po jednej sztuce');
  };
  checkBalance(targetBalance);
  const body = {...scope, document_line_id: line.id, expected_document_version: document.version, kind, quantity: amount};
  if (kind === 'transfer') {
    checkBalance(sourceBalance);
    if (sourceBalance.position_id === targetBalance.position_id) error('Wybierz inny adres docelowy');
    if (['lot_id', 'serial_id', 'stock_state'].some(field => (sourceBalance[field] ?? null) !== (targetBalance[field] ?? null))) error('Partia, numer seryjny lub stan towaru są niezgodne');
    if (units(amount) > units(sourceBalance.available_qty)) error('Pod adresem nie ma wystarczającej ilości dostępnego towaru');
    body.source_balance_id = sourceBalance.id;
  }
  body.target_balance_id = targetBalance.id;
  return body;
}

export const formatQuantity = value => decimal(units(value)).replace('.', ',');
export const completed = document => document.lines.length > 0 && document.lines.every(line => remaining(line.requested_qty, line.fulfilled_qty) === '0' && units(line.reserved_qty) === 0n);
