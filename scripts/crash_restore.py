"""Kill/recover/restore only PostgreSQL processes spawned by this lab.

The workload uses real WMS document/reservation/movement functions. A separate
fsynced JSONL witness records acknowledged results before PostgreSQL is killed.
This proves bounded process-crash recovery, not hardware power-loss durability.
"""
import argparse
from concurrent.futures import ThreadPoolExecutor
from decimal import Decimal
import hashlib
import json
import os
from pathlib import Path
import re
import signal
import socket
import subprocess
import sys
import threading
import time
import uuid

from migrate_core import build_script

ROOT = Path(__file__).resolve().parent.parent
STAGES = ('receipt_create', 'receipt_activate', 'receipt_post', 'receipt_close',
          'shipment_create', 'shipment_activate', 'reservation', 'shipment_post', 'shipment_close')


def canonical(value):
    return json.dumps(value, sort_keys=True, separators=(',', ':'), ensure_ascii=True)


def source_manifest(repo):
    repo = Path(repo).resolve()
    paths = [repo / name for name in ('scripts/crash_restore.py', 'scripts/migrate_core.py',
             'scripts/test_crash_restore.py', 'scripts/test_resilience_assurance_critic.py',
             'scripts/test_onprem_recovery_critic.py', 'scripts/collect_resilience_evidence.py',
             'scripts/test_collect_resilience_evidence.py')]
    paths += sorted((repo / 'db/migrations').glob('*.sql'))
    paths += sorted((repo / 'db/bootstrap').glob('*.psql'))
    paths += sorted((repo / 'db/profiles').glob('*.sql'))
    result = {}
    for path in paths:
        raw = path.read_bytes()
        result[path.relative_to(repo).as_posix()] = {
            'sha256_bytes': hashlib.sha256(raw).hexdigest(),
            'sha256_lf': hashlib.sha256(raw.replace(b'\r\n', b'\n')).hexdigest(),
        }
    return result


def verify_source_manifest(before, after):
    if not before or before != after:
        raise AssertionError('Executable source changed during the recovery experiment')


def sql_string(value):
    return "'" + str(value).replace("'", "''") + "'"


def validate_kill_target(work_root, data, spawned_pid, pid_file):
    root, target = Path(work_root).resolve(), Path(data).resolve()
    lines = pid_file.splitlines()
    if (target == root or not target.is_relative_to(root) or spawned_pid < 2
            or spawned_pid == os.getpid() or len(lines) < 2
            or not lines[0].isdigit() or int(lines[0]) != spawned_pid
            or os.path.normcase(str(Path(lines[1]).resolve())) != os.path.normcase(str(target))):
        raise ValueError('Refusing to kill a process outside the owned disposable cluster')
    return spawned_pid


def validate_durability_settings(settings):
    for name in ('fsync', 'synchronous_commit', 'full_page_writes', 'data_checksums'):
        if settings.get(name) != 'on':
            raise ValueError('Durability evidence requires ' + name + '=on')


def verify_forced_process_exit(code):
    if code is None or code == 0:
        raise ValueError('An abnormal owned PostgreSQL process exit was not observed')


def append_witness(path, record):
    with Path(path).open('a', encoding='utf8', newline='\n') as out:
        out.write(canonical(record) + '\n')
        out.flush()
        os.fsync(out.fileno())


def read_witness(path):
    result = {}
    if not Path(path).exists():
        return result
    for line in Path(path).read_text(encoding='utf8').splitlines():
        record = json.loads(line)
        unit = record['unit_id']
        if unit in result and canonical(result[unit]) != canonical(record):
            raise ValueError('Conflicting results in the external acknowledgement witness')
        result[unit] = record
    return result


def verify_backup(path, digest):
    actual = hashlib.sha256(Path(path).read_bytes()).hexdigest()
    if Path(path).stat().st_size == 0 or actual != digest:
        raise ValueError('Backup checksum mismatch; restore was not started')
    return actual


def verify_acknowledged(expected, observed):
    if not expected:
        raise ValueError('An empty acknowledgement witness is not recovery evidence')
    for unit, record in expected.items():
        if unit not in observed or canonical(record['result']) != canonical(observed[unit]):
            raise AssertionError('Acknowledged WMS result missing or changed: ' + unit)


def verify_canary_state(expected, observed):
    if not expected:
        raise ValueError('An empty persistent canary witness is not recovery evidence')
    balances = {}
    for record in expected.values():
        request = record['request']
        quantity = Decimal(request.get('quantity', '10'))
        reserved = Decimal(request.get('reserve_quantity', '3'))
        if not quantity.is_finite() or not reserved.is_finite() or quantity <= 0 or not 0 < reserved <= quantity:
            raise AssertionError('Invalid persistent canary quantity in the witness')
        totals = balances.setdefault(request['balance'], [Decimal(0), Decimal(0)])
        totals[0] += quantity
        totals[1] += reserved
    for unit, record in expected.items():
        request = record['request']
        quantity, reserved = Decimal(request.get('quantity', '10')), Decimal(request.get('reserve_quantity', '3'))
        on_hand, balance_reserved = balances[request['balance']]
        amounts = {'on_hand': on_hand, 'reserved_qty': balance_reserved, 'active_qty': reserved,
                   'line_reserved': reserved, 'fulfilled_qty': 0, 'requested_qty': quantity}
        state = observed.get(unit)
        if state is None or state.get('state') != 'active' or state.get('version') != 3:
            raise AssertionError('Persistent active canary disappeared or changed state')
        if any(Decimal(state[key]) != value for key, value in amounts.items()):
            raise AssertionError('Persistent nonzero stock/reservation canary changed')
        original = record['result']['shipment_create']
        if (state.get('balance_id') != record['request']['balance'] or state.get('document_id') != original['document_id']
                or state.get('document_line_id') != original['lines'][0]['id']):
            raise AssertionError('Persistent canary identity changed')


def verify_installation_state(expected, observed):
    required = {'installation_id', 'private_state_sha256', 'roles_sha256', 'api_profile', 'worker_profile'}
    if not required.issubset(expected) or canonical(expected) != canonical(observed):
        raise AssertionError('Private installation, signing keys or role capabilities changed during recovery/restore')


