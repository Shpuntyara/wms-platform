import {parseQuantity,remaining,formatQuantity} from './domain.js';

const scoped=(row,scope)=>Boolean(row && scope && row.organization_id === scope.organization_id && row.warehouse_id === scope.warehouse_id);
const member=(context,id)=>context?.memberships?.some(row=>row.department_id === id);
const uniqueDepartment=(context,id)=>{
  const matches=context?.departments?.filter(row=>row.id === id && row.enabled === true)||[];
  return matches.length === 1 ? matches[0] : null;
};
export function departmentForPosition(context,positionID) {
  const matches=(context?.departments||[]).filter(row=>row.position_id === positionID || (Array.isArray(row.position_ids)&&row.position_ids.includes(positionID)));
  return matches.length === 1?matches[0]:null;
}
export function handoffDestinations({context,balance,item,scope}) {
  if (!context?.can_send || !context.employee?.actor_id || !scoped(balance,scope) || item?.organization_id !== scope?.organization_id || item.id !== balance.item_id || balance.stock_state !== 'available') return [];
  try {parseQuantity(formatQuantity(balance.available_qty),item.quantity_scale);} catch {return [];}
  const source=departmentForPosition(context,balance.position_id);
  if (!source || source.enabled !== true || !member(context,source.id) || !uniqueDepartment(context,source.id)) return [];
  return context.departments.filter(department=>uniqueDepartment(context,department.id) === department && department.id !== source.id && context.routes.some(route=>route.source_department_id === source.id && route.destination_department_id === department.id));
}
export function handoffSendIntent({context,balance,item,scope,destinationID,quantity}) {
  const destinations=handoffDestinations({context,balance,item,scope});
  if (!destinations.some(row=>row.id === destinationID)) throw new Error('Wybierz dział z dostępnej trasy przekazania. Sprawdź swoje uprawnienia i miejsce towaru.');
  const amount=parseQuantity(quantity,item.quantity_scale);
  try {remaining(balance.available_qty,amount);} catch {throw new Error('Ilość przekracza dostępny stan w tym miejscu. Odśwież stany.');}
  if (balance.serial_id) {try {remaining('1',amount);} catch {throw new Error('Towar z numerem seryjnym przekazuj po jednej sztuce.');}}
  return {...scope,source_balance_id:balance.id,destination_department_id:destinationID,quantity:amount};
}
export function canAcceptHandoff({context,handoff,scope}) {
  return Boolean(context?.can_accept && context.employee?.actor_id && scoped(handoff,scope) && handoff.state === 'sent' && context.employee.actor_id !== handoff.sender_actor_id && member(context,handoff.destination_department_id) && uniqueDepartment(context,handoff.destination_department_id));
}
export function handoffAcceptIntent(args) {
  if (!canAcceptHandoff(args)) throw new Error('Przekazanie może przyjąć inny pracownik działu docelowego. Sprawdź uprawnienia i stan przekazania.');
  if (!Number.isSafeInteger(args.handoff.version) || args.handoff.version < 1) throw new Error('Odśwież wersję przekazania.');
  return {...args.scope,expected_version:args.handoff.version};
}
