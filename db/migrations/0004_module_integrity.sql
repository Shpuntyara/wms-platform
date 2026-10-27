-- Installation preflight reads migration history through a published contract.
-- This is an additive migration; earlier deployed migrations remain unchanged.
BEGIN;
SET LOCAL lock_timeout = '5s';
SET LOCAL statement_timeout = '30s';
CREATE OR REPLACE VIEW contracts_v1.active_modules AS
SELECT id, version, manifest, schema_name FROM platform.modules WHERE state='active';
CREATE VIEW contracts_v1.module_migrations AS
SELECT module_id, migration_id, checksum FROM platform.module_migrations;
REVOKE ALL ON contracts_v1.module_migrations FROM PUBLIC;
COMMIT;
