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

export function materialRequirements(recipe, quantity) {
  const amount = units(parseQuantity(quantity));
  if (!Array.isArray(recipe?.components) || !recipe.components.length) error('Brak składu produktu');
  const requirements = new Map();
  for (const component of recipe.components) {
    if (typeof component.item_id !== 'string' || !component.item_id) error('Brak towaru w składzie produktu');
    const product = units(parseQuantity(component.quantity)) * amount;
    if (product % 1000000n) error('Ilość materiału przekracza dokładność magazynu');
    const total = (requirements.get(component.item_id) || 0n) + product / 1000000n;
    if (total > 999999999999999999999999n) error('Ilość materiału jest zbyt duża');
    requirements.set(component.item_id,total);
  }
  return [...requirements].map(([item_id,value]) => ({item_id,quantity:decimal(value)}));
}

export function materialAvailability(requirements, balances, scope) {
  return requirements.map(line => {
    const available = balances.filter(row => row.item_id === line.item_id && row.organization_id === scope.organization_id && row.warehouse_id === scope.warehouse_id && row.owner_id === scope.owner_id && (!row.stock_state || row.stock_state === 'available')).reduce((sum,row) => sum + units(row.available_qty),0n);
    const shortage = units(line.quantity) > available ? units(line.quantity) - available : 0n;
    return {...line,available_qty:decimal(available),shortage_qty:decimal(shortage)};
  });
}

export function findStock(balances, items, positions, search) {
  const normalize = value => String(value ?? '').toLocaleLowerCase('pl').normalize('NFD').replace(/[\u0300-\u036f]/g,'').replace(/ł/g,'l');
  const term = normalize(search.trim());
  return balances.filter(balance => {
    const item = items.find(row => row.id === balance.item_id), position = positions.find(row => row.id === balance.position_id);
    return normalize([item?.sku,item?.name,position?.code,position?.name].join(' ')).includes(term);
  });
}

function shipmentQuantity({document,line,item,sourceBalance,quantity,scope}) {
  if (!document || document.kind !== 'shipment' || document.state !== 'active') error('Wydanie nie jest otwarte do realizacji');
  if (Number.isSafeInteger(document.version) === false || document.version < 1) error('Odśwież wersję dokumentu');
  const current = document.lines.find(row => row.id === line?.id && row.item_id === item?.id);
  if (!current || !line || ['requested_qty','fulfilled_qty','reserved_qty'].some(key => current[key] !== line[key])) error('Pozycja wydania uległa zmianie. Odśwież dokument');
  if (!sourceBalance || ['organization_id','warehouse_id','owner_id'].some(key => sourceBalance[key] !== document[key]) || sourceBalance.item_id !== item.id) error('Adres nie pasuje do towaru lub właściciela');
  if (scope.organization_id !== document.organization_id || scope.warehouse_id !== document.warehouse_id) error('Wybrano inny magazyn');
  if (sourceBalance.stock_state && sourceBalance.stock_state !== 'available') error('Towar nie jest dostępny do wydania');
  const amount = parseQuantity(quantity,item.quantity_scale);
  if (units(amount) > units(remaining(line.requested_qty,line.fulfilled_qty))) error('Ilość przekracza pozostałą ilość wydania');
  if (sourceBalance.serial_id && units(amount) > 1000000n) error('Towar z numerem seryjnym wydawaj po jednej sztuce');
  return amount;
}

export function reservationIntent(input) {
  const amount = shipmentQuantity(input), {document,line,sourceBalance,scope} = input;
  if (units(amount) > units(remaining(remaining(line.requested_qty,line.fulfilled_qty),line.reserved_qty))) error('Ta ilość została już zarezerwowana do wydania');
  if (units(amount) > units(input.sourceBalance.available_qty)) error('Pod adresem nie ma wystarczającej ilości dostępnego towaru');
  return {...scope,document_line_id:line.id,expected_document_version:document.version,balance_id:sourceBalance.id,quantity:amount};
}

export function issueIntent(input) {
  const amount = shipmentQuantity(input), {document,line,sourceBalance,reservation,scope} = input;
  if (!reservation?.id || reservation.document_line_id !== line.id || reservation.balance_id !== sourceBalance.id || reservation.item_id !== line.item_id || reservation.owner_id !== document.owner_id || reservation.organization_id !== document.organization_id || reservation.warehouse_id !== document.warehouse_id) error('Rezerwacja nie należy do tej pozycji i adresu');
  if (units(amount) > units(reservation.active_qty) || units(amount) > units(line.reserved_qty)) error('Ilość przekracza aktywną rezerwację');
  if (units(amount) > units(sourceBalance.on_hand)) error('Pod adresem nie ma wystarczającej ilości towaru');
  return {...scope,document_line_id:line.id,expected_document_version:document.version,kind:'issue',source_balance_id:sourceBalance.id,reservation_id:reservation.id,quantity:amount};
}
