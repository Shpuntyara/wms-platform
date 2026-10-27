BEGIN;
INSERT INTO platform.actors(id,issuer,subject) VALUES('00000000-0000-4000-8000-000000000001','wms:system','reservation-expiry');
CREATE FUNCTION inventory.expire_reservations(p_limit integer DEFAULT 100) RETURNS integer
LANGUAGE plpgsql SECURITY DEFINER SET search_path=pg_catalog AS $$
DECLARE candidate record; r inventory.reservations; d warehouse.documents; op uuid; qty numeric; total integer:=0;
BEGIN
 IF p_limit IS NULL OR p_limit<1 OR p_limit>1000 THEN RAISE EXCEPTION 'invalid_batch_size'; END IF;
 PERFORM set_config('lock_timeout','2s',true);
 FOR candidate IN SELECT x.id,l.document_id FROM inventory.reservations x
 JOIN warehouse.document_lines l ON l.id=x.document_line_id
 JOIN warehouse.documents locked_document ON locked_document.id=l.document_id
 WHERE x.active_qty>0 AND x.expires_at<=clock_timestamp() ORDER BY x.expires_at,x.id LIMIT p_limit FOR UPDATE OF locked_document SKIP LOCKED LOOP
  SELECT * INTO d FROM warehouse.documents WHERE id=candidate.document_id FOR UPDATE SKIP LOCKED;
  IF NOT FOUND THEN CONTINUE; END IF;
  SELECT * INTO r FROM inventory.reservations WHERE id=candidate.id;
  PERFORM 1 FROM warehouse.document_lines WHERE id=r.document_line_id FOR UPDATE;
  PERFORM 1 FROM inventory.stock_balances WHERE id=r.balance_id FOR UPDATE;
  SELECT * INTO r FROM inventory.reservations WHERE id=candidate.id FOR UPDATE;
  IF r.active_qty<=0 OR r.expires_at IS NULL OR r.expires_at>clock_timestamp() THEN CONTINUE; END IF;
  qty:=r.active_qty; op:=gen_random_uuid();
  INSERT INTO platform.operations(id,actor_id,organization_id,warehouse_id,operation_type,request)
   VALUES(op,'00000000-0000-4000-8000-000000000001',r.organization_id,r.warehouse_id,'reservation_expiry',jsonb_build_object('reservation_id',r.id));
  UPDATE inventory.reservations SET released_qty=released_qty+qty WHERE id=r.id;
  UPDATE inventory.stock_balances SET reserved_qty=reserved_qty-qty,version=version+1 WHERE id=r.balance_id;
  UPDATE warehouse.document_lines SET reserved_qty=reserved_qty-qty WHERE id=r.document_line_id;
  UPDATE warehouse.documents SET version=version+1,content_revision=content_revision+1 WHERE id=d.id RETURNING * INTO d;
  INSERT INTO inventory.reservation_events(operation_id,reservation_id,kind,quantity,reason) VALUES(op,r.id,'released',qty,'expired');
  PERFORM platform.finish_operation(op,jsonb_build_object('operation_id',op,'reservation_id',r.id,'quantity',qty,
   'document_id',d.id,'document_version',d.version,'content_revision',d.content_revision),d.id,'wms.reservation.expired');
  total:=total+1;
 END LOOP;
 RETURN total;
END $$;

-- One durable relay to an event broker. Broker fan-out owns consumer subscriptions.
ALTER TABLE platform.outbox ADD COLUMN lease_token uuid;
CREATE FUNCTION platform.claim_outbox(p_limit integer DEFAULT 25) RETURNS TABLE(id uuid,payload jsonb,token uuid,attempt integer)
LANGUAGE plpgsql SECURITY DEFINER SET search_path=pg_catalog AS $$
BEGIN
 IF p_limit IS NULL OR p_limit<1 OR p_limit>100 THEN RAISE EXCEPTION 'invalid_batch_size'; END IF;
 RETURN QUERY WITH ready AS (
  SELECT o.id FROM platform.outbox o WHERE o.delivered_at IS NULL AND o.dead_at IS NULL
  AND o.available_at<=clock_timestamp() AND (o.leased_until IS NULL OR o.leased_until<=clock_timestamp())
  ORDER BY o.available_at,o.id LIMIT p_limit FOR UPDATE SKIP LOCKED
 ) UPDATE platform.outbox o SET lease_token=gen_random_uuid(),leased_until=clock_timestamp()+interval '30 seconds',attempts=o.attempts+1
 FROM ready WHERE o.id=ready.id RETURNING o.id,o.payload,o.lease_token,o.attempts;
END $$;
CREATE FUNCTION platform.complete_outbox(p_id uuid,p_token uuid,p_success boolean) RETURNS boolean
LANGUAGE plpgsql SECURITY DEFINER SET search_path=pg_catalog AS $$
DECLARE affected integer;
BEGIN
 UPDATE platform.outbox SET delivered_at=CASE WHEN p_success THEN clock_timestamp() END,
 dead_at=CASE WHEN NOT p_success AND attempts>=12 THEN clock_timestamp() END,
 available_at=clock_timestamp()+least(3600,power(2,least(attempts,12))::integer)*interval '1 second',
 leased_until=NULL,lease_token=NULL
 WHERE id=p_id AND lease_token=p_token AND leased_until>clock_timestamp() AND delivered_at IS NULL AND dead_at IS NULL;
 GET DIAGNOSTICS affected=ROW_COUNT;
 RETURN affected=1;
END $$;
CREATE VIEW contracts_v1.outbox_health AS SELECT count(*) FILTER(WHERE delivered_at IS NULL AND dead_at IS NULL) AS pending,
 count(*) FILTER(WHERE dead_at IS NOT NULL) AS dead, min(created_at) FILTER(WHERE delivered_at IS NULL AND dead_at IS NULL) AS oldest_pending_at FROM platform.outbox;
REVOKE ALL ON FUNCTION inventory.expire_reservations(integer),platform.claim_outbox(integer),platform.complete_outbox(uuid,uuid,boolean) FROM PUBLIC;
REVOKE ALL ON contracts_v1.outbox_health FROM PUBLIC;
COMMIT;
