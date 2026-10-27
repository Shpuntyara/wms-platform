-- Synthetic load-test fixture. Admin-only, isolated database, one transaction.
-- Caller supplies exactly one row in TEMP load_config:
-- (org uuid, wh uuid, owner_id uuid, actor uuid, item uuid, wh2 uuid,
--  movements bigint, reservations bigint, balances bigint).
-- Constraints, row triggers, history protection and durability remain enabled.
DO $validate$
DECLARE c record;
BEGIN
  IF current_database() NOT LIKE 'wms_kernel_test%' THEN
    RAISE EXCEPTION 'refusing_non_isolated_load_history_database';
  END IF;
  IF (SELECT count(*) FROM load_config) <> 1 THEN
    RAISE EXCEPTION 'load_config_requires_exactly_one_row';
  END IF;
  SELECT * INTO STRICT c FROM load_config;
  IF c.org IS NULL OR c.wh IS NULL OR c.owner_id IS NULL OR c.actor IS NULL
     OR c.item IS NULL OR c.wh2 IS NULL OR c.wh2=c.wh
     OR c.movements IS NULL OR c.reservations IS NULL OR c.balances IS NULL
     OR c.movements < 0 OR c.reservations < 0 OR c.balances < 200
     OR c.balances % 200 <> 0
     OR c.movements > 5000000 OR c.reservations > 5000000 OR c.balances > 1000000 THEN
    RAISE EXCEPTION 'invalid_load_config';
  END IF;
  IF c.reservations > 0 AND c.movements < least(200::bigint,c.reservations) THEN
    RAISE EXCEPTION 'reservation_history_requires_received_stock_in_each_scope';
  END IF;
  IF NOT EXISTS (
    SELECT 1 FROM warehouse.warehouses w
    JOIN catalog.stock_owners o ON o.organization_id=w.organization_id
    JOIN catalog.items i ON i.organization_id=w.organization_id
    WHERE w.id=c.wh AND w.organization_id=c.org AND o.id=c.owner_id
      AND i.id=c.item AND i.tracking='none'
  ) OR NOT EXISTS (SELECT 1 FROM platform.actors WHERE id=c.actor) THEN
    RAISE EXCEPTION 'load_config_master_data_mismatch';
  END IF;
  IF EXISTS (SELECT 1 FROM inventory.stock_balances WHERE id=md5(c.org::text||':load-balance:0:1')::uuid) THEN
    RAISE EXCEPTION 'history_fixture_already_seeded_for_organization';
  END IF;
END
$validate$;

INSERT INTO warehouse.warehouses(id,organization_id,code)
SELECT wh2,org,'load-history-warehouse-2' FROM load_config
ON CONFLICT (id) DO NOTHING;

DO $warehouse$
BEGIN
  IF EXISTS (
    SELECT 1 FROM load_config c JOIN warehouse.warehouses w ON w.id=c.wh2
    WHERE w.organization_id<>c.org
  ) THEN
    RAISE EXCEPTION 'load_history_second_warehouse_scope_mismatch';
  END IF;
END
$warehouse$;

INSERT INTO catalog.stock_owners(id,organization_id,code)
SELECT md5(c.org::text||':load-owner:'||n)::uuid,c.org,'load-history-owner-'||n
FROM load_config c CROSS JOIN generate_series(1,99) AS n;

-- 100 owners x two warehouses. Scope zero belongs to the existing owner/wh.
CREATE TEMP TABLE load_scopes ON COMMIT DROP AS
SELECT s::integer AS scope_id,c.org AS organization_id,
       CASE WHEN s<100 THEN c.wh ELSE c.wh2 END AS warehouse_id,
       CASE WHEN s%100=0 THEN c.owner_id ELSE md5(c.org::text||':load-owner:'||(s%100))::uuid END AS owner_id,
       c.actor AS actor_id,c.item AS item_id,
       md5(c.org::text||':load-receipt-document:'||s)::uuid AS receipt_document_id,
       md5(c.org::text||':load-receipt-line:'||s)::uuid AS receipt_line_id,
       md5(c.org::text||':load-shipment-document:'||s)::uuid AS shipment_document_id,
       md5(c.org::text||':load-shipment-line:'||s)::uuid AS shipment_line_id,
       md5(c.org::text||':load-balance:'||s||':1')::uuid AS balance_id,
       c.movements/200 + CASE WHEN s<c.movements%200 THEN 1 ELSE 0 END AS movement_count,
       c.reservations/200 + CASE WHEN s<c.reservations%200 THEN 1 ELSE 0 END AS reservation_count
