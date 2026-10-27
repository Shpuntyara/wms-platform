BEGIN;
-- Illustrative module storage, not a complete executable approval module.
CREATE SCHEMA mod_example_dispatch_quality;
CREATE TABLE mod_example_dispatch_quality.checks (
  id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  organization_id uuid NOT NULL,
  warehouse_id uuid NOT NULL,
  document_id uuid NOT NULL,
  checked_content_revision bigint NOT NULL CHECK (checked_content_revision > 0),
  policy_revision bigint NOT NULL CHECK (policy_revision > 0),
  decision text NOT NULL CHECK (decision IN ('approved', 'rejected')),
  reviewer_id uuid NOT NULL REFERENCES platform.actors(id),
  evidence_reference text NOT NULL,
  created_at timestamptz NOT NULL DEFAULT clock_timestamp(),
  expires_at timestamptz NOT NULL,
  CHECK (expires_at > created_at),
  FOREIGN KEY (organization_id, warehouse_id, document_id)
    REFERENCES warehouse.documents(organization_id, warehouse_id, id)
);
CREATE INDEX checks_document_idx ON mod_example_dispatch_quality.checks
  (organization_id, warehouse_id, document_id, created_at DESC, id DESC);
CREATE INDEX checks_reviewer_idx ON mod_example_dispatch_quality.checks(reviewer_id);
CREATE TRIGGER immutable_quality_checks BEFORE UPDATE OR DELETE ON mod_example_dispatch_quality.checks
  FOR EACH ROW EXECUTE FUNCTION platform.reject_history_change();
REVOKE ALL ON SCHEMA mod_example_dispatch_quality FROM PUBLIC;
COMMIT;
