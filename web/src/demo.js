import {movementIntent, remaining, completed, materialRequirements, reservationIntent, issueIntent} from './domain.js';
const id = n => `22222222-2222-4222-8222-${String(n).padStart(12, '0')}`;
const scope = {organization_id: id(1), warehouse_id: id(2)};
const count = text => { const [whole, fraction = ''] = text.split('.'); return BigInt(whole) * 1000000n + BigInt(fraction.padEnd(6, '0')); };
const sum = (a, b) => {
  const value = count(a) + count(b);
  const tail = String(value % 1000000n).padStart(6, '0').replace(/0+$/, '');
  return `${value / 1000000n}${tail ? '.' + tail : ''}`;
};
function nextNumber(state, body, now) {
  const warehouse = state.workspace.catalog.warehouses.find(row => row.id === body.warehouse_id);
  const year = new Intl.DateTimeFormat('en', {year:'numeric', timeZone:warehouse?.timezone || 'Europe/Warsaw'}).format(now);
  const prefix = `${body.kind === 'shipment' ? 'WZ' : (body.kind === 'receipt' ? 'PZ' : 'MM')}/${year}/`;
  const last = state.documents.reduce((highest, doc) => {
    if (doc.organization_id !== body.organization_id || doc.warehouse_id !== body.warehouse_id || doc.kind !== body.kind || !doc.number.startsWith(prefix)) return highest;
    const serial = doc.number.slice(prefix.length);
    if (!/^\d{6,}$/.test(serial)) return highest;
    const value = BigInt(serial);
    return value > highest ? value : highest;
  }, 0n);
  return prefix + String(last + 1n).padStart(6, '0');
}
function initial() {
  const catalog = {
    organizations: [{id: id(1), name: 'Firma magazynowa'}],
    warehouses: [{id: id(2), organization_id: id(1), name: 'Magazyn Północny', timezone:'Europe/Warsaw'}],
    owners: [{id: id(3), organization_id: id(1), name: 'Północ · Zaopatrzenie'}],
    items: [
      {id: id(10), organization_id: id(1), sku: 'BOLT-M8', name: 'Śruba M8 × 40, ocynkowana', unit: 'szt.', quantity_scale: 0},
      {id: id(11), organization_id: id(1), sku: 'NUT-M8', name: 'Nakrętka M8, klasa wytrzymałości 8', unit: 'szt.', quantity_scale: 0},
      {id: id(12), organization_id: id(1), sku: 'WASHER-8', name: 'Podkładka płaska 8 mm', unit: 'szt.', quantity_scale: 0},
      {id: id(13), organization_id: id(1), sku: 'CABLE-25', name: 'Przewód zasilający 3 × 2,5', unit: 'm', quantity_scale: 3}
    ],
    positions: [{id: id(20), ...scope, code: 'PRZYJECIE-01', kind: 'receiving'}, {id: id(21), ...scope, code: 'A-01-02', kind: 'storage'}, {id: id(22), ...scope, code: 'A-02-01', kind: 'storage'}]
  };
  const balances = catalog.items.flatMap(item => catalog.positions.map((position, i) => ({id: id(100 + Number(item.id.slice(-2)) * 10 + i), ...scope, owner_id: id(3), item_id: item.id, position_id: position.id, stock_state: 'available', lot_id: null, serial_id: null, on_hand: i === 1 ? '24' : '0', reserved_qty: '0', available_qty: i === 1 ? '24' : '0', version: 1})));
  const docs = [
    ['PZ-2026-0142', 'active', ['120', '80'], [10, 11]],
    ['PZ-2026-0141', 'draft', ['250'], [12]],
    ['PZ-2026-0140', 'active', ['60'], [13]],
    ['PZ-2026-0139', 'closed', ['48', '96'], [10, 12]],
    ['PZ-2026-0138', 'closed', ['100'], [11]]
  ].map(([number, state, quantities, items], index) => ({id: id(300 + index), ...scope, owner_id: id(3), kind: 'receipt', number, state, version: state === 'draft' ? 1 : 2, lines: quantities.map((qty, n) => ({id: id(400 + index * 10 + n), item_id: id(items[n]), requested_qty: qty, fulfilled_qty: state === 'closed' ? qty : '0', reserved_qty: '0'}))}));
  return {workspace: {actor_id: id(9), scopes: [{...scope, grants: ['document.read','document.create','document.issue','document.move','document.transition','stock.read','movement.create','reservation.read','reservation.create'].map(permission => ({permission, owner_id: null}))}], catalog}, documents: docs, balances, orders:[], reservations:[], commands: {}};
}