FROM load_config c CROSS JOIN generate_series(0,199) AS s;
ALTER TABLE load_scopes ADD PRIMARY KEY(scope_id);

-- Each position is shared by 100 owners. 100k balances use 500 bins per wh.
INSERT INTO warehouse.locations(id,organization_id,warehouse_id,code,kind)
SELECT md5(c.org::text||':load-location:'||w||':'||p)::uuid,c.org,
       CASE WHEN w=0 THEN c.wh ELSE c.wh2 END,'load-history-location-'||p,'storage'
FROM load_config c CROSS JOIN generate_series(0,1) AS w
CROSS JOIN LATERAL generate_series(1,c.balances/200) AS p;

INSERT INTO warehouse.positions(id,organization_id,warehouse_id,location_id)
SELECT md5(c.org::text||':load-position:'||w||':'||p)::uuid,c.org,
       CASE WHEN w=0 THEN c.wh ELSE c.wh2 END,
       md5(c.org::text||':load-location:'||w||':'||p)::uuid
FROM load_config c CROSS JOIN generate_series(0,1) AS w
CROSS JOIN LATERAL generate_series(1,c.balances/200) AS p;

INSERT INTO warehouse.documents(id,organization_id,warehouse_id,owner_id,kind,number,state,version,content_revision,created_at)
SELECT receipt_document_id,organization_id,warehouse_id,owner_id,'receipt','load-history-receipt-'||scope_id,
       'closed',1+movement_count,1+movement_count,'2025-01-01 00:00:00+00'::timestamptz
FROM load_scopes
UNION ALL
SELECT shipment_document_id,organization_id,warehouse_id,owner_id,'shipment','load-history-shipment-'||scope_id,
       'closed',1+2*reservation_count,1+2*reservation_count,'2025-03-01 00:00:00+00'::timestamptz
FROM load_scopes;

INSERT INTO warehouse.document_lines(id,document_id,organization_id,warehouse_id,owner_id,item_id,line_no,requested_qty,reserved_qty,fulfilled_qty)
SELECT receipt_line_id,receipt_document_id,organization_id,warehouse_id,owner_id,item_id,1,
       greatest(1::bigint,movement_count),0,movement_count
FROM load_scopes
UNION ALL
SELECT shipment_line_id,shipment_document_id,organization_id,warehouse_id,owner_id,item_id,1,1,0,0
FROM load_scopes;

INSERT INTO inventory.stock_balances(id,organization_id,warehouse_id,owner_id,item_id,position_id,stock_state,on_hand,reserved_qty,version)
SELECT md5(c.org::text||':load-balance:'||s.scope_id||':'||p)::uuid,c.org,s.warehouse_id,s.owner_id,c.item,
       md5(c.org::text||':load-position:'||(s.scope_id/100)||':'||p)::uuid,'available',
       CASE WHEN p=1 THEN s.movement_count ELSE 0 END,0,
       CASE WHEN p=1 THEN 1+s.movement_count+2*s.reservation_count ELSE 1 END
FROM load_config c CROSS JOIN load_scopes s
CROSS JOIN LATERAL generate_series(1,c.balances/200) AS p;

