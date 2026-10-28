"""Reproduce independent readiness checks in an owned loopback PG17 cluster.

No real credentials, keys, connection strings, or request signatures are emitted.
The synthetic cluster is stopped even when the deliberately red test fails.
"""
import argparse
import base64
import hashlib
import json
import os
from pathlib import Path
import secrets
import subprocess
import sys
import uuid

ROOT = Path(__file__).resolve().parents[3]
sys.path.insert(0, str(ROOT / 'scripts'))
from crash_restore import Cluster
from migrate_core import build_script


def main():
    parser = argparse.ArgumentParser()
    parser.add_argument('--phase', required=True)
    parser.add_argument('--go', required=True)
    parser.add_argument('--work-root', required=True)
    parser.add_argument('--postgres-bin', default=r'C:\Program Files\PostgreSQL\17\bin')
    args = parser.parse_args()
    if args.phase not in ('red', 'green', 'characterization', 'worker-red', 'worker-green'):
        raise ValueError('unsupported evidence phase')
    source_names = ['internal/postgres/hardened_onprem_critic_integration_test.go',
                    'internal/postgres/hardened.go', 'internal/postgres/hardened_jobs.go', 'internal/api/server.go',
                    'db/profiles/hardened-v1.sql', 'db/profiles/onprem-v1.sql',
                    'db/bootstrap/onprem-grants.psql',
                    'docs/verification/onprem/security-critic-run.py']
    def hashes():
        return {p: hashlib.sha256((ROOT / p).read_bytes()).hexdigest() for p in source_names}
    before = hashes()
    work_root = Path(args.work_root).resolve()
    work_root.mkdir(parents=True, exist_ok=True)
    cluster = Cluster(args.postgres_bin, work_root, 'security-critic-' + uuid.uuid4().hex[:8])
    key = secrets.token_bytes(32)
    result = None
    try:
        cluster.initialize()
        cluster.query('CREATE ROLE critic_owner NOLOGIN; CREATE ROLE critic_api LOGIN;'
                      'CREATE ROLE critic_worker LOGIN; CREATE ROLE critic_module NOLOGIN;', database='postgres')
        cluster.query('CREATE DATABASE security_critic OWNER critic_owner;', database='postgres')
        cluster.query('SET ROLE critic_owner;\n' + build_script(ROOT), database='security_critic')
        cluster.query('\\set runtime_role critic_api\n\\set module_role critic_module\nSET ROLE critic_owner;\n' +
                      (ROOT / 'db/bootstrap/grants.psql').read_text() + '\n' +
                      (ROOT / 'db/bootstrap/backend-grants.psql').read_text(), database='security_critic')
        cluster.query('SET ROLE critic_owner;\n' + (ROOT / 'db/profiles/hardened-v1.sql').read_text() + '\n' +
                      (ROOT / 'db/profiles/onprem-v1.sql').read_text(), database='security_critic')
        cluster.query('\\set api_role critic_api\n\\set worker_role critic_worker\nSET ROLE critic_owner;\n' +
                      (ROOT / 'db/bootstrap/onprem-grants.psql').read_text(), database='security_critic')
        cluster.query("INSERT INTO hardened_private.signing_keys(login,key) VALUES('critic_api',decode('" +
                      key.hex() + "','hex'));", database='security_critic')
        env = dict(os.environ)
        env['WMS_HARDENED_ADMIN_DSN'] = f'postgres://wms_test_admin@127.0.0.1:{cluster.port}/security_critic?sslmode=disable'
        env['WMS_HARDENED_API_DSN'] = f'postgres://critic_api@127.0.0.1:{cluster.port}/security_critic?sslmode=disable'
        env['WMS_HARDENED_WORKER_DSN'] = f'postgres://critic_worker@127.0.0.1:{cluster.port}/security_critic?sslmode=disable'
        env['WMS_HARDENED_KEY'] = base64.b64encode(key).decode()
        pattern = '^TestHardenedOnPremCriticWorkerRejectsInstallationDrift$' if args.phase.startswith('worker-') else '^TestHardenedOnPremCritic'
        command = [args.go, 'test', '-race', '-tags=integration,hardened', './internal/postgres',
                   '-run', pattern, '-count=1', '-v', '-timeout=60s']
        run = subprocess.run(command, cwd=ROOT, env=env, text=True, encoding='utf8', capture_output=True,
                             timeout=120, creationflags=subprocess.CREATE_NO_WINDOW if os.name == 'nt' else 0)
        output = run.stdout + run.stderr
        if base64.b64encode(key).decode() in output or key.hex() in output:
            raise AssertionError('Synthetic key unexpectedly present in output; refusing to write evidence')
        if '--- SKIP:' in output:
            raise AssertionError('A skipped test is not behavioral evidence')
        destination = ROOT / 'docs/verification/onprem' / ('security-critic-' + args.phase)
        destination.with_suffix('.txt').write_text(output, encoding='utf8')
        after = hashes()
        result = {'phase': args.phase, 'exit_code': run.returncode,
                  'postgres': cluster.query('SHOW server_version;', database='security_critic'),
                  'source_sha256_before': before, 'source_sha256_after': after,
                  'sources_unchanged': before == after,
                  'command': ['go'] + command[1:],
                  'test_output_sha256': hashlib.sha256(output.encode()).hexdigest()}
        destination.with_suffix('.json').write_text(json.dumps(result, indent=2) + '\n', encoding='utf8')
        print(output, end='')
        if before != after:
            raise AssertionError('Executable sources changed during independent experiment')
        return run.returncode
    finally:
        cluster.stop()


if __name__ == '__main__':
    raise SystemExit(main())
