BEGIN;
CREATE INDEX outbox_undelivered_health_idx ON platform.outbox(created_at) INCLUDE(dead_at) WHERE delivered_at IS NULL;

-- Filter first so completed event history is excluded from health aggregation.
CREATE VIEW contracts_v1.background_health AS
 SELECT o.pending,o.dead,o.oldest_pending_at,r.expired_reservations,r.oldest_expired_at
 FROM (SELECT count(*) FILTER(WHERE dead_at IS NULL) AS pending,
 count(*) FILTER(WHERE dead_at IS NOT NULL) AS dead,
 min(created_at) FILTER(WHERE dead_at IS NULL) AS oldest_pending_at
 FROM platform.outbox WHERE delivered_at IS NULL) o
 CROSS JOIN (SELECT count(*) AS expired_reservations,min(expires_at) AS oldest_expired_at
 FROM inventory.reservations WHERE active_qty>0 AND expires_at<=clock_timestamp()) r;
REVOKE ALL ON contracts_v1.background_health FROM PUBLIC;

-- Existing v1 packages retain their original semantics through the same SQL entry.
ALTER FUNCTION platform.check_declarative_guard(text,text,uuid,uuid) RENAME TO check_declarative_guard_v1;
CREATE FUNCTION platform.declarative_condition_v2(rule jsonb,facts jsonb,actor uuid) RETURNS boolean
LANGUAGE plpgsql SET search_path=pg_catalog AS $$
DECLARE actual jsonb; value text; expected text; op text; entry jsonb;
BEGIN
 IF jsonb_typeof(rule)<>'object' OR rule->>'field' IS NULL OR rule->>'field' !~ '^[a-z][a-z0-9_]{0,62}$'
 OR EXISTS(SELECT 1 FROM jsonb_object_keys(rule) k WHERE k NOT IN ('field','op','value','values')) THEN RAISE EXCEPTION 'required_guard_unavailable'; END IF;
 op:=rule->>'op'; actual:=facts->(rule->>'field');
 IF op IN ('eq','neq','decimal_gte','decimal_lte') THEN
  IF jsonb_typeof(rule->'value') IS DISTINCT FROM 'string' OR rule ? 'values' OR length(rule->>'value')>1024 THEN RAISE EXCEPTION 'required_guard_unavailable'; END IF;
 ELSIF op='in' THEN
  IF rule ? 'value' OR jsonb_typeof(rule->'values') IS DISTINCT FROM 'array' THEN RAISE EXCEPTION 'required_guard_unavailable'; END IF;
  IF jsonb_array_length(rule->'values') NOT BETWEEN 1 AND 64 THEN RAISE EXCEPTION 'required_guard_unavailable'; END IF;
  FOR entry IN SELECT * FROM jsonb_array_elements(rule->'values') LOOP
   IF jsonb_typeof(entry)<>'string' OR length(entry#>>'{}')>1024 THEN RAISE EXCEPTION 'required_guard_unavailable'; END IF;
  END LOOP;
 ELSIF op IN ('nonempty','future','not_actor','is_true','exists') THEN
  IF rule ? 'value' OR rule ? 'values' THEN RAISE EXCEPTION 'required_guard_unavailable'; END IF;
 ELSE RAISE EXCEPTION 'required_guard_unavailable'; END IF;
 expected:=rule->>'value';
 IF op IN ('decimal_gte','decimal_lte') AND (length(expected)>128 OR expected !~ '^-?(0|[1-9][0-9]*)([.][0-9]+)?$') THEN RAISE EXCEPTION 'required_guard_unavailable'; END IF;
 IF op='exists' THEN RETURN actual IS NOT NULL AND actual<>'null'::jsonb; END IF;
 IF op='is_true' THEN RETURN coalesce(actual='true'::jsonb,false); END IF;
 IF jsonb_typeof(actual) IS DISTINCT FROM 'string' THEN RETURN false; END IF;
 value:=actual#>>'{}';
 CASE op
 WHEN 'eq' THEN RETURN value=expected;
 WHEN 'neq' THEN RETURN value<>expected;
 WHEN 'in' THEN RETURN (rule->'values') @> jsonb_build_array(value);
 WHEN 'nonempty' THEN RETURN btrim(value)<>'';
 WHEN 'not_actor' THEN RETURN value<>'' AND lower(value)<>lower(actor::text);
 WHEN 'future' THEN
  BEGIN RETURN value ~ '^[0-9]{4}-[0-9]{2}-[0-9]{2}T[0-9]{2}:[0-9]{2}:[0-9]{2}([.][0-9]+)?(Z|[+-][0-9]{2}:[0-9]{2})$' AND value::timestamptz>clock_timestamp();
  EXCEPTION WHEN invalid_datetime_format OR datetime_field_overflow THEN RETURN false; END;
 WHEN 'decimal_gte','decimal_lte' THEN
  IF length(value)>128 OR value !~ '^-?(0|[1-9][0-9]*)([.][0-9]+)?$' THEN RETURN false; END IF;
  IF op='decimal_gte' THEN RETURN value::numeric>=expected::numeric; END IF;
  RETURN value::numeric<=expected::numeric;
 ELSE RAISE EXCEPTION 'required_guard_unavailable'; END CASE;
END $$;
CREATE FUNCTION platform.check_declarative_guard(p_module text,p_handler text,p_line uuid,p_actor uuid) RETURNS void
LANGUAGE plpgsql SET search_path=pg_catalog AS $$
DECLARE m platform.modules; d warehouse.documents; policy jsonb; alternative jsonb; rule jsonb; facts jsonb; matches integer; total integer:=0; allowed boolean:=false; matched boolean; passed boolean;
BEGIN
 SELECT * INTO STRICT m FROM platform.modules WHERE id=p_module AND state='active';
 IF m.manifest->>'runtime'='declarative_v1' THEN PERFORM platform.check_declarative_guard_v1(p_module,p_handler,p_line,p_actor);RETURN; END IF;
 IF m.manifest->>'runtime' IS DISTINCT FROM 'declarative_v2' THEN RAISE EXCEPTION 'required_guard_unavailable'; END IF;
 SELECT x.* INTO STRICT d FROM warehouse.documents x JOIN warehouse.document_lines l ON l.document_id=x.id WHERE l.id=p_line;
 SELECT count(*) INTO matches FROM jsonb_array_elements(m.manifest->'policies') p WHERE p->>'handler'=p_handler;
 IF matches<>1 OR m.schema_name::text<>('mod_'||replace(m.id,'.','_')) THEN RAISE EXCEPTION 'required_guard_unavailable'; END IF;
 SELECT p INTO policy FROM jsonb_array_elements(m.manifest->'policies') p WHERE p->>'handler'=p_handler;
 IF jsonb_typeof(policy->'any') IS DISTINCT FROM 'array' OR EXISTS(SELECT 1 FROM jsonb_object_keys(policy) k WHERE k NOT IN ('handler','any')) THEN RAISE EXCEPTION 'required_guard_unavailable'; END IF;
 IF jsonb_array_length(policy->'any') NOT BETWEEN 1 AND 16 THEN RAISE EXCEPTION 'required_guard_unavailable'; END IF;
 BEGIN
 EXECUTE format('SELECT payload FROM %I.document_facts_v1 WHERE organization_id=$1 AND warehouse_id=$2 AND document_id=$3 AND content_revision=$4',m.schema_name)
 INTO STRICT facts USING d.organization_id,d.warehouse_id,d.id,d.content_revision;
 EXCEPTION WHEN no_data_found OR too_many_rows THEN RAISE EXCEPTION 'module_guard_denied'; END;
 FOR alternative IN SELECT * FROM jsonb_array_elements(policy->'any') LOOP
  IF jsonb_typeof(alternative->'conditions') IS DISTINCT FROM 'array' OR EXISTS(SELECT 1 FROM jsonb_object_keys(alternative) k WHERE k<>'conditions') THEN RAISE EXCEPTION 'required_guard_unavailable'; END IF;
  IF jsonb_array_length(alternative->'conditions') NOT BETWEEN 1 AND 32 THEN RAISE EXCEPTION 'required_guard_unavailable'; END IF;
  total:=total+jsonb_array_length(alternative->'conditions');IF total>128 THEN RAISE EXCEPTION 'required_guard_unavailable'; END IF;
  matched:=true;
  FOR rule IN SELECT * FROM jsonb_array_elements(alternative->'conditions') LOOP
   passed:=platform.declarative_condition_v2(rule,facts,p_actor);
   matched:=matched AND coalesce(passed,false);
  END LOOP;
  allowed:=allowed OR matched;
 END LOOP;
 IF NOT allowed THEN RAISE EXCEPTION 'module_guard_denied'; END IF;
END $$;
REVOKE ALL ON FUNCTION platform.declarative_condition_v2(jsonb,jsonb,uuid),platform.check_declarative_guard(text,text,uuid,uuid),platform.check_declarative_guard_v1(text,text,uuid,uuid) FROM PUBLIC;
CREATE OR REPLACE FUNCTION platform.assert_required_guards(p_point text,p_line uuid,p_actor uuid) RETURNS void
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
  IF m.manifest->>'runtime' IN ('declarative_v1','declarative_v2') THEN
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

COMMIT;
