"""Real PostgreSQL proof for the optional SQL-credential containment profile.

Use only an isolated loopback cluster. No passwords or signing keys are written.
The same legacy-denial assertions run before and after profile installation.
"""
import argparse
import hashlib
import json
import os
import secrets
import subprocess
import sys
import time
import uuid
from pathlib import Path

sys.path.insert(0, str(Path(__file__).resolve().parents[1] / 'scripts'))
from migrate_core import build_script

ROOT = Path(__file__).resolve().parents[1]
ORG, WH, OWNER, ITEM, ACTOR, REVIEWER, DOC, LINE, BAL, POS, LOC = [str(uuid.uuid4()) for _ in range(11)]
OTHER_ORG, OTHER_WH, OTHER_DOC, OTHER_OWNER = [str(uuid.uuid4()) for _ in range(4)]


def main():
    parser = argparse.ArgumentParser()
    parser.add_argument('--psql', required=True)
    parser.add_argument('--port', type=int, required=True)
    parser.add_argument('--profile', choices=['baseline', 'hardened'], required=True)
    parser.add_argument('--report', required=True)
    parser.add_argument('--go', help='Optional Go executable for real HTTP/backend integration tests')
    parser.add_argument('--mutant', choices=['signature', 'action', 'worker_fence', 'document_page', 'installation'], help='Only mutate the profile installed in this disposable database')
    parser.add_argument('--legacy-profile', action='store_true', help='SQL-only historical warehouse-bound profile characterization; new adapters require onprem-v1')
    args = parser.parse_args()
    db = 'white_' + secrets.token_hex(5)
    suffix = secrets.token_hex(4)
    admin, owner, api, worker, module = ['arena_admin'] + [x + '_' + suffix for x in ['owner', 'api', 'worker', 'module']]
    base = [args.psql, '-X', '-q', '-A', '-t', '-h', '127.0.0.1', '-p', str(args.port), '-v', 'ON_ERROR_STOP=1', '-v', 'VERBOSITY=verbose']

    def sql(text, role=admin, database=db, must=True):
        result = subprocess.run(base + ['-U', role, '-d', database], input=text, text=True, encoding='utf8', capture_output=True)
        if must and result.returncode:
            raise RuntimeError(result.stderr)
        return result

    sql(f'CREATE ROLE {owner} NOLOGIN; CREATE ROLE {api} LOGIN; CREATE ROLE {worker} LOGIN; CREATE ROLE {module} NOLOGIN;', database='postgres')
    sql(f'CREATE DATABASE {db} OWNER {owner};', database='postgres')
    sql(f'SET ROLE {owner};\n' + build_script(ROOT))
    grant = f'\\set runtime_role {api}\n\\set module_role {module}\nSET ROLE {owner};\n'
    sql(grant + (ROOT / 'db/bootstrap/grants.psql').read_text() + '\n' + (ROOT / 'db/bootstrap/backend-grants.psql').read_text())
    sql(f"""
    INSERT INTO platform.organizations(id,code,name) VALUES('{ORG}','one','one'),('{OTHER_ORG}','two','two');
    INSERT INTO platform.actors(id,issuer,subject) VALUES('{ACTOR}','test','actor'),('{REVIEWER}','test','reviewer');
    INSERT INTO warehouse.warehouses(id,organization_id,code) VALUES('{WH}','{ORG}','one'),('{OTHER_WH}','{OTHER_ORG}','two');
    INSERT INTO catalog.stock_owners(id,organization_id,code) VALUES('{OWNER}','{ORG}','owner'),('{OTHER_OWNER}','{OTHER_ORG}','other');
    INSERT INTO catalog.units VALUES('ea','count');
    INSERT INTO catalog.items(id,organization_id,sku,base_unit,quantity_scale,tracking) VALUES('{ITEM}','{ORG}','sku','ea',0,'none');
    INSERT INTO warehouse.locations(id,organization_id,warehouse_id,code,kind) VALUES('{LOC}','{ORG}','{WH}','one','storage');
    INSERT INTO warehouse.positions(id,organization_id,warehouse_id,location_id) VALUES('{POS}','{ORG}','{WH}','{LOC}');
    INSERT INTO inventory.stock_balances(id,organization_id,warehouse_id,owner_id,item_id,position_id,stock_state,on_hand)
      VALUES('{BAL}','{ORG}','{WH}','{OWNER}','{ITEM}','{POS}','available',10);
    INSERT INTO warehouse.documents(id,organization_id,warehouse_id,owner_id,kind,number,state) VALUES('{DOC}','{ORG}','{WH}','{OWNER}','shipment','one','active');
    INSERT INTO warehouse.documents(id,organization_id,warehouse_id,owner_id,kind,number,state) VALUES('{OTHER_DOC}','{OTHER_ORG}','{OTHER_WH}','{OTHER_OWNER}','shipment','foreign','draft');
    INSERT INTO warehouse.document_lines(id,document_id,organization_id,warehouse_id,owner_id,item_id,line_no,requested_qty)
      VALUES('{LINE}','{DOC}','{ORG}','{WH}','{OWNER}','{ITEM}',1,10);
    INSERT INTO platform.actor_grants(actor_id,organization_id,warehouse_id,owner_id,permission)
      SELECT a,'{ORG}','{WH}','{OWNER}',p FROM unnest(ARRAY['{ACTOR}'::uuid,'{REVIEWER}'::uuid]) a,
      unnest(ARRAY['stock.read','document.read','reservation.read','movement.read','operation.read','reservation.create','reservation.release','movement.create','movement.adjust','document.create','document.transition']) p;
    """)
    key = secrets.token_bytes(32)
    profile_source = ''
    installation = None
    if args.profile == 'hardened':
        historical = (ROOT / 'db/profiles/hardened-v1.sql').read_text()
        forward = '' if args.legacy_profile else (ROOT / 'db/profiles/onprem-v1.sql').read_text()
        original = historical + forward
        if args.mutant == 'signature':
            forward = forward.replace('IF difference<>0 OR parts[10]', 'IF false OR parts[10]')
        elif args.mutant == 'action':
            forward = forward.replace("parts[10] IS DISTINCT FROM encode(hardened_crypto.digest(convert_to(p_action,'UTF8'),'sha256'),'hex')", 'false')
        elif args.mutant == 'worker_fence':
            forward = forward.replace('RETURN platform.complete_outbox(p_id,p_token,p_success);', 'RETURN true;')
        elif args.mutant == 'document_page':
            historical = historical.replace(" IF p_resource='documents' THEN\n  SELECT", " IF false THEN\n  SELECT")
        elif args.mutant == 'installation':
            forward = forward.replace('parts[2] IS DISTINCT FROM installation_id::text', 'false')
        profile_source = historical + forward
        if args.mutant and profile_source == original:
            raise RuntimeError('Mutant did not alter the installed profile')
        sql(f'SET ROLE {owner};\n' + profile_source)
        bootstrap = 'hardened-grants.psql' if args.legacy_profile else 'onprem-grants.psql'
        sql(f'\\set api_role {api}\n\\set worker_role {worker}\n\\set organization_id {ORG}\n\\set warehouse_id {WH}\nSET ROLE {owner};\n' + (ROOT / 'db/bootstrap' / bootstrap).read_text())
        # Synthetic key exists only in process memory and this disposable DB.
        sql(f"INSERT INTO hardened_private.signing_keys(login,key) VALUES('{api}',decode('{key.hex()}','hex'));")
        if not args.legacy_profile:
            installation = sql('SELECT id::text FROM hardened_private.installation WHERE singleton;').stdout.strip()
    results = []

    def check(name, body):
        try:
            body()
            results.append({'name': name, 'status': 'pass'})
        except Exception as exc:
            results.append({'name': name, 'status': 'fail', 'detail': str(exc)})

    def denied(query, role=api):
        r = sql('BEGIN;\n' + query + '\nROLLBACK;', role, must=False)
        if r.returncode == 0:
            raise AssertionError('stolen login accepted forbidden SQL; result=' + r.stdout.strip())
        if '42501' not in r.stderr:
            raise AssertionError('expected permission denial 42501, got ' + r.stderr.strip())

    check('stolen API cannot enumerate foreign documents', lambda: denied(f"SELECT id FROM contracts_v1.documents WHERE id='{OTHER_DOC}';"))
    check('stolen API cannot forge actor via legacy command', lambda: denied(f"SELECT inventory.reserve_stock('{uuid.uuid4()}','{REVIEWER}','{LINE}',1,'{BAL}',1);"))
    check('stolen API cannot claim outbox', lambda: denied('SELECT * FROM platform.claim_outbox(1);'))
    check('stolen API cannot acknowledge outbox', lambda: denied(f"SELECT platform.complete_outbox('{uuid.uuid4()}','{uuid.uuid4()}',true);"))
    check('stolen API cannot run expiry', lambda: denied('SELECT inventory.expire_reservations(1);'))
    if args.profile == 'hardened':
        check('API cannot read signing secret', lambda: denied('SELECT * FROM hardened_private.signing_keys;'))
        check('API cannot read binding registry', lambda: denied('SELECT * FROM hardened_private.bindings;'))
        if not args.legacy_profile:
            check('API cannot read installation registry', lambda: denied('SELECT * FROM hardened_private.installation;'))
        check('API cannot call hardened worker', lambda: denied('SELECT * FROM hardened_worker.claim_outbox(1);'))
        check('API cannot call HMAC signer', lambda: denied("SELECT hardened_crypto.hmac('x','y','sha256');"))
        check('API cannot change owner role', lambda: denied(f'SET ROLE {owner};'))
        check('unsigned identity lookup rejected', lambda: denied("SELECT * FROM hardened_api.resolve('[\"test\",\"reviewer\"]');"))
        check('worker cannot read API', lambda: denied("SELECT * FROM hardened_api.read('documents','{\"limit\":100}');", worker))
        check('missing request context rejected', lambda: denied("SELECT * FROM hardened_api.read('documents','{\"limit\":100}');"))
        check('freely set actor GUC rejected', lambda: denied(f"SET LOCAL wms.actor_id='{REVIEWER}'; SELECT * FROM hardened_api.read('documents','{{\"limit\":100}}');"))
        check('fabricated signed context rejected', lambda: denied("SET LOCAL wms.context='{}'; SET LOCAL wms.context_mac='00'; SELECT * FROM hardened_api.read('documents','{\"limit\":100}');"))

        def signed(body, actor=ACTOR, org=ORG, wh=WH, expiry=None, replay=False, tamper=False, other_backend=False, installation_override=None, action='read|documents|{"limit":100}'):
            # One psql session retains its backend and transaction; stdout protocol
            # deliberately never emits a key, token, or MAC into verification files.
            p = subprocess.Popen(base + ['-U', api, '-d', db], stdin=subprocess.PIPE, stdout=subprocess.PIPE, stderr=subprocess.PIPE, text=True, encoding='utf8', bufsize=1)
            p.stdin.write("BEGIN; SELECT pg_backend_pid()||'|'||pg_current_xact_id()::text;\n")
            p.stdin.flush()
            coordinates = p.stdout.readline().strip()
            pid, xid = coordinates.split('|')
            expires = expiry or int(time.time()) + 20
            digest = hashlib.sha256(action.encode()).hexdigest()
            payload = '|'.join(['1', api, pid, xid, actor, org, wh, str(expires), digest] if args.legacy_profile else
                               ['2', installation_override or installation, api, pid, xid, actor, org, wh, str(expires), digest])
            import hmac
            mac = hmac.new(key, payload.encode(), hashlib.sha256).hexdigest()
            if tamper:
                payload = payload.replace(actor, REVIEWER)
            if other_backend:
                result = sql(f"BEGIN; SELECT set_config('wms.context','{payload}',true),set_config('wms.context_mac','{mac}',true); {body} ROLLBACK;", api, must=False)
                p.stdin.write('ROLLBACK;\n')
                p.stdin.close()
                p.wait(timeout=15)
                return result.returncode, '', result.stderr
            before = 'COMMIT; BEGIN;' if replay else ''
            p.stdin.write(f"{before} SELECT set_config('wms.context','{payload}',true),set_config('wms.context_mac','{mac}',true);\n")
            p.stdin.write(body + '\n')
            p.stdin.write('ROLLBACK;\n')
            p.stdin.close()
            out = p.stdout.read()
            err = p.stderr.read()
            code = p.wait(timeout=15)
            # First result is the opaque context. Do not retain it.
            return code, out.split('\n', 1)[-1].strip(), err

        def signed_denied(**kwargs):
            code, _, error = signed("SELECT * FROM hardened_api.read('documents','{\"limit\":100}');", **kwargs)
            if code == 0 or '42501' not in error:
                raise AssertionError('invalid signed context not rejected')

        check('tampered actor signature rejected', lambda: signed_denied(tamper=True))
        check('expired signed context rejected', lambda: signed_denied(expiry=int(time.time())-1))
        check('excessive context lifetime rejected', lambda: signed_denied(expiry=int(time.time())+120))
        check('signed context for another scope rejected', lambda: signed_denied(org=OTHER_ORG, wh=OTHER_WH))
        if not args.legacy_profile:
            check('signed context for another installation rejected', lambda: signed_denied(installation_override=str(uuid.uuid4())))
            system_actor = '00000000-0000-4000-8000-000000000001'
            sql(f"INSERT INTO platform.actor_grants(actor_id,organization_id,warehouse_id,permission) VALUES('{system_actor}','{ORG}','{WH}','document.read');")
            check('system actor cannot use API despite explicit grants', lambda: signed_denied(actor=system_actor))
        check('signed context replay in another transaction rejected', lambda: signed_denied(replay=True))
        check('signed context replay on another connection rejected', lambda: signed_denied(other_backend=True))
        def raw_bytes():
            code, _, error = signed("SELECT * FROM hardened_api.read('documents','{\"limit\": 100}');")
            if code == 0 or '42501' not in error:
                raise AssertionError('signature accepted changed raw argument bytes')
        check('signature binds exact raw argument bytes', raw_bytes)
        def other_action():
            data = json.dumps({'line': LINE, 'balance': BAL, 'expected_version': 1, 'quantity': '1'})
            code, _, error = signed(f"SELECT hardened_api.command('{uuid.uuid4()}','reserve','{data}');")
            if code == 0 or '42501' not in error:
                raise AssertionError('read signature authorized a different write action')
        check('signature for read cannot authorize a command', other_action)

        def valid_read():
            code, out, err = signed("SELECT count(*) FROM hardened_api.read('documents','{\"limit\":100}');")
            if code or out != '1':
                raise AssertionError(f'valid scoped read failed: code={code}, result={out}, error={err}')
        check('valid signed scoped read returns only local document', valid_read)

        def valid_command():
            command = str(uuid.uuid4())
            data = json.dumps({'line': LINE, 'balance': BAL, 'expected_version': 1, 'quantity': '1'})
            query = f"SELECT hardened_api.command('{command}','reserve','{data}')->>'operation_id';"
            code, out, err = signed(query + '\n' + query, action='command|' + command + '|reserve|' + data)
            if code or out.splitlines() != [command, command]:
                raise AssertionError(f'valid command/retry failed: {code}, {out}, {err}')
            if sql(f"SELECT count(*) FROM platform.operations WHERE id='{command}';").stdout.strip() != '0':
                raise AssertionError('rollback leaked operation')
        check('valid signed command retries identically and rollback persists nothing', valid_command)

        def valid_worker():
            result = sql('SELECT count(*) FROM hardened_worker.claim_outbox(1);', worker)
            if result.stdout.strip() != '0':
                raise AssertionError('unexpected event in fresh fixture')
        check('separate scoped worker surface works', valid_worker)
    files = ['tests/hardened_profile.py', 'scripts/hardened_gate.py', 'db/profiles/hardened-v1.sql', 'db/bootstrap/hardened-grants.psql', 'db/profiles/onprem-v1.sql', 'db/bootstrap/onprem-grants.psql', 'internal/postgres/hardened.go', 'internal/postgres/hardened_jobs.go', 'internal/postgres/hardened_integration_test.go', 'internal/postgres/hardened_onprem_integration_test.go', 'internal/postgres/hardened_load_integration_test.go', 'cmd/wms-api/main.go', 'cmd/wms-api/runtime_profile.go', 'cmd/wms-api/runtime_profile_test.go']
    files += ['internal/postgres/hardened_durability.go', 'internal/postgres/hardened_durability_test.go', 'internal/postgres/hardened_onprem_critic_integration_test.go']
    files += [str(p.relative_to(ROOT)).replace('\\','/') for p in sorted((ROOT / 'db/migrations').glob('*.sql'))]
    report = {'profile': args.profile, 'deployment_profile': 'hardened-v1' if args.legacy_profile else 'onprem-v1', 'installation_id': installation, 'mutant': args.mutant, 'postgres': sql('SHOW server_version;').stdout.strip(),
              'test_sha256': hashlib.sha256(Path(__file__).read_bytes()).hexdigest(),
              'sources_sha256': {p: hashlib.sha256((ROOT / p).read_bytes()).hexdigest() for p in files if (ROOT / p).exists()},
              'executed_profile_sha256': hashlib.sha256(profile_source.encode()).hexdigest() if profile_source else None,
              'results': results, 'passed': all(r['status'] == 'pass' for r in results)}
    path = ROOT / args.report
    path.parent.mkdir(parents=True, exist_ok=True)
    for r in results:
        print(r['status'].upper() + ' ' + r['name'] + (': ' + r['detail'] if 'detail' in r else ''))
    if args.go and args.profile == 'hardened' and report['passed'] and not args.legacy_profile:
        import base64
        env = os.environ.copy()
        env['WMS_HARDENED_ADMIN_DSN'] = f'postgres://{admin}@127.0.0.1:{args.port}/{db}?sslmode=disable'
        env['WMS_HARDENED_API_DSN'] = f'postgres://{api}@127.0.0.1:{args.port}/{db}?sslmode=disable'
        env['WMS_HARDENED_WORKER_DSN'] = f'postgres://{worker}@127.0.0.1:{args.port}/{db}?sslmode=disable'
        env['WMS_HARDENED_KEY'] = base64.b64encode(key).decode()
        env['WMS_HARDENED_FIXTURE'] = json.dumps(dict(org=ORG, wh=WH, owner=OWNER, item=ITEM, actor=ACTOR, reviewer=REVIEWER, document=DOC, line=LINE, balance=BAL, other_org=OTHER_ORG, other_wh=OTHER_WH, other_document=OTHER_DOC))
        run = subprocess.run([args.go, 'test', '-race', '-tags=integration,hardened', './internal/postgres', '-run', '^TestHardened', '-shuffle=on', '-count=1', '-v', '-timeout=180s'], env=env, text=True, encoding='utf8', capture_output=True)
        if '--- SKIP:' in run.stdout:
            raise RuntimeError('hardened tests were skipped')
        # Go test failure messages must never print config/key/context parameters.
        path.with_suffix('.go.txt').write_text(run.stdout + run.stderr)
        print(run.stdout + run.stderr)
        report['go_exit_code'] = run.returncode
        report['go_passed'] = run.returncode == 0
        report['passed'] = report['passed'] and run.returncode == 0
    path.write_text(json.dumps(report, indent=2) + '\n')
    return 0 if report['passed'] else 1


if __name__ == '__main__':
    raise SystemExit(main())
