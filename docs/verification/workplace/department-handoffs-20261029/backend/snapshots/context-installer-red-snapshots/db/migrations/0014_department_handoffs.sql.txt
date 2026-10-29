BEGIN;
SET LOCAL lock_timeout='2s';
SET LOCAL statement_timeout='30s';

CREATE TABLE warehouse.department_employees (
 actor_id uuid NOT NULL REFERENCES platform.actors(id),
 organization_id uuid NOT NULL REFERENCES platform.organizations(id),
 display_name text NOT NULL CHECK(warehouse.work_order_label(display_name,200)),
 enabled boolean NOT NULL DEFAULT true,
 PRIMARY KEY(organization_id,actor_id)
);
CREATE TABLE warehouse.departments (
 id uuid PRIMARY KEY DEFAULT gen_random_uuid(), organization_id uuid NOT NULL, warehouse_id uuid NOT NULL,
 code text NOT NULL CHECK(warehouse.work_order_label(code,100)), name text NOT NULL CHECK(warehouse.work_order_label(name,200)),
 kind text NOT NULL CHECK(kind IN('warehouse','production')), position_id uuid NOT NULL, enabled boolean NOT NULL DEFAULT true,
 FOREIGN KEY(organization_id,warehouse_id,position_id) REFERENCES warehouse.positions(organization_id,warehouse_id,id),
 UNIQUE(organization_id,warehouse_id,id), UNIQUE(organization_id,warehouse_id,code)
);
CREATE TABLE warehouse.department_positions (
 organization_id uuid NOT NULL, warehouse_id uuid NOT NULL, department_id uuid NOT NULL, position_id uuid PRIMARY KEY,
 FOREIGN KEY(organization_id,warehouse_id,department_id) REFERENCES warehouse.departments(organization_id,warehouse_id,id),
 FOREIGN KEY(organization_id,warehouse_id,position_id) REFERENCES warehouse.positions(organization_id,warehouse_id,id)
);
CREATE INDEX department_positions_department_idx ON warehouse.department_positions(organization_id,warehouse_id,department_id,position_id);
CREATE TABLE warehouse.department_members (
 organization_id uuid NOT NULL, warehouse_id uuid NOT NULL, department_id uuid NOT NULL, actor_id uuid NOT NULL,
 enabled boolean NOT NULL DEFAULT true, PRIMARY KEY(organization_id,warehouse_id,department_id,actor_id),
 FOREIGN KEY(organization_id,warehouse_id,department_id) REFERENCES warehouse.departments(organization_id,warehouse_id,id),
 FOREIGN KEY(organization_id,actor_id) REFERENCES warehouse.department_employees(organization_id,actor_id)
);
CREATE INDEX department_members_actor_idx ON warehouse.department_members(organization_id,warehouse_id,actor_id,department_id);
CREATE TABLE warehouse.department_routes (
 organization_id uuid NOT NULL, warehouse_id uuid NOT NULL, source_department_id uuid NOT NULL, destination_department_id uuid NOT NULL,
 enabled boolean NOT NULL DEFAULT true, PRIMARY KEY(organization_id,warehouse_id,source_department_id,destination_department_id),
 CHECK(source_department_id<>destination_department_id),
 FOREIGN KEY(organization_id,warehouse_id,source_department_id) REFERENCES warehouse.departments(organization_id,warehouse_id,id),
 FOREIGN KEY(organization_id,warehouse_id,destination_department_id) REFERENCES warehouse.departments(organization_id,warehouse_id,id)
);
INSERT INTO inventory.stock_states(code,allocatable,shippable) VALUES('in_transit',false,false);
CREATE TABLE warehouse.handoffs (
 id uuid PRIMARY KEY, organization_id uuid NOT NULL, warehouse_id uuid NOT NULL, owner_id uuid NOT NULL, item_id uuid NOT NULL,
 lot_id uuid, serial_id uuid, source_department_id uuid NOT NULL, destination_department_id uuid NOT NULL,
 source_department_name text NOT NULL, destination_department_name text NOT NULL,
 source_balance_id uuid NOT NULL, transit_balance_id uuid NOT NULL UNIQUE, destination_balance_id uuid NOT NULL,
 source_position_id uuid NOT NULL, transit_position_id uuid NOT NULL UNIQUE, destination_position_id uuid NOT NULL,
 quantity inventory.quantity NOT NULL CHECK(quantity>0), state text NOT NULL CHECK(state IN('sent','accepted')), version bigint NOT NULL,
 sender_actor_id uuid NOT NULL, sender_name text NOT NULL, receiver_actor_id uuid, receiver_name text,
 sent_at timestamptz NOT NULL, accepted_at timestamptz,
 send_document_id uuid NOT NULL REFERENCES warehouse.documents(id), send_line_id uuid NOT NULL REFERENCES warehouse.document_lines(id),
 accept_document_id uuid REFERENCES warehouse.documents(id), accept_line_id uuid REFERENCES warehouse.document_lines(id),
 send_movement_command_id uuid NOT NULL UNIQUE, accept_movement_command_id uuid UNIQUE,
 FOREIGN KEY(organization_id,warehouse_id,owner_id,item_id,source_balance_id) REFERENCES inventory.stock_balances(organization_id,warehouse_id,owner_id,item_id,id),
 FOREIGN KEY(organization_id,warehouse_id,owner_id,item_id,transit_balance_id) REFERENCES inventory.stock_balances(organization_id,warehouse_id,owner_id,item_id,id),
 FOREIGN KEY(organization_id,warehouse_id,owner_id,item_id,destination_balance_id) REFERENCES inventory.stock_balances(organization_id,warehouse_id,owner_id,item_id,id),
 FOREIGN KEY(organization_id,warehouse_id,source_department_id) REFERENCES warehouse.departments(organization_id,warehouse_id,id),
 FOREIGN KEY(organization_id,warehouse_id,destination_department_id) REFERENCES warehouse.departments(organization_id,warehouse_id,id),
 FOREIGN KEY(organization_id,sender_actor_id) REFERENCES warehouse.department_employees(organization_id,actor_id),
 FOREIGN KEY(organization_id,receiver_actor_id) REFERENCES warehouse.department_employees(organization_id,actor_id),
 CHECK(sender_actor_id IS DISTINCT FROM receiver_actor_id),
 CHECK(accepted_at IS NULL OR accepted_at>=sent_at),
 CHECK((state='sent' AND version=1 AND receiver_actor_id IS NULL AND receiver_name IS NULL AND accepted_at IS NULL
 AND accept_document_id IS NULL AND accept_line_id IS NULL AND accept_movement_command_id IS NULL)
 OR (state='accepted' AND version=2 AND receiver_actor_id IS NOT NULL AND receiver_name IS NOT NULL AND accepted_at IS NOT NULL
 AND accept_document_id IS NOT NULL AND accept_line_id IS NOT NULL AND accept_movement_command_id IS NOT NULL)),
 UNIQUE(organization_id,warehouse_id,id)
);
CREATE INDEX handoffs_warehouse_page_idx ON warehouse.handoffs(organization_id,warehouse_id,id);
CREATE INDEX handoffs_owner_page_idx ON warehouse.handoffs(organization_id,owner_id,warehouse_id,id);
CREATE TABLE warehouse.handoff_transit_positions (
 position_id uuid PRIMARY KEY, organization_id uuid NOT NULL, warehouse_id uuid NOT NULL, handoff_id uuid NOT NULL UNIQUE,
 FOREIGN KEY(organization_id,warehouse_id,position_id) REFERENCES warehouse.positions(organization_id,warehouse_id,id),
 FOREIGN KEY(organization_id,warehouse_id,handoff_id) REFERENCES warehouse.handoffs(organization_id,warehouse_id,id) DEFERRABLE INITIALLY DEFERRED
);
REVOKE ALL ON warehouse.department_employees,warehouse.departments,warehouse.department_positions,warehouse.department_members,
 warehouse.department_routes,warehouse.handoffs,warehouse.handoff_transit_positions FROM PUBLIC;

