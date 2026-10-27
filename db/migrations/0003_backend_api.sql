-- Forward-only addition. Apply with the schema owner, before backend grants.
BEGIN;

CREATE TABLE platform.actor_grants (
  id bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
  actor_id uuid NOT NULL REFERENCES platform.actors(id),
  organization_id uuid NOT NULL,
  warehouse_id uuid NOT NULL,
  owner_id uuid,
  permission text NOT NULL CHECK (permission ~ '^[a-z][a-z0-9_]*(\.[a-z][a-z0-9_]*)+$'),
  FOREIGN KEY (organization_id, warehouse_id) REFERENCES warehouse.warehouses(organization_id, id),
  FOREIGN KEY (organization_id, owner_id) REFERENCES catalog.stock_owners(organization_id, id),
  UNIQUE NULLS NOT DISTINCT (actor_id, organization_id, warehouse_id, owner_id, permission)
);
CREATE INDEX actor_grants_warehouse_idx ON platform.actor_grants(organization_id, warehouse_id);
CREATE INDEX actor_grants_owner_idx ON platform.actor_grants(organization_id, owner_id) WHERE owner_id IS NOT NULL;

CREATE VIEW contracts_v1.actor_grants AS
SELECT a.id AS actor_id, a.issuer, a.subject, g.organization_id, g.warehouse_id, g.owner_id, g.permission
FROM platform.actors a JOIN platform.actor_grants g ON g.actor_id = a.id;

CREATE VIEW contracts_v1.document_lines AS
SELECT l.id, l.document_id, l.organization_id, l.warehouse_id, l.owner_id, l.item_id,
       l.requested_qty, l.reserved_qty, l.fulfilled_qty
FROM warehouse.document_lines l;

CREATE VIEW contracts_v1.reservations AS
SELECT id, organization_id, warehouse_id, owner_id, item_id, document_line_id,
       balance_id, quantity, consumed_qty, released_qty, active_qty, expires_at, created_at
FROM inventory.reservations;

CREATE VIEW contracts_v1.movements AS
SELECT m.id, m.organization_id, m.warehouse_id, m.owner_id, m.item_id, m.operation_id,
       m.document_line_id, m.kind, m.reason, m.correction_of, m.created_at,
       (SELECT jsonb_agg(jsonb_build_object('line_no', l.line_no,
          'source_balance_id', l.source_balance_id, 'target_balance_id', l.target_balance_id,
          'quantity', l.quantity::text) ORDER BY l.line_no)
        FROM inventory.movement_lines l WHERE l.movement_id = m.id) AS lines
FROM inventory.movements m;

CREATE VIEW contracts_v1.operations AS
SELECT p.id, p.actor_id, p.organization_id, p.warehouse_id, d.owner_id, p.result, p.completed_at
FROM platform.operations p JOIN warehouse.documents d ON d.id = (p.result->>'document_id')::uuid
WHERE p.completed_at IS NOT NULL;

CREATE VIEW contracts_v1.active_modules AS
SELECT id, version, manifest FROM platform.modules WHERE state = 'active';

CREATE FUNCTION inventory.prepare_reservation(p_command uuid, p_actor uuid, p_line uuid,
  p_expected_version bigint, p_balance uuid, p_qty numeric, p_expires_at timestamptz DEFAULT NULL)
RETURNS jsonb LANGUAGE plpgsql SECURITY DEFINER SET search_path = pg_catalog AS $$
DECLARE v_saved jsonb; v_line warehouse.document_lines;
BEGIN
  v_saved := platform.begin_operation(p_command, p_actor, p_line, 'reserve',
    jsonb_build_object('line', p_line, 'expected_version', p_expected_version, 'balance', p_balance,
      'quantity', p_qty, 'expiry_epoch', extract(epoch FROM p_expires_at)));
  IF v_saved IS NOT NULL THEN RETURN jsonb_build_object('replayed', true, 'result', v_saved); END IF;
  v_line := warehouse.lock_line(p_line, p_expected_version);
  PERFORM inventory.assert_quantity(v_line.item_id, p_qty);
  RETURN jsonb_build_object('replayed', false, 'document', warehouse.lock_document(v_line.document_id, p_expected_version));
END $$;

CREATE FUNCTION inventory.prepare_release(p_command uuid, p_actor uuid, p_reservation uuid,
  p_expected_version bigint, p_qty numeric, p_reason text)
RETURNS jsonb LANGUAGE plpgsql SECURITY DEFINER SET search_path = pg_catalog AS $$
DECLARE v_saved jsonb; v_res inventory.reservations; v_line warehouse.document_lines;
BEGIN
  SELECT * INTO STRICT v_res FROM inventory.reservations WHERE id = p_reservation;
  v_saved := platform.begin_operation(p_command, p_actor, v_res.document_line_id, 'release_reservation',
    jsonb_build_object('reservation', p_reservation, 'expected_version', p_expected_version,
      'quantity', p_qty, 'reason', p_reason));
  IF v_saved IS NOT NULL THEN RETURN jsonb_build_object('replayed', true, 'result', v_saved); END IF;
  v_line := warehouse.lock_line(v_res.document_line_id, p_expected_version);
  PERFORM inventory.assert_quantity(v_line.item_id, p_qty);
  RETURN jsonb_build_object('replayed', false, 'document', warehouse.lock_document(v_line.document_id, p_expected_version));
END $$;

REVOKE ALL ON platform.actor_grants FROM PUBLIC;
REVOKE ALL ON FUNCTION inventory.prepare_reservation(uuid,uuid,uuid,bigint,uuid,numeric,timestamptz) FROM PUBLIC;
REVOKE ALL ON FUNCTION inventory.prepare_release(uuid,uuid,uuid,bigint,numeric,text) FROM PUBLIC;
COMMIT;
