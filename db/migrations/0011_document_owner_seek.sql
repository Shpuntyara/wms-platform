-- Forward-only, online replacement. Preserve the published organization/owner
-- FK lookup prefix while making warehouse and cursor bounds index conditions.
-- Both DDL statements are resumable after an interrupted migration receipt;
-- the catalog guard always runs before the old index can be removed.
SET lock_timeout = '5s';
SET statement_timeout = '10min';
CREATE INDEX CONCURRENTLY IF NOT EXISTS documents_owner_warehouse_page_idx
ON warehouse.documents USING btree (organization_id, owner_id, warehouse_id, id);

DO $$
DECLARE v_uuid_ops oid;
BEGIN
 SELECT c.oid INTO v_uuid_ops
 FROM pg_catalog.pg_opclass c
 JOIN pg_catalog.pg_namespace n ON n.oid=c.opcnamespace
 JOIN pg_catalog.pg_am am ON am.oid=c.opcmethod
 WHERE n.nspname='pg_catalog' AND c.opcname='uuid_ops'
  AND am.amname='btree' AND c.opcdefault AND c.opcintype='uuid'::regtype;
 IF v_uuid_ops IS NULL OR NOT EXISTS (
  SELECT 1 FROM pg_catalog.pg_index i
  JOIN pg_catalog.pg_class ix ON ix.oid=i.indexrelid
  JOIN pg_catalog.pg_am am ON am.oid=ix.relam
  WHERE i.indexrelid=pg_catalog.to_regclass('warehouse.documents_owner_warehouse_page_idx')
   AND i.indrelid='warehouse.documents'::regclass AND am.amname='btree'
   AND i.indisvalid AND i.indisready AND i.indislive
   AND NOT i.indisunique AND NOT i.indisprimary AND NOT i.indisexclusion
   AND i.indnkeyatts=4 AND i.indnatts=4
   AND i.indexprs IS NULL AND i.indpred IS NULL
   AND i.indoption::text='0 0 0 0' AND i.indcollation::text='0 0 0 0'
   AND i.indclass::text=pg_catalog.format('%s %s %s %s',v_uuid_ops,v_uuid_ops,v_uuid_ops,v_uuid_ops)
   AND ARRAY(SELECT a.attname::text
    FROM pg_catalog.unnest(i.indkey) WITH ORDINALITY k(attnum,ordinality)
    JOIN pg_catalog.pg_attribute a ON a.attrelid=i.indrelid AND a.attnum=k.attnum
    ORDER BY k.ordinality)=ARRAY['organization_id','owner_id','warehouse_id','id']
 ) THEN
  RAISE EXCEPTION 'Document owner seek replacement index is not valid and compatible; original index retained';
 END IF;
 -- A same-name object with a different contract is drift, not a retry.
 IF pg_catalog.to_regclass('warehouse.documents_owner_idx') IS NOT NULL AND NOT EXISTS (
  SELECT 1 FROM pg_catalog.pg_index i
  JOIN pg_catalog.pg_class ix ON ix.oid=i.indexrelid
  JOIN pg_catalog.pg_am am ON am.oid=ix.relam
  WHERE i.indexrelid=pg_catalog.to_regclass('warehouse.documents_owner_idx')
   AND i.indrelid='warehouse.documents'::regclass AND am.amname='btree'
   AND NOT i.indisunique AND NOT i.indisprimary AND NOT i.indisexclusion
   AND i.indnkeyatts=2 AND i.indnatts=2
   AND i.indexprs IS NULL AND i.indpred IS NULL
   AND i.indoption::text='0 0' AND i.indcollation::text='0 0'
   AND i.indclass::text=pg_catalog.format('%s %s',v_uuid_ops,v_uuid_ops)
   AND ARRAY(SELECT a.attname::text
    FROM pg_catalog.unnest(i.indkey) WITH ORDINALITY k(attnum,ordinality)
    JOIN pg_catalog.pg_attribute a ON a.attrelid=i.indrelid AND a.attnum=k.attnum
    ORDER BY k.ordinality)=ARRAY['organization_id','owner_id']
 ) THEN
  RAISE EXCEPTION 'Original document owner index has an unexpected contract; index retained';
 END IF;
END;
$$;

DROP INDEX CONCURRENTLY IF EXISTS warehouse.documents_owner_idx;
RESET lock_timeout;
RESET statement_timeout;
