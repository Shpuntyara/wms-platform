BEGIN;
SET LOCAL lock_timeout='2s';
SET LOCAL statement_timeout='30s';

-- Counters are transactional rows, not PostgreSQL sequences: rejected commands
-- and caller rollbacks do not consume a warehouse document number.
CREATE TABLE warehouse.document_number_counters (
 organization_id uuid NOT NULL,
 warehouse_id uuid NOT NULL,
 kind text NOT NULL CHECK(kind IN('receipt','transfer')),
 number_year integer NOT NULL CHECK(number_year BETWEEN 1 AND 9999),
 last_value bigint NOT NULL CHECK(last_value>=0),
 PRIMARY KEY(organization_id,warehouse_id,kind,number_year),
 FOREIGN KEY(organization_id,warehouse_id) REFERENCES warehouse.warehouses(organization_id,id)
);
REVOKE ALL ON warehouse.document_number_counters FROM PUBLIC;

-- Only the schema owner/kernel can reserve a series. A manual legacy number
-- outside the automatic namespace remains unchanged and does not use a series.
CREATE FUNCTION warehouse.reserve_document_number(p_org uuid,p_wh uuid,p_kind text,p_number text)
RETURNS void LANGUAGE plpgsql SET search_path=pg_catalog AS $$
DECLARE parts text[]; serial_value numeric; series_year integer; prefix text;
BEGIN
 IF p_kind NOT IN('receipt','transfer') THEN RETURN; END IF;
 prefix:=CASE p_kind WHEN 'receipt' THEN 'PZ' ELSE 'MM' END;
 parts:=regexp_match(p_number,'^'||prefix||'/([0-9]{4})/([0-9]{6,})$');
 IF parts IS NULL THEN RETURN; END IF;
 series_year:=parts[1]::integer;serial_value:=parts[2]::numeric;
 -- Such legacy strings cannot be produced by this bigint allocator. Keeping
 -- them manual preserves compatibility without overflowing the migration.
 IF series_year=0 OR serial_value>9223372036854775807 THEN RETURN; END IF;
 INSERT INTO warehouse.document_number_counters AS counters(organization_id,warehouse_id,kind,number_year,last_value)
 VALUES(p_org,p_wh,p_kind,series_year,serial_value::bigint)
 ON CONFLICT(organization_id,warehouse_id,kind,number_year)
 DO UPDATE SET last_value=greatest(counters.last_value,excluded.last_value);
END $$;
REVOKE ALL ON FUNCTION warehouse.reserve_document_number(uuid,uuid,text,text) FROM PUBLIC;

CREATE FUNCTION warehouse.allocate_document_number(p_org uuid,p_wh uuid,p_kind text,p_at timestamptz)
RETURNS text LANGUAGE plpgsql SET search_path=pg_catalog AS $$
DECLARE zone text; series_year integer; serial_value bigint; prefix text; generated_number text;
BEGIN
 IF p_kind IS NULL OR p_kind NOT IN('receipt','transfer') OR p_at IS NULL THEN
  RAISE EXCEPTION 'invalid_document_input' USING ERRCODE='22023'; END IF;
 SELECT timezone INTO STRICT zone FROM warehouse.warehouses WHERE organization_id=p_org AND id=p_wh;
 series_year:=extract(year FROM p_at AT TIME ZONE zone)::integer;
 prefix:=CASE p_kind WHEN 'receipt' THEN 'PZ' ELSE 'MM' END;
 INSERT INTO warehouse.document_number_counters(organization_id,warehouse_id,kind,number_year,last_value)
 VALUES(p_org,p_wh,p_kind,series_year,0) ON CONFLICT DO NOTHING;
 LOOP
  UPDATE warehouse.document_number_counters SET last_value=last_value+1
  WHERE organization_id=p_org AND warehouse_id=p_wh AND kind=p_kind AND number_year=series_year
  RETURNING last_value INTO STRICT serial_value;
  generated_number:=prefix||'/'||lpad(series_year::text,4,'0')||'/'||lpad(serial_value::text,greatest(6,length(serial_value::text)),'0');
  -- Also tolerate a canonical number inserted administratively after upgrade.
  -- Ordinary manual commands reserve this same locked row before their INSERT.
  IF NOT EXISTS(SELECT 1 FROM warehouse.documents WHERE organization_id=p_org AND warehouse_id=p_wh
   AND kind=p_kind AND number=generated_number) THEN RETURN generated_number; END IF;
 END LOOP;
END $$;
REVOKE ALL ON FUNCTION warehouse.allocate_document_number(uuid,uuid,text,timestamptz) FROM PUBLIC;

-- Seed existing canonical manual numbers without rewriting any document.
INSERT INTO warehouse.document_number_counters(organization_id,warehouse_id,kind,number_year,last_value)
SELECT d.organization_id,d.warehouse_id,d.kind,parsed.parts[1]::integer,max(parsed.parts[2]::numeric)::bigint
FROM warehouse.documents d
CROSS JOIN LATERAL regexp_match(d.number,CASE d.kind WHEN 'receipt' THEN '^PZ/([0-9]{4})/([0-9]{6,})$' ELSE '^MM/([0-9]{4})/([0-9]{6,})$' END) AS parsed(parts)
WHERE d.kind IN('receipt','transfer') AND parsed.parts IS NOT NULL
 AND parsed.parts[1]::integer>0 AND parsed.parts[2]::numeric<=9223372036854775807
GROUP BY d.organization_id,d.warehouse_id,d.kind,parsed.parts[1]::integer;

-- Preserve the published function identity/ACL and hardened RPC delegation.
CREATE OR REPLACE FUNCTION warehouse.command_document(p_command uuid,p_actor uuid,p_document uuid,p_input jsonb)
RETURNS jsonb LANGUAGE plpgsql SECURITY DEFINER SET search_path=pg_catalog AS $$
DECLARE org uuid; wh uuid; v_owner uuid; v_permission text; action text;
 d warehouse.documents; previous platform.operations; inserted uuid; request jsonb;
 line jsonb; item uuid; qty numeric; n integer:=0; target text; result jsonb;
 v_number text; created timestamptz:=transaction_timestamp();
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
  v_number:=p_input->>'number';
  IF p_input->>'kind' IS NULL OR p_input->>'kind' NOT IN('receipt','shipment','transfer','adjustment')
   OR length(v_number)>100 OR (v_number IS NOT NULL AND v_number<>'' AND
    btrim(v_number,E' \t\n\r\f\v'||U&'\0085\00A0\1680\2000\2001\2002\2003\2004\2005\2006\2007\2008\2009\200A\2028\2029\202F\205F\3000')='')
   OR jsonb_typeof(p_input->'lines') IS DISTINCT FROM 'array' THEN
   RAISE EXCEPTION 'invalid_document_input' USING ERRCODE='22023'; END IF;
  IF jsonb_array_length(p_input->'lines') NOT BETWEEN 1 AND 200 THEN
   RAISE EXCEPTION 'invalid_document_lines' USING ERRCODE='22023'; END IF;
  IF coalesce(v_number,'')='' THEN
   v_number:=warehouse.allocate_document_number(org,wh,p_input->>'kind',created);
  ELSE
   PERFORM warehouse.reserve_document_number(org,wh,p_input->>'kind',v_number);
  END IF;
  INSERT INTO warehouse.documents(organization_id,warehouse_id,owner_id,kind,number,state,created_at)
  VALUES(org,wh,v_owner,p_input->>'kind',v_number,'draft',created) RETURNING * INTO d;
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
