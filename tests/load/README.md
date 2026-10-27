# Synthetic history fixture

`seed-history.sql` runs only through the disposable load-test harness. It needs an administrative connection, an existing inventory fixture and one row in temporary `load_config`:

```sql
CREATE TEMP TABLE load_config (
  org uuid, wh uuid, owner_id uuid, actor uuid, item uuid, wh2 uuid,
  movements bigint, reservations bigint, balances bigint
);
```

The caller opens one isolated transaction, inserts the configuration, runs the SQL and commits. The database name must begin with `wms_kernel_test`; the script refuses other databases. The initial organization, first warehouse, first owner, actor and untracked item must already exist. `wh2` is a different UUID; the script creates that warehouse or checks its organization. The script deliberately refuses a second seed for the same organization.

The fixture spreads one SKU over 100 owners and two warehouses. `balances` must be a positive multiple of 200. With 100,000 balances there are 500 positions per warehouse and one balance per owner/position. Scope zero uses the original owner and warehouse; scopes 0–99 use the first warehouse and scopes 100–199 the second.

Historical receipts are dated January 2025. Each movement contributes one unit to its scope's position-one balance. Reservations dated March 2025 are fully released; active quantities and document reserved totals finish at zero. Receipt document fulfillment and final physical balances match the journal. Audit records and complete outbox envelopes accompany every historical operation; outbox entries are marked delivered so history size does not implicitly create a queue backlog.

Bulk insertion is fixture setup, not an application import path or a throughput measurement. Constraints, normal tables, durability, row triggers and deferred completed-operation checks remain enabled. The history setup cost is excluded from API throughput timings. Measured commands still pass through the real backend/kernel.

During the transaction, `load_scopes` exposes the 200 scopes and their counts, balances and document/line IDs. It drops on commit. Stable identifiers for scope zero can also be computed afterward:

```sql
SELECT md5(:'org'::text || ':load-balance:0:1')::uuid AS balance_id,
       md5(:'org'::text || ':load-receipt-document:0')::uuid AS receipt_document_id,
       md5(:'org'::text || ':load-receipt-line:0')::uuid AS receipt_line_id,
       md5(:'org'::text || ':load-shipment-document:0')::uuid AS shipment_document_id,
       md5(:'org'::text || ':load-shipment-line:0')::uuid AS shipment_line_id;
```

The fixture bounds movement and reservation counts at five million each and balances at one million. At least one prior receipt is required for every scope with reservation history. Reconciliation and load measurements are the caller's responsibility; the script ends by analyzing the populated tables.
