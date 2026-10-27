BEGIN;

-- Requirements survive disabling a module and removing guards from its manifest.
-- Only the installation owner can uninstall a module or change this policy.
CREATE TABLE platform.required_guards (
 module_id text NOT NULL REFERENCES platform.modules(id) ON DELETE CASCADE,
 point text NOT NULL, handler text NOT NULL,
 PRIMARY KEY(module_id,point,handler)
);
CREATE FUNCTION platform.retain_guard_requirements() RETURNS trigger
LANGUAGE plpgsql SET search_path=pg_catalog AS $$
DECLARE g jsonb;
BEGIN
 IF NEW.state='active' THEN
  FOR g IN SELECT * FROM jsonb_array_elements(NEW.manifest->'guards') LOOP
   IF g->>'required' IS DISTINCT FROM 'true' OR g->>'failure_policy' IS DISTINCT FROM 'deny'
    OR g->>'point' NOT IN ('inventory.before_reservation.v1','inventory.before_release.v1','inventory.before_movement.v1','outbound.shipment_dispatch_guard.v1')
    OR g->>'handler' IS NULL OR g->>'point' IS NULL THEN
    RAISE EXCEPTION 'required_guard_unavailable';
   END IF;
   INSERT INTO platform.required_guards VALUES(NEW.id,g->>'point',g->>'handler') ON CONFLICT DO NOTHING;
  END LOOP;
 END IF;
 RETURN NEW;
END $$;
CREATE TRIGGER retain_guard_requirements AFTER INSERT OR UPDATE ON platform.modules
 FOR EACH ROW EXECUTE FUNCTION platform.retain_guard_requirements();
INSERT INTO platform.required_guards
 SELECT m.id,g->>'point',g->>'handler' FROM platform.modules m,
 LATERAL jsonb_array_elements(m.manifest->'guards') g WHERE m.state='active';
CREATE VIEW contracts_v1.required_modules AS
 SELECT DISTINCT m.id,m.version,m.schema_name,m.state,m.manifest
 FROM platform.modules m JOIN platform.required_guards r ON r.module_id=m.id;


CREATE FUNCTION platform.check_declarative_guard(p_module text,p_handler text,p_line uuid,p_actor uuid) RETURNS void
LANGUAGE plpgsql SET search_path=pg_catalog AS $$
DECLARE m platform.modules; d warehouse.documents; rule jsonb; policy jsonb; facts jsonb; value text; passed boolean; matches integer;
BEGIN
 SELECT * INTO STRICT m FROM platform.modules WHERE id=p_module AND state='active';
 SELECT x.* INTO STRICT d FROM warehouse.documents x JOIN warehouse.document_lines l ON l.document_id=x.id WHERE l.id=p_line;
 SELECT count(*) INTO matches FROM jsonb_array_elements(m.manifest->'policies') p WHERE p->>'handler'=p_handler;
 IF matches<>1 OR m.schema_name::text<>('mod_'||replace(m.id,'.','_')) THEN RAISE EXCEPTION 'required_guard_unavailable'; END IF;
 SELECT p INTO policy FROM jsonb_array_elements(m.manifest->'policies') p WHERE p->>'handler'=p_handler;
 IF jsonb_typeof(policy->'conditions') IS DISTINCT FROM 'array' OR jsonb_array_length(policy->'conditions') NOT BETWEEN 1 AND 32 THEN RAISE EXCEPTION 'required_guard_unavailable'; END IF;
 -- INTO STRICT rejects both missing and ambiguous fact rows.
 BEGIN
  EXECUTE format('SELECT payload FROM %I.document_facts_v1 WHERE organization_id=$1 AND warehouse_id=$2 AND document_id=$3 AND content_revision=$4',m.schema_name)
   INTO STRICT facts USING d.organization_id,d.warehouse_id,d.id,d.content_revision;
 EXCEPTION WHEN no_data_found OR too_many_rows THEN RAISE EXCEPTION 'module_guard_denied'; END;
 FOR rule IN SELECT * FROM jsonb_array_elements(policy->'conditions') LOOP
  IF rule->>'field' !~ '^[a-z][a-z0-9_]{0,62}$' OR jsonb_typeof(facts->(rule->>'field')) IS DISTINCT FROM 'string' THEN RAISE EXCEPTION 'module_guard_denied'; END IF;
  value:=facts->>(rule->>'field'); passed:=false;
  CASE rule->>'op'
   WHEN 'eq' THEN passed:=value=rule->>'value';
   WHEN 'nonempty' THEN passed:=btrim(value)<>'';
   WHEN 'not_actor' THEN passed:=value<>'' AND lower(value)<>lower(p_actor::text);
   WHEN 'future' THEN BEGIN passed:=value ~ '^[0-9]{4}-[0-9]{2}-[0-9]{2}T[0-9]{2}:[0-9]{2}:[0-9]{2}([.][0-9]+)?(Z|[+-][0-9]{2}:[0-9]{2})$' AND value::timestamptz>clock_timestamp(); EXCEPTION WHEN invalid_datetime_format OR datetime_field_overflow THEN passed:=false; END;
   ELSE RAISE EXCEPTION 'required_guard_unavailable';
  END CASE;
  IF passed IS DISTINCT FROM true THEN RAISE EXCEPTION 'module_guard_denied'; END IF;
 END LOOP;