CREATE FUNCTION warehouse.handoff_member(p_actor uuid,p_org uuid,p_wh uuid,p_department uuid) RETURNS boolean
LANGUAGE sql STABLE SET search_path=pg_catalog AS $$
 SELECT EXISTS(SELECT 1 FROM warehouse.department_members m JOIN warehouse.department_employees e
 ON e.organization_id=m.organization_id AND e.actor_id=m.actor_id
 JOIN warehouse.departments d ON d.id=m.department_id AND d.organization_id=m.organization_id AND d.warehouse_id=m.warehouse_id
 WHERE m.actor_id=p_actor AND m.organization_id=p_org AND m.warehouse_id=p_wh AND m.department_id=p_department AND m.enabled AND e.enabled AND d.enabled)
$$;
CREATE FUNCTION warehouse.handoff_authorize(p_actor uuid,p_org uuid,p_wh uuid,p_owner uuid,p_permission text) RETURNS void
LANGUAGE plpgsql SET search_path=pg_catalog AS $$
BEGIN
 IF NOT EXISTS(SELECT 1 FROM platform.actor_grants WHERE actor_id=p_actor AND organization_id=p_org AND warehouse_id=p_wh
 AND (owner_id IS NULL OR owner_id=p_owner) AND permission=p_permission) THEN RAISE EXCEPTION 'forbidden' USING ERRCODE='42501'; END IF;
