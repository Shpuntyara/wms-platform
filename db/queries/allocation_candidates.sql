-- Parameters: $1 organization UUID, $2 warehouse UUID, $3 owner UUID,
-- $4 item UUID, $5 maximum candidate count. This is a candidate query,
-- not a reservation. Post through reserve_stock with an expected version.
SELECT b.id, b.position_id, b.lot_id, b.on_hand - b.reserved_qty AS available_qty, l.expires_on
FROM inventory.stock_balances b
JOIN inventory.stock_states s ON s.code = b.stock_state AND s.allocatable
LEFT JOIN catalog.lots l ON l.id = b.lot_id
WHERE b.organization_id = $1 AND b.warehouse_id = $2
  AND b.owner_id = $3 AND b.item_id = $4 AND b.on_hand > b.reserved_qty
ORDER BY l.expires_on NULLS LAST, b.id
LIMIT $5;

-- Expiry and warehouse-local shelf-life rules belong to the allocation/guard
-- contract. The query alone does not authorize shipping or guarantee freshness.