def installation_state(cluster):
    state = cluster.json("""SELECT jsonb_build_object(
 'installation_id',(SELECT id::text FROM hardened_private.installation WHERE singleton),
 'private_state_sha256',encode(sha256(convert_to(jsonb_build_object(
  'installation',(SELECT jsonb_agg(to_jsonb(i) ORDER BY id) FROM hardened_private.installation i),
  'bindings',(SELECT jsonb_agg(to_jsonb(b) ORDER BY login) FROM hardened_private.bindings b),
  'signing_keys',(SELECT jsonb_agg(to_jsonb(k) ORDER BY login) FROM hardened_private.signing_keys k))::text,'UTF8')),'hex'),
 'roles_sha256',encode(sha256(convert_to((SELECT jsonb_agg(jsonb_build_object(
  'login',rolname,'canlogin',rolcanlogin,'super',rolsuper,'createdb',rolcreatedb,'createrole',rolcreaterole,
  'replication',rolreplication,'bypassrls',rolbypassrls,'config',rolconfig,
  'memberships',(SELECT coalesce(jsonb_agg(parent.rolname ORDER BY parent.rolname),'[]'::jsonb) FROM pg_auth_members m JOIN pg_roles parent ON parent.oid=m.roleid WHERE m.member=r.oid)) ORDER BY rolname)
  FROM pg_roles r WHERE rolname IN('wms_resilience_api','wms_resilience_worker'))::text,'UTF8')),'hex'));
""")
    state['api_profile'] = cluster.json('SELECT hardened_api.profile();', user='wms_resilience_api')
    state['worker_profile'] = cluster.json('SELECT hardened_worker.profile();', user='wms_resilience_worker')
    if (state['api_profile'].get('profile') != 'onprem-v1' or state['worker_profile'].get('profile') != 'onprem-v1'
            or state['api_profile'].get('installation_id') != state['installation_id']
            or state['worker_profile'].get('installation_id') != state['installation_id']):
        raise AssertionError('Recovered runtime profiles do not attest the original installation')
    return state


def cleanup_clusters(clusters):
    errors = {}
    for cluster in clusters:
        try:
            cluster.stop()
        except Exception as error:
            errors[cluster.directory.name] = type(error).__name__
    return errors


class Cluster:
    def __init__(self, binaries, root, label):
        self.binaries = Path(binaries).resolve()
        self.root = Path(root).resolve()
        if not re.fullmatch(r'[a-z][a-z0-9_-]{0,63}', label):
            raise ValueError('Invalid owned cluster directory label')
        self.directory = (self.root / label).resolve()
        if self.directory.parent != self.root:
            raise ValueError('Cluster directory is outside the owned work root')
        self.data = self.directory / 'pgdata'
        self.directory.mkdir(parents=True)
        self.log = self.directory / 'postgres.log'
        self.process = None
        self.log_handle = None
        self.log_offset = 0
        self.port = self.available_port()
        self.environment = {k: v for k, v in os.environ.items() if not k.upper().startswith('PG')}
        self.environment.update(PGHOST='127.0.0.1', PGPORT=str(self.port), PGUSER='wms_test_admin', PGDATABASE='postgres', LC_ALL='C')

    @staticmethod
    def available_port():
        with socket.socket() as listener:
            listener.bind(('127.0.0.1', 0))
            return listener.getsockname()[1]

    def executable(self, name):
        path = self.binaries / (name + ('.exe' if os.name == 'nt' else ''))
        if not path.is_file():
            raise FileNotFoundError('Missing PostgreSQL tool: ' + name)
        return str(path)

    def command(self, name, arguments, data=None, database='postgres', user='wms_test_admin', app=None, timeout=30):
        env = dict(self.environment, PGDATABASE=database, PGUSER=user)
        if app:
            env['PGAPPNAME'] = app
        result = subprocess.run([self.executable(name)] + arguments, input=data, capture_output=True,
                                env=env, timeout=timeout, creationflags=subprocess.CREATE_NO_WINDOW if os.name == 'nt' else 0)
        if result.returncode:
            with (self.directory / 'client-errors.txt').open('ab') as out:
                out.write(result.stderr)
            raise RuntimeError('Disposable PostgreSQL command failed: ' + name)
        return result.stdout

    def query(self, sql, database='wms_kernel_test_resilience', user='wms_test_admin', app=None):
        return self.command('psql', ['-X', '-q', '-At', '-v', 'ON_ERROR_STOP=1'],
                            data=sql.encode('utf8'), database=database, user=user, app=app).decode('utf8').strip()

    def json(self, sql, database='wms_kernel_test_resilience', user='wms_test_admin'):
        return json.loads(self.query(sql, database, user))

    def background(self, sql, app):
        env = dict(self.environment, PGDATABASE='wms_kernel_test_resilience', PGAPPNAME=app)
        stdout = (self.directory / (app + '.out')).open('wb')
        stderr = (self.directory / (app + '.err')).open('wb')
        process = subprocess.Popen([self.executable('psql'), '-X', '-q', '-At', '-v', 'ON_ERROR_STOP=1'],
                                   stdin=subprocess.PIPE, stdout=stdout, stderr=stderr, env=env,
                                   creationflags=subprocess.CREATE_NO_WINDOW if os.name == 'nt' else 0)
        stdout.close()
        stderr.close()
        process.stdin.write(sql.encode('utf8'))
        process.stdin.close()
        return process

    def initialize(self):
        output = self.command('initdb', ['-D', str(self.data), '-U', 'wms_test_admin', '-A', 'trust',
                                        '--data-checksums', '--encoding=UTF8', '--locale=C'], timeout=60)
        (self.directory / 'initdb.txt').write_bytes(output)
        self.start()

    def start(self):
        self.log_offset = self.log.stat().st_size if self.log.exists() else 0
        self.log_handle = self.log.open('ab', buffering=0)
        options = dict(stdin=subprocess.DEVNULL, stdout=self.log_handle, stderr=self.log_handle, env=self.environment)
        if os.name == 'nt':
            options['creationflags'] = subprocess.CREATE_NEW_PROCESS_GROUP | subprocess.CREATE_NO_WINDOW
        else:
            options['start_new_session'] = True
        self.process = subprocess.Popen([self.executable('postgres'), '-D', str(self.data), '-h', '127.0.0.1', '-p', str(self.port),
                                         '-c', 'fsync=on', '-c', 'synchronous_commit=on', '-c', 'full_page_writes=on',
                                         '-c', 'unix_socket_directories=',
                                         '-c', 'shared_buffers=32MB', '-c', 'max_connections=40', '-c', 'log_checkpoints=on'], **options)
        started = time.monotonic()
        deadline = started + 40
        while time.monotonic() < deadline:
            if self.process.poll() is not None:
                raise RuntimeError('Owned PostgreSQL exited while starting; inspect postgres.log')
            try:
                if self.query('SELECT 1;', database='postgres') == '1':
                    actual_data = Path(self.query('SHOW data_directory;', database='postgres')).resolve()
                    if os.path.normcase(str(actual_data)) != os.path.normcase(str(self.data)):
                        raise ValueError('Readiness belongs to a different PGDATA, not the owned server')
                    validate_kill_target(self.root, self.data, self.process.pid, (self.data / 'postmaster.pid').read_text())
                    return round(time.monotonic() - started, 6)
            except (RuntimeError, subprocess.TimeoutExpired):
                time.sleep(.05)
        raise TimeoutError('Owned PostgreSQL did not become ready')

    def kill(self):
        if self.process is None or self.process.poll() is not None:
            raise ValueError('Owned PostgreSQL is not running')
        pid = validate_kill_target(self.root, self.data, self.process.pid, (self.data / 'postmaster.pid').read_text())
        if os.name == 'nt':
            result = subprocess.run(['taskkill.exe', '/F', '/T', '/PID', str(pid)], capture_output=True,
                                    creationflags=subprocess.CREATE_NO_WINDOW, timeout=20)
            (self.directory / 'forced-termination.txt').write_bytes(result.stdout + result.stderr)
            method = 'Windows TerminateProcess via taskkill /F /T'
        else:
            if os.getpgid(pid) != pid:
                raise ValueError('Owned PostgreSQL process group changed')
            os.killpg(pid, signal.SIGKILL)
            method = 'SIGKILL to the owned PostgreSQL process group'
        self.process.wait(timeout=20)
        verify_forced_process_exit(self.process.returncode)
        if self.log_handle:
            self.log_handle.close()
            self.log_handle = None
        return {'pid': pid, 'method': method, 'exit_code': self.process.returncode,
                'tree_tool_returncode': result.returncode if os.name == 'nt' else None,
                'pgdata': str(self.data.relative_to(self.root))}

    def recovery_log(self):
        with self.log.open('rb') as source:
            source.seek(self.log_offset)
            text = source.read().decode('utf8', errors='replace')
        if 'database system was interrupted' not in text and 'database system was not properly shut down' not in text:
            raise AssertionError('Restart did not prove an unclean PostgreSQL shutdown')
        if 'redo starts at' not in text or 'database system is ready to accept connections' not in text:
            raise AssertionError('Restart did not prove WAL redo and readiness')
        return text

    def stop(self):
        if self.process is not None and self.process.poll() is None:
            validate_kill_target(self.root, self.data, self.process.pid, (self.data / 'postmaster.pid').read_text())
            self.command('pg_ctl', ['-D', str(self.data), '-m', 'fast', '-w', 'stop'], timeout=30)
            self.process.wait(timeout=10)
        if self.log_handle:
            self.log_handle.close()
            self.log_handle = None


