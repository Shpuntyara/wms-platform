import {parseQuantity,remaining} from './domain.js';

const inScope = (row,scope) => Boolean(row && scope && row.organization_id === scope.organization_id && row.warehouse_id === scope.warehouse_id);
// PostgreSQL serializes numeric quantities with six places even for pieces.
// Only representation zeros are removed; fractional pieces remain invalid.
const canonicalData = value => typeof value === 'string' ? value.replace(/(\.\d*?[1-9])0+$/,'$1').replace(/\.0+$/,'') : value;
const positive = (value,scale=6) => {try {return parseQuantity(canonicalData(value),scale) !== '0';} catch {return false;}};
const zero = value => {try {return parseQuantity(canonicalData(value),6) === '0';} catch {return false;}};
const unique = (rows,id) => rows.filter(row => row.id === id).length === 1;

// Choices retain the original line and balance identities. A human-readable
// address is not unique: several owners, batches or serials may share it.
export function lineChoices(document,items,scope) {
  if (!inScope(document,scope) || document.state !== 'active') return [];
  return document.lines.flatMap(line => {
    const matches=items.filter(item => item.id === line.item_id && item.organization_id === scope.organization_id);
    if (matches.length !== 1 || !unique(document.lines,line.id)) return [];
    try {return remaining(line.requested_qty,line.fulfilled_qty) !== '0' ? [{line,item:matches[0]}] : [];} catch {return [];}
  });
}

export function balanceChoices({document,line,item,balances,positions,scope,role,sourceBalance,reservations=[]}) {
  if (!lineChoices(document,[item],scope).some(row => row.line.id === line?.id && row.line === line)) return [];
  if (role !== 'source' && role !== 'target') return [];
  if (role === 'source' && document.kind === 'receipt') return [];
  if (role === 'target' && document.kind === 'shipment') return [];
  if (role === 'target' && document.kind === 'transfer' && !sourceBalance) return [];
  return balances.filter(balance => {
    if (!unique(balances,balance.id) || !inScope(balance,scope) || balance.owner_id !== document.owner_id || balance.item_id !== item.id || balance.stock_state !== 'available') return false;
    const places=positions.filter(position => position.id === balance.position_id && inScope(position,scope));
    if (places.length !== 1) return false;
    if (role === 'source') {
      if (positive(balance.available_qty,item.quantity_scale)) return true;
      return document.kind === 'shipment' && reservations.some(reservation => inScope(reservation,scope) && reservation.owner_id === document.owner_id && reservation.item_id === item.id && reservation.document_line_id === line.id && reservation.balance_id === balance.id && positive(reservation.active_qty,item.quantity_scale));
    }
    if (document.kind === 'receipt') return places[0].kind === 'receiving' && (!balance.serial_id || zero(balance.on_hand));
    if (document.kind !== 'transfer' || places[0].kind !== 'storage' || balance.position_id === sourceBalance.position_id) return false;
    if ((balance.lot_id??null) !== (sourceBalance.lot_id??null) || (balance.serial_id??null) !== (sourceBalance.serial_id??null) || balance.stock_state !== sourceBalance.stock_state) return false;
    return !balance.serial_id || zero(balance.on_hand);
  });
}

export function resolveSelection(args) {
  const rows=lineChoices(args.document,args.items,args.scope).filter(row => row.line.id === args.lineID);
  if (!args.lineID || rows.length !== 1) throw new Error('Wybierz aktualną pozycję dokumentu. Odśwież dane, jeżeli pozycja się zmieniła.');
  const {line,item}=rows[0];
  const choose=(id,candidates) => {
    const matches=candidates.filter(row => row.id === id);
    if (!id || matches.length !== 1) throw new Error('Wybrane miejsce lub stan zmieniły się. Odśwież dane i wybierz właściwą pozycję.');
    return matches[0];
  };
  const common={...args,line,item};
  const sourceBalance=args.document.kind === 'receipt'?undefined:choose(args.sourceID,balanceChoices({...common,role:'source'}));
  const targetBalance=args.document.kind === 'shipment'?undefined:choose(args.targetID,balanceChoices({...common,sourceBalance,role:'target'}));
  return {line,item,sourceBalance,targetBalance};
}
