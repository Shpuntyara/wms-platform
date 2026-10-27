BEGIN;

CREATE FUNCTION inventory.assert_quantity(p_item uuid, p_qty numeric) RETURNS void
LANGUAGE plpgsql SET search_path = pg_catalog AS $$
DECLARE v_scale integer;
BEGIN
  SELECT quantity_scale INTO STRICT v_scale FROM catalog.items WHERE id = p_item;
  IF p_qty IS NULL OR NOT (p_qty > 0 AND p_qty <= 999999999999999999.999999)
     OR trunc(p_qty, v_scale) <> p_qty THEN
    RAISE EXCEPTION 'invalid_quantity' USING ERRCODE = '22023';
  END IF;
END $$;

CREATE FUNCTION inventory.validate_balance() RETURNS trigger
LANGUAGE plpgsql SET search_path = pg_catalog AS $$
DECLARE v_item catalog.items; v_serial catalog.serials;
BEGIN
  IF TG_OP = 'UPDATE' AND
     ROW(NEW.id, NEW.organization_id, NEW.warehouse_id, NEW.owner_id, NEW.item_id,
         NEW.position_id, NEW.stock_state, NEW.lot_id, NEW.serial_id)
     IS DISTINCT FROM
     ROW(OLD.id, OLD.organization_id, OLD.warehouse_id, OLD.owner_id, OLD.item_id,
         OLD.position_id, OLD.stock_state, OLD.lot_id, OLD.serial_id) THEN
    RAISE EXCEPTION 'stock_identity_is_immutable';
  END IF;
  SELECT * INTO STRICT v_item FROM catalog.items WHERE id = NEW.item_id;
  IF (v_item.tracking IN ('lot', 'lot_serial')) <> (NEW.lot_id IS NOT NULL)
     OR (v_item.tracking IN ('serial', 'lot_serial')) <> (NEW.serial_id IS NOT NULL) THEN
    RAISE EXCEPTION 'tracking_identity_mismatch';
  END IF;
  IF NEW.serial_id IS NOT NULL THEN
    SELECT * INTO STRICT v_serial FROM catalog.serials WHERE id = NEW.serial_id;
    IF v_serial.lot_id IS DISTINCT FROM NEW.lot_id THEN RAISE EXCEPTION 'serial_lot_mismatch'; END IF;
  END IF;
  IF trunc(NEW.on_hand, v_item.quantity_scale) <> NEW.on_hand
     OR trunc(NEW.reserved_qty, v_item.quantity_scale) <> NEW.reserved_qty THEN
    RAISE EXCEPTION 'quantity_scale_mismatch';
  END IF;
  IF NEW.reserved_qty > 0 AND NOT (SELECT allocatable FROM inventory.stock_states WHERE code = NEW.stock_state) THEN
    RAISE EXCEPTION 'stock_state_not_allocatable';
  END IF;
  RETURN NEW;
END $$;
CREATE TRIGGER validate_balance BEFORE INSERT OR UPDATE ON inventory.stock_balances
  FOR EACH ROW EXECUTE FUNCTION inventory.validate_balance();

CREATE FUNCTION catalog.protect_item_accounting() RETURNS trigger
LANGUAGE plpgsql SET search_path = pg_catalog AS $$
BEGIN
  IF ROW(NEW.organization_id, NEW.base_unit, NEW.quantity_scale, NEW.tracking)
      IS DISTINCT FROM ROW(OLD.organization_id, OLD.base_unit, OLD.quantity_scale, OLD.tracking)
     AND (EXISTS (SELECT 1 FROM inventory.stock_balances WHERE item_id = OLD.id)
          OR EXISTS (SELECT 1 FROM warehouse.document_lines WHERE item_id = OLD.id)) THEN
    RAISE EXCEPTION 'item_accounting_requires_explicit_migration';
  END IF;
  RETURN NEW;
END $$;
CREATE TRIGGER protect_item_accounting BEFORE UPDATE ON catalog.items
  FOR EACH ROW EXECUTE FUNCTION catalog.protect_item_accounting();

CREATE FUNCTION platform.reject_history_change() RETURNS trigger
LANGUAGE plpgsql SET search_path = pg_catalog AS $$
BEGIN RAISE EXCEPTION 'append_only_history'; END $$;
CREATE TRIGGER immutable_movements BEFORE UPDATE OR DELETE ON inventory.movements
  FOR EACH ROW EXECUTE FUNCTION platform.reject_history_change();
