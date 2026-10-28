-- Optional, additive profile after core 0009. Run as the core installation owner.
-- Intentionally separate from published migrations and contracts_v1.
BEGIN;
CREATE SCHEMA hardened_private;
CREATE SCHEMA hardened_api;
CREATE SCHEMA hardened_worker;
CREATE SCHEMA hardened_crypto;
CREATE EXTENSION pgcrypto WITH SCHEMA hardened_crypto;
REVOKE ALL ON SCHEMA hardened_private,hardened_api,hardened_worker,hardened_crypto FROM PUBLIC;

CREATE TABLE hardened_private.bindings (
 login name PRIMARY KEY,
 kind text NOT NULL CHECK(kind IN ('api','worker')),
 organization_id uuid NOT NULL,
 warehouse_id uuid NOT NULL,
 FOREIGN KEY(organization_id,warehouse_id) REFERENCES warehouse.warehouses(organization_id,id)
);
CREATE TABLE hardened_private.signing_keys (
 login name PRIMARY KEY REFERENCES hardened_private.bindings(login),
 key bytea NOT NULL CHECK(octet_length(key)=32)
);
CREATE TYPE hardened_private.request_context AS (actor_id uuid,organization_id uuid,warehouse_id uuid);

-- Caller-controlled GUCs are merely transport. Every RPC verifies the MAC again.
-- The MAC covers protocol/login/backend/xid/actor/org/warehouse/expiry, in that order.
CREATE FUNCTION hardened_private.context(p_action text,p_resolve boolean DEFAULT false) RETURNS hardened_private.request_context
LANGUAGE plpgsql SECURITY DEFINER SET search_path=pg_catalog AS $$
DECLARE parts text[]; binding hardened_private.bindings; secret bytea; supplied bytea; expected bytea;
 result hardened_private.request_context; deadline bigint; difference integer:=0; n integer;
