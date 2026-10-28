"""Real-PostgreSQL compatibility checks for frozen original Windows receipts.

Only test databases are written. Expected historical checksums come from the
unmodified ea0eb2f runner and are checked as correlated source/execution pairs.
The current migrator must preserve every old receipt while accepting it.
"""
import argparse
import hashlib
import json
from pathlib import Path
import subprocess
import uuid

from migrate_core import build_script, migration_sql, source

ROOT = Path(__file__).resolve().parent.parent
MANIFEST = ROOT / 'scripts/testdata/windows_ledger_ea0eb2f.json'


def verify(psql, port, admin='wms_test_admin'):
    fixture_bytes = MANIFEST.read_bytes()
    historical = json.loads(fixture_bytes)
    name = 'wms_kernel_test_windows_' + uuid.uuid4().hex
    base = [psql, '-X', '-q', '-h', '127.0.0.1', '-p', str(port), '-U', admin, '-v', 'ON_ERROR_STOP=1']
    subprocess.run(base + ['-d', 'postgres', '-c', f'CREATE DATABASE {name} OWNER wms_test_owner'], check=True, capture_output=True)
    command = base + ['-d', name]

    def execute(sql, accepted=True):
        result = subprocess.run(command, input=('SET ROLE wms_test_owner;\n' + sql).encode(), capture_output=True)
        if accepted:
            assert result.returncode == 0, result.stderr.decode(errors='replace')
        else:
            assert result.returncode != 0, 'Unexpectedly accepted drifted migration receipt'
            assert b'Migration checksum mismatch' in result.stderr, result.stderr.decode(errors='replace')

    def snapshot():
        return subprocess.check_output(command + ['-At', '-c', 'SELECT json_agg(row_to_json(h) ORDER BY name) FROM wms_migrations.history h']).strip()

    plan = build_script(ROOT)
    execute(plan)
    canonical = snapshot()
    # A test fault-injection fixture emulates the exact receipts the published
    # original runner produced. It is never a production history-repair path.
    for row in historical['receipts']:
        execute("UPDATE wms_migrations.history SET checksum='{source_checksum}',execution_checksum='{execution_checksum}' WHERE name='{name}';".format(**row))
    frozen = snapshot()
    execute(plan)
    assert snapshot() == frozen, 'Repeat upgrade rewrote historical migration receipts'
    execute(plan)
    assert snapshot() == frozen, 'Second repeat upgrade rewrote historical receipts'

    row = next(row for row in historical['receipts'] if row['name'] == '0006_security_boundary.sql')
    path = ROOT / 'db/migrations' / row['name']
    _, canonical_source = source(path)
    canonical_execution = hashlib.sha256(migration_sql(path).encode()).hexdigest()
    invalid_pairs = [
        (row['source_checksum'], 'tampered'),
        ('tampered', row['execution_checksum']),
        (canonical_source, row['execution_checksum']),
        (row['source_checksum'], canonical_execution),
    ]
    for source_checksum, execution_checksum in invalid_pairs:
        execute("UPDATE wms_migrations.history SET checksum='" + source_checksum + "',execution_checksum='" + execution_checksum + "' WHERE name='" + row['name'] + "';")
        corrupt = snapshot()
        execute(plan, accepted=False)
        assert snapshot() == corrupt, 'Refused upgrade rewrote a corrupted receipt'
        execute("UPDATE wms_migrations.history SET checksum='{source_checksum}',execution_checksum='{execution_checksum}' WHERE name='{name}';".format(**row))
    assert snapshot() == frozen
    execute(plan)
    assert snapshot() == frozen
    report = {
        'status': 'passed',
        'original_revision': historical['revision'],
        'original_runner_git_blob': historical['runner_git_blob'],
        'golden_manifest_sha256': hashlib.sha256(fixture_bytes).hexdigest(),
        'historical_receipts': len(historical['receipts']),
        'checks': ['original mixed Windows checkout receipts accepted', 'all historical receipt rows unchanged across repeated upgrades', 'source and execution drift rejected', 'LF-source/CRLF-execution mismatched pair rejected', 'CRLF-source/LF-execution mismatched pair rejected', 'refused upgrade preserves damaged evidence'],
    }
    target = ROOT / 'work/windows-migration-verification.json'
    target.write_text(json.dumps(report, indent=2) + '\n', encoding='utf8')
    return report


if __name__ == '__main__':
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--psql', required=True)
    parser.add_argument('--port', type=int, required=True)
    parser.add_argument('--admin', default='wms_test_admin')
    args = parser.parse_args()
    print(json.dumps(verify(args.psql, args.port, args.admin), indent=2))