CREATE TRIGGER immutable_movement_lines BEFORE UPDATE OR DELETE ON inventory.movement_lines
  FOR EACH ROW EXECUTE FUNCTION platform.reject_history_change();
CREATE TRIGGER immutable_reservation_events BEFORE UPDATE OR DELETE ON inventory.reservation_events
  FOR EACH ROW EXECUTE FUNCTION platform.reject_history_change();
CREATE TRIGGER immutable_audit BEFORE UPDATE OR DELETE ON platform.audit_entries
  FOR EACH ROW EXECUTE FUNCTION platform.reject_history_change();

CREATE FUNCTION platform.require_completed_operation() RETURNS trigger
LANGUAGE plpgsql SECURITY DEFINER SET search_path = pg_catalog AS $$
BEGIN
  IF EXISTS (SELECT 1 FROM platform.operations WHERE id = NEW.id AND result IS NULL) THEN
    RAISE EXCEPTION 'incomplete_operation_cannot_commit';
  END IF;
  RETURN NULL;
END $$;
CREATE CONSTRAINT TRIGGER completed_operation AFTER INSERT OR UPDATE ON platform.operations
  DEFERRABLE INITIALLY DEFERRED FOR EACH ROW EXECUTE FUNCTION platform.require_completed_operation();

-- Internal helper. Unique operation ID serializes identical concurrent retries.
CREATE FUNCTION platform.begin_operation(p_id uuid, p_actor uuid, p_line uuid, p_type text, p_request jsonb)
RETURNS jsonb LANGUAGE plpgsql SET search_path = pg_catalog AS $$
DECLARE v_line warehouse.document_lines; v_existing platform.operations; v_inserted uuid;
BEGIN
  SELECT * INTO STRICT v_line FROM warehouse.document_lines WHERE id = p_line;
  INSERT INTO platform.operations(id, actor_id, organization_id, warehouse_id, operation_type, request)
  VALUES (p_id, p_actor, v_line.organization_id, v_line.warehouse_id, p_type, p_request)
  ON CONFLICT (id) DO NOTHING RETURNING id INTO v_inserted;
  IF v_inserted IS NOT NULL THEN RETURN NULL; END IF;
  SELECT * INTO STRICT v_existing FROM platform.operations WHERE id = p_id;
  IF v_existing.actor_id <> p_actor OR v_existing.operation_type <> p_type
     OR v_existing.request <> p_request THEN
    RAISE EXCEPTION 'idempotency_key_reused' USING ERRCODE = '22023';
  END IF;
  -- A pending row is visible here only inside this transaction: the unique
  -- insert waits on other transactions, and the deferred constraint forbids
  -- committing a pending row. This permits prepare -> guards -> post.
  RETURN v_existing.result;
END $$;

CREATE FUNCTION inventory.movement_request(p_line uuid, p_expected_version bigint, p_kind text,
  p_source uuid, p_target uuid, p_qty numeric, p_reservation uuid, p_reason text, p_correction_of uuid)
RETURNS jsonb LANGUAGE sql IMMUTABLE SET search_path = pg_catalog AS $$
  SELECT jsonb_build_object('line', p_line, 'expected_version', p_expected_version, 'kind', p_kind,
    'source', p_source, 'target', p_target, 'quantity', p_qty, 'reservation', p_reservation,
    'reason', p_reason, 'correction_of', p_correction_of)
$$;

CREATE FUNCTION inventory.prepare_movement(p_command uuid, p_actor uuid, p_line uuid,
  p_expected_version bigint, p_kind text, p_source uuid, p_target uuid, p_qty numeric,
  p_reservation uuid DEFAULT NULL, p_reason text DEFAULT NULL, p_correction_of uuid DEFAULT NULL)
