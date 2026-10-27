BEGIN;
SET LOCAL lock_timeout='2s';
SET LOCAL statement_timeout='30s';

-- Additive contract: existing document views and client module FKs stay intact.
CREATE VIEW contracts_v1.document_details AS
SELECT d.*, coalesce((SELECT jsonb_agg(jsonb_build_object(
 'id',l.id,'line_no',l.line_no,'item_id',l.item_id,
 'requested_qty',l.requested_qty::text,'reserved_qty',l.reserved_qty::text,'fulfilled_qty',l.fulfilled_qty::text)
 ORDER BY l.line_no) FROM warehouse.document_lines l WHERE l.document_id=d.id),'[]'::jsonb) AS lines
FROM contracts_v1.documents d;
REVOKE ALL ON contracts_v1.document_details FROM PUBLIC;

CREATE FUNCTION warehouse.command_document(p_command uuid,p_actor uuid,p_document uuid,p_input jsonb)
RETURNS jsonb LANGUAGE plpgsql SECURITY DEFINER SET search_path=pg_catalog AS $$
DECLARE org uuid; wh uuid; v_owner uuid; v_permission text; action text;
 d warehouse.documents; previous platform.operations; inserted uuid; request jsonb;
 line jsonb; item uuid; qty numeric; n integer:=0; target text; result jsonb;
BEGIN
 PERFORM set_config('lock_timeout','2s',true);
 PERFORM set_config('statement_timeout','5s',true);
 org:=(p_input->>'organization_id')::uuid;wh:=(p_input->>'warehouse_id')::uuid;
 IF p_command IS NULL OR p_actor IS NULL OR org IS NULL OR wh IS NULL OR jsonb_typeof(p_input) IS DISTINCT FROM 'object' THEN
  RAISE EXCEPTION 'invalid_document_input' USING ERRCODE='22023'; END IF;
 IF p_document IS NULL THEN
  v_owner:=(p_input->>'owner_id')::uuid;v_permission:='document.create';action:='document_create';
 ELSE
  SELECT * INTO STRICT d FROM warehouse.documents WHERE id=p_document AND organization_id=org AND warehouse_id=wh;
  v_owner:=d.owner_id;v_permission:='document.transition';action:='document_transition';
 END IF;
 IF NOT EXISTS(SELECT 1 FROM platform.actor_grants g WHERE g.actor_id=p_actor AND g.organization_id=org
 AND g.warehouse_id=wh AND (g.owner_id IS NULL OR g.owner_id=v_owner) AND g.permission=v_permission) THEN
  RAISE EXCEPTION 'forbidden' USING ERRCODE='42501'; END IF;
 request:=jsonb_build_object('document_id',p_document,'input',p_input);
 INSERT INTO platform.operations(id,actor_id,organization_id,warehouse_id,operation_type,request)
 VALUES(p_command,p_actor,org,wh,action,request) ON CONFLICT(id) DO NOTHING RETURNING id INTO inserted;
 IF inserted IS NULL THEN
  SELECT * INTO STRICT previous FROM platform.operations WHERE id=p_command;
  IF previous.actor_id<>p_actor OR previous.operation_type<>action OR previous.request<>request THEN
   RAISE EXCEPTION 'idempotency_key_reused' USING ERRCODE='22023'; END IF;
  RETURN previous.result;
 END IF;
 IF p_document IS NULL THEN
  IF p_input->>'kind' IS NULL OR p_input->>'kind' NOT IN('receipt','shipment','transfer','adjustment')
   OR nullif(btrim(p_input->>'number'),'') IS NULL OR length(p_input->>'number')>100
   OR jsonb_typeof(p_input->'lines') IS DISTINCT FROM 'array' THEN
   RAISE EXCEPTION 'invalid_document_input' USING ERRCODE='22023'; END IF;
  IF jsonb_array_length(p_input->'lines') NOT BETWEEN 1 AND 200 THEN
   RAISE EXCEPTION 'invalid_document_lines' USING ERRCODE='22023'; END IF;
  INSERT INTO warehouse.documents(organization_id,warehouse_id,owner_id,kind,number,state)
  VALUES(org,wh,v_owner,p_input->>'kind',p_input->>'number','draft') RETURNING * INTO d;
  FOR line IN SELECT * FROM jsonb_array_elements(p_input->'lines') LOOP
   item:=(line->>'item_id')::uuid;
   IF jsonb_typeof(line->'quantity') IS DISTINCT FROM 'string' OR line->>'quantity' !~ '^(0|[1-9][0-9]{0,17})(\.[0-9]{1,6})?$' THEN
    RAISE EXCEPTION 'invalid_quantity' USING ERRCODE='22023'; END IF;
   qty:=(line->>'quantity')::numeric;
   PERFORM 1 FROM catalog.items WHERE id=item AND organization_id=org;
   IF NOT FOUND THEN RAISE EXCEPTION 'item_not_found' USING ERRCODE='P0002'; END IF;
   PERFORM inventory.assert_quantity(item,qty);n:=n+1;
   INSERT INTO warehouse.document_lines(document_id,organization_id,warehouse_id,owner_id,item_id,line_no,requested_qty)
   VALUES(d.id,org,wh,v_owner,item,n,qty);
  END LOOP;
 ELSE
  SELECT * INTO STRICT d FROM warehouse.documents WHERE id=p_document FOR UPDATE;
  IF (p_input->>'expected_document_version')::bigint IS DISTINCT FROM d.version THEN RAISE EXCEPTION 'document_version_conflict'; END IF;
  target:=p_input->>'state';
  IF target IS NULL OR NOT((d.state='draft' AND target IN('active','cancelled')) OR (d.state='active' AND target IN('closed','cancelled'))) THEN
   RAISE EXCEPTION 'invalid_document_transition'; END IF;
  IF target='closed' AND EXISTS(SELECT 1 FROM warehouse.document_lines WHERE document_id=d.id AND (fulfilled_qty<>requested_qty OR reserved_qty<>0)) THEN
   RAISE EXCEPTION 'document_incomplete'; END IF;
  IF target='cancelled' AND EXISTS(SELECT 1 FROM warehouse.document_lines WHERE document_id=d.id AND (fulfilled_qty>0 OR reserved_qty>0)) THEN
   RAISE EXCEPTION 'document_has_activity'; END IF;
  UPDATE warehouse.documents SET state=target,version=version+1,content_revision=content_revision+1 WHERE id=d.id RETURNING * INTO d;
 END IF;
 SELECT jsonb_build_object('operation_id',p_command,'document_id',d.id,'document_version',d.version,
 'content_revision',d.content_revision,'state',d.state,'lines',v.lines) INTO result FROM contracts_v1.document_details v WHERE id=d.id;
 RETURN platform.finish_operation(p_command,result,d.id,CASE WHEN p_document IS NULL THEN 'wms.document.created' ELSE 'wms.document.state_changed' END);
END $$;
REVOKE ALL ON FUNCTION warehouse.command_document(uuid,uuid,uuid,jsonb) FROM PUBLIC;
COMMIT;