END $$;
CREATE FUNCTION warehouse.protect_handoff_history() RETURNS trigger
LANGUAGE plpgsql SET search_path=pg_catalog AS $$
BEGIN
 IF TG_OP='DELETE' THEN RAISE EXCEPTION 'append_only_history'; END IF;
 IF OLD.state<>'sent' OR NEW.state<>'accepted' OR OLD.version<>1 OR NEW.version<>2
 OR (to_jsonb(NEW)-ARRAY['state','version','receiver_actor_id','receiver_name','accepted_at','accept_document_id','accept_line_id','accept_movement_command_id'])
 IS DISTINCT FROM (to_jsonb(OLD)-ARRAY['state','version','receiver_actor_id','receiver_name','accepted_at','accept_document_id','accept_line_id','accept_movement_command_id']) THEN
 RAISE EXCEPTION 'append_only_history'; END IF;
 RETURN NEW;
END $$;
CREATE TRIGGER protect_handoff_history BEFORE UPDATE OR DELETE ON warehouse.handoffs FOR EACH ROW EXECUTE FUNCTION warehouse.protect_handoff_history();

-- A managed address is a fixed location, never a movable handling unit.
CREATE FUNCTION warehouse.validate_department_position() RETURNS trigger
LANGUAGE plpgsql SET search_path=pg_catalog AS $$
BEGIN
 IF NOT EXISTS(SELECT 1 FROM warehouse.positions WHERE id=NEW.position_id AND organization_id=NEW.organization_id
 AND warehouse_id=NEW.warehouse_id AND location_id IS NOT NULL AND handling_unit_id IS NULL) THEN RAISE EXCEPTION 'handoff_managed_position'; END IF;
 IF TG_TABLE_NAME='handoff_transit_positions' THEN
 IF EXISTS(SELECT 1 FROM warehouse.department_positions WHERE position_id=NEW.position_id)
 OR EXISTS(SELECT 1 FROM inventory.stock_balances WHERE position_id=NEW.position_id AND (stock_state<>'in_transit' OR on_hand<>0 OR reserved_qty<>0)) THEN RAISE EXCEPTION 'handoff_managed_position'; END IF;
 ELSE
 IF EXISTS(SELECT 1 FROM warehouse.handoff_transit_positions WHERE position_id=NEW.position_id) THEN RAISE EXCEPTION 'handoff_managed_position'; END IF;
 END IF;
 RETURN NEW;
END $$;
CREATE TRIGGER validate_department_position BEFORE INSERT OR UPDATE ON warehouse.departments FOR EACH ROW EXECUTE FUNCTION warehouse.validate_department_position();
CREATE TRIGGER validate_department_position BEFORE INSERT OR UPDATE ON warehouse.department_positions FOR EACH ROW EXECUTE FUNCTION warehouse.validate_department_position();
CREATE TRIGGER validate_department_position BEFORE INSERT OR UPDATE ON warehouse.handoff_transit_positions FOR EACH ROW EXECUTE FUNCTION warehouse.validate_department_position();
CREATE FUNCTION warehouse.protect_managed_position() RETURNS trigger
LANGUAGE plpgsql SET search_path=pg_catalog AS $$
BEGIN
 IF EXISTS(SELECT 1 FROM warehouse.department_positions WHERE position_id=OLD.id)
 OR EXISTS(SELECT 1 FROM warehouse.handoff_transit_positions WHERE position_id=OLD.id)
 OR EXISTS(SELECT 1 FROM warehouse.departments WHERE position_id=OLD.id) THEN
 IF TG_OP='DELETE' OR to_jsonb(NEW) IS DISTINCT FROM to_jsonb(OLD) THEN RAISE EXCEPTION 'handoff_managed_position'; END IF;
 END IF;
 RETURN NEW;