RETURNS jsonb LANGUAGE plpgsql SECURITY DEFINER SET search_path = pg_catalog AS $$
DECLARE v_saved jsonb; v_line warehouse.document_lines; v_snapshot jsonb;
BEGIN
  v_saved := platform.begin_operation(p_command, p_actor, p_line, 'movement',
    inventory.movement_request(p_line, p_expected_version, p_kind, p_source, p_target, p_qty, p_reservation, p_reason, p_correction_of));
  IF v_saved IS NOT NULL THEN RETURN jsonb_build_object('replayed', true, 'result', v_saved); END IF;
  v_line := warehouse.lock_line(p_line, p_expected_version);
  PERFORM inventory.assert_quantity(v_line.item_id, p_qty);
  v_snapshot := warehouse.lock_document(v_line.document_id, p_expected_version);
  RETURN jsonb_build_object('replayed', false, 'document', v_snapshot);
END $$;

CREATE FUNCTION warehouse.lock_line(p_line uuid, p_expected_version bigint)
RETURNS warehouse.document_lines LANGUAGE plpgsql SET search_path = pg_catalog AS $$
DECLARE v_line warehouse.document_lines; v_doc warehouse.documents;
BEGIN
  SELECT * INTO STRICT v_line FROM warehouse.document_lines WHERE id = p_line;
  SELECT * INTO STRICT v_doc FROM warehouse.documents WHERE id = v_line.document_id FOR UPDATE;
  IF p_expected_version IS NULL OR v_doc.version <> p_expected_version THEN
    RAISE EXCEPTION 'document_version_conflict';
  END IF;
  IF v_doc.state <> 'active' THEN RAISE EXCEPTION 'document_not_active'; END IF;
  SELECT * INTO STRICT v_line FROM warehouse.document_lines WHERE id = p_line FOR UPDATE;
  RETURN v_line;
END $$;

-- Extension host uses this in the SAME transaction as its guard and posting.
CREATE FUNCTION warehouse.lock_document(p_document uuid, p_expected_version bigint)
RETURNS jsonb LANGUAGE plpgsql SECURITY DEFINER SET search_path = pg_catalog AS $$
DECLARE v_doc warehouse.documents;
BEGIN
  SELECT * INTO STRICT v_doc FROM warehouse.documents WHERE id = p_document FOR UPDATE;
  IF p_expected_version IS NULL OR v_doc.version <> p_expected_version THEN RAISE EXCEPTION 'document_version_conflict'; END IF;
  IF v_doc.state <> 'active' THEN RAISE EXCEPTION 'document_not_active'; END IF;
  RETURN jsonb_build_object('id', v_doc.id, 'organization_id', v_doc.organization_id,
    'warehouse_id', v_doc.warehouse_id, 'owner_id', v_doc.owner_id, 'kind', v_doc.kind,
    'version', v_doc.version, 'content_revision', v_doc.content_revision);
END $$;

CREATE FUNCTION platform.get_operation_result(p_command uuid, p_actor uuid, p_org uuid, p_warehouse uuid)
RETURNS jsonb LANGUAGE sql SECURITY DEFINER SET search_path = pg_catalog AS $$
  SELECT result FROM platform.operations WHERE id = p_command AND actor_id = p_actor
    AND organization_id = p_org AND warehouse_id = p_warehouse
$$;

CREATE FUNCTION platform.finish_operation(p_id uuid, p_result jsonb, p_aggregate uuid, p_event text)
RETURNS jsonb LANGUAGE plpgsql SET search_path = pg_catalog AS $$
DECLARE v_op platform.operations; v_event uuid := gen_random_uuid(); v_occurred timestamptz := clock_timestamp();
BEGIN
  UPDATE platform.operations SET result = p_result, completed_at = clock_timestamp()
    WHERE id = p_id AND result IS NULL RETURNING * INTO STRICT v_op;
  INSERT INTO platform.audit_entries(operation_id, action, details) VALUES (p_id, v_op.operation_type, p_result);
  INSERT INTO platform.outbox(id, operation_id, event_type, aggregate_id, created_at, payload)
  VALUES (v_event, p_id, p_event, p_aggregate, v_occurred,
    jsonb_build_object('event_id', v_event, 'event_type', p_event, 'event_version', 1,
      'aggregate_id', p_aggregate, 'aggregate_version', p_result->'document_version',
      'organization_id', v_op.organization_id, 'warehouse_id', v_op.warehouse_id,
      'actor_id', v_op.actor_id, 'occurred_at', v_occurred, 'correlation_id', p_id,
      'causation_id', p_id, 'data', p_result));
  RETURN p_result;
END $$;

