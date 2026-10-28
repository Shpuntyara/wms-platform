-- Forward-only on-premise profile, installed AFTER unchanged hardened-v1.sql.
-- One installation is one customer's dedicated database. Organizations and
-- warehouses below it are business scopes, never separate hosted customers.
BEGIN;
SET LOCAL lock_timeout='5s';
CREATE TABLE hardened_private.installation (
 singleton boolean PRIMARY KEY DEFAULT true CHECK(singleton),
 id uuid NOT NULL UNIQUE DEFAULT gen_random_uuid() CHECK(id<>'00000000-0000-0000-0000-000000000000')
);
INSERT INTO hardened_private.installation(singleton) VALUES(true);
-- Retain login/kind and signing_keys FK; remove the obsolete per-login scope.
ALTER TABLE hardened_private.bindings DROP COLUMN organization_id,DROP COLUMN warehouse_id;
REVOKE ALL ON hardened_private.installation FROM PUBLIC;

-- Protocol 2 signs exact UTF-8 bytes of:
-- 2|installation|login|backend|full_xid|actor|organization|warehouse|expiry|action_hash
-- GUCs carry the signed value; they never establish identity by themselves.
CREATE OR REPLACE FUNCTION hardened_private.context(p_action text,p_resolve boolean DEFAULT false)
RETURNS hardened_private.request_context
LANGUAGE plpgsql SECURITY DEFINER SET search_path=pg_catalog AS $$
DECLARE parts text[]; installation_id uuid; secret bytea; supplied bytea; expected bytea;
 result hardened_private.request_context; deadline bigint; difference integer:=0; n integer;
 zero_id constant uuid:='00000000-0000-0000-0000-000000000000';