def create_roles(cluster):
    cluster.query('CREATE ROLE wms_test_owner NOLOGIN NOSUPERUSER NOCREATEDB NOCREATEROLE;'
                  'CREATE ROLE wms_test_runtime NOLOGIN; CREATE ROLE wms_test_module NOLOGIN;'
                  'CREATE ROLE wms_go_runtime LOGIN NOSUPERUSER NOCREATEDB NOCREATEROLE IN ROLE wms_test_runtime;'
                  'CREATE ROLE wms_resilience_api LOGIN NOSUPERUSER NOCREATEDB NOCREATEROLE;'
                  'CREATE ROLE wms_resilience_worker LOGIN NOSUPERUSER NOCREATEDB NOCREATEROLE;', database='postgres')


def grants(cluster, repo, database):
    script = '\\set runtime_role wms_test_runtime\n\\set module_role wms_test_module\nSET ROLE wms_test_owner;\n'
    script += (repo / 'db/bootstrap/grants.psql').read_text() + '\n' + (repo / 'db/bootstrap/backend-grants.psql').read_text()
    cluster.query(script, database=database)


def onprem_grants(cluster, repo):
    # The synthetic lab schema belongs to its administrative fixture creator;
    # use that administrator to strip grants on every schema during bootstrap.
    cluster.query('\\set api_role wms_resilience_api\n\\set worker_role wms_resilience_worker\n'
                  + (repo / 'db/bootstrap/onprem-grants.psql').read_text())