END $$;
CREATE TRIGGER protect_managed_position BEFORE UPDATE OR DELETE ON warehouse.positions FOR EACH ROW EXECUTE FUNCTION warehouse.protect_managed_position();
CREATE FUNCTION warehouse.protect_managed_handling_unit() RETURNS trigger
LANGUAGE plpgsql SET search_path=pg_catalog AS $$
BEGIN
 IF NEW.location_id IS DISTINCT FROM OLD.location_id AND EXISTS(SELECT 1 FROM warehouse.positions p
 WHERE p.handling_unit_id=OLD.id AND (EXISTS(SELECT 1 FROM warehouse.department_positions d WHERE d.position_id=p.id)
 OR EXISTS(SELECT 1 FROM warehouse.handoff_transit_positions t WHERE t.position_id=p.id))) THEN RAISE EXCEPTION 'handoff_managed_position'; END IF;
 RETURN NEW;
END $$;
CREATE TRIGGER protect_managed_handling_unit BEFORE UPDATE ON warehouse.handling_units FOR EACH ROW EXECUTE FUNCTION warehouse.protect_managed_handling_unit();
CREATE FUNCTION inventory.guard_handoff_balance() RETURNS trigger
LANGUAGE plpgsql SECURITY DEFINER SET search_path=pg_catalog AS $$
DECLARE transit boolean;
BEGIN
 SELECT EXISTS(SELECT 1 FROM warehouse.handoff_transit_positions WHERE position_id=NEW.position_id) INTO transit;
 IF transit IS DISTINCT FROM (NEW.stock_state='in_transit') THEN RAISE EXCEPTION 'handoff_managed_stock'; END IF;
 IF NEW.reserved_qty>0 AND EXISTS(SELECT 1 FROM warehouse.department_positions p JOIN warehouse.departments d ON d.id=p.department_id
 WHERE p.position_id=NEW.position_id AND d.kind='production') THEN RAISE EXCEPTION 'handoff_managed_stock'; END IF;
 RETURN NEW;
END $$;
CREATE TRIGGER guard_handoff_balance BEFORE INSERT OR UPDATE ON inventory.stock_balances FOR EACH ROW EXECUTE FUNCTION inventory.guard_handoff_balance();
CREATE FUNCTION inventory.guard_handoff_movement() RETURNS trigger
LANGUAGE plpgsql SECURITY DEFINER SET search_path=pg_catalog AS $$
DECLARE m inventory.movements; op platform.operations; source inventory.stock_balances; target inventory.stock_balances;
 source_dept warehouse.departments; target_dept warehouse.departments; managed boolean;
BEGIN
 LOCK TABLE warehouse.departments,warehouse.department_positions,warehouse.department_members,warehouse.department_employees,warehouse.department_routes IN SHARE MODE;
 SELECT * INTO STRICT m FROM inventory.movements WHERE id=NEW.movement_id;
 SELECT * INTO STRICT op FROM platform.operations WHERE id=m.operation_id;
 SELECT * INTO source FROM inventory.stock_balances WHERE id=NEW.source_balance_id;
 SELECT * INTO target FROM inventory.stock_balances WHERE id=NEW.target_balance_id;
 SELECT d.* INTO source_dept FROM warehouse.department_positions p JOIN warehouse.departments d ON d.id=p.department_id WHERE p.position_id=source.position_id;
 SELECT d.* INTO target_dept FROM warehouse.department_positions p JOIN warehouse.departments d ON d.id=p.department_id WHERE p.position_id=target.position_id;
 SELECT EXISTS(SELECT 1 FROM warehouse.handoff_transit_positions WHERE position_id IN(source.position_id,target.position_id)) INTO managed;
 IF managed THEN
 IF m.kind<>'transfer' OR op.result IS NOT NULL OR NOT EXISTS(
 SELECT 1 FROM warehouse.handoffs h WHERE h.organization_id=m.organization_id AND h.warehouse_id=m.warehouse_id AND h.owner_id=m.owner_id AND h.item_id=m.item_id
 AND h.quantity=NEW.quantity AND NEW.line_no=1
 AND ROW(source.lot_id,source.serial_id) IS NOT DISTINCT FROM ROW(h.lot_id,h.serial_id)
 AND ROW(target.lot_id,target.serial_id) IS NOT DISTINCT FROM ROW(h.lot_id,h.serial_id)
 AND ((h.state='sent' AND h.send_movement_command_id=m.operation_id AND h.sender_actor_id=op.actor_id AND h.send_line_id=m.document_line_id
 AND h.source_balance_id=source.id AND h.transit_balance_id=target.id AND source.stock_state='available' AND target.stock_state='in_transit')
 OR (h.state='accepted' AND h.accept_movement_command_id=m.operation_id AND h.receiver_actor_id=op.actor_id AND h.accept_line_id=m.document_line_id
 AND h.transit_balance_id=source.id AND h.destination_balance_id=target.id AND source.stock_state='in_transit' AND target.stock_state='available')))
 THEN RAISE EXCEPTION 'handoff_managed_stock'; END IF;
 ELSIF source_dept.kind='production' OR target_dept.kind='production' THEN
 IF m.kind<>'transfer' OR source_dept.id IS DISTINCT FROM target_dept.id
 OR NOT warehouse.handoff_member(op.actor_id,m.organization_id,m.warehouse_id,source_dept.id) THEN RAISE EXCEPTION 'handoff_managed_stock'; END IF;
 ELSIF source_dept.id IS NOT NULL AND target_dept.id IS NOT NULL AND source_dept.id<>target_dept.id THEN RAISE EXCEPTION 'handoff_managed_stock';
 END IF;
 RETURN NEW;