-- One unit receipt per historical movement. The complete results permit normal
-- idempotent-result reads, while the receipt ledger exactly backs on_hand.
INSERT INTO platform.operations(id,actor_id,organization_id,warehouse_id,operation_type,request,result,created_at,completed_at)
SELECT md5(c.org::text||':load-movement-operation:'||g)::uuid,c.actor,c.org,s.warehouse_id,'movement',
       jsonb_build_object('line',s.receipt_line_id,'expected_version',1+(g-1)/200,'kind','receipt',
         'source',NULL,'target',s.balance_id,'quantity',1,'reservation',NULL,'reason',NULL,'correction_of',NULL),
       jsonb_build_object('operation_id',md5(c.org::text||':load-movement-operation:'||g)::uuid,
         'movement_id',md5(c.org::text||':load-movement:'||g)::uuid,'new_reservation_id',NULL,
         'document_id',s.receipt_document_id,'document_version',2+(g-1)/200,'content_revision',2+(g-1)/200),
       '2025-01-01 00:00:00+00'::timestamptz+g*interval '1 second',
       '2025-01-01 00:00:00+00'::timestamptz+g*interval '1 second'+interval '1 millisecond'
FROM load_config c CROSS JOIN LATERAL generate_series(1,c.movements) AS g
JOIN load_scopes s ON s.scope_id=(g-1)%200;

INSERT INTO inventory.movements(id,operation_id,document_line_id,organization_id,warehouse_id,owner_id,item_id,kind,created_at)
SELECT md5(c.org::text||':load-movement:'||g)::uuid,md5(c.org::text||':load-movement-operation:'||g)::uuid,
       s.receipt_line_id,c.org,s.warehouse_id,s.owner_id,c.item,'receipt',
       '2025-01-01 00:00:00+00'::timestamptz+g*interval '1 second'
FROM load_config c CROSS JOIN LATERAL generate_series(1,c.movements) AS g
JOIN load_scopes s ON s.scope_id=(g-1)%200;

INSERT INTO inventory.movement_lines(movement_id,line_no,target_balance_id,quantity)
SELECT md5(c.org::text||':load-movement:'||g)::uuid,1,s.balance_id,1
FROM load_config c CROSS JOIN LATERAL generate_series(1,c.movements) AS g
JOIN load_scopes s ON s.scope_id=(g-1)%200;

-- Each reservation held one unit and was fully released before the next one
-- for the same scope. Historical receipts precede all reservation operations.
INSERT INTO platform.operations(id,actor_id,organization_id,warehouse_id,operation_type,request,result,created_at,completed_at)
SELECT md5(c.org::text||':load-reserve-operation:'||g)::uuid,c.actor,c.org,s.warehouse_id,'reserve',
       jsonb_build_object('line',s.shipment_line_id,'expected_version',1+2*((g-1)/200),
         'balance',s.balance_id,'quantity',1,'expiry_epoch',NULL),
       jsonb_build_object('operation_id',md5(c.org::text||':load-reserve-operation:'||g)::uuid,
         'reservation_id',md5(c.org::text||':load-reservation:'||g)::uuid,'document_id',s.shipment_document_id,
         'document_version',2+2*((g-1)/200),'content_revision',2+2*((g-1)/200)),
       '2025-03-01 00:00:00+00'::timestamptz+g*interval '1 second',
       '2025-03-01 00:00:00+00'::timestamptz+g*interval '1 second'+interval '1 millisecond'
FROM load_config c CROSS JOIN LATERAL generate_series(1,c.reservations) AS g
JOIN load_scopes s ON s.scope_id=(g-1)%200;

INSERT INTO platform.operations(id,actor_id,organization_id,warehouse_id,operation_type,request,result,created_at,completed_at)
SELECT md5(c.org::text||':load-release-operation:'||g)::uuid,c.actor,c.org,s.warehouse_id,'release_reservation',
       jsonb_build_object('reservation',md5(c.org::text||':load-reservation:'||g)::uuid,
         'expected_version',2+2*((g-1)/200),'quantity',1,'reason','Historical reservation released'),
       jsonb_build_object('operation_id',md5(c.org::text||':load-release-operation:'||g)::uuid,
         'reservation_id',md5(c.org::text||':load-reservation:'||g)::uuid,'document_id',s.shipment_document_id,
         'document_version',3+2*((g-1)/200),'content_revision',3+2*((g-1)/200)),
       '2025-03-01 00:00:00+00'::timestamptz+g*interval '1 second'+interval '500 milliseconds',
       '2025-03-01 00:00:00+00'::timestamptz+g*interval '1 second'+interval '501 milliseconds'