-- Public kernel primitive. Zero balance identities may exist without movements.
CREATE FUNCTION inventory.ensure_balance(p_org uuid, p_warehouse uuid, p_owner uuid, p_item uuid,
  p_position uuid, p_state text, p_lot uuid DEFAULT NULL, p_serial uuid DEFAULT NULL)
RETURNS uuid LANGUAGE plpgsql SECURITY DEFINER SET search_path = pg_catalog AS $$
DECLARE v_id uuid;
BEGIN
  INSERT INTO inventory.stock_balances(organization_id, warehouse_id, owner_id, item_id, position_id, stock_state, lot_id, serial_id)
  VALUES (p_org, p_warehouse, p_owner, p_item, p_position, p_state, p_lot, p_serial)
  ON CONFLICT (organization_id, warehouse_id, owner_id, item_id, position_id, stock_state, lot_id, serial_id)
  DO NOTHING RETURNING id INTO v_id;
  IF v_id IS NULL THEN
    SELECT id INTO STRICT v_id FROM inventory.stock_balances
    WHERE organization_id = p_org AND warehouse_id = p_warehouse AND owner_id = p_owner AND item_id = p_item
      AND position_id = p_position AND stock_state = p_state
      AND lot_id IS NOT DISTINCT FROM p_lot AND serial_id IS NOT DISTINCT FROM p_serial;
  END IF;
  RETURN v_id;
END $$;

CREATE FUNCTION inventory.reserve_stock(p_command uuid, p_actor uuid, p_line uuid,
  p_expected_version bigint, p_balance uuid, p_qty numeric, p_expires_at timestamptz DEFAULT NULL)
RETURNS jsonb LANGUAGE plpgsql SECURITY DEFINER SET search_path = pg_catalog AS $$
DECLARE v_line warehouse.document_lines; v_doc warehouse.documents; v_balance inventory.stock_balances;
  v_saved jsonb; v_reservation uuid;
BEGIN
  v_saved := platform.begin_operation(p_command, p_actor, p_line, 'reserve',
    jsonb_build_object('line', p_line, 'expected_version', p_expected_version, 'balance', p_balance,
      'quantity', p_qty, 'expiry_epoch', extract(epoch FROM p_expires_at)));
  IF v_saved IS NOT NULL THEN RETURN v_saved; END IF;
  v_line := warehouse.lock_line(p_line, p_expected_version);
  PERFORM inventory.assert_quantity(v_line.item_id, p_qty);
  SELECT * INTO STRICT v_doc FROM warehouse.documents WHERE id = v_line.document_id;
  IF v_doc.kind <> 'shipment' THEN RAISE EXCEPTION 'reservation_requires_shipment'; END IF;
  IF p_expires_at IS NOT NULL AND p_expires_at <= clock_timestamp() THEN RAISE EXCEPTION 'reservation_expiry_in_past'; END IF;
  SELECT * INTO STRICT v_balance FROM inventory.stock_balances WHERE id = p_balance FOR UPDATE;
  IF ROW(v_balance.organization_id, v_balance.warehouse_id, v_balance.owner_id, v_balance.item_id)
     <> ROW(v_line.organization_id, v_line.warehouse_id, v_line.owner_id, v_line.item_id) THEN
    RAISE EXCEPTION 'stock_scope_mismatch';
  END IF;
  IF NOT (SELECT allocatable FROM inventory.stock_states WHERE code = v_balance.stock_state) THEN
    RAISE EXCEPTION 'stock_state_not_allocatable';
  END IF;
  IF v_balance.on_hand - v_balance.reserved_qty < p_qty THEN RAISE EXCEPTION 'insufficient_available_stock'; END IF;
  UPDATE warehouse.document_lines SET reserved_qty = reserved_qty + p_qty WHERE id = p_line;
  UPDATE inventory.stock_balances SET reserved_qty = reserved_qty + p_qty, version = version + 1 WHERE id = p_balance;
  INSERT INTO inventory.reservations(organization_id, warehouse_id, owner_id, item_id, document_line_id, balance_id, quantity, expires_at)
  VALUES (v_line.organization_id, v_line.warehouse_id, v_line.owner_id, v_line.item_id, p_line, p_balance, p_qty, p_expires_at)
  RETURNING id INTO v_reservation;
  INSERT INTO inventory.reservation_events(operation_id, reservation_id, kind, quantity)
    VALUES (p_command, v_reservation, 'reserved', p_qty);
  UPDATE warehouse.documents SET version = version + 1, content_revision = content_revision + 1
    WHERE id = v_line.document_id RETURNING * INTO v_doc;
  RETURN platform.finish_operation(p_command,
    jsonb_build_object('operation_id', p_command, 'reservation_id', v_reservation,
      'document_id', v_doc.id, 'document_version', v_doc.version, 'content_revision', v_doc.content_revision),
    v_doc.id, 'wms.stock.reserved');
