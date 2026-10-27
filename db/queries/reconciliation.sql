-- Run all reconciliation queries in one consistent read-only snapshot.
BEGIN TRANSACTION ISOLATION LEVEL REPEATABLE READ READ ONLY;

-- Both result sets must be empty. A mismatch requires investigation, not an
-- automatic overwrite of balances from a potentially incomplete history.
WITH active AS (
  SELECT balance_id, sum(active_qty) AS qty FROM inventory.reservations GROUP BY balance_id
)
SELECT b.id, b.reserved_qty, coalesce(a.qty, 0) AS actual_active_reservations
FROM inventory.stock_balances b LEFT JOIN active a ON a.balance_id = b.id
WHERE b.reserved_qty <> coalesce(a.qty, 0);

WITH deltas AS (
  SELECT target_balance_id AS balance_id, quantity AS qty FROM inventory.movement_lines WHERE target_balance_id IS NOT NULL
  UNION ALL
  SELECT source_balance_id, -quantity FROM inventory.movement_lines WHERE source_balance_id IS NOT NULL
), totals AS (SELECT balance_id, sum(qty) AS qty FROM deltas GROUP BY balance_id)
SELECT b.id, b.on_hand, coalesce(t.qty, 0) AS ledger_qty
FROM inventory.stock_balances b LEFT JOIN totals t ON t.balance_id = b.id
WHERE b.on_hand <> coalesce(t.qty, 0);

WITH active AS (
  SELECT document_line_id, sum(active_qty) AS qty FROM inventory.reservations GROUP BY document_line_id
)
SELECT l.id, l.reserved_qty, coalesce(a.qty, 0) AS actual_active_reservations
FROM warehouse.document_lines l LEFT JOIN active a ON a.document_line_id = l.id
WHERE l.reserved_qty <> coalesce(a.qty, 0);

COMMIT;