END $$;
REVOKE ALL ON FUNCTION platform.check_declarative_guard(text,text,uuid,uuid) FROM PUBLIC;

-- Owner-installed SQL guards have a fixed signature (line,actor). No session GUC
-- or runtime-minted receipt can assert that a required check has passed.
CREATE TABLE platform.sql_guards (
 module_id text NOT NULL,
 point text NOT NULL, handler text NOT NULL, implementation regprocedure NOT NULL,
 PRIMARY KEY(module_id,point,handler)
);
CREATE FUNCTION platform.assert_required_guards(p_point text,p_line uuid,p_actor uuid) RETURNS void
LANGUAGE plpgsql SET search_path=pg_catalog AS $$
DECLARE m record; g record; proc record;
BEGIN
 -- Serializes policy changes against commands; concurrent commands share the lock.
 LOCK TABLE platform.modules,platform.required_guards,platform.sql_guards IN SHARE MODE;
 FOR m IN SELECT r.*,x.state,x.manifest,x.schema_name FROM platform.required_guards r
 JOIN platform.modules x ON x.id=r.module_id ORDER BY r.module_id,r.point,r.handler LOOP
  IF m.state<>'active' OR NOT EXISTS(SELECT 1 FROM jsonb_array_elements(m.manifest->'guards') j
   WHERE j->>'point'=m.point AND j->>'handler'=m.handler AND j->>'required'='true' AND j->>'failure_policy'='deny') THEN
   RAISE EXCEPTION 'required_guard_unavailable';
  END IF;
  IF m.manifest->>'runtime'='declarative_v1' THEN
   IF has_schema_privilege(current_user,m.schema_name,'USAGE') IS DISTINCT FROM true
    OR has_table_privilege(current_user,to_regclass(format('%I.document_facts_v1',m.schema_name)),'SELECT') IS DISTINCT FROM true THEN
    RAISE EXCEPTION 'required_guard_unavailable';
   END IF;
   IF m.point=p_point THEN PERFORM platform.check_declarative_guard(m.module_id,m.handler,p_line,p_actor); END IF;
  ELSE
   SELECT n.nspname,p.proname,p.pronargs,p.proargtypes,p.prorettype INTO proc
   FROM platform.sql_guards s JOIN pg_proc p ON p.oid=s.implementation::oid
   JOIN pg_namespace n ON n.oid=p.pronamespace
   WHERE s.module_id=m.module_id AND s.point=m.point AND s.handler=m.handler;
   IF NOT FOUND OR proc.nspname<>m.schema_name::text OR proc.pronargs<>2
      OR proc.proargtypes::text<>'2950 2950' OR proc.prorettype<>'void'::regtype THEN
    RAISE EXCEPTION 'required_guard_unavailable';
   END IF;
   IF m.point=p_point THEN EXECUTE format('SELECT %I.%I($1,$2)',proc.nspname,proc.proname) USING p_line,p_actor; END IF;
  END IF;
 END LOOP;
END $$;

REVOKE ALL ON TABLE platform.required_guards,platform.sql_guards FROM PUBLIC;
REVOKE ALL ON contracts_v1.required_modules FROM PUBLIC;
REVOKE ALL ON FUNCTION platform.assert_required_guards(text,uuid,uuid) FROM PUBLIC;
CREATE OR REPLACE FUNCTION inventory.reserve_stock(p_command uuid, p_actor uuid, p_line uuid,
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
  PERFORM platform.assert_required_guards('inventory.before_reservation.v1', v_line.id, p_actor);
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

CREATE OR REPLACE FUNCTION inventory.release_reservation(p_command uuid, p_actor uuid, p_reservation uuid,
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
  PERFORM platform.assert_required_guards('inventory.before_release.v1', v_line.id, p_actor);
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

CREATE OR REPLACE FUNCTION inventory.post_movement(p_command uuid, p_actor uuid, p_line uuid,
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
  PERFORM platform.assert_required_guards('inventory.before_movement.v1', v_line.id, p_actor);
  IF p_kind='issue' THEN PERFORM platform.assert_required_guards('outbound.shipment_dispatch_guard.v1',v_line.id,p_actor); END IF;
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
CREATE FUNCTION platform.lock_module_configuration() RETURNS void
LANGUAGE sql SECURITY DEFINER SET search_path=pg_catalog AS $$
 LOCK TABLE platform.modules,platform.required_guards,platform.sql_guards IN SHARE MODE;
 SELECT platform.assert_required_guards(NULL,NULL,NULL);
$$;
REVOKE ALL ON FUNCTION platform.lock_module_configuration() FROM PUBLIC;
COMMIT;
