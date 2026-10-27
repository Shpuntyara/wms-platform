-- Forward-only, additive performance migration. Run as schema owner, outside
-- a transaction: CONCURRENTLY keeps normal warehouse writes available.
-- A failed concurrent build may leave an INVALID index. Inspect pg_index and
-- remove that invalid object explicitly before retrying; do not hide it with
-- IF NOT EXISTS. Rollback of the app does not require dropping this index.
SET lock_timeout = '5s';
SET statement_timeout = '10min';
CREATE INDEX CONCURRENTLY movements_owner_page_idx
ON inventory.movements(organization_id,warehouse_id,owner_id,id);
CREATE INDEX CONCURRENTLY reservations_owner_page_idx
ON inventory.reservations(organization_id,warehouse_id,owner_id,id);
CREATE INDEX CONCURRENTLY stock_owner_page_idx
ON inventory.stock_balances(organization_id,warehouse_id,owner_id,id);
RESET lock_timeout;
RESET statement_timeout;