BEGIN
 IF NOT EXISTS(SELECT 1 FROM hardened_private.bindings WHERE login=session_user AND kind='api') THEN
  RAISE EXCEPTION 'request_context_denied' USING ERRCODE='42501'; END IF;
 SELECT id INTO STRICT installation_id FROM hardened_private.installation WHERE singleton;
 BEGIN
  parts:=string_to_array(current_setting('wms.context',true),'|');
  IF coalesce(array_length(parts,1),0)<>10 OR parts[1]<>'2' OR parts[2] IS DISTINCT FROM installation_id::text
   OR parts[3]<>session_user::text OR parts[4]<>pg_backend_pid()::text OR parts[5]<>pg_current_xact_id()::text THEN
   RAISE EXCEPTION 'invalid_context'; END IF;
  result:=ROW(parts[6]::uuid,parts[7]::uuid,parts[8]::uuid);
  deadline:=parts[9]::bigint;
  IF deadline<=extract(epoch FROM clock_timestamp()) OR deadline>extract(epoch FROM clock_timestamp())+30 THEN
   RAISE EXCEPTION 'invalid_context'; END IF;
  SELECT key INTO STRICT secret FROM hardened_private.signing_keys WHERE login=session_user;
  supplied:=decode(current_setting('wms.context_mac',true),'hex');
  expected:=hardened_crypto.hmac(convert_to(array_to_string(parts,'|'),'UTF8'),secret,'sha256');
  IF supplied IS NULL OR octet_length(supplied)<>32 THEN RAISE EXCEPTION 'invalid_context'; END IF;
  FOR n IN 0..31 LOOP difference:=difference | (get_byte(supplied,n) # get_byte(expected,n)); END LOOP;
  IF difference<>0 OR parts[10] IS DISTINCT FROM encode(hardened_crypto.digest(convert_to(p_action,'UTF8'),'sha256'),'hex') THEN
   RAISE EXCEPTION 'invalid_context'; END IF;
 EXCEPTION WHEN OTHERS THEN RAISE EXCEPTION 'request_context_denied' USING ERRCODE='42501'; END;
 IF p_resolve THEN
  IF ROW(result.actor_id,result.organization_id,result.warehouse_id) IS DISTINCT FROM ROW(zero_id,zero_id,zero_id) THEN
   RAISE EXCEPTION 'request_context_denied' USING ERRCODE='42501'; END IF;
 ELSE
  IF result.actor_id=zero_id OR result.organization_id=zero_id OR result.warehouse_id=zero_id
   OR NOT EXISTS(SELECT 1 FROM platform.actors WHERE id=result.actor_id AND issuer<>'wms:system')
   OR NOT EXISTS(SELECT 1 FROM warehouse.warehouses WHERE organization_id=result.organization_id AND id=result.warehouse_id)
   OR NOT EXISTS(SELECT 1 FROM platform.actor_grants WHERE actor_id=result.actor_id
    AND organization_id=result.organization_id AND warehouse_id=result.warehouse_id) THEN
   RAISE EXCEPTION 'forbidden' USING ERRCODE='42501'; END IF;
 END IF;
 RETURN result;
END $$;

CREATE OR REPLACE FUNCTION hardened_api.resolve(p_raw text) RETURNS SETOF jsonb
LANGUAGE plpgsql SECURITY DEFINER SET search_path=pg_catalog AS $$
DECLARE c hardened_private.request_context:=hardened_private.context('resolve|'||p_raw,true); identity jsonb:=p_raw::jsonb;
BEGIN
 IF jsonb_typeof(identity)<>'array' OR jsonb_array_length(identity)<>2 THEN RAISE EXCEPTION 'invalid_identity' USING ERRCODE='22023'; END IF;
 RETURN QUERY SELECT to_jsonb(g) FROM contracts_v1.actor_grants g
 WHERE issuer=identity->>0 AND subject=identity->>1 AND issuer<>'wms:system';
END $$;

-- These functions deliberately delegate the global kernel algorithm. All rows
-- are in this customer's installation; there is no warehouse relay partition.
CREATE OR REPLACE FUNCTION hardened_worker.claim_outbox(p_limit integer DEFAULT 25)
RETURNS TABLE(id uuid,payload jsonb,token uuid,attempt integer)
LANGUAGE plpgsql SECURITY DEFINER SET search_path=pg_catalog AS $$
BEGIN
 PERFORM hardened_private.worker_scope();
 RETURN QUERY SELECT * FROM platform.claim_outbox(p_limit);
END $$;
CREATE OR REPLACE FUNCTION hardened_worker.complete_outbox(p_id uuid,p_token uuid,p_success boolean) RETURNS boolean
LANGUAGE plpgsql SECURITY DEFINER SET search_path=pg_catalog AS $$
BEGIN
 PERFORM hardened_private.worker_scope();
 RETURN platform.complete_outbox(p_id,p_token,p_success);
END $$;
CREATE OR REPLACE FUNCTION hardened_worker.expire_reservations(p_limit integer DEFAULT 100) RETURNS integer
LANGUAGE plpgsql SECURITY DEFINER SET search_path=pg_catalog AS $$
BEGIN
 PERFORM hardened_private.worker_scope();
 RETURN inventory.expire_reservations(p_limit);
END $$;
CREATE OR REPLACE FUNCTION hardened_worker.queue_state()
RETURNS TABLE(pending bigint,dead bigint,oldest_pending_seconds double precision,expired_reservations bigint,oldest_expired_seconds double precision)
LANGUAGE plpgsql SECURITY DEFINER SET search_path=pg_catalog AS $$
BEGIN
 PERFORM hardened_private.worker_scope();
 RETURN QUERY SELECT h.pending,h.dead,coalesce(extract(epoch FROM clock_timestamp()-h.oldest_pending_at)::double precision,0),
 h.expired_reservations,coalesce(extract(epoch FROM clock_timestamp()-h.oldest_expired_at)::double precision,0)
 FROM contracts_v1.background_health h;
END $$;

CREATE OR REPLACE FUNCTION hardened_private.profile(p_kind text) RETURNS jsonb
LANGUAGE plpgsql SECURITY DEFINER SET search_path=pg_catalog AS $$
DECLARE r record; installation_id uuid;
BEGIN
 IF NOT EXISTS(SELECT 1 FROM hardened_private.bindings WHERE login=session_user AND kind=p_kind)
  OR p_kind='api' AND NOT EXISTS(SELECT 1 FROM hardened_private.signing_keys WHERE login=session_user) THEN
  RAISE EXCEPTION 'onprem_profile_unavailable' USING ERRCODE='42501'; END IF;
 SELECT id INTO STRICT installation_id FROM hardened_private.installation WHERE singleton;
 SELECT * INTO r FROM pg_roles WHERE rolname=session_user;
 IF r.rolsuper OR r.rolcreaterole OR r.rolcreatedb OR r.rolreplication OR r.rolbypassrls
  OR EXISTS(SELECT 1 FROM pg_auth_members WHERE member=r.oid)
  OR has_database_privilege(session_user,current_database(),'CREATE')
  OR EXISTS(SELECT 1 FROM pg_namespace n WHERE n.nspname NOT LIKE 'pg_%' AND n.nspname<>'information_schema'
   AND (has_schema_privilege(session_user,n.oid,'CREATE') OR
    (n.nspname NOT IN('hardened_'||p_kind,'public') AND has_schema_privilege(session_user,n.oid,'USAGE')))) THEN
  RAISE EXCEPTION 'unsafe_onprem_role' USING ERRCODE='42501'; END IF;
 RETURN jsonb_build_object('profile','onprem-v1','login',session_user,'installation_id',installation_id);
END $$;
COMMIT;
