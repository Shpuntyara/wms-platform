"""Serialized core migrations with immutable sources and reviewed online recipes."""
import argparse
import hashlib
import re
import subprocess
from pathlib import Path

PINNED = {
 '0005_inventory_pagination.sql': '3693ac805fa60eac065499764442ce13763f882a2e257c9082871ef18cc80045',
 '0006_security_boundary.sql': 'd905ffbcf713ba19de3e96523c803a74c436963332d008068cdddef03d135574',
 '0008_background_health_and_policy_v2.sql': '95a77fe3d691ee040c63792892b55b9e1eeb20a661b5547da02acee50c28e213',
}
PUBLISHED_MIGRATIONS = (
    '0001_inventory_schema.sql',
    '0002_inventory_commands.sql',
    '0003_backend_api.sql',
    '0004_module_integrity.sql',
    '0005_inventory_pagination.sql',
    '0006_security_boundary.sql',
    '0007_background_jobs.sql',
    '0008_background_health_and_policy_v2.sql',
    '0009_warehouse_documents.sql',
    '0010_document_numbering.sql',
)

def source(path):
    raw = path.read_bytes().replace(b'\r\n', b'\n')
    digest = hashlib.sha256(raw).hexdigest()
    # The published 0006 pin was calculated from a Windows CRLF checkout.
    # Preserve that trust root, validating the exact historical representation;
    # execute and record canonical LF bytes on every platform. Other whitespace
    # and SQL edits still change the verified digest and remain rejected.
    # Historical Git blob: 9e2cc5798b490173058694227190980c162bd2fe (316247b).
    pinned_raw = raw.replace(b'\n', b'\r\n') if path.name == '0006_security_boundary.sql' else raw
    if path.name in PINNED and hashlib.sha256(pinned_raw).hexdigest() != PINNED[path.name]:
        raise ValueError('Published migration checksum changed: ' + path.name)
    return raw.decode('utf8'), digest


def migration_sql(path, legacy_windows=False):
    sql, _ = source(path)
    if legacy_windows and path.name not in ('0005_inventory_pagination.sql', '0008_background_health_and_policy_v2.sql'):
        sql = sql.replace('\n', '\r\n')
    if path.name.startswith('0005_'):
        sql = sql.replace('CREATE INDEX CONCURRENTLY ', 'CREATE INDEX CONCURRENTLY IF NOT EXISTS ')
    if path.name.startswith('0008_'):
        old, _ = source(path.parent / '0006_security_boundary.sql')
        if legacy_windows:
            old = old.replace('\n', '\r\n')
        signature = 'CREATE FUNCTION platform.check_declarative_guard('
        start = old.index(signature)
        end = old.index('END $$;', start) + len('END $$;')
        copy = old[start:end].replace(signature, 'CREATE FUNCTION platform.check_declarative_guard_v1(', 1)
        index = 'CREATE INDEX outbox_undelivered_health_idx ON platform.outbox(created_at) INCLUDE(dead_at) WHERE delivered_at IS NULL;'
        sql = sql.replace(index, '')
        sql = sql.replace('ALTER FUNCTION platform.check_declarative_guard(text,text,uuid,uuid) RENAME TO check_declarative_guard_v1;', copy)
        sql = sql.replace(signature, 'CREATE OR REPLACE FUNCTION platform.check_declarative_guard(', 1)
        sql = index.replace('CREATE INDEX ', 'CREATE INDEX CONCURRENTLY IF NOT EXISTS ') + '\n' + sql
    return sql


