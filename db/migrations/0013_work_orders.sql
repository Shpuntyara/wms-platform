BEGIN;
SET LOCAL lock_timeout='2s';
SET LOCAL statement_timeout='30s';

CREATE FUNCTION warehouse.work_order_label(p_value text,p_limit integer) RETURNS boolean
LANGUAGE sql IMMUTABLE STRICT SET search_path=pg_catalog AS $$
 SELECT length(p_value) BETWEEN 1 AND p_limit
 AND length(btrim(p_value,(SELECT string_agg(chr(code),'') FROM unnest(ARRAY[9,10,11,12,13,32,133,160,5760,8192,8193,8194,8195,8196,8197,8198,8199,8200,8201,8202,8232,8233,8239,8287,12288]) code)))>0
 AND p_value !~ ('['||chr(1)||'-'||chr(31)||chr(127)||'-'||chr(159)||']')
$$;
REVOKE ALL ON FUNCTION warehouse.work_order_label(text,integer) FROM PUBLIC;

-- Plans are immutable demand snapshots. They never post stock movements.
CREATE TABLE warehouse.work_orders (
 id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
 organization_id uuid NOT NULL,
 warehouse_id uuid NOT NULL,
 owner_id uuid NOT NULL,
 customer text NOT NULL CHECK(warehouse.work_order_label(customer,200)),
 reference text NOT NULL CHECK(warehouse.work_order_label(reference,100)),
 recipe_id uuid,
 lines jsonb NOT NULL CHECK(jsonb_typeof(lines)='array'),
 requirements jsonb NOT NULL CHECK(jsonb_typeof(requirements)='array'),
 steps jsonb NOT NULL CHECK(jsonb_typeof(steps)='array'),
 state text NOT NULL DEFAULT 'planned' CHECK(state='planned'),
 created_at timestamptz NOT NULL DEFAULT transaction_timestamp(),
 FOREIGN KEY(organization_id,warehouse_id) REFERENCES warehouse.warehouses(organization_id,id),
 FOREIGN KEY(organization_id,owner_id) REFERENCES catalog.stock_owners(organization_id,id),
 UNIQUE(organization_id,warehouse_id,reference)
);
CREATE INDEX work_orders_warehouse_page_idx ON warehouse.work_orders(organization_id,warehouse_id,id);
CREATE INDEX work_orders_owner_page_idx ON warehouse.work_orders(organization_id,owner_id,warehouse_id,id);
CREATE TABLE warehouse.work_order_counters (
 organization_id uuid NOT NULL,
 warehouse_id uuid NOT NULL,
 number_year integer NOT NULL CHECK(number_year BETWEEN 1 AND 9999),
 last_value bigint NOT NULL CHECK(last_value>=0),
 PRIMARY KEY(organization_id,warehouse_id,number_year),
 FOREIGN KEY(organization_id,warehouse_id) REFERENCES warehouse.warehouses(organization_id,id)
);
REVOKE ALL ON warehouse.work_orders,warehouse.work_order_counters FROM PUBLIC;

CREATE FUNCTION warehouse.command_work_order(p_command uuid,p_actor uuid,p_input jsonb)
RETURNS jsonb LANGUAGE plpgsql SECURITY DEFINER SET search_path=pg_catalog AS $$
DECLARE org uuid:=(p_input->>'organization_id')::uuid; wh uuid:=(p_input->>'warehouse_id')::uuid;
 v_owner uuid:=(p_input->>'owner_id')::uuid; inserted uuid; prior platform.operations;
 entry jsonb; item uuid; qty numeric; created warehouse.work_orders; v_reference text:=p_input->>'reference';
 zone text; series_year integer; serial_value bigint; result jsonb; key text;