END $$;
CREATE TRIGGER guard_handoff_movement BEFORE INSERT ON inventory.movement_lines FOR EACH ROW EXECUTE FUNCTION inventory.guard_handoff_movement();

CREATE FUNCTION warehouse.handoff_mm(p_org uuid,p_wh uuid,p_owner uuid,p_item uuid,p_qty numeric)
RETURNS TABLE(document_id uuid,line_id uuid) LANGUAGE plpgsql SET search_path=pg_catalog AS $$
BEGIN
 INSERT INTO warehouse.documents(organization_id,warehouse_id,owner_id,kind,number,state)
 VALUES(p_org,p_wh,p_owner,'transfer',warehouse.allocate_document_number(p_org,p_wh,'transfer',transaction_timestamp()),'active') RETURNING id INTO document_id;
 INSERT INTO warehouse.document_lines(document_id,organization_id,warehouse_id,owner_id,item_id,line_no,requested_qty)
 VALUES(document_id,p_org,p_wh,p_owner,p_item,1,p_qty) RETURNING id INTO line_id;
 RETURN NEXT;
END $$;

CREATE FUNCTION warehouse.command_handoff(p_command uuid,p_actor uuid,p_handoff uuid,p_input jsonb) RETURNS jsonb
LANGUAGE plpgsql SECURITY DEFINER SET search_path=pg_catalog AS $$
DECLARE org uuid:=(p_input->>'organization_id')::uuid; wh uuid:=(p_input->>'warehouse_id')::uuid;
 source inventory.stock_balances; h warehouse.handoffs; source_dept warehouse.departments; dest_dept warehouse.departments;
 prior platform.operations; inserted uuid; request jsonb; action text; key text; qty numeric; receiver_label text;
 transit_loc uuid; transit_pos uuid; transit_balance uuid; dest_balance uuid; document uuid; line uuid; movement_command uuid;
 result jsonb; new_id uuid;