BEGIN
 SELECT * INTO binding FROM hardened_private.bindings WHERE login=session_user AND kind='api';
 IF NOT FOUND THEN RAISE EXCEPTION 'request_context_denied' USING ERRCODE='42501'; END IF;
 BEGIN
  parts:=string_to_array(current_setting('wms.context',true),'|');
  IF coalesce(array_length(parts,1),0)<>9 OR parts[1]<>'1' OR parts[2]<>session_user::text
   OR parts[3]<>pg_backend_pid()::text OR parts[4]<>pg_current_xact_id()::text THEN
   RAISE EXCEPTION 'invalid_context'; END IF;
  result:=ROW(parts[5]::uuid,parts[6]::uuid,parts[7]::uuid);
  deadline:=parts[8]::bigint;
  IF deadline<=extract(epoch FROM clock_timestamp()) OR deadline>extract(epoch FROM clock_timestamp())+30
   OR result.organization_id<>binding.organization_id OR result.warehouse_id<>binding.warehouse_id THEN
   RAISE EXCEPTION 'invalid_context'; END IF;
  SELECT key INTO STRICT secret FROM hardened_private.signing_keys WHERE login=session_user;
  supplied:=decode(current_setting('wms.context_mac',true),'hex');
  expected:=hardened_crypto.hmac(convert_to(array_to_string(parts,'|'),'UTF8'),secret,'sha256');
  IF supplied IS NULL OR octet_length(supplied)<>32 THEN RAISE EXCEPTION 'invalid_context'; END IF;
  -- Fixed-work comparison; no prefix-equality timing oracle.
  FOR n IN 0..31 LOOP difference:=difference | (get_byte(supplied,n) # get_byte(expected,n)); END LOOP;
  IF difference<>0 OR parts[9] IS DISTINCT FROM encode(hardened_crypto.digest(convert_to(p_action,'UTF8'),'sha256'),'hex')
   OR NOT (p_resolve AND result.actor_id='00000000-0000-0000-0000-000000000000')
    AND NOT EXISTS(SELECT 1 FROM platform.actors WHERE id=result.actor_id AND issuer<>'wms:system') THEN
   RAISE EXCEPTION 'invalid_context'; END IF;
 EXCEPTION WHEN OTHERS THEN RAISE EXCEPTION 'request_context_denied' USING ERRCODE='42501'; END;
 RETURN result;
END $$;

CREATE FUNCTION hardened_private.allowed(c hardened_private.request_context,p_owner uuid,p_permission text) RETURNS boolean
LANGUAGE sql SET search_path=pg_catalog AS $$
 SELECT EXISTS(SELECT 1 FROM platform.actor_grants g WHERE g.actor_id=c.actor_id
 AND g.organization_id=c.organization_id AND g.warehouse_id=c.warehouse_id
 AND (g.owner_id IS NULL OR g.owner_id=p_owner) AND g.permission=p_permission)
$$;

-- Bounded, owner-filtered reads. No global views are granted to these logins.
CREATE FUNCTION hardened_api.read(p_resource text,p_raw text) RETURNS SETOF jsonb
LANGUAGE plpgsql SECURITY DEFINER SET search_path=pg_catalog AS $$
DECLARE c hardened_private.request_context:=hardened_private.context('read|'||p_resource||'|'||p_raw);
 view_name text; permission text; shape text:='to_jsonb(v)'; f jsonb:=p_raw::jsonb; p_limit integer:=(f->>'limit')::integer;
 extra text:=''; key text; value text; prefix text:=''; source text; candidates text; candidate_where text;
 permitted_owners uuid[]; all_owners boolean;
BEGIN
 IF p_limit IS NULL OR p_limit NOT BETWEEN 1 AND 201 THEN RAISE EXCEPTION 'invalid_page_size' USING ERRCODE='22023'; END IF;
 CASE p_resource
  WHEN 'documents' THEN view_name:='document_details';permission:='document.read';
  WHEN 'stock-balances' THEN view_name:='stock_balances';permission:='stock.read';
  WHEN 'reservations' THEN view_name:='reservations';permission:='reservation.read';
  WHEN 'movements' THEN view_name:='movements';permission:='movement.read';
  WHEN 'operations' THEN view_name:='operations';permission:='operation.read';
  ELSE RAISE EXCEPTION 'invalid_resource' USING ERRCODE='22023';
 END CASE;
 IF p_resource='stock-balances' THEN shape:=shape||'||jsonb_build_object(''on_hand'',on_hand::text,''reserved_qty'',reserved_qty::text,''available_qty'',available_qty::text)'; END IF;
 IF p_resource='reservations' THEN shape:=shape||'||jsonb_build_object(''quantity'',quantity::text,''consumed_qty'',consumed_qty::text,''released_qty'',released_qty::text,''active_qty'',active_qty::text)'; END IF;
 IF p_resource='operations' THEN shape:='jsonb_build_object(''id'',id,''status'',''completed'',''result'',result,''completed_at'',completed_at)'; END IF;
 FOREACH key IN ARRAY ARRAY['owner_id','item_id','position_id','document_line_id'] LOOP
  value:=f->>key;
  IF value IS NOT NULL THEN
   IF p_resource='documents' AND key<>'owner_id' OR key='position_id' AND p_resource<>'stock-balances'
    OR key='document_line_id' AND p_resource='stock-balances' THEN
    RAISE EXCEPTION 'unsupported_filter' USING ERRCODE='22023'; END IF;
   extra:=extra||format(' AND %I=%L::uuid',key,value::uuid);
  END IF;
 END LOOP;
 source:=format('contracts_v1.%I v',view_name);
 IF p_resource='documents' THEN
  SELECT coalesce(bool_or(g.owner_id IS NULL),false),coalesce(array_agg(DISTINCT g.owner_id) FILTER(WHERE g.owner_id IS NOT NULL),'{}'::uuid[])
  INTO all_owners,permitted_owners FROM platform.actor_grants g
  WHERE g.actor_id=c.actor_id AND g.organization_id=c.organization_id AND g.warehouse_id=c.warehouse_id AND g.permission='document.read';
  IF NOT all_owners AND cardinality(permitted_owners)=0 THEN RETURN; END IF;
  candidate_where:='organization_id=$1 AND warehouse_id=$2';
  -- Separate query shapes expose bounds to both custom and generic plans.
  IF f->>'after' IS NOT NULL THEN candidate_where:=candidate_where||' AND id>$5'; END IF;
  IF f->>'id' IS NOT NULL THEN candidate_where:=candidate_where||' AND id=$7'; END IF;
  IF f->>'owner_id' IS NOT NULL THEN
   IF NOT all_owners AND NOT((f->>'owner_id')::uuid=ANY(permitted_owners)) THEN RETURN; END IF;
   candidate_where:=candidate_where||format(' AND owner_id=%L::uuid',(f->>'owner_id')::uuid);
   candidates:=format('SELECT id FROM contracts_v1.documents WHERE %s ORDER BY id LIMIT $6',candidate_where);
  ELSIF all_owners THEN
   candidates:=format('SELECT id FROM contracts_v1.documents WHERE %s ORDER BY id LIMIT $6',candidate_where);
  ELSIF cardinality(permitted_owners)=1 THEN
   candidates:=format('SELECT id FROM contracts_v1.documents WHERE %s AND owner_id=%L::uuid ORDER BY id LIMIT $6',candidate_where,permitted_owners[1]);
  ELSE
   -- Bound each permitted owner's ordered candidates before the merge. An ANY
   -- predicate can otherwise walk other owners to fill a sparse owner's page.
   candidates:=format('SELECT chosen.id FROM unnest(%L::uuid[]) permitted(owner_id)
    CROSS JOIN LATERAL (SELECT id FROM contracts_v1.documents WHERE %s AND owner_id=permitted.owner_id ORDER BY id LIMIT $6) chosen
    ORDER BY chosen.id LIMIT $6',permitted_owners,candidate_where);
  END IF;
  prefix:='WITH document_page AS MATERIALIZED ('||candidates||') ';
  source:='document_page JOIN contracts_v1.document_details v USING(id)';
 END IF;
 RETURN QUERY EXECUTE format('%sSELECT %s FROM %s
  WHERE organization_id=$1 AND warehouse_id=$2 AND hardened_private.allowed($3,owner_id,$4)
  AND ($5 IS NULL OR id>$5) AND ($7 IS NULL OR id=$7) %s %s ORDER BY id LIMIT $6',prefix,shape,source,
  CASE WHEN p_resource='operations' THEN 'AND actor_id=($3).actor_id' ELSE '' END,extra)
 USING c.organization_id,c.warehouse_id,c,permission,(f->>'after')::uuid,p_limit,(f->>'id')::uuid;
END $$;

-- Actor comes only from verified context. Permission checks precede all legacy
-- kernel calls; core guards, idempotency and accounting remain authoritative.
CREATE FUNCTION hardened_api.command(p_command uuid,p_kind text,p_raw text) RETURNS jsonb
LANGUAGE plpgsql SECURITY DEFINER SET search_path=pg_catalog AS $$
DECLARE c hardened_private.request_context:=hardened_private.context('command|'||p_command::text||'|'||p_kind||'|'||p_raw);
 p_input jsonb:=p_raw::jsonb; l warehouse.document_lines; d warehouse.documents; permission text; linked uuid; value uuid;
BEGIN
 IF p_command IS NULL OR jsonb_typeof(p_input) IS DISTINCT FROM 'object' THEN
  RAISE EXCEPTION 'invalid_command' USING ERRCODE='22023'; END IF;
 IF p_kind='document' THEN
  IF (p_input->>'organization_id')::uuid IS DISTINCT FROM c.organization_id
   OR (p_input->>'warehouse_id')::uuid IS DISTINCT FROM c.warehouse_id THEN
   RAISE EXCEPTION 'forbidden' USING ERRCODE='42501'; END IF;
  value:=(p_input->>'document_id')::uuid;
  IF value IS NOT NULL THEN
   SELECT * INTO d FROM warehouse.documents WHERE id=value AND organization_id=c.organization_id AND warehouse_id=c.warehouse_id;
   IF NOT FOUND THEN RAISE EXCEPTION 'forbidden' USING ERRCODE='42501'; END IF;
  END IF;
  RETURN warehouse.command_document(p_command,c.actor_id,value,p_input-'document_id');
 END IF;
 CASE p_kind
  WHEN 'reserve' THEN permission:='reservation.create';linked:=(p_input->>'line')::uuid;
  WHEN 'release' THEN
   permission:='reservation.release';
   SELECT document_line_id INTO linked FROM inventory.reservations WHERE id=(p_input->>'reservation')::uuid
    AND organization_id=c.organization_id AND warehouse_id=c.warehouse_id;
  WHEN 'movement' THEN
   permission:=CASE WHEN p_input->>'kind' IN('adjustment_in','adjustment_out') THEN 'movement.adjust' ELSE 'movement.create' END;
   linked:=(p_input->>'line')::uuid;
  ELSE RAISE EXCEPTION 'invalid_command' USING ERRCODE='22023';
 END CASE;
 SELECT * INTO l FROM warehouse.document_lines WHERE id=linked AND organization_id=c.organization_id AND warehouse_id=c.warehouse_id;
 IF NOT FOUND OR NOT hardened_private.allowed(c,l.owner_id,permission) THEN
  RAISE EXCEPTION 'forbidden' USING ERRCODE='42501'; END IF;
 -- Check optional referenced IDs before the kernel, including correction history.
 FOR value IN SELECT x::uuid FROM jsonb_array_elements_text(jsonb_build_array(p_input->>'balance',p_input->>'source',p_input->>'target')) x WHERE x IS NOT NULL LOOP
  IF NOT EXISTS(SELECT 1 FROM inventory.stock_balances WHERE id=value AND organization_id=c.organization_id AND warehouse_id=c.warehouse_id AND owner_id=l.owner_id) THEN
   RAISE EXCEPTION 'forbidden' USING ERRCODE='42501'; END IF;
 END LOOP;
 IF p_input->>'reservation' IS NOT NULL AND NOT EXISTS(SELECT 1 FROM inventory.reservations WHERE id=(p_input->>'reservation')::uuid
  AND organization_id=c.organization_id AND warehouse_id=c.warehouse_id AND owner_id=l.owner_id) THEN
  RAISE EXCEPTION 'forbidden' USING ERRCODE='42501'; END IF;
 IF p_input->>'correction_of' IS NOT NULL AND NOT EXISTS(SELECT 1 FROM inventory.movements WHERE id=(p_input->>'correction_of')::uuid
  AND organization_id=c.organization_id AND warehouse_id=c.warehouse_id AND owner_id=l.owner_id) THEN
  RAISE EXCEPTION 'forbidden' USING ERRCODE='42501'; END IF;
 CASE p_kind
  WHEN 'reserve' THEN RETURN inventory.reserve_stock(p_command,c.actor_id,l.id,(p_input->>'expected_version')::bigint,
   (p_input->>'balance')::uuid,(p_input->>'quantity')::numeric,(p_input->>'expires_at')::timestamptz);
  WHEN 'release' THEN RETURN inventory.release_reservation(p_command,c.actor_id,(p_input->>'reservation')::uuid,
   (p_input->>'expected_version')::bigint,(p_input->>'quantity')::numeric,p_input->>'reason');
  WHEN 'movement' THEN RETURN inventory.post_movement(p_command,c.actor_id,l.id,(p_input->>'expected_version')::bigint,p_input->>'kind',
   (p_input->>'source')::uuid,(p_input->>'target')::uuid,(p_input->>'quantity')::numeric,
   (p_input->>'reservation')::uuid,p_input->>'reason',(p_input->>'correction_of')::uuid);
 END CASE;
END $$;

-- The API signs this only after OIDC verification. No unsigned identity lookup.
CREATE FUNCTION hardened_api.resolve(p_raw text) RETURNS SETOF jsonb
LANGUAGE plpgsql SECURITY DEFINER SET search_path=pg_catalog AS $$
DECLARE c hardened_private.request_context:=hardened_private.context('resolve|'||p_raw,true); identity jsonb:=p_raw::jsonb;
BEGIN
 IF jsonb_typeof(identity)<>'array' OR jsonb_array_length(identity)<>2 THEN RAISE EXCEPTION 'invalid_identity' USING ERRCODE='22023'; END IF;
 RETURN QUERY SELECT to_jsonb(g) FROM contracts_v1.actor_grants g WHERE issuer=identity->>0 AND subject=identity->>1
 AND organization_id=c.organization_id AND warehouse_id=c.warehouse_id;
END $$;

-- Metadata only: never grant runtime SELECT on module business/fact views.
CREATE FUNCTION hardened_api.module_metadata() RETURNS SETOF jsonb
LANGUAGE plpgsql SECURITY DEFINER SET search_path=pg_catalog AS $$
DECLARE m record; projection regclass;
BEGIN
 PERFORM platform.lock_module_configuration();
 FOR m IN SELECT id,manifest,state,schema_name FROM platform.modules WHERE state='active'
  OR id IN(SELECT module_id FROM platform.required_guards) LOOP
  projection:=to_regclass(format('%I.document_facts_v1',m.schema_name));
  IF projection IS NOT NULL THEN
   EXECUTE format('SELECT organization_id,warehouse_id,document_id,content_revision,payload FROM %I.document_facts_v1 LIMIT 0',m.schema_name);
  ELSIF jsonb_array_length(coalesce(m.manifest->'guards','[]'::jsonb))>0 THEN
   RAISE EXCEPTION 'required_guard_unavailable';
  END IF;
  RETURN NEXT jsonb_build_object('manifest',m.manifest,'state',m.state,'schema',m.schema_name,
   'migrations',coalesce((SELECT jsonb_object_agg(migration_id,checksum) FROM platform.module_migrations WHERE module_id=m.id),'{}'::jsonb));
 END LOOP;
END $$;

CREATE FUNCTION hardened_private.worker_scope() RETURNS hardened_private.bindings
LANGUAGE plpgsql SECURITY DEFINER SET search_path=pg_catalog AS $$
DECLARE b hardened_private.bindings;
BEGIN
 SELECT * INTO b FROM hardened_private.bindings WHERE login=session_user AND kind='worker';
 IF NOT FOUND THEN RAISE EXCEPTION 'worker_role_denied' USING ERRCODE='42501'; END IF;
 RETURN b;
END $$;

CREATE FUNCTION hardened_worker.claim_outbox(p_limit integer DEFAULT 25) RETURNS TABLE(id uuid,payload jsonb,token uuid,attempt integer)
LANGUAGE plpgsql SECURITY DEFINER SET search_path=pg_catalog AS $$
DECLARE b hardened_private.bindings:=hardened_private.worker_scope();
BEGIN
 IF p_limit IS NULL OR p_limit NOT BETWEEN 1 AND 100 THEN RAISE EXCEPTION 'invalid_batch_size' USING ERRCODE='22023'; END IF;
 RETURN QUERY WITH ready AS (
  SELECT o.id FROM platform.outbox o JOIN platform.operations op ON op.id=o.operation_id
  WHERE op.organization_id=b.organization_id AND op.warehouse_id=b.warehouse_id
  AND o.delivered_at IS NULL AND o.dead_at IS NULL AND o.available_at<=clock_timestamp()
  AND (o.leased_until IS NULL OR o.leased_until<=clock_timestamp())
  ORDER BY o.available_at,o.id LIMIT p_limit FOR UPDATE OF o SKIP LOCKED
 ) UPDATE platform.outbox o SET lease_token=gen_random_uuid(),leased_until=clock_timestamp()+interval '30 seconds',attempts=o.attempts+1
 FROM ready WHERE o.id=ready.id RETURNING o.id,o.payload,o.lease_token,o.attempts;
END $$;
CREATE FUNCTION hardened_worker.complete_outbox(p_id uuid,p_token uuid,p_success boolean) RETURNS boolean
LANGUAGE plpgsql SECURITY DEFINER SET search_path=pg_catalog AS $$
DECLARE b hardened_private.bindings:=hardened_private.worker_scope();
BEGIN
 IF NOT EXISTS(SELECT 1 FROM platform.outbox o JOIN platform.operations op ON op.id=o.operation_id
  WHERE o.id=p_id AND op.organization_id=b.organization_id AND op.warehouse_id=b.warehouse_id) THEN RETURN false; END IF;
 RETURN platform.complete_outbox(p_id,p_token,p_success);
END $$;

CREATE FUNCTION hardened_worker.expire_reservations(p_limit integer DEFAULT 100) RETURNS integer
LANGUAGE plpgsql SECURITY DEFINER SET search_path=pg_catalog AS $$
DECLARE b hardened_private.bindings:=hardened_private.worker_scope(); candidate record; r inventory.reservations;
 d warehouse.documents; op uuid; qty numeric; total integer:=0;
BEGIN
 IF p_limit IS NULL OR p_limit NOT BETWEEN 1 AND 1000 THEN RAISE EXCEPTION 'invalid_batch_size' USING ERRCODE='22023'; END IF;
 PERFORM set_config('lock_timeout','2s',true);
 FOR candidate IN SELECT x.id,l.document_id FROM inventory.reservations x JOIN warehouse.document_lines l ON l.id=x.document_line_id
 JOIN warehouse.documents locked_document ON locked_document.id=l.document_id
 WHERE x.organization_id=b.organization_id AND x.warehouse_id=b.warehouse_id
 AND x.active_qty>0 AND x.expires_at<=clock_timestamp() ORDER BY x.expires_at,x.id LIMIT p_limit FOR UPDATE OF locked_document SKIP LOCKED LOOP
  SELECT * INTO d FROM warehouse.documents WHERE id=candidate.document_id FOR UPDATE SKIP LOCKED;
  IF NOT FOUND THEN CONTINUE; END IF;
  SELECT * INTO r FROM inventory.reservations WHERE id=candidate.id;
  PERFORM 1 FROM warehouse.document_lines WHERE id=r.document_line_id FOR UPDATE;
  PERFORM 1 FROM inventory.stock_balances WHERE id=r.balance_id FOR UPDATE;
  SELECT * INTO r FROM inventory.reservations WHERE id=candidate.id FOR UPDATE;
  IF r.active_qty<=0 OR r.expires_at IS NULL OR r.expires_at>clock_timestamp() THEN CONTINUE; END IF;
  qty:=r.active_qty;op:=gen_random_uuid();
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

CREATE FUNCTION hardened_worker.queue_state() RETURNS TABLE(pending bigint,dead bigint,oldest_pending_seconds double precision,expired_reservations bigint,oldest_expired_seconds double precision)
LANGUAGE plpgsql SECURITY DEFINER SET search_path=pg_catalog AS $$
DECLARE b hardened_private.bindings:=hardened_private.worker_scope();
BEGIN
 RETURN QUERY SELECT o.pending,o.dead,coalesce(extract(epoch FROM clock_timestamp()-o.oldest)::double precision,0),r.expired,coalesce(extract(epoch FROM clock_timestamp()-r.oldest)::double precision,0)
 FROM (SELECT count(*) FILTER(WHERE o.dead_at IS NULL) pending,count(*) FILTER(WHERE o.dead_at IS NOT NULL) dead,
 min(o.created_at) FILTER(WHERE o.dead_at IS NULL) oldest FROM platform.outbox o JOIN platform.operations op ON op.id=o.operation_id
 WHERE o.delivered_at IS NULL AND op.organization_id=b.organization_id AND op.warehouse_id=b.warehouse_id) o
 CROSS JOIN (SELECT count(*) expired,min(expires_at) oldest FROM inventory.reservations WHERE organization_id=b.organization_id AND warehouse_id=b.warehouse_id
 AND active_qty>0 AND expires_at<=clock_timestamp()) r;
END $$;

-- Attested startup boundary: reject accidental legacy/group/admin grants.
CREATE FUNCTION hardened_private.profile(p_kind text) RETURNS jsonb
LANGUAGE plpgsql SECURITY DEFINER SET search_path=pg_catalog AS $$
DECLARE b hardened_private.bindings; r record;
BEGIN
 SELECT * INTO b FROM hardened_private.bindings WHERE login=session_user AND kind=p_kind;
 IF NOT FOUND OR p_kind='api' AND NOT EXISTS(SELECT 1 FROM hardened_private.signing_keys WHERE login=session_user) THEN
  RAISE EXCEPTION 'hardened_profile_unavailable' USING ERRCODE='42501'; END IF;
 SELECT * INTO r FROM pg_roles WHERE rolname=session_user;
 IF r.rolsuper OR r.rolcreaterole OR r.rolcreatedb OR r.rolreplication OR r.rolbypassrls
  OR EXISTS(SELECT 1 FROM pg_auth_members WHERE member=r.oid)
  OR has_database_privilege(session_user,current_database(),'CREATE')
  OR EXISTS(SELECT 1 FROM pg_namespace n WHERE n.nspname NOT LIKE 'pg_%' AND n.nspname<>'information_schema'
   AND (has_schema_privilege(session_user,n.oid,'CREATE') OR
    (n.nspname NOT IN('hardened_'||p_kind,'public') AND has_schema_privilege(session_user,n.oid,'USAGE')))) THEN
  RAISE EXCEPTION 'unsafe_hardened_role' USING ERRCODE='42501'; END IF;
 RETURN jsonb_build_object('profile','hardened-v1','login',session_user,'organization_id',b.organization_id,'warehouse_id',b.warehouse_id);
END $$;
CREATE FUNCTION hardened_api.profile() RETURNS jsonb LANGUAGE sql SECURITY DEFINER SET search_path=pg_catalog AS $$ SELECT hardened_private.profile('api') $$;
CREATE FUNCTION hardened_worker.profile() RETURNS jsonb LANGUAGE sql SECURITY DEFINER SET search_path=pg_catalog AS $$ SELECT hardened_private.profile('worker') $$;
REVOKE ALL ON ALL TABLES IN SCHEMA hardened_private FROM PUBLIC;
REVOKE ALL ON ALL FUNCTIONS IN SCHEMA hardened_private,hardened_api,hardened_worker FROM PUBLIC;
COMMIT;