BEGIN
 PERFORM set_config('lock_timeout','2s',true);
 IF p_command IS NULL OR p_actor IS NULL OR org IS NULL OR wh IS NULL OR v_owner IS NULL
 OR jsonb_typeof(p_input) IS DISTINCT FROM 'object' OR jsonb_typeof(p_input->'request') IS DISTINCT FROM 'object' THEN
 RAISE EXCEPTION 'invalid_work_order' USING ERRCODE='22023'; END IF;
 -- The server-calculated snapshot and replay identity belong to one request.
 -- A different outer owner must never unlock an old operation's result.
 FOREACH key IN ARRAY ARRAY['organization_id','warehouse_id','owner_id','customer'] LOOP
 IF p_input->'request'->>key IS DISTINCT FROM p_input->>key THEN RAISE EXCEPTION 'invalid_work_order' USING ERRCODE='22023'; END IF;
 END LOOP;
 IF coalesce(p_input->'request'->>'reference','') IS DISTINCT FROM coalesce(p_input->>'reference','')
 OR coalesce(p_input->'request'->>'recipe_id','') IS DISTINCT FROM coalesce(p_input->>'recipe_id','')
 OR p_input->'request'->'lines' IS DISTINCT FROM p_input->'lines' THEN RAISE EXCEPTION 'invalid_work_order' USING ERRCODE='22023'; END IF;
 IF NOT EXISTS(SELECT 1 FROM platform.actor_grants WHERE actor_id=p_actor AND organization_id=org
 AND warehouse_id=wh AND (owner_id IS NULL OR owner_id=v_owner) AND permission='document.create') THEN
 RAISE EXCEPTION 'forbidden' USING ERRCODE='42501'; END IF;
 INSERT INTO platform.operations(id,actor_id,organization_id,warehouse_id,operation_type,request)
 VALUES(p_command,p_actor,org,wh,'work_order_create',p_input->'request') ON CONFLICT(id) DO NOTHING RETURNING id INTO inserted;
 IF inserted IS NULL THEN
 SELECT * INTO STRICT prior FROM platform.operations WHERE id=p_command;
 IF prior.actor_id<>p_actor OR prior.organization_id<>org OR prior.warehouse_id<>wh OR prior.operation_type<>'work_order_create'
 OR prior.request IS DISTINCT FROM p_input->'request' THEN RAISE EXCEPTION 'idempotency_key_reused' USING ERRCODE='22023'; END IF;
 IF (prior.result->>'owner_id')::uuid IS DISTINCT FROM v_owner THEN RAISE EXCEPTION 'forbidden' USING ERRCODE='42501'; END IF;
 RETURN prior.result;
 END IF;
 IF jsonb_typeof(p_input->'customer') IS DISTINCT FROM 'string' OR NOT warehouse.work_order_label(p_input->>'customer',200)
 OR v_reference IS NULL OR v_reference<>'' AND NOT warehouse.work_order_label(v_reference,100)
 OR jsonb_typeof(p_input->'lines') IS DISTINCT FROM 'array'
 OR jsonb_typeof(p_input->'requirements') IS DISTINCT FROM 'array' OR jsonb_typeof(p_input->'steps') IS DISTINCT FROM 'array' THEN
 RAISE EXCEPTION 'invalid_work_order' USING ERRCODE='22023'; END IF;
 IF jsonb_array_length(p_input->'lines') NOT BETWEEN 1 AND 200 OR jsonb_array_length(p_input->'requirements') NOT BETWEEN 1 AND 200
 OR jsonb_array_length(p_input->'steps')>20 THEN RAISE EXCEPTION 'invalid_work_order' USING ERRCODE='22023'; END IF;
 FOR entry IN SELECT value FROM jsonb_array_elements((p_input->'lines')||(p_input->'requirements')) LOOP
 item:=(entry->>'item_id')::uuid;
 IF item IS NULL OR jsonb_typeof(entry->'quantity') IS DISTINCT FROM 'string' OR entry->>'quantity' !~ '^(0|[1-9][0-9]{0,17})(\.[0-9]{1,6})?$' THEN
 RAISE EXCEPTION 'invalid_quantity' USING ERRCODE='22023'; END IF;
 qty:=(entry->>'quantity')::numeric;
 IF NOT EXISTS(SELECT 1 FROM catalog.items WHERE id=item AND organization_id=org) THEN RAISE EXCEPTION 'invalid_work_order' USING ERRCODE='22023'; END IF;
 PERFORM inventory.assert_quantity(item,qty);
 END LOOP;
 FOR entry IN SELECT value FROM jsonb_array_elements(p_input->'steps') LOOP
 IF jsonb_typeof(entry) IS DISTINCT FROM 'string' OR NOT warehouse.work_order_label(entry#>>'{}',200) THEN
 RAISE EXCEPTION 'invalid_work_order' USING ERRCODE='22023'; END IF;
 END LOOP;
 IF v_reference='' THEN
 SELECT timezone INTO STRICT zone FROM warehouse.warehouses WHERE id=wh AND organization_id=org;
 series_year:=extract(year FROM transaction_timestamp() AT TIME ZONE zone)::integer;
 INSERT INTO warehouse.work_order_counters(organization_id,warehouse_id,number_year,last_value)
 VALUES(org,wh,series_year,0) ON CONFLICT DO NOTHING;
 LOOP
 UPDATE warehouse.work_order_counters SET last_value=last_value+1 WHERE organization_id=org AND warehouse_id=wh AND number_year=series_year
 RETURNING last_value INTO STRICT serial_value;
 v_reference:='ZAM/'||lpad(series_year::text,4,'0')||'/'||lpad(serial_value::text,greatest(6,length(serial_value::text)),'0');
 EXIT WHEN NOT EXISTS(SELECT 1 FROM warehouse.work_orders WHERE organization_id=org AND warehouse_id=wh AND work_orders.reference=v_reference);
 END LOOP;
 END IF;
 INSERT INTO warehouse.work_orders(organization_id,warehouse_id,owner_id,customer,reference,recipe_id,lines,requirements,steps)
 VALUES(org,wh,v_owner,p_input->>'customer',v_reference,nullif(p_input->>'recipe_id','')::uuid,p_input->'lines',p_input->'requirements',p_input->'steps') RETURNING * INTO created;
 result:=to_jsonb(created)||jsonb_build_object('operation_id',p_command);
 RETURN platform.finish_operation(p_command,result,created.id,'wms.work_order.created');
END $$;
REVOKE ALL ON FUNCTION warehouse.command_work_order(uuid,uuid,jsonb) FROM PUBLIC;

CREATE FUNCTION warehouse.read_work_orders(p_actor uuid,p_org uuid,p_wh uuid,p_input jsonb)
RETURNS SETOF jsonb LANGUAGE plpgsql SECURITY DEFINER SET search_path=pg_catalog AS $$
DECLARE p_limit integer:=(p_input->>'limit')::integer; p_after uuid:=coalesce(nullif(p_input->>'after','')::uuid,'00000000-0000-0000-0000-000000000000'::uuid);
 p_owner uuid:=nullif(p_input->>'owner_id','')::uuid; owners uuid[]; all_owners boolean;
BEGIN
 IF p_limit IS NULL OR p_limit NOT BETWEEN 1 AND 201 THEN RAISE EXCEPTION 'invalid_limit' USING ERRCODE='22023'; END IF;
 SELECT coalesce(bool_or(owner_id IS NULL),false),array_agg(DISTINCT owner_id) FILTER(WHERE owner_id IS NOT NULL) INTO all_owners,owners
 FROM platform.actor_grants WHERE actor_id=p_actor AND organization_id=p_org AND warehouse_id=p_wh AND permission='document.read';
 IF NOT all_owners AND coalesce(cardinality(owners),0)=0 OR p_owner IS NOT NULL AND NOT all_owners AND NOT p_owner=ANY(owners) THEN
 RAISE EXCEPTION 'forbidden' USING ERRCODE='42501'; END IF;
 IF p_owner IS NOT NULL THEN all_owners:=false;owners:=ARRAY[p_owner]; END IF;
 IF all_owners THEN
 RETURN QUERY SELECT to_jsonb(o) FROM warehouse.work_orders o WHERE o.organization_id=p_org AND o.warehouse_id=p_wh AND o.id>p_after ORDER BY o.id LIMIT p_limit;
 ELSE
 RETURN QUERY WITH page AS MATERIALIZED (
 SELECT candidates.id FROM unnest(owners) permitted(owner_id) CROSS JOIN LATERAL (
 SELECT o.id FROM warehouse.work_orders o WHERE o.organization_id=p_org AND o.owner_id=permitted.owner_id AND o.warehouse_id=p_wh AND o.id>p_after
 ORDER BY o.id LIMIT p_limit) candidates ORDER BY candidates.id LIMIT p_limit)
 SELECT to_jsonb(o) FROM page JOIN warehouse.work_orders o ON o.id=page.id ORDER BY o.id;
 END IF;
END $$;
REVOKE ALL ON FUNCTION warehouse.read_work_orders(uuid,uuid,uuid,jsonb) FROM PUBLIC;
COMMIT;