BEGIN
 PERFORM set_config('lock_timeout','2s',true); PERFORM set_config('statement_timeout','5s',true);
 IF p_command IS NULL OR p_actor IS NULL OR org IS NULL OR wh IS NULL OR jsonb_typeof(p_input) IS DISTINCT FROM 'object' THEN RAISE EXCEPTION 'invalid_handoff' USING ERRCODE='22023'; END IF;
 FOR key IN SELECT jsonb_object_keys(p_input) LOOP
 IF NOT(key=ANY(CASE WHEN p_handoff IS NULL THEN ARRAY['organization_id','warehouse_id','source_balance_id','destination_department_id','quantity'] ELSE ARRAY['organization_id','warehouse_id','expected_version'] END)) THEN RAISE EXCEPTION 'invalid_handoff' USING ERRCODE='22023'; END IF;
 END LOOP;
 LOCK TABLE warehouse.departments,warehouse.department_positions,warehouse.department_members,warehouse.department_employees,warehouse.department_routes IN SHARE MODE;
 IF p_handoff IS NULL THEN
 action:='handoff_send';
 SELECT * INTO STRICT source FROM inventory.stock_balances WHERE id=(p_input->>'source_balance_id')::uuid AND organization_id=org AND warehouse_id=wh;
 PERFORM warehouse.handoff_authorize(p_actor,org,wh,source.owner_id,'handoff.send');
 SELECT d.* INTO STRICT source_dept FROM warehouse.department_positions p JOIN warehouse.departments d ON d.id=p.department_id WHERE p.position_id=source.position_id AND d.organization_id=org AND d.warehouse_id=wh AND d.enabled;
 SELECT * INTO STRICT dest_dept FROM warehouse.departments WHERE id=(p_input->>'destination_department_id')::uuid AND organization_id=org AND warehouse_id=wh AND enabled;
 IF source.stock_state<>'available' OR NOT warehouse.handoff_member(p_actor,org,wh,source_dept.id)
 OR NOT EXISTS(SELECT 1 FROM warehouse.department_routes WHERE organization_id=org AND warehouse_id=wh AND source_department_id=source_dept.id AND destination_department_id=dest_dept.id AND enabled)
 OR NOT EXISTS(SELECT 1 FROM warehouse.department_positions WHERE department_id=dest_dept.id AND position_id=dest_dept.position_id) THEN RAISE EXCEPTION 'forbidden' USING ERRCODE='42501'; END IF;
 IF jsonb_typeof(p_input->'quantity') IS DISTINCT FROM 'string' OR p_input->>'quantity' !~ '^(0|[1-9][0-9]{0,17})(\.[0-9]{1,6})?$' THEN RAISE EXCEPTION 'invalid_quantity' USING ERRCODE='22023'; END IF;
 qty:=(p_input->>'quantity')::numeric;PERFORM inventory.assert_quantity(source.item_id,qty);
 ELSE
 action:='handoff_accept';
 SELECT * INTO STRICT h FROM warehouse.handoffs WHERE id=p_handoff AND organization_id=org AND warehouse_id=wh FOR UPDATE;
 PERFORM warehouse.handoff_authorize(p_actor,org,wh,h.owner_id,'handoff.accept');
 IF p_actor=h.sender_actor_id OR NOT warehouse.handoff_member(p_actor,org,wh,h.destination_department_id) THEN RAISE EXCEPTION 'forbidden' USING ERRCODE='42501'; END IF;
 END IF;
 request:=jsonb_build_object('handoff_id',p_handoff,'input',p_input);
 INSERT INTO platform.operations(id,actor_id,organization_id,warehouse_id,operation_type,request) VALUES(p_command,p_actor,org,wh,action,request)
 ON CONFLICT(id) DO NOTHING RETURNING id INTO inserted;
 IF inserted IS NULL THEN
 SELECT * INTO STRICT prior FROM platform.operations WHERE id=p_command;
 IF prior.actor_id<>p_actor OR prior.organization_id<>org OR prior.warehouse_id<>wh OR prior.operation_type<>action OR prior.request IS DISTINCT FROM request THEN RAISE EXCEPTION 'idempotency_key_reused' USING ERRCODE='22023'; END IF;
 RETURN prior.result;
 END IF;
 IF p_handoff IS NULL THEN
 new_id:=gen_random_uuid();transit_loc:=gen_random_uuid();transit_pos:=gen_random_uuid();movement_command:=gen_random_uuid();
 INSERT INTO warehouse.locations(id,organization_id,warehouse_id,code,kind) VALUES(transit_loc,org,wh,'PRZEKAZANIE-'||new_id::text,'staging');
 INSERT INTO warehouse.positions(id,organization_id,warehouse_id,location_id) VALUES(transit_pos,org,wh,transit_loc);
 INSERT INTO warehouse.handoff_transit_positions(position_id,organization_id,warehouse_id,handoff_id) VALUES(transit_pos,org,wh,new_id);
 transit_balance:=inventory.ensure_balance(org,wh,source.owner_id,source.item_id,transit_pos,'in_transit',source.lot_id,source.serial_id);
 dest_balance:=inventory.ensure_balance(org,wh,source.owner_id,source.item_id,dest_dept.position_id,'available',source.lot_id,source.serial_id);
 SELECT * INTO document,line FROM warehouse.handoff_mm(org,wh,source.owner_id,source.item_id,qty);
 SELECT display_name INTO STRICT receiver_label FROM warehouse.department_employees WHERE organization_id=org AND actor_id=p_actor AND enabled;
 INSERT INTO warehouse.handoffs(id,organization_id,warehouse_id,owner_id,item_id,lot_id,serial_id,source_department_id,destination_department_id,
 source_department_name,destination_department_name,source_balance_id,transit_balance_id,destination_balance_id,source_position_id,transit_position_id,destination_position_id,
 quantity,state,version,sender_actor_id,sender_name,sent_at,send_document_id,send_line_id,send_movement_command_id)
 VALUES(new_id,org,wh,source.owner_id,source.item_id,source.lot_id,source.serial_id,source_dept.id,dest_dept.id,source_dept.name,dest_dept.name,
 source.id,transit_balance,dest_balance,source.position_id,transit_pos,dest_dept.position_id,qty,'sent',1,p_actor,receiver_label,clock_timestamp(),document,line,movement_command) RETURNING * INTO h;
 PERFORM inventory.post_movement(movement_command,p_actor,line,1,'transfer',source.id,transit_balance,qty);
 ELSE
 IF h.state<>'sent' THEN RAISE EXCEPTION 'handoff_already_accepted'; END IF;
 IF jsonb_typeof(p_input->'expected_version') IS DISTINCT FROM 'number' OR (p_input->>'expected_version')::bigint IS DISTINCT FROM h.version THEN RAISE EXCEPTION 'handoff_version_conflict'; END IF;
 SELECT * INTO document,line FROM warehouse.handoff_mm(org,wh,h.owner_id,h.item_id,h.quantity);
 movement_command:=gen_random_uuid();
 SELECT display_name INTO STRICT receiver_label FROM warehouse.department_employees WHERE organization_id=org AND actor_id=p_actor AND enabled;
 UPDATE warehouse.handoffs SET state='accepted',version=2,receiver_actor_id=p_actor,receiver_name=receiver_label,accepted_at=clock_timestamp(),
 accept_document_id=document,accept_line_id=line,accept_movement_command_id=movement_command WHERE id=h.id RETURNING * INTO h;
 PERFORM inventory.post_movement(movement_command,p_actor,line,1,'transfer',h.transit_balance_id,h.destination_balance_id,h.quantity);
 END IF;
 UPDATE warehouse.documents SET state='closed',version=version+1,content_revision=content_revision+1 WHERE id=document;
 result:=jsonb_build_object('operation_id',p_command,'handoff_id',h.id,'version',h.version,'state',h.state);
 RETURN platform.finish_operation(p_command,result,h.id,CASE WHEN p_handoff IS NULL THEN 'wms.handoff.sent' ELSE 'wms.handoff.accepted' END);