UNIT_FUNCTION = r'''
CREATE SCHEMA resilience_lab;
CREATE FUNCTION resilience_lab.execute_unit(p jsonb) RETURNS jsonb LANGUAGE plpgsql AS $$
DECLARE rc jsonb;ra jsonb;rp jsonb;rz jsonb;sc jsonb;sa jsonb;sr jsonb;sp jsonb;sz jsonb;
 org uuid:=(p->>'org')::uuid; wh uuid:=(p->>'warehouse')::uuid; own uuid:=(p->>'owner')::uuid;
 actor uuid:=(p->>'actor')::uuid; balance uuid:=(p->>'balance')::uuid; result jsonb;
 qty numeric:=coalesce(p->>'quantity','1')::numeric; reserve_qty numeric:=coalesce(p->>'reserve_quantity','1')::numeric;
BEGIN
 rc:=warehouse.command_document((p->'commands'->>0)::uuid,actor,NULL,jsonb_build_object(
  'organization_id',org,'warehouse_id',wh,'owner_id',own,'kind','receipt','number','r-'||(p->>'unit_id'),
  'lines',jsonb_build_array(jsonb_build_object('item_id',p->>'item','quantity',qty::text))));
 ra:=warehouse.command_document((p->'commands'->>1)::uuid,actor,(rc->>'document_id')::uuid,
  jsonb_build_object('organization_id',org,'warehouse_id',wh,'expected_document_version',1,'state','active'));
 rp:=inventory.post_movement((p->'commands'->>2)::uuid,actor,(rc#>>'{lines,0,id}')::uuid,2,'receipt',NULL,balance,qty);
 rz:=warehouse.command_document((p->'commands'->>3)::uuid,actor,(rc->>'document_id')::uuid,
  jsonb_build_object('organization_id',org,'warehouse_id',wh,'expected_document_version',3,'state','closed'));
 sc:=warehouse.command_document((p->'commands'->>4)::uuid,actor,NULL,jsonb_build_object(
  'organization_id',org,'warehouse_id',wh,'owner_id',own,'kind','shipment','number','s-'||(p->>'unit_id'),
  'lines',jsonb_build_array(jsonb_build_object('item_id',p->>'item','quantity',qty::text))));
 sa:=warehouse.command_document((p->'commands'->>5)::uuid,actor,(sc->>'document_id')::uuid,
  jsonb_build_object('organization_id',org,'warehouse_id',wh,'expected_document_version',1,'state','active'));
 sr:=inventory.reserve_stock((p->'commands'->>6)::uuid,actor,(sc#>>'{lines,0,id}')::uuid,2,balance,reserve_qty,NULL);
 IF p->>'mode'='canary' THEN
  RETURN jsonb_build_object('receipt_create',rc,'receipt_activate',ra,'receipt_post',rp,'receipt_close',rz,
   'shipment_create',sc,'shipment_activate',sa,'reservation',sr);
 END IF;
 sp:=inventory.post_movement((p->'commands'->>7)::uuid,actor,(sc#>>'{lines,0,id}')::uuid,3,'issue',balance,NULL,qty,(sr->>'reservation_id')::uuid);
 sz:=warehouse.command_document((p->'commands'->>8)::uuid,actor,(sc->>'document_id')::uuid,
  jsonb_build_object('organization_id',org,'warehouse_id',wh,'expected_document_version',4,'state','closed'));
 RETURN jsonb_build_object('receipt_create',rc,'receipt_activate',ra,'receipt_post',rp,'receipt_close',rz,
  'shipment_create',sc,'shipment_activate',sa,'reservation',sr,'shipment_post',sp,'shipment_close',sz);
END $$;
'''


def seed_fixture(cluster, organization, index, fractional):
    fixture = {key: str(uuid.uuid4()) for key in ('org', 'warehouse', 'owner', 'actor', 'item', 'position', 'probe_position', 'canary_position')}
    fixture['org'] = organization
    s = lambda key: sql_string(fixture[key])
    cluster.query(f"""
INSERT INTO platform.organizations(id,code,name) VALUES({s('org')},'resilience-lab-{organization[-8:]}','Synthetic resilience lab') ON CONFLICT(id) DO NOTHING;
INSERT INTO platform.actors(id,issuer,subject) VALUES({s('actor')},'https://resilience.invalid','synthetic-worker-{index}');
INSERT INTO catalog.stock_owners(id,organization_id,code) VALUES({s('owner')},{s('org')},'synthetic-owner-{index}');
INSERT INTO catalog.units(code,dimension) VALUES('ea','count') ON CONFLICT DO NOTHING;
INSERT INTO catalog.items(id,organization_id,sku,base_unit,quantity_scale,tracking) VALUES({s('item')},{s('org')},'LAB-SKU-{index}','ea',{6 if fractional else 0},'none');
INSERT INTO warehouse.warehouses(id,organization_id,code) VALUES({s('warehouse')},{s('org')},'LAB-{index}');
INSERT INTO warehouse.locations(id,organization_id,warehouse_id,code,kind) VALUES({s('position')},{s('org')},{s('warehouse')},'MAIN','storage'),({s('probe_position')},{s('org')},{s('warehouse')},'PROBE','storage');
INSERT INTO warehouse.positions(id,organization_id,warehouse_id,location_id) VALUES({s('position')},{s('org')},{s('warehouse')},{s('position')}),({s('probe_position')},{s('org')},{s('warehouse')},{s('probe_position')});
INSERT INTO platform.actor_grants(actor_id,organization_id,warehouse_id,owner_id,permission)
 SELECT {s('actor')},{s('org')},{s('warehouse')},{s('owner')},p FROM unnest(ARRAY['document.create','document.transition','document.read']) p;
""")
    cluster.query(f"INSERT INTO warehouse.locations(id,organization_id,warehouse_id,code,kind) VALUES({s('canary_position')},{s('org')},{s('warehouse')},'CANARY','storage');"
                  f"INSERT INTO warehouse.positions(id,organization_id,warehouse_id,location_id) VALUES({s('canary_position')},{s('org')},{s('warehouse')},{s('canary_position')});")
    for key, position in (('balance', 'position'), ('probe_balance', 'probe_position'), ('canary_balance', 'canary_position')):
        fixture[key] = cluster.query(f"SELECT inventory.ensure_balance({s('org')},{s('warehouse')},{s('owner')},{s('item')},{s(position)},'available');")
    if fractional:
        fixture.update(fractional=True, canary_quantity=str(Decimal('10.125001') + Decimal(index)/1000000),
                       canary_reserved=str(Decimal('0.375001') + Decimal(index)/1000000))
    return fixture