def build_script(root, through=None, legacy_through=None):
    available = {path.name for path in (root / 'db/migrations').glob('*.sql')}
    missing = set(PUBLISHED_MIGRATIONS) - available
    if missing:
        raise ValueError('Missing published migration: ' + ', '.join(sorted(missing)))
    if legacy_through not in (None, "0007", "0008"):
        raise ValueError("Only released legacy versions 0007 and 0008 are supported")
    parts = [r'''\set ON_ERROR_STOP on
SET lock_timeout='2s';
SET statement_timeout='10min';
DO $$ BEGIN
 IF NOT pg_try_advisory_lock(19890627,1001) THEN RAISE EXCEPTION 'core migration already running'; END IF;
 IF to_regnamespace('platform') IS NOT NULL AND to_regclass('wms_migrations.history') IS NULL THEN
  RAISE EXCEPTION 'Untracked existing database: use documented existing-installation upgrade; never auto-baseline';
 END IF;
END $$;
CREATE SCHEMA IF NOT EXISTS wms_migrations;
REVOKE ALL ON SCHEMA wms_migrations FROM PUBLIC;
CREATE TABLE IF NOT EXISTS wms_migrations.history (
 name text PRIMARY KEY, checksum text NOT NULL, execution_checksum text NOT NULL,
 applied_at timestamptz NOT NULL DEFAULT clock_timestamp());
REVOKE ALL ON wms_migrations.history FROM PUBLIC;
''']
    if legacy_through:
        # An explicit operator declaration, not an inferred migration history.
        # Catalog probes reject obvious mismatches; release records are still required.
        parts[0] = parts[0].replace("IF to_regnamespace('platform') IS NOT NULL AND to_regclass('wms_migrations.history') IS NULL THEN", "IF false THEN")
        marker = """DO $$ BEGIN
 IF to_regclass('warehouse.documents') IS NULL OR to_regclass('platform.required_guards') IS NULL
 OR to_regprocedure('platform.complete_outbox(uuid,uuid,boolean)') IS NULL
 OR to_regprocedure('platform.check_declarative_guard(text,text,uuid,uuid)') IS NULL
 OR to_regclass('contracts_v1.document_details') IS NOT NULL
 OR ((to_regprocedure('platform.check_declarative_guard_v1(text,text,uuid,uuid)') IS NOT NULL) <> EXPECT_V2)
 OR ((to_regclass('contracts_v1.background_health') IS NOT NULL) <> EXPECT_V2)
 THEN RAISE EXCEPTION 'expected legacy schema version does not match declaration'; END IF;
END $$;
""".replace('EXPECT_V2', 'true' if legacy_through == '0008' else 'false')
        # Execute the probe before creating a ledger, so a refused adoption leaves no state.
        start = parts[0].index('CREATE SCHEMA IF NOT EXISTS')
        parts[0] = parts[0][:start] + marker + parts[0][start:]
        baseline = ['BEGIN;']
        for path in sorted((root / 'db/migrations').glob('*.sql')):
            if path.name[:4] <= legacy_through:
                _, digest = source(path)
                baseline.append(f"INSERT INTO wms_migrations.history(name,checksum,execution_checksum) VALUES('{path.name}','{digest}','legacy_baseline:{digest}') ON CONFLICT (name) DO NOTHING;")
        parts.append('\n'.join(baseline) + '\nCOMMIT;')
    for path in sorted((root / 'db/migrations').glob('*.sql')):
        if through and path.name[:4] > through:
            continue
        _, digest = source(path)
        sql = migration_sql(path)
        execution = hashlib.sha256(sql.encode()).hexdigest()
        # Reproduce the original accepted mixed Windows checkout, including
        # 0008's embedded CRLF guard copied from 0006. Accept correlated pairs
        # only; never rewrite historical receipts or accept independent hashes.
        historical_source, _ = source(path)
        if path.name not in ('0005_inventory_pagination.sql', '0008_background_health_and_policy_v2.sql'):
            historical_source = historical_source.replace('\n', '\r\n')
        historical_digest = hashlib.sha256(historical_source.encode()).hexdigest()
        historical_execution = hashlib.sha256(migration_sql(path, legacy_windows=True).encode()).hexdigest()
        name = path.name
        parts.append(fr"""DO $$ BEGIN
 IF EXISTS(SELECT 1 FROM wms_migrations.history WHERE name='{name}' AND
 NOT ((checksum='{digest}' AND execution_checksum IN ('{execution}','legacy_baseline:{digest}'))
 OR (checksum='{historical_digest}' AND execution_checksum IN ('{historical_execution}','legacy_baseline:{historical_digest}')))) THEN
 RAISE EXCEPTION 'Migration checksum mismatch: {name}'; END IF;
END $$;
SELECT NOT EXISTS(SELECT 1 FROM wms_migrations.history WHERE name='{name}') AS apply_step \gset
\if :apply_step
""")
        check = ''
        for index in re.findall(r'CREATE INDEX CONCURRENTLY IF NOT EXISTS (\w+)', sql):
            schema = 'platform' if 'outbox' in index else 'inventory'
            table = {'movements_owner_page_idx':'movements','reservations_owner_page_idx':'reservations','stock_owner_page_idx':'stock_balances','outbox_undelivered_health_idx':'outbox'}[index]
            columns = "ARRAY['created_at','dead_at']" if table == 'outbox' else "ARRAY['organization_id','warehouse_id','owner_id','id']"
            predicate = '(delivered_at IS NULL)' if table == 'outbox' else ''
            key_count = 1 if table == 'outbox' else 4
            check += f"""DO $$ BEGIN
 IF NOT EXISTS(SELECT 1 FROM pg_index i JOIN pg_class c ON c.oid=i.indexrelid
 JOIN pg_namespace n ON n.oid=c.relnamespace
 WHERE c.relname='{index}' AND n.nspname='{schema}' AND i.indisvalid AND i.indisready
 AND i.indrelid='{schema}.{table}'::regclass AND NOT i.indisunique AND i.indnkeyatts={key_count}
 AND coalesce(pg_get_expr(i.indpred,i.indrelid),'')='{predicate}'
 AND ARRAY(SELECT a.attname::text FROM unnest(i.indkey) WITH ORDINALITY k(attnum,n)
 JOIN pg_attribute a ON a.attrelid=i.indrelid AND a.attnum=k.attnum ORDER BY k.n)={columns}::text[])
 THEN RAISE EXCEPTION 'Missing/invalid index: {index}; inspect and repair before retry'; END IF;
END $$;
"""
        receipt = f"INSERT INTO wms_migrations.history(name,checksum,execution_checksum) VALUES('{name}','{digest}','{execution}');\n"
        if 'COMMIT;' in sql:
            sql = sql.rsplit('COMMIT;', 1)[0] + check + receipt + 'COMMIT;\n'
        else:
            sql += '\n' + check + receipt
        parts.append(sql + "\n\\endif\nSET lock_timeout='2s';\nSET statement_timeout='10min';\n")
    parts.append('SELECT pg_advisory_unlock(19890627,1001);\n')
    return '\n'.join(parts)


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--psql', default='psql')
    parser.add_argument('--database', required=True, help='database name or libpq connection; prefer PGSERVICE/PGPASSFILE for secrets')
    parser.add_argument('--owner-role', help='optional existing schema-owner role')
    parser.add_argument('--legacy-through', choices=['0007', '0008'], help='explicit baseline of a verified legacy release; requires operator release records')
    args = parser.parse_args()
    script = build_script(Path(__file__).resolve().parent.parent, legacy_through=args.legacy_through)
    if args.owner_role:
        script = 'SET ROLE "' + args.owner_role.replace('"', '""') + '";\n' + script
    # Do not render argv in a CalledProcessError: a caller may have supplied a credential-bearing DSN.
    result = subprocess.run([args.psql, '-X', '-q', '-v', 'ON_ERROR_STOP=1', '-d', args.database], input=script.encode('utf8'))
    raise SystemExit(result.returncode)

if __name__ == '__main__':
    main()