END $$;

CREATE FUNCTION warehouse.handoff_json(p_h warehouse.handoffs) RETURNS jsonb
LANGUAGE sql STABLE SET search_path=pg_catalog AS $$
 SELECT (to_jsonb(p_h)-ARRAY['send_line_id','accept_line_id','send_movement_command_id','accept_movement_command_id']) || jsonb_build_object('quantity',p_h.quantity::text)
$$;
CREATE FUNCTION warehouse.read_handoff(p_actor uuid,p_org uuid,p_wh uuid,p_id uuid) RETURNS jsonb
LANGUAGE plpgsql SECURITY DEFINER SET search_path=pg_catalog AS $$
DECLARE h warehouse.handoffs;
BEGIN
 SELECT * INTO STRICT h FROM warehouse.handoffs WHERE id=p_id AND organization_id=p_org AND warehouse_id=p_wh
 AND EXISTS(SELECT 1 FROM platform.actor_grants WHERE actor_id=p_actor AND organization_id=p_org AND warehouse_id=p_wh
 AND (owner_id IS NULL OR owner_id=handoffs.owner_id) AND permission='handoff.read');
 RETURN warehouse.handoff_json(h);
END $$;
CREATE FUNCTION warehouse.read_handoffs(p_actor uuid,p_org uuid,p_wh uuid,p_input jsonb) RETURNS SETOF jsonb
LANGUAGE plpgsql SECURITY DEFINER SET search_path=pg_catalog AS $$
DECLARE p_limit integer:=(p_input->>'limit')::integer;p_after uuid:=coalesce(nullif(p_input->>'after','')::uuid,'00000000-0000-0000-0000-000000000000'::uuid);
 p_owner uuid:=nullif(p_input->>'owner_id','')::uuid; owners uuid[]; all_owners boolean;