// Isolated synthetic training data. This adapter never contacts the warehouse API.
export class DemoWarehouse {
  constructor(storage, {now = () => new Date()} = {}) {
    this.storage = storage;
    this.now = now;
    this.state = JSON.parse(storage.getItem('wms-training-v2') || 'null') || initial();
  }
  async request(path, body, command) {
    const state = structuredClone(JSON.parse(this.storage.getItem('wms-training-v2') || 'null') || this.state);
    let data;
    const clean = path.split('?')[0];
    if (command && state.commands[command]) {
      const prior = state.commands[command];
      if (prior.path !== clean || prior.body !== JSON.stringify(body)) throw Object.assign(new Error('Klucz został użyty z inną zawartością'), {status: 409});
      return structuredClone(prior.result);
    }
    if (!body) {
      if (clean.endsWith('/handoffs/context')) data = state.handoff_context;
      else if (clean.endsWith('/handoffs')) {
        const params=new URLSearchParams(path.split('?')[1]),view=params.get('view')||'all',cursor=params.get('cursor');
        if(!['all','incoming','outgoing'].includes(view))throw Object.assign(new Error('Nieprawidłowy widok przekazań'),{status:400,code:'invalid_handoff_view'});
        if(cursor&&!cursor.startsWith(view+':'))throw Object.assign(new Error('Kursor należy do innego widoku'),{status:400});
        const start=cursor?Number(cursor.split(':')[1]):0,limit=Number(params.get('limit')||100);
        if(!Number.isSafeInteger(start)||start<0||!Number.isInteger(limit)||limit<1||limit>200)throw Object.assign(new Error('Nieprawidłowa strona przekazań'),{status:400});
        const rows=(state.handoffs||[]).filter(row=>view==='incoming'?row.state==='sent'&&row.sender_actor_id!==state.workspace.actor_id&&state.handoff_context.memberships.some(member=>member.department_id===row.destination_department_id):view==='outgoing'?row.sender_actor_id===state.workspace.actor_id:true);
        return {data:rows.slice(start,start+limit),pagination:{next_cursor:start+limit<rows.length?view+':'+(start+limit):null}};
      }
      else if (clean.includes('/handoffs/')) data = (state.handoffs || []).find(row => row.id === clean.split('/').at(-1));
      else if (clean.endsWith('/workspace')) data = state.workspace;
      else if (clean.endsWith('/stock-balances')) data = state.balances;
      else if (clean.endsWith('/work-orders')) data = state.orders || [];
      else if (clean.endsWith('/reservations')) data = (state.reservations || []).filter(row => !new URLSearchParams(path.split('?')[1]).get('document_line_id') || row.document_line_id === new URLSearchParams(path.split('?')[1]).get('document_line_id'));
      else if (clean.endsWith('/documents')) data = state.documents;
      else data = state.documents.find(doc => doc.id === clean.split('/').at(-1));
      return {data, pagination: {next_cursor: null}};
    }
    if (clean === '/api/v1/handoffs/send') {
      const {handoffSendIntent,departmentForPosition}=await import('./handoff.js');
      const source=state.balances.find(row=>row.id === body.source_balance_id),context=state.handoff_context;
      const item=state.workspace.catalog.items.find(row=>row.id === source?.item_id);
      const intent=handoffSendIntent({context,balance:source,item,scope:body,destinationID:body.destination_department_id,quantity:body.quantity});
      const sourceDepartment=departmentForPosition(context,source.position_id);
      const destination=context.departments.find(row=>row.id === body.destination_department_id);
      const targets=state.balances.filter(row=>row.owner_id === source.owner_id && row.item_id === source.item_id && row.position_id === destination.position_id && row.stock_state === 'available' && row.lot_id === source.lot_id && row.serial_id === source.serial_id);
      if (targets.length > 1) throw Object.assign(new Error('Nie można jednoznacznie wybrać miejsca docelowego'),{status:409});
      const target=targets[0] || {...source,id:crypto.randomUUID(),position_id:destination.position_id,on_hand:'0',reserved_qty:'0',available_qty:'0',version:1};
      if (!targets.length) state.balances.push(target);
      const transitPosition={id:crypto.randomUUID(),organization_id:body.organization_id,warehouse_id:body.warehouse_id,code:'W przekazaniu',kind:'transit'};
      const transit={...source,id:crypto.randomUUID(),position_id:transitPosition.id,stock_state:'in_transit',on_hand:intent.quantity,reserved_qty:'0',available_qty:'0',version:1};
      state.workspace.catalog.positions.push(transitPosition);state.balances.push(transit);
      source.on_hand=remaining(source.on_hand,intent.quantity);source.available_qty=remaining(source.available_qty,intent.quantity);source.version++;
      const record={id:crypto.randomUUID(),organization_id:body.organization_id,warehouse_id:body.warehouse_id,owner_id:source.owner_id,item_id:source.item_id,lot_id:source.lot_id,serial_id:source.serial_id,source_department_id:sourceDepartment.id,source_department_name:sourceDepartment.name,destination_department_id:destination.id,destination_department_name:destination.name,source_balance_id:source.id,transit_balance_id:transit.id,destination_balance_id:target.id,source_position_id:source.position_id,transit_position_id:transit.position_id,destination_position_id:target.position_id,quantity:intent.quantity,state:'sent',version:1,sender_actor_id:context.employee.actor_id,sender_name:context.employee.display_name,receiver_actor_id:null,receiver_name:null,sent_at:this.now().toISOString(),accepted_at:null,send_document_id:crypto.randomUUID(),accept_document_id:null};
      state.handoffs ||= [];state.handoffs.unshift(record);
      data={operation_id:command,handoff_id:record.id,version:record.version,state:record.state};
    } else if (clean.startsWith('/api/v1/handoffs/') && clean.endsWith('/accept')) {
      const {handoffAcceptIntent}=await import('./handoff.js');
      const record=(state.handoffs || []).find(row=>row.id === clean.split('/').at(-2));
      if (!record || record.version !== body.expected_version) throw Object.assign(new Error('Przekazanie zmieniło się. Odśwież dane'),{status:409});
      handoffAcceptIntent({context:state.handoff_context,handoff:record,scope:body});
      const transit=state.balances.find(row=>row.id === record.transit_balance_id),target=state.balances.find(row=>row.id === record.destination_balance_id);
      if (target.serial_id && count(target.on_hand) !== 0n) throw Object.assign(new Error('Numer seryjny jest już w miejscu docelowym'),{status:409});
      transit.on_hand=remaining(transit.on_hand,record.quantity);transit.version++;
      target.on_hand=sum(target.on_hand,record.quantity);target.available_qty=sum(target.available_qty,record.quantity);target.version++;
      Object.assign(record,{state:'accepted',version:record.version+1,receiver_actor_id:state.handoff_context.employee.actor_id,receiver_name:state.handoff_context.employee.display_name,accepted_at:this.now().toISOString(),accept_document_id:crypto.randomUUID()});
      data={operation_id:command,handoff_id:record.id,version:record.version,state:record.state};
    } else if (clean === '/api/v1/documents') {
      if (body.kind === 'shipment' && !body.recipient?.trim()) throw Object.assign(new Error('Wpisz odbiorcę wydania'),{status:400});
      const number = body.number === '' || body.number == null ? nextNumber(state, body, this.now()) : body.number;
      const document = {id: crypto.randomUUID(), organization_id:body.organization_id, warehouse_id:body.warehouse_id, owner_id: body.owner_id, kind: body.kind, number, state: 'draft', version: 1, lines: body.lines.map(line => ({id: crypto.randomUUID(), item_id: line.item_id, requested_qty: line.quantity, fulfilled_qty: '0', reserved_qty: '0'}))};
      if (body.recipient) document.recipient = body.recipient;
      if (body.order_reference) document.order_reference = body.order_reference;
      if (state.documents.some(doc => doc.warehouse_id === body.warehouse_id && doc.kind === body.kind && doc.number === number)) throw Object.assign(new Error('Numer dokumentu już istnieje'), {status: 409});
      state.documents.unshift(document);
      data = {document_id: document.id, document_version: 1, state: 'draft', lines: document.lines};
    } else if (clean === '/api/v1/work-orders') {
      const recipe = state.workspace.catalog.recipes?.find(row => row.id === body.recipe_id);
      const order = {...body,id:crypto.randomUUID(),requirements:recipe?materialRequirements(recipe,body.lines[0].quantity):[],steps:recipe?[...recipe.steps]:[],state:'planned'};
      state.orders ||= []; state.orders.unshift(order);
      data = order;
    } else if (clean === '/api/v1/reservations') {
      const doc = state.documents.find(row => row.lines.some(line => line.id === body.document_line_id));
      const line = doc?.lines.find(row => row.id === body.document_line_id);
      if (!doc || doc.version !== body.expected_document_version) throw Object.assign(new Error('Dokument uległ zmianie. Odśwież dane'),{status:409});
      const sourceBalance = state.balances.find(row => row.id === body.balance_id);
      reservationIntent({document:doc,line,item:state.workspace.catalog.items.find(row => row.id === line.item_id),sourceBalance,quantity:body.quantity,scope});
      const reservation = {id:crypto.randomUUID(),...scope,owner_id:doc.owner_id,item_id:line.item_id,document_line_id:line.id,balance_id:sourceBalance.id,quantity:body.quantity,active_qty:body.quantity};
      state.reservations ||= []; state.reservations.push(reservation);
      sourceBalance.available_qty = remaining(sourceBalance.available_qty,body.quantity);
      sourceBalance.reserved_qty = sum(sourceBalance.reserved_qty,body.quantity);
      line.reserved_qty = sum(line.reserved_qty,body.quantity); doc.version++;
      data = {reservation_id:reservation.id,document_version:doc.version};
    } else if (clean.endsWith('/transitions')) {
      const doc = state.documents.find(entry => entry.id === clean.split('/').at(-2));
      if (!doc || doc.version !== body.expected_document_version) throw Object.assign(new Error('Dokument uległ zmianie. Odśwież dane'), {status: 409});
      if (!(doc.state === 'draft' && body.state === 'active' || doc.state === 'active' && body.state === 'closed' && completed(doc))) throw Object.assign(new Error('Najpierw wykonaj wszystkie pozycje dokumentu'), {status: 409});
      doc.state = body.state;
      doc.version++;
      data = {document_id: doc.id, document_version: doc.version, state: doc.state};
    } else if (clean === '/api/v1/movements') {
      const doc = state.documents.find(entry => entry.lines.some(line => line.id === body.document_line_id));
      const line = doc?.lines.find(entry => entry.id === body.document_line_id);
      if (!doc || doc.version !== body.expected_document_version) throw Object.assign(new Error('Dokument uległ zmianie. Odśwież dane'), {status: 409});
      const targetBalance = state.balances.find(entry => entry.id === body.target_balance_id);
      const sourceBalance = state.balances.find(entry => entry.id === body.source_balance_id);
      if (body.kind === 'issue') {
        const reservation = (state.reservations || []).find(row => row.id === body.reservation_id);
        issueIntent({document:doc,line,item:state.workspace.catalog.items.find(row => row.id === line.item_id),sourceBalance,reservation,quantity:body.quantity,scope});
        reservation.active_qty = remaining(reservation.active_qty,body.quantity);
        sourceBalance.reserved_qty = remaining(sourceBalance.reserved_qty,body.quantity);
        line.reserved_qty = remaining(line.reserved_qty,body.quantity);
      } else {
        movementIntent({document: doc, line, item: state.workspace.catalog.items.find(item => item.id === line.item_id), targetBalance, sourceBalance, quantity: body.quantity, scope, kind: body.kind});
        targetBalance.on_hand = sum(targetBalance.on_hand, body.quantity);
        targetBalance.available_qty = sum(targetBalance.available_qty, body.quantity);
      }
      if (sourceBalance) {
        sourceBalance.on_hand = remaining(sourceBalance.on_hand, body.quantity);
        if (body.kind !== 'issue') sourceBalance.available_qty = remaining(sourceBalance.available_qty, body.quantity);
      }
      line.fulfilled_qty = sum(line.fulfilled_qty, body.quantity);
      doc.version++;
      data = {movement_id: crypto.randomUUID(), document_version: doc.version};
    } else throw new Error('Operacja nie jest dostępna w magazynie szkoleniowym');
    const result = {data};
    state.commands[command] = {path: clean, body: JSON.stringify(body), result};
    this.storage.setItem('wms-training-v2', JSON.stringify(state));
    this.state = state;
    return structuredClone(result);
  }
}
