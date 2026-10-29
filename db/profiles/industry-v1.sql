-- Additive onprem-v1 RPCs; every call verifies the exact signed action again.
BEGIN;
CREATE OR REPLACE FUNCTION hardened_api.work_orders(p_raw text) RETURNS SETOF jsonb
LANGUAGE plpgsql SECURITY DEFINER SET search_path=pg_catalog AS $$
DECLARE c hardened_private.request_context:=hardened_private.context('work_orders|'||p_raw);
BEGIN
 RETURN QUERY SELECT * FROM warehouse.read_work_orders(c.actor_id,c.organization_id,c.warehouse_id,p_raw::jsonb);
END $$;
CREATE OR REPLACE FUNCTION hardened_api.work_order_command(p_command uuid,p_raw text) RETURNS jsonb
LANGUAGE plpgsql SECURITY DEFINER SET search_path=pg_catalog AS $$
DECLARE c hardened_private.request_context:=hardened_private.context('work_order_create|'||p_command::text||'|'||p_raw); input jsonb:=p_raw::jsonb;
BEGIN
 IF (input->>'organization_id')::uuid IS DISTINCT FROM c.organization_id OR (input->>'warehouse_id')::uuid IS DISTINCT FROM c.warehouse_id THEN
 RAISE EXCEPTION 'request_context_denied' USING ERRCODE='42501'; END IF;
 RETURN warehouse.command_work_order(p_command,c.actor_id,input);
END $$;
REVOKE ALL ON FUNCTION hardened_api.work_orders(text),hardened_api.work_order_command(uuid,text) FROM PUBLIC;
COMMIT;