END $$;

CREATE FUNCTION inventory.release_reservation(p_command uuid, p_actor uuid, p_reservation uuid,
  p_expected_version bigint, p_qty numeric, p_reason text)
RETURNS jsonb LANGUAGE plpgsql SECURITY DEFINER SET search_path = pg_catalog AS $$
DECLARE v_res inventory.reservations; v_line warehouse.document_lines; v_doc warehouse.documents; v_saved jsonb;
BEGIN
  SELECT * INTO STRICT v_res FROM inventory.reservations WHERE id = p_reservation;
  v_saved := platform.begin_operation(p_command, p_actor, v_res.document_line_id, 'release_reservation',
    jsonb_build_object('reservation', p_reservation, 'expected_version', p_expected_version, 'quantity', p_qty, 'reason', p_reason));
  IF v_saved IS NOT NULL THEN RETURN v_saved; END IF;
  v_line := warehouse.lock_line(v_res.document_line_id, p_expected_version);
  PERFORM inventory.assert_quantity(v_line.item_id, p_qty);
  IF nullif(btrim(p_reason), '') IS NULL THEN RAISE EXCEPTION 'release_reason_required'; END IF;
  PERFORM 1 FROM inventory.stock_balances WHERE id = v_res.balance_id FOR UPDATE;
  SELECT * INTO STRICT v_res FROM inventory.reservations WHERE id = p_reservation FOR UPDATE;
  IF v_res.active_qty < p_qty THEN RAISE EXCEPTION 'insufficient_active_reservation'; END IF;
  UPDATE inventory.reservations SET released_qty = released_qty + p_qty WHERE id = p_reservation;
  UPDATE inventory.stock_balances SET reserved_qty = reserved_qty - p_qty, version = version + 1 WHERE id = v_res.balance_id;
  UPDATE warehouse.document_lines SET reserved_qty = reserved_qty - p_qty WHERE id = v_line.id;
  INSERT INTO inventory.reservation_events(operation_id, reservation_id, kind, quantity, reason)
    VALUES (p_command, p_reservation, 'released', p_qty, p_reason);
  UPDATE warehouse.documents SET version = version + 1, content_revision = content_revision + 1
    WHERE id = v_line.document_id RETURNING * INTO v_doc;
  RETURN platform.finish_operation(p_command,
    jsonb_build_object('operation_id', p_command, 'reservation_id', p_reservation,
      'document_id', v_doc.id, 'document_version', v_doc.version, 'content_revision', v_doc.content_revision),
    v_doc.id, 'wms.reservation.released');
END $$;

CREATE FUNCTION inventory.post_movement(p_command uuid, p_actor uuid, p_line uuid,
  p_expected_version bigint, p_kind text, p_source uuid, p_target uuid, p_qty numeric,
  p_reservation uuid DEFAULT NULL, p_reason text DEFAULT NULL, p_correction_of uuid DEFAULT NULL)
RETURNS jsonb LANGUAGE plpgsql SECURITY DEFINER SET search_path = pg_catalog AS $$
DECLARE v_line warehouse.document_lines; v_doc warehouse.documents;
  v_source inventory.stock_balances; v_target inventory.stock_balances; v_res inventory.reservations;
  v_saved jsonb; v_movement uuid; v_new_reservation uuid; v_corrected inventory.movements;
