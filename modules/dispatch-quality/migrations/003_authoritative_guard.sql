-- The host locks the document before reading quality facts. Approval writers
-- take the same lock so revocation and dispatch have a defined serial order.
CREATE TABLE mod_example_dispatch_quality.policies (
 organization_id uuid NOT NULL,warehouse_id uuid NOT NULL,revision bigint NOT NULL CHECK(revision>0),
 PRIMARY KEY(organization_id,warehouse_id),
 FOREIGN KEY(organization_id,warehouse_id) REFERENCES warehouse.warehouses(organization_id,id)
);
CREATE FUNCTION mod_example_dispatch_quality.lock_quality_document() RETURNS trigger
LANGUAGE plpgsql SECURITY DEFINER SET search_path=pg_catalog AS $$
BEGIN
 PERFORM 1 FROM warehouse.documents WHERE id=NEW.document_id FOR UPDATE;
 RETURN NEW;
END $$;
CREATE TRIGGER lock_quality_document BEFORE INSERT ON mod_example_dispatch_quality.checks
 FOR EACH ROW EXECUTE FUNCTION mod_example_dispatch_quality.lock_quality_document();
CREATE FUNCTION mod_example_dispatch_quality.check_dispatch(p_line uuid,p_actor uuid) RETURNS void
LANGUAGE plpgsql SET search_path=pg_catalog AS $$
DECLARE d warehouse.documents; c mod_example_dispatch_quality.checks; policy bigint;
BEGIN
 LOCK TABLE mod_example_dispatch_quality.policies,platform.actor_grants IN SHARE MODE;
 SELECT x.* INTO STRICT d FROM warehouse.documents x JOIN warehouse.document_lines l ON l.document_id=x.id WHERE l.id=p_line;
 SELECT revision INTO policy FROM mod_example_dispatch_quality.policies WHERE organization_id=d.organization_id AND warehouse_id=d.warehouse_id;
 SELECT * INTO c FROM mod_example_dispatch_quality.checks WHERE document_id=d.id
  AND organization_id=d.organization_id AND warehouse_id=d.warehouse_id ORDER BY created_at DESC,id DESC LIMIT 1;
 IF NOT FOUND OR policy IS NULL OR c.policy_revision<>policy OR c.checked_content_revision<>d.content_revision
  OR c.decision<>'approved' OR length(btrim(c.evidence_reference)) NOT BETWEEN 8 AND 2048
  OR c.expires_at<=clock_timestamp() OR c.reviewer_id=p_actor
  OR NOT EXISTS(SELECT 1 FROM platform.actor_grants g WHERE g.actor_id=c.reviewer_id AND g.organization_id=d.organization_id
    AND g.warehouse_id=d.warehouse_id AND (g.owner_id IS NULL OR g.owner_id=d.owner_id) AND g.permission='quality.approve') THEN
  RAISE EXCEPTION 'module_guard_denied';
 END IF;
END $$;
INSERT INTO platform.sql_guards(module_id,point,handler,implementation)
 VALUES('example.dispatch_quality','outbound.shipment_dispatch_guard.v1','CheckApproval',
 'mod_example_dispatch_quality.check_dispatch(uuid,uuid)'::regprocedure)
 ON CONFLICT(module_id,point,handler) DO UPDATE SET implementation=EXCLUDED.implementation;
REVOKE ALL ON FUNCTION mod_example_dispatch_quality.check_dispatch(uuid,uuid),mod_example_dispatch_quality.lock_quality_document() FROM PUBLIC;