def seed(cluster, repo, onprem=False, multiple=False):
    create_roles(cluster)
    cluster.query('CREATE DATABASE wms_kernel_test_resilience OWNER wms_test_owner;', database='postgres')
    cluster.query('SET ROLE wms_test_owner;\n' + build_script(repo))
    grants(cluster, repo, 'wms_kernel_test_resilience')
    organizations = [str(uuid.uuid4()) for _ in range(2 if multiple else 1)]
    fixtures = [seed_fixture(cluster, organizations[index//2] if multiple else organizations[0], index, multiple)
                for index in range(4 if multiple else 1)]
    cluster.query(UNIT_FUNCTION)
    if onprem:
        cluster.query('SET ROLE wms_test_owner;\n' + (repo / 'db/profiles/hardened-v1.sql').read_text()
                      + '\n' + (repo / 'db/profiles/onprem-v1.sql').read_text())
        onprem_grants(cluster, repo)
        cluster.query("INSERT INTO hardened_private.signing_keys(login,key) SELECT 'wms_resilience_api',hardened_crypto.gen_random_bytes(32);")
    fixture = dict(fixtures[0])
    if multiple:
        fixture['warehouse_fixtures'] = fixtures
    return fixture


def unit_request(fixture, probe=False, canary=False):
    unit = uuid.uuid4()
    if 'warehouse_fixtures' in fixture:
        fixture = fixture['warehouse_fixtures'][unit.int % len(fixture['warehouse_fixtures'])]
    request = {key: fixture[key] for key in ('org', 'warehouse', 'owner', 'actor', 'item')}
    quantity = ('0.000001', '1.234567', '7.000003')[(unit.int//4) % 3] if fixture.get('fractional') else '1'
    request.update(unit_id=str(unit), commands=[str(uuid.uuid4()) for _ in STAGES], balance=fixture['probe_balance' if probe else 'balance'], quantity=quantity, reserve_quantity=quantity)
    if canary:
        request.update(mode='canary', quantity=fixture.get('canary_quantity', '10'), reserve_quantity=fixture.get('canary_reserved', '3'), balance=fixture['canary_balance'], commands=request['commands'][:7])
    return request


def unit_sql(request):
    return 'SELECT resilience_lab.execute_unit(' + sql_string(canonical(request)) + '::jsonb);'


def operation_results(cluster, records, database='wms_kernel_test_resilience'):
    operations = cluster.json("SELECT coalesce(jsonb_object_agg(id::text,result),'{}'::jsonb) FROM platform.operations;", database)
    observed = {}
    for unit, record in records.items():
        commands = record['request']['commands']
        if all(command in operations for command in commands):
            observed[unit] = {stage: operations[command] for stage, command in zip(STAGES, commands)}
    return observed


def canary_checks(cluster, canaries, database='wms_kernel_test_resilience'):
    verify_acknowledged(canaries, operation_results(cluster, canaries, database))
    observed = {}
    for unit, record in canaries.items():
        reservation = record['result']['reservation']['reservation_id']
        value = cluster.query("SELECT jsonb_build_object('balance_id',b.id::text,'document_id',d.id::text,'document_line_id',l.id::text,"
                              "'on_hand',b.on_hand::text,'reserved_qty',b.reserved_qty::text,"
                              "'active_qty',r.active_qty::text,'line_reserved',l.reserved_qty::text,'fulfilled_qty',l.fulfilled_qty::text,"
                              "'requested_qty',l.requested_qty::text,'state',d.state,'version',d.version)"
                              " FROM inventory.reservations r JOIN warehouse.document_lines l ON l.id=r.document_line_id"
                              " JOIN warehouse.documents d ON d.id=l.document_id JOIN inventory.stock_balances b ON b.id=r.balance_id"
                              " WHERE r.id=" + sql_string(reservation) + '::uuid;', database)
        if value:
            observed[unit] = json.loads(value)
    verify_canary_state(canaries, observed)
    return {'acknowledged_nonzero_canaries': len(canaries),
            'on_hand': str(sum((Decimal(record['request'].get('quantity', '10')) for record in canaries.values()), Decimal(0))),
            'active_reserved': str(sum((Decimal(record['request'].get('reserve_quantity', '3')) for record in canaries.values()), Decimal(0))),
            'all_original_results_and_live_states': 'passed'}


def business_checks(cluster, records, canaries, database='wms_kernel_test_resilience'):
    n, c = len(records), len(canaries)
    counts = cluster.json("""SELECT jsonb_build_object(
 'operations',(SELECT count(*) FROM platform.operations),'incomplete',(SELECT count(*) FROM platform.operations WHERE completed_at IS NULL OR result IS NULL),
 'audit',(SELECT count(*) FROM platform.audit_entries),'outbox',(SELECT count(*) FROM platform.outbox),
 'documents',(SELECT count(*) FROM warehouse.documents),'closed',(SELECT count(*) FROM warehouse.documents WHERE state='closed'),
 'lines',(SELECT count(*) FROM warehouse.document_lines),'movements',(SELECT count(*) FROM inventory.movements),
 'reservations',(SELECT count(*) FROM inventory.reservations),'active',(SELECT coalesce(sum(active_qty),0)::text FROM inventory.reservations),
 'on_hand',(SELECT coalesce(sum(on_hand),0)::text FROM inventory.stock_balances),
 'reserved',(SELECT coalesce(sum(reserved_qty),0)::text FROM inventory.stock_balances));""", database)
    for key, expected in {'operations': 9*n+7*c, 'incomplete': 0, 'audit': 9*n+7*c, 'outbox': 9*n+7*c, 'documents': 2*(n+c), 'closed': 2*n+c, 'lines': 2*(n+c), 'movements': 2*n+c, 'reservations': n+c}.items():
        if counts[key] != expected:
            raise AssertionError('Post-recovery exact effect mismatch: ' + key)
    on_hand = sum((Decimal(record['request'].get('quantity', '10')) for record in canaries.values()), Decimal(0))
    reserved = sum((Decimal(record['request'].get('reserve_quantity', '3')) for record in canaries.values()), Decimal(0))
    for key, expected in (('active', reserved), ('on_hand', on_hand), ('reserved', reserved)):
        if Decimal(counts[key]) != expected:
            raise AssertionError('Post-recovery quantity mismatch: ' + key)
    mismatches = cluster.json("""WITH active AS(SELECT balance_id,sum(active_qty) qty FROM inventory.reservations GROUP BY balance_id),
 deltas AS(SELECT target_balance_id balance_id,quantity qty FROM inventory.movement_lines WHERE target_balance_id IS NOT NULL UNION ALL SELECT source_balance_id,-quantity FROM inventory.movement_lines WHERE source_balance_id IS NOT NULL),
 totals AS(SELECT balance_id,sum(qty) qty FROM deltas GROUP BY balance_id),
 line_active AS(SELECT document_line_id,sum(active_qty) qty FROM inventory.reservations GROUP BY document_line_id)
 SELECT jsonb_build_object('balance_reservations',(SELECT count(*) FROM inventory.stock_balances b LEFT JOIN active a ON a.balance_id=b.id WHERE b.reserved_qty<>coalesce(a.qty,0)),
 'movement_ledger',(SELECT count(*) FROM inventory.stock_balances b LEFT JOIN totals t ON t.balance_id=b.id WHERE b.on_hand<>coalesce(t.qty,0)),
 'document_reservations',(SELECT count(*) FROM warehouse.document_lines l LEFT JOIN line_active a ON a.document_line_id=l.id WHERE l.reserved_qty<>coalesce(a.qty,0)));""", database)
    if any(mismatches.values()):
        raise AssertionError('Post-recovery reconciliation mismatch')
    if int(cluster.query('SELECT count(*) FROM contracts_v1.document_details;', database, 'wms_go_runtime')) != 2*(n+c):
        raise AssertionError('Restored application read contract differs')
    expected_scopes = {}
    for kind, units in (('normal', records), ('canary', canaries)):
        for record in units.values():
            request = record['request']
            scope = (request['org'], request['warehouse'])
            expected = expected_scopes.setdefault(scope, {'operations': 0, 'documents': 0, 'on_hand': Decimal(0), 'reserved': Decimal(0)})
            expected['operations'] += 9 if kind == 'normal' else 7
            expected['documents'] += 2
            if kind == 'canary':
                expected['on_hand'] += Decimal(request.get('quantity', '10'))
                expected['reserved'] += Decimal(request.get('reserve_quantity', '3'))
    observed_scopes = cluster.json("""SELECT jsonb_agg(jsonb_build_object('org',w.organization_id::text,'warehouse',w.id::text,
 'operations',(SELECT count(*) FROM platform.operations o WHERE o.organization_id=w.organization_id AND o.warehouse_id=w.id),
 'documents',(SELECT count(*) FROM warehouse.documents d WHERE d.organization_id=w.organization_id AND d.warehouse_id=w.id),
 'on_hand',(SELECT coalesce(sum(b.on_hand),0)::text FROM inventory.stock_balances b WHERE b.organization_id=w.organization_id AND b.warehouse_id=w.id),
 'reserved',(SELECT coalesce(sum(b.reserved_qty),0)::text FROM inventory.stock_balances b WHERE b.organization_id=w.organization_id AND b.warehouse_id=w.id)) ORDER BY w.organization_id,w.id)
 FROM warehouse.warehouses w;""", database)
    if len(observed_scopes) != len(expected_scopes):
        raise AssertionError('Warehouse coverage changed during recovery')
    for observed in observed_scopes:
        expected = expected_scopes[(observed['org'], observed['warehouse'])]
        if (observed['operations'] != expected['operations'] or observed['documents'] != expected['documents']
                or Decimal(observed['on_hand']) != expected['on_hand'] or Decimal(observed['reserved']) != expected['reserved']):
            raise AssertionError('Per-warehouse independent decimal oracle differs after recovery')
    return {'exact_counts': counts, 'reconciliation': mismatches, 'runtime_read_contract': 'passed',
            'warehouse_decimal_oracle': observed_scopes,
            'organizations_verified': len({scope[0] for scope in expected_scopes}), 'warehouses_verified': len(expected_scopes)}


def wait_barrier(cluster, application, command_ids=None):
    deadline = time.monotonic() + 15
    while time.monotonic() < deadline:
        sleeping = int(cluster.query("SELECT count(*) FROM pg_stat_activity WHERE application_name=" + sql_string(application) + " AND wait_event='PgSleep' AND xact_start IS NOT NULL;"))
        committed = True
        if command_ids:
            literals = ','.join(sql_string(value) for value in command_ids)
            committed = int(cluster.query('SELECT count(*) FROM platform.operations WHERE completed_at IS NOT NULL AND id IN (' + literals + ');')) == len(command_ids)
        if sleeping and committed:
            return
        time.sleep(.03)
    raise AssertionError('Explicit transaction barrier was not reached: ' + application)


def percentile(values, percentile):
    if not values:
        return None
    return round(sorted(values)[min(len(values)-1, int((len(values)-1)*percentile))], 6)


def crash_cycle(cluster, fixture, directory, cycle, seconds, writer_count, expected_installation=None):
    cluster.query('CHECKPOINT;')
    # The new nonzero state is acknowledged AFTER the checkpoint, so each
    # crash must recover its latest stock and live reservation from WAL.
    for warehouse in fixture.get('warehouse_fixtures', [fixture]):
        canary = unit_request(warehouse, canary=True)
        canary_result = cluster.json(unit_sql(canary))
        append_witness(directory / 'canaries.jsonl', {'unit_id': canary['unit_id'], 'request': canary, 'result': canary_result})
    candidates_path, witness_path = directory / 'candidates.jsonl', directory / 'acknowledged.jsonl'
    acknowledgement_count_at_start = len(read_witness(witness_path))
    committed_lost = unit_request(fixture)
    uncommitted = unit_request(fixture, probe=True)
    for request in (committed_lost, uncommitted):
        append_witness(candidates_path, {'unit_id': request['unit_id'], 'request': request})
    lost_app, open_app = f'resilience_lost_{cycle}', f'resilience_open_{cycle}'
    lost = cluster.background('BEGIN;' + unit_sql(committed_lost) + 'COMMIT;SELECT pg_sleep(600);', lost_app)
    wait_barrier(cluster, lost_app, committed_lost['commands'])
    held = cluster.background('BEGIN;' + unit_sql(uncommitted) + 'SELECT pg_sleep(600);COMMIT;', open_app)
    wait_barrier(cluster, open_app)
    stop = threading.Event()
    lock = threading.Lock()
    statistics_data = {'acknowledged_units': 0, 'read_requests': 0, 'interrupted_writes': 0, 'interrupted_reads': 0, 'latencies': []}

    def writer():
        while not stop.is_set():
            request = unit_request(fixture)
            with lock:
                append_witness(candidates_path, {'unit_id': request['unit_id'], 'request': request})
            started = time.monotonic()
            try:
                result = cluster.json(unit_sql(request))
                with lock:
                    append_witness(witness_path, {'unit_id': request['unit_id'], 'request': request, 'result': result})
                    statistics_data['acknowledged_units'] += 1
                    statistics_data['latencies'].append(time.monotonic() - started)
            except (RuntimeError, subprocess.TimeoutExpired):
                with lock:
                    statistics_data['interrupted_writes'] += 1
                if not stop.wait(.02):
                    continue

    def reader():
        while not stop.is_set():
            try:
                result = cluster.json("SELECT jsonb_build_object('incomplete',(SELECT count(*) FROM platform.operations WHERE completed_at IS NULL),'documents',(SELECT coalesce(jsonb_agg(p),'[]') FROM (SELECT id,state,version FROM contracts_v1.document_details ORDER BY id LIMIT 20) p));")
                if result['incomplete'] != 0:
                    raise AssertionError('Concurrent reader saw an incomplete operation')
                with lock:
                    statistics_data['read_requests'] += 1
            except (RuntimeError, subprocess.TimeoutExpired):
                with lock:
                    statistics_data['interrupted_reads'] += 1
            stop.wait(.01)

    started = time.monotonic()
    with ThreadPoolExecutor(max_workers=writer_count+1) as pool:
        futures = [pool.submit(writer) for _ in range(writer_count)] + [pool.submit(reader)]
        try:
            deadline = started + max(seconds+20, 30)
            while time.monotonic() - started < seconds or statistics_data['acknowledged_units'] < 8 or statistics_data['read_requests'] < 5:
                for future in futures:
                    if future.done():
                        future.result()
                if time.monotonic() > deadline:
                    raise AssertionError('Concurrent WMS workload failed to reach the minimum witness/read counts')
                time.sleep(.03)
            # Readers of the witness use the same lock as append+fsync, so a
            # partially written final JSON line cannot masquerade as evidence.
            with lock:
                acknowledged_at_kill = len(read_witness(witness_path))
                killed = cluster.kill()
        finally:
            stop.set()
        for future in futures:
            future.result(timeout=15)
    for client in (lost, held):
        client.wait(timeout=15)
    # A response can reach a writer just before the kill but be witnessed just
    # after it. Verify these late acknowledgements BEFORE replay can repair any
    # missing operation; all producers have joined, so this snapshot is stable.
    acknowledged_before_restart = read_witness(witness_path)
    if len(acknowledged_before_restart) - acknowledgement_count_at_start != statistics_data['acknowledged_units']:
        raise AssertionError('The external witness lost or duplicated an observed acknowledgement')
    elapsed = time.monotonic() - started
    recovery_seconds = cluster.start()
    recovery_text = cluster.recovery_log()
    (directory / f'cycle-{cycle}-wal-recovery.txt').write_text(recovery_text, encoding='utf8')
    if expected_installation is not None:
        verify_installation_state(expected_installation, installation_state(cluster))
    observed = operation_results(cluster, acknowledged_before_restart)
    verify_acknowledged(acknowledged_before_restart, observed)
    canaries = read_witness(directory / 'canaries.jsonl')
    canary_evidence = canary_checks(cluster, canaries)
    literals = ','.join(sql_string(value) for value in uncommitted['commands'])
    if cluster.query('SELECT count(*) FROM platform.operations WHERE id IN (' + literals + ');') != '0':
        raise AssertionError('Uncommitted WMS operation survived process death')
    if cluster.query('SELECT count(*) FROM warehouse.documents WHERE number IN (' + sql_string('r-'+uncommitted['unit_id']) + ',' + sql_string('s-'+uncommitted['unit_id']) + ');') != '0':
        raise AssertionError('Uncommitted document survived process death')
    lost_literals = ','.join(sql_string(value) for value in committed_lost['commands'])
    if cluster.query('SELECT count(*) FROM platform.operations WHERE id IN (' + lost_literals + ');') != '9':
        raise AssertionError('Committed but unacknowledged unit did not survive WAL recovery')
    replayed = 0
    all_candidates = read_witness(candidates_path)
    for unit, candidate in all_candidates.items():
        result = cluster.json(unit_sql(candidate['request']))
        if unit in acknowledged_before_restart:
            if canonical(result) != canonical(acknowledged_before_restart[unit]['result']):
                raise AssertionError('Same-ID replay changed a known acknowledged result')
        else:
            append_witness(witness_path, dict(candidate, result=result))
            replayed += 1
    witness = read_witness(witness_path)
    verify_acknowledged(witness, operation_results(cluster, witness))
    checks = business_checks(cluster, witness, canaries)
    return {'cycle': cycle, 'kill': killed, 'restart_pid': cluster.process.pid, 'recovery_seconds': recovery_seconds,
            'workload_seconds': round(elapsed, 6), 'writers': writer_count,
            'acknowledged_units_before_kill': acknowledged_at_kill,
            'acknowledged_units_verified_before_replay': len(acknowledged_before_restart),
            'new_acknowledged_units': statistics_data['acknowledged_units'], 'read_requests': statistics_data['read_requests'],
            'interrupted_writes': statistics_data['interrupted_writes'], 'interrupted_reads': statistics_data['interrupted_reads'],
            'unit_latency_p50_seconds': percentile(statistics_data['latencies'], .5),
            'unit_latency_p95_seconds': percentile(statistics_data['latencies'], .95),
            'same_id_replayed_units': len(all_candidates), 'newly_confirmed_after_replay': replayed,
            'known_uncommitted_absent_before_replay': True, 'known_committed_without_application_witness_survived': True,
            'nonzero_canaries_verified_before_replay': canary_evidence,
            'onprem_identity_verified_before_replay': expected_installation is not None,
            'checks': checks}


def restore_proof(source, target, repo, directory, witness, canaries, expected_installation=None):
    archive = directory / 'synthetic-wms.dump'
    started = time.monotonic()
    source.command('pg_dump', ['-Fc', '--no-owner', '--no-privileges', '-f', str(archive)], database='wms_kernel_test_resilience', timeout=90)
    with archive.open('r+b') as backup:
        os.fsync(backup.fileno())
    digest = hashlib.sha256(archive.read_bytes()).hexdigest()
    verify_backup(archive, digest)
    corrupt = directory / 'corrupt-negative-control.dump'
    raw = bytearray(archive.read_bytes())
    raw[len(raw)//2] ^= 1
    corrupt.write_bytes(raw)
    refused = False
    try:
        verify_backup(corrupt, digest)
    except ValueError:
        refused = True
    if not refused:
        raise AssertionError('Corrupted backup passed the integrity gate')
    target.initialize()
    create_roles(target)
    target.query('CREATE DATABASE wms_kernel_test_resilience OWNER wms_test_owner;', database='postgres')
    restore_started = time.monotonic()
    target.command('pg_restore', ['--exit-on-error', '--no-owner', '--no-privileges', '--role=wms_test_owner', '-d', 'wms_kernel_test_resilience', str(archive)], timeout=90)
    grants(target, repo, 'wms_kernel_test_resilience')
    if expected_installation is not None:
        onprem_grants(target, repo)
        verify_installation_state(expected_installation, installation_state(target))
    verify_acknowledged(witness, operation_results(target, witness))
    canary_evidence = canary_checks(target, canaries)
    checks = business_checks(target, witness, canaries)
    source_history = source.query('SELECT md5(jsonb_agg(to_jsonb(h) ORDER BY name)::text) FROM wms_migrations.history h;')
    target_history = target.query('SELECT md5(jsonb_agg(to_jsonb(h) ORDER BY name)::text) FROM wms_migrations.history h;')
    if source_history != target_history:
        raise AssertionError('Restored migration receipts changed')
    for record in list(witness.values()) + list(canaries.values()):
        result = target.json(unit_sql(record['request']))
        if canonical(result) != canonical(record['result']):
            raise AssertionError('Same-ID replay after isolated restore changed an acknowledged result')
    canary_checks(target, canaries)
    checks_after_replay = business_checks(target, witness, canaries)
    if expected_installation is not None:
        verify_installation_state(expected_installation, installation_state(target))
    return {'status': 'passed', 'backup_sha256': digest, 'backup_bytes': archive.stat().st_size,
            'backup_restore_total_seconds': round(time.monotonic()-started, 6),
            'restore_verify_replay_seconds': round(time.monotonic()-restore_started, 6),
            'corrupt_archive_refused_before_restore': True, 'separate_pgdata': True,
            'migration_history_preserved': True, 'checks': checks, 'checks_after_replay': checks_after_replay,
            'nonzero_canaries_verified_before_replay': canary_evidence,
            'onprem_identity_verified_before_replay': expected_installation is not None,
            'installation_and_role_state': installation_state(target) if expected_installation is not None else None,
            'synthetic_signing_key_only': True,
            'quarantine': 'Synthetic database, loopback only, no application relay/consumer started.'}


def run(arguments):
    repo = Path(arguments.repo).resolve()
    if os.path.normcase(str(Path(__file__).resolve())) != os.path.normcase(str(repo / 'scripts/crash_restore.py')):
        raise ValueError('The executable harness must belong to the declared source repository')
    work = Path(arguments.work_root).resolve()
    if work == Path(work.anchor) or work == repo:
        raise ValueError('A dedicated writable evidence/work directory is required')
    directory = work / ('crash-restore-' + uuid.uuid4().hex)
    directory.mkdir(parents=True)
    report = {'status': 'failed', 'scope': 'Bounded PostgreSQL process-crash recovery and isolated logical backup restore using synthetic WMS data.',
              'cycles': [], 'work_directory': str(directory), 'postgresql_tools': str(Path(arguments.pg_bin).resolve()),
              'source_files': source_manifest(repo),
              'production_data_or_consumers_used': False,
              'limitations': ['Not hardware power-loss, disk-cache honesty, disk failure, PITR, replication or HA evidence.',
                              'Workload and latency measurements are a bounded local/container experiment, not a production throughput SLA.']}
    source, target = Cluster(arguments.pg_bin, directory, 'source'), Cluster(arguments.pg_bin, directory, 'restored')
    try:
        source.initialize()
        settings = source.json("SELECT jsonb_object_agg(name,setting) FROM pg_settings WHERE name IN('fsync','synchronous_commit','full_page_writes','data_checksums');", database='postgres')
        validate_durability_settings(settings)
        report['settings'] = settings
        report['postgres_version'] = source.query('SHOW server_version;', database='postgres')
        fixture = seed(source, repo, onprem=True, multiple=True)
        report['installation_and_role_state'] = installation_state(source)
        report['signing_key'] = 'Fresh synthetic random 32 bytes generated inside this disposable database; never returned to runner. The synthetic dump contains that synthetic key.'
        for cycle in range(1, arguments.cycles+1):
            print('Starting owned PostgreSQL crash cycle ' + str(cycle), flush=True)
            result = crash_cycle(source, fixture, directory, cycle, arguments.seconds, arguments.writers, report['installation_and_role_state'])
            report['cycles'].append(result)
            print('Recovered cycle ' + str(cycle) + ': ' + str(result['acknowledged_units_before_kill']) + ' witnessed units survived', flush=True)
        witness = read_witness(directory / 'acknowledged.jsonl')
        canaries = read_witness(directory / 'canaries.jsonl')
        report['witness_units'] = len(witness)
        report['canary_units'] = len(canaries)
        report['witness_commands'] = len(witness)*len(STAGES) + 7*len(canaries)
        report['witness_sha256'] = hashlib.sha256((directory / 'acknowledged.jsonl').read_bytes()).hexdigest()
        report['canary_witness_sha256'] = hashlib.sha256((directory / 'canaries.jsonl').read_bytes()).hexdigest()
        verify_installation_state(report['installation_and_role_state'], installation_state(source))
        report['restore'] = restore_proof(source, target, repo, directory, witness, canaries, report['installation_and_role_state'])
        verify_source_manifest(report['source_files'], source_manifest(repo))
        report['source_unchanged_during_run'] = True
        report['status'] = 'passed'
        return report
    except Exception as error:
        report['error'] = {'type': type(error).__name__, 'message': str(error)}
        raise
    finally:
        cleanup_errors = cleanup_clusters((source, target))
        report['clusters_stopped'] = not cleanup_errors
        if cleanup_errors:
            report['status'] = 'failed'
            report['cleanup_errors'] = cleanup_errors
        (directory / 'report.json').write_text(json.dumps(report, indent=2)+'\n', encoding='utf8')
        output = Path(arguments.report).resolve()
        output.parent.mkdir(parents=True, exist_ok=True)
        output.write_text(json.dumps(report, indent=2)+'\n', encoding='utf8')
        if cleanup_errors:
            raise RuntimeError('Owned cluster cleanup failed; failed evidence was preserved')


if __name__ == '__main__':
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--repo', default=str(ROOT))
    parser.add_argument('--work-root', default=str(ROOT / 'work'))
    parser.add_argument('--pg-bin', required=True)
    parser.add_argument('--report', default=str(ROOT / 'work/crash-restore-report.json'))
    parser.add_argument('--cycles', type=int, choices=range(2, 9), default=3)
    parser.add_argument('--seconds', type=float, default=5)
    parser.add_argument('--writers', type=int, choices=range(2, 9), default=2)
    args = parser.parse_args()
    if not 2 <= args.seconds <= 60:
        parser.error('--seconds must be between 2 and 60 per cycle')
    result = run(args)
    print(json.dumps({'status': result['status'], 'cycles': len(result['cycles']), 'witness_commands': result['witness_commands'], 'restore': result['restore']['status']}))