BEGIN
 IF p_limit IS NULL OR p_limit NOT BETWEEN 1 AND 201 THEN RAISE EXCEPTION 'invalid_limit' USING ERRCODE='22023'; END IF;
 SELECT coalesce(bool_or(owner_id IS NULL),false),array_agg(DISTINCT owner_id) FILTER(WHERE owner_id IS NOT NULL) INTO all_owners,owners
 FROM platform.actor_grants WHERE actor_id=p_actor AND organization_id=p_org AND warehouse_id=p_wh AND permission='handoff.read';
 IF NOT all_owners AND coalesce(cardinality(owners),0)=0 OR p_owner IS NOT NULL AND NOT all_owners AND NOT p_owner=ANY(owners) THEN RAISE EXCEPTION 'forbidden' USING ERRCODE='42501'; END IF;
 IF p_owner IS NOT NULL THEN all_owners:=false;owners:=ARRAY[p_owner]; END IF;
 IF all_owners THEN
 RETURN QUERY SELECT warehouse.handoff_json(h) FROM warehouse.handoffs h WHERE h.organization_id=p_org AND h.warehouse_id=p_wh AND h.id>p_after ORDER BY h.id LIMIT p_limit;
 ELSE
 RETURN QUERY WITH page AS MATERIALIZED(SELECT c.id FROM unnest(owners) permitted(owner_id) CROSS JOIN LATERAL(
 SELECT h.id FROM warehouse.handoffs h WHERE h.organization_id=p_org AND h.owner_id=permitted.owner_id AND h.warehouse_id=p_wh AND h.id>p_after ORDER BY h.id LIMIT p_limit)c ORDER BY c.id LIMIT p_limit)
 SELECT warehouse.handoff_json(h) FROM page JOIN warehouse.handoffs h ON h.id=page.id ORDER BY h.id;
 END IF;
END $$;
CREATE FUNCTION warehouse.handoff_context(p_actor uuid,p_org uuid,p_wh uuid) RETURNS jsonb
LANGUAGE plpgsql SECURITY DEFINER SET search_path=pg_catalog AS $$
DECLARE result jsonb;
BEGIN
 IF NOT EXISTS(SELECT 1 FROM platform.actor_grants WHERE actor_id=p_actor AND organization_id=p_org AND warehouse_id=p_wh AND permission='handoff.read') THEN RAISE EXCEPTION 'forbidden' USING ERRCODE='42501'; END IF;
 SELECT jsonb_build_object(
 'departments',coalesce((SELECT jsonb_agg(jsonb_build_object('id',id,'code',code,'name',name,'kind',kind,'position_id',position_id,'enabled',enabled) ORDER BY code) FROM warehouse.departments WHERE organization_id=p_org AND warehouse_id=p_wh AND enabled),'[]'::jsonb),
 'routes',coalesce((SELECT jsonb_agg(jsonb_build_object('source_department_id',source_department_id,'destination_department_id',destination_department_id) ORDER BY source_department_id,destination_department_id) FROM warehouse.department_routes WHERE organization_id=p_org AND warehouse_id=p_wh AND enabled),'[]'::jsonb),
 'memberships',coalesce((SELECT jsonb_agg(jsonb_build_object('department_id',department_id) ORDER BY department_id) FROM warehouse.department_members WHERE organization_id=p_org AND warehouse_id=p_wh AND actor_id=p_actor AND warehouse.handoff_member(p_actor,p_org,p_wh,department_id)),'[]'::jsonb),
 'employee',(SELECT jsonb_build_object('actor_id',actor_id,'display_name',display_name) FROM warehouse.department_employees WHERE organization_id=p_org AND actor_id=p_actor AND enabled),
 'can_send',EXISTS(SELECT 1 FROM platform.actor_grants WHERE actor_id=p_actor AND organization_id=p_org AND warehouse_id=p_wh AND permission='handoff.send'),
 'can_accept',EXISTS(SELECT 1 FROM platform.actor_grants WHERE actor_id=p_actor AND organization_id=p_org AND warehouse_id=p_wh AND permission='handoff.accept')) INTO result;
 RETURN result;
END $$;
REVOKE ALL ON FUNCTION warehouse.handoff_member(uuid,uuid,uuid,uuid),warehouse.handoff_authorize(uuid,uuid,uuid,uuid,text),
 warehouse.protect_handoff_history(),warehouse.validate_department_position(),warehouse.protect_managed_position(),warehouse.protect_managed_handling_unit(),
 inventory.guard_handoff_balance(),inventory.guard_handoff_movement(),warehouse.handoff_mm(uuid,uuid,uuid,uuid,numeric),
 warehouse.command_handoff(uuid,uuid,uuid,jsonb),warehouse.handoff_json(warehouse.handoffs),warehouse.read_handoff(uuid,uuid,uuid,uuid),
 warehouse.read_handoffs(uuid,uuid,uuid,jsonb),warehouse.handoff_context(uuid,uuid,uuid) FROM PUBLIC;
COMMIT;
