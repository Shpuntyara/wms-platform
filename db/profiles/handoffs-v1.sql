-- Additive signed department custody RPCs; no new access to base tables.
BEGIN;
CREATE OR REPLACE FUNCTION hardened_api.handoff_context() RETURNS jsonb
LANGUAGE plpgsql SECURITY DEFINER SET search_path=pg_catalog AS $$
DECLARE c hardened_private.request_context:=hardened_private.context('handoff_context');
BEGIN RETURN warehouse.handoff_context(c.actor_id,c.organization_id,c.warehouse_id); END $$;
CREATE OR REPLACE FUNCTION hardened_api.handoffs(p_raw text) RETURNS SETOF jsonb
LANGUAGE plpgsql SECURITY DEFINER SET search_path=pg_catalog AS $$
DECLARE c hardened_private.request_context:=hardened_private.context('handoffs|'||p_raw);
BEGIN RETURN QUERY SELECT * FROM warehouse.read_handoffs(c.actor_id,c.organization_id,c.warehouse_id,p_raw::jsonb); END $$;
CREATE OR REPLACE FUNCTION hardened_api.handoff(p_id uuid) RETURNS jsonb
LANGUAGE plpgsql SECURITY DEFINER SET search_path=pg_catalog AS $$
DECLARE c hardened_private.request_context:=hardened_private.context('handoff|'||p_id::text);
BEGIN RETURN warehouse.read_handoff(c.actor_id,c.organization_id,c.warehouse_id,p_id); END $$;
CREATE OR REPLACE FUNCTION hardened_api.handoff_command(p_command uuid,p_handoff uuid,p_raw text) RETURNS jsonb
LANGUAGE plpgsql SECURITY DEFINER SET search_path=pg_catalog AS $$
DECLARE c hardened_private.request_context:=hardened_private.context('handoff_command|'||p_command::text||'|'||coalesce(p_handoff::text,'')||'|'||p_raw);input jsonb:=p_raw::jsonb;
BEGIN
 IF (input->>'organization_id')::uuid IS DISTINCT FROM c.organization_id OR (input->>'warehouse_id')::uuid IS DISTINCT FROM c.warehouse_id THEN RAISE EXCEPTION 'request_context_denied' USING ERRCODE='42501'; END IF;
 RETURN warehouse.command_handoff(p_command,c.actor_id,p_handoff,input);
END $$;
REVOKE ALL ON FUNCTION hardened_api.handoff_context(),hardened_api.handoffs(text),hardened_api.handoff(uuid),hardened_api.handoff_command(uuid,uuid,text) FROM PUBLIC;
COMMIT;
