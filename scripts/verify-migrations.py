"""Bounded real-PostgreSQL migration regressions in a new disposable database."""
import argparse
import json
import subprocess
import time
from pathlib import Path
from migrate_core import build_script
from verify_windows_migrations import verify as verify_windows_migrations


def verify(psql, port, admin):
    root = Path(__file__).resolve().parent.parent
    base = [psql, '-X', '-q', '-h', '127.0.0.1', '-p', str(port), '-U', admin, '-v', 'ON_ERROR_STOP=1']
    name = 'wms_kernel_test_migrations'
    subprocess.run(base + ['-d','postgres','-c',f'CREATE DATABASE {name} OWNER wms_test_owner'],check=True,capture_output=True)
    command = base + ['-d',name]
    prefix = 'SET ROLE wms_test_owner;\n'
    def run(sql, ok=True):
        result = subprocess.run(command, input=(prefix+sql).encode(), capture_output=True)
        if (result.returncode == 0) != ok:
            raise RuntimeError(result.stderr.decode(errors='replace'))
        return result
    def value(sql):
        return subprocess.check_output(command + ['-At','-c',sql]).strip().decode()
    # Reproduce the previously released installation without a core ledger.
    for file in sorted((root/'db/migrations').glob('*.sql')):
        if file.name[:4] <= '0007':
            run(file.read_text())
    rejected = run(build_script(root),ok=False)
    assert b'Untracked existing database' in rejected.stderr
    oid = value("SELECT 'platform.check_declarative_guard(text,text,uuid,uuid)'::regprocedure::oid")
    run("CREATE VIEW contracts_v1.guard_dependency_probe AS SELECT 'platform.check_declarative_guard(text,text,uuid,uuid)'::regprocedure AS guard;")
    run(build_script(root,legacy_through='0007'))
    assert value("SELECT guard::oid FROM contracts_v1.guard_dependency_probe") == oid
    run(build_script(root))
    assert value('SELECT count(*) FROM wms_migrations.history') == str(len(list((root/'db/migrations').glob('*.sql'))))
    source_checksum = value("SELECT checksum FROM wms_migrations.history WHERE name='0009_warehouse_documents.sql'")
    execution_checksum = value("SELECT execution_checksum FROM wms_migrations.history WHERE name='0009_warehouse_documents.sql'")
    run("UPDATE wms_migrations.history SET checksum='tampered' WHERE name LIKE '0009%';")
    assert b'checksum mismatch' in run(build_script(root),ok=False).stderr
    # Restore the receipt captured before fault injection, never a fresh hash of
    # whatever source happens to exist after the test has corrupted the ledger.
    run("UPDATE wms_migrations.history SET checksum='" + source_checksum + "' WHERE name LIKE '0009%';")
    run("UPDATE wms_migrations.history SET execution_checksum='tampered' WHERE name LIKE '0009%';")
    assert b'checksum mismatch' in run(build_script(root),ok=False).stderr
    run("UPDATE wms_migrations.history SET execution_checksum='" + execution_checksum + "' WHERE name LIKE '0009%';")
    # A second migrator must fail promptly instead of racing DDL.
    holder = subprocess.Popen(command,stdin=subprocess.PIPE,stdout=subprocess.PIPE,stderr=subprocess.PIPE)
    holder.stdin.write(b'SELECT pg_advisory_lock(19890627,1001);SELECT pg_sleep(3);');holder.stdin.close()
    try:
        limit = time.monotonic()+2
        while value('SELECT count(*) FROM pg_locks WHERE locktype=\'advisory\' AND classid=19890627 AND objid=1001 AND granted') == '0':
            if time.monotonic()>limit: raise RuntimeError('lock holder did not start')
            time.sleep(.02)
        assert b'core migration already running' in run(build_script(root),ok=False).stderr
    finally:
        holder.wait(timeout=5)
    # A partially completed concurrent-index migration must resume valid indexes
    # and refuse a same-name index whose definition is wrong.
    run("DELETE FROM wms_migrations.history WHERE name LIKE '0005%';DROP INDEX inventory.movements_owner_page_idx;CREATE INDEX movements_owner_page_idx ON inventory.movements(id);")
    assert b'Missing/invalid index' in run(build_script(root),ok=False).stderr
    run('DROP INDEX inventory.movements_owner_page_idx;')
    run(build_script(root))
    # The other supported legacy release has already run historical 0008.
    legacy8 = 'wms_kernel_test_legacy8'
    subprocess.run(base + ['-d','postgres','-c',f'CREATE DATABASE {legacy8} OWNER wms_test_owner'],check=True,capture_output=True)
    old_command = command
    command = base + ['-d',legacy8]
    for file in sorted((root/'db/migrations').glob('*.sql')):
        if file.name[:4] <= '0008':
            run(file.read_text())
    oid8 = value("SELECT 'platform.check_declarative_guard(text,text,uuid,uuid)'::regprocedure::oid")
    run(build_script(root,legacy_through='0008'))
    run(build_script(root))
    assert value("SELECT 'platform.check_declarative_guard(text,text,uuid,uuid)'::regprocedure::oid") == oid8
    command = old_command
    report = {'status':'passed','checks':['legacy database refused without declaration','explicit 0007 adoption and upgrade through latest migration','guard OID and dependent view preserved','repeat all migrations','source checksum drift refused','execution checksum drift refused','simultaneous migrator refused','wrong index definition refused','partial concurrent migration resumed','explicit 0008 adoption and upgrade through latest migration']}
    report['windows_receipts'] = verify_windows_migrations(psql, port, admin)
    target=root/'work/migration-verification.json';target.parent.mkdir(exist_ok=True);target.write_text(json.dumps(report,indent=2)+'\n')
    print(json.dumps(report))

if __name__ == '__main__':
    p=argparse.ArgumentParser();p.add_argument('--psql',required=True);p.add_argument('--port',required=True,type=int);p.add_argument('--admin',default='wms_test_admin');a=p.parse_args();verify(a.psql,a.port,a.admin)
