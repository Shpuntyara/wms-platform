CREATE VIEW mod_example_dispatch_quality.document_facts_v1 AS
SELECT organization_id, warehouse_id, document_id, checked_content_revision AS content_revision,
       jsonb_build_object('decision', decision, 'evidence_reference', evidence_reference,
                         'expires_at', expires_at, 'policy_revision', policy_revision) AS payload
FROM (
  SELECT DISTINCT ON (organization_id, warehouse_id, document_id) *
  FROM mod_example_dispatch_quality.checks
  ORDER BY organization_id, warehouse_id, document_id, created_at DESC, id DESC
) latest;