FROM load_config c CROSS JOIN LATERAL generate_series(1,c.reservations) AS g
JOIN load_scopes s ON s.scope_id=(g-1)%200;

INSERT INTO inventory.reservations(id,organization_id,warehouse_id,owner_id,item_id,document_line_id,balance_id,quantity,consumed_qty,released_qty,created_at)
SELECT md5(c.org::text||':load-reservation:'||g)::uuid,c.org,s.warehouse_id,s.owner_id,c.item,
       s.shipment_line_id,s.balance_id,1,0,1,'2025-03-01 00:00:00+00'::timestamptz+g*interval '1 second'
FROM load_config c CROSS JOIN LATERAL generate_series(1,c.reservations) AS g
JOIN load_scopes s ON s.scope_id=(g-1)%200;

INSERT INTO inventory.reservation_events(operation_id,reservation_id,kind,quantity,reason,created_at)
SELECT md5(c.org::text||':load-reserve-operation:'||g)::uuid,md5(c.org::text||':load-reservation:'||g)::uuid,
       'reserved',1,NULL,'2025-03-01 00:00:00+00'::timestamptz+g*interval '1 second'
FROM load_config c CROSS JOIN LATERAL generate_series(1,c.reservations) AS g
UNION ALL
SELECT md5(c.org::text||':load-release-operation:'||g)::uuid,md5(c.org::text||':load-reservation:'||g)::uuid,
       'released',1,'Historical reservation released',
       '2025-03-01 00:00:00+00'::timestamptz+g*interval '1 second'+interval '500 milliseconds'
FROM load_config c CROSS JOIN LATERAL generate_series(1,c.reservations) AS g;

INSERT INTO platform.audit_entries(operation_id,action,details,created_at)
SELECT p.id,p.operation_type,p.result,p.completed_at
FROM platform.operations p JOIN load_config c ON p.organization_id=c.org
WHERE p.created_at>='2025-01-01 00:00:00+00'::timestamptz
  AND p.created_at<'2026-01-01 00:00:00+00'::timestamptz;

-- Delivered history is separate from an undelivered outbox/backlog scenario.
INSERT INTO platform.outbox(id,operation_id,event_type,event_version,aggregate_id,payload,created_at,available_at,attempts,delivered_at)
SELECT e.id,p.id,e.event_type,1,(p.result->>'document_id')::uuid,
       jsonb_build_object('event_id',e.id,'event_type',e.event_type,'event_version',1,
         'aggregate_id',(p.result->>'document_id')::uuid,'aggregate_version',p.result->'document_version',
         'organization_id',p.organization_id,'warehouse_id',p.warehouse_id,'actor_id',p.actor_id,
         'occurred_at',p.completed_at,'correlation_id',p.id,'causation_id',p.id,'data',p.result),
       p.completed_at,p.completed_at,1,p.completed_at+interval '1 second'
FROM platform.operations p JOIN load_config c ON p.organization_id=c.org
CROSS JOIN LATERAL (
  SELECT md5(c.org::text||':load-outbox:'||p.id::text)::uuid AS id,
         CASE p.operation_type WHEN 'movement' THEN 'wms.stock.moved'
              WHEN 'reserve' THEN 'wms.stock.reserved' ELSE 'wms.reservation.released' END AS event_type
) e
WHERE p.created_at>='2025-01-01 00:00:00+00'::timestamptz
  AND p.created_at<'2026-01-01 00:00:00+00'::timestamptz;

ANALYZE warehouse.locations;
ANALYZE warehouse.positions;
ANALYZE warehouse.documents;
ANALYZE warehouse.document_lines;
ANALYZE inventory.stock_balances;
ANALYZE inventory.movements;
ANALYZE inventory.movement_lines;
ANALYZE inventory.reservations;
ANALYZE inventory.reservation_events;
ANALYZE platform.operations;
ANALYZE platform.audit_entries;
ANALYZE platform.outbox;