BEGIN
  v_saved := platform.begin_operation(p_command, p_actor, p_line, 'movement',
    inventory.movement_request(p_line, p_expected_version, p_kind, p_source, p_target, p_qty, p_reservation, p_reason, p_correction_of));
  IF v_saved IS NOT NULL THEN RETURN v_saved; END IF;
  v_line := warehouse.lock_line(p_line, p_expected_version);
  PERFORM inventory.assert_quantity(v_line.item_id, p_qty);
  SELECT * INTO STRICT v_doc FROM warehouse.documents WHERE id = v_line.document_id;
  IF p_kind IS NULL OR p_kind NOT IN ('receipt', 'transfer', 'issue', 'adjustment_in', 'adjustment_out') THEN
    RAISE EXCEPTION 'invalid_movement_kind';
  END IF;
  IF (p_kind IN ('receipt', 'adjustment_in') AND (p_source IS NOT NULL OR p_target IS NULL OR p_reservation IS NOT NULL))
     OR (p_kind IN ('issue', 'adjustment_out') AND (p_source IS NULL OR p_target IS NOT NULL))
     OR (p_kind = 'transfer' AND (p_source IS NULL OR p_target IS NULL OR p_source = p_target))
     OR (p_kind = 'adjustment_out' AND p_reservation IS NOT NULL)
     OR (p_kind = 'issue' AND p_reservation IS NULL) THEN
    RAISE EXCEPTION 'invalid_movement_shape';
  END IF;
  IF (p_kind = 'receipt' AND v_doc.kind <> 'receipt')
     OR (p_kind = 'issue' AND v_doc.kind <> 'shipment')
     OR (p_kind IN ('adjustment_in', 'adjustment_out') AND v_doc.kind <> 'adjustment')
     OR (p_kind = 'transfer' AND p_reservation IS NULL AND v_doc.kind <> 'transfer')
     OR (p_kind = 'transfer' AND p_reservation IS NOT NULL AND v_doc.kind <> 'shipment') THEN
    RAISE EXCEPTION 'document_kind_mismatch';
  END IF;
  IF p_kind IN ('adjustment_in', 'adjustment_out') AND nullif(btrim(p_reason), '') IS NULL THEN
    RAISE EXCEPTION 'adjustment_reason_required';
  END IF;
  IF p_correction_of IS NOT NULL THEN
    IF p_kind NOT IN ('adjustment_in', 'adjustment_out') THEN RAISE EXCEPTION 'correction_requires_adjustment'; END IF;
    SELECT * INTO STRICT v_corrected FROM inventory.movements WHERE id = p_correction_of;
    IF ROW(v_corrected.organization_id, v_corrected.warehouse_id, v_corrected.owner_id, v_corrected.item_id)
       <> ROW(v_line.organization_id, v_line.warehouse_id, v_line.owner_id, v_line.item_id) THEN
      RAISE EXCEPTION 'correction_scope_mismatch';
    END IF;
  END IF;
  -- Always lock source and target in UUID order, not in movement direction.
  PERFORM id FROM inventory.stock_balances WHERE id IN (p_source, p_target) ORDER BY id FOR UPDATE;
  IF p_source IS NOT NULL THEN
    SELECT * INTO STRICT v_source FROM inventory.stock_balances WHERE id = p_source;
    IF ROW(v_source.organization_id, v_source.warehouse_id, v_source.owner_id, v_source.item_id)
       <> ROW(v_line.organization_id, v_line.warehouse_id, v_line.owner_id, v_line.item_id) THEN RAISE EXCEPTION 'stock_scope_mismatch'; END IF;
  END IF;
  IF p_target IS NOT NULL THEN
    SELECT * INTO STRICT v_target FROM inventory.stock_balances WHERE id = p_target;
    IF ROW(v_target.organization_id, v_target.warehouse_id, v_target.owner_id, v_target.item_id)
       <> ROW(v_line.organization_id, v_line.warehouse_id, v_line.owner_id, v_line.item_id) THEN RAISE EXCEPTION 'stock_scope_mismatch'; END IF;
  END IF;
  IF p_source IS NOT NULL AND p_target IS NOT NULL AND
    ROW(v_source.lot_id, v_source.serial_id) IS DISTINCT FROM ROW(v_target.lot_id, v_target.serial_id) THEN
    RAISE EXCEPTION 'transfer_tracking_mismatch';
  END IF;
  IF p_reservation IS NOT NULL THEN
    SELECT * INTO STRICT v_res FROM inventory.reservations WHERE id = p_reservation FOR UPDATE;
    IF v_res.balance_id <> p_source OR v_res.document_line_id <> p_line OR v_res.active_qty < p_qty THEN
      RAISE EXCEPTION 'reservation_mismatch';
    END IF;
    IF v_res.expires_at IS NOT NULL AND v_res.expires_at <= clock_timestamp() THEN RAISE EXCEPTION 'reservation_expired'; END IF;
    IF p_kind = 'issue' AND NOT (SELECT shippable FROM inventory.stock_states WHERE code = v_source.stock_state) THEN
      RAISE EXCEPTION 'stock_state_not_shippable';
    END IF;
    IF p_kind = 'transfer' AND NOT (SELECT allocatable FROM inventory.stock_states WHERE code = v_target.stock_state) THEN
      RAISE EXCEPTION 'target_state_not_allocatable';
    END IF;
    UPDATE inventory.stock_balances SET reserved_qty = reserved_qty - p_qty, version = version + 1 WHERE id = p_source;
    IF p_kind = 'issue' THEN
      UPDATE inventory.reservations SET consumed_qty = consumed_qty + p_qty WHERE id = p_reservation;
      UPDATE warehouse.document_lines SET reserved_qty = reserved_qty - p_qty WHERE id = p_line;
      INSERT INTO inventory.reservation_events(operation_id, reservation_id, kind, quantity)
        VALUES (p_command, p_reservation, 'consumed', p_qty);
    ELSE
      -- Relocation releases the source reservation; it does not fulfill the order.
      UPDATE inventory.reservations SET released_qty = released_qty + p_qty WHERE id = p_reservation;
      INSERT INTO inventory.reservation_events(operation_id, reservation_id, kind, quantity, reason)
        VALUES (p_command, p_reservation, 'released', p_qty, 'relocated');
    END IF;
  ELSIF p_source IS NOT NULL AND v_source.on_hand - v_source.reserved_qty < p_qty THEN
    RAISE EXCEPTION 'insufficient_available_stock';
  END IF;
  -- Debit before credit also preserves the unique live location of a serial.
  IF p_source IS NOT NULL THEN
    UPDATE inventory.stock_balances SET on_hand = on_hand - p_qty, version = version + 1 WHERE id = p_source;
  END IF;
  IF p_target IS NOT NULL THEN
    UPDATE inventory.stock_balances SET on_hand = on_hand + p_qty, version = version + 1 WHERE id = p_target;
  END IF;
  IF p_kind = 'transfer' AND p_reservation IS NOT NULL THEN
    INSERT INTO inventory.reservations(organization_id, warehouse_id, owner_id, item_id, document_line_id, balance_id, quantity, expires_at)
    VALUES (v_line.organization_id, v_line.warehouse_id, v_line.owner_id, v_line.item_id, p_line, p_target, p_qty, v_res.expires_at)
    RETURNING id INTO v_new_reservation;
    UPDATE inventory.stock_balances SET reserved_qty = reserved_qty + p_qty, version = version + 1 WHERE id = p_target;
    INSERT INTO inventory.reservation_events(operation_id, reservation_id, kind, quantity)
      VALUES (p_command, v_new_reservation, 'reserved', p_qty);
  ELSE
    UPDATE warehouse.document_lines SET fulfilled_qty = fulfilled_qty + p_qty WHERE id = p_line;
  END IF;
  INSERT INTO inventory.movements(operation_id, document_line_id, organization_id, warehouse_id, owner_id, item_id, kind, reason, correction_of)
  VALUES (p_command, p_line, v_line.organization_id, v_line.warehouse_id, v_line.owner_id, v_line.item_id, p_kind, p_reason, p_correction_of)
  RETURNING id INTO v_movement;
  INSERT INTO inventory.movement_lines(movement_id, source_balance_id, target_balance_id, quantity)
    VALUES (v_movement, p_source, p_target, p_qty);
  UPDATE warehouse.documents SET version = version + 1, content_revision = content_revision + 1
    WHERE id = v_line.document_id RETURNING * INTO v_doc;
  RETURN platform.finish_operation(p_command,
    jsonb_build_object('operation_id', p_command, 'movement_id', v_movement, 'new_reservation_id', v_new_reservation,
      'document_id', v_doc.id, 'document_version', v_doc.version, 'content_revision', v_doc.content_revision),
    v_doc.id, 'wms.stock.moved');
END $$;

REVOKE ALL ON ALL FUNCTIONS IN SCHEMA platform, catalog, warehouse, inventory FROM PUBLIC;
COMMIT;
