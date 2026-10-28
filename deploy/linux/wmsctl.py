#!/usr/bin/env python3
"""Install/maintain one dedicated WMS VM. Secrets never enter subprocess argv.

The archive digest must arrive over the operator's trusted SSH channel. It is
an integrity reference, not a publisher signature. Do not download untrusted
archives and accept their self-declared manifest as authorization.
"""
import argparse
import base64
import datetime
import hashlib
import importlib.util
import io
import ipaddress
import json
import os
from pathlib import Path
import re
import secrets
import shutil
import ssl
import subprocess
import sys
import tarfile
import tempfile
import time
import urllib.request
import uuid

CONFIG = Path('/etc/wms')
BASE = Path('/opt/wms')
STATE = Path('/var/lib/wms')
# Digests obtained from the official Docker Hub/Quay manifest APIs 2026-10-28.
IMAGES = {'postgres': 'postgres:17.11-bookworm@sha256:839fbb5a4942a7758dd93e18b42ea5649807491a0545b5eeeb8f0b8bb11857a5',
          'keycloak': 'quay.io/keycloak/keycloak:26.8.1@sha256:a78d6b79572168cb25886b80a8b5b8d4ed4390743e3eecb810538683e209dd6b',
          'grafana': 'grafana/grafana:12.3.0@sha256:70d9599b186ce287be0d2c5ba9a78acb2e86c1a68c9c41449454d0fc3eeb84e8'}
DURABILITY = {'fsync': 'on', 'full_page_writes': 'on', 'data_checksums': 'on',
              'ignore_checksum_failure': 'off', 'zero_damaged_pages': 'off',
              'ignore_invalid_pages': 'off'}
SECRET_KEYS = ('db_admin_password', 'api_password', 'worker_password', 'keycloak_password',
               'oidc_client_secret', 'request_key', 'outbox_key', 'grafana_password', 'monitor_password')


def sha256(path):
    digest = hashlib.sha256()
    with Path(path).open('rb') as source:
        for chunk in iter(lambda: source.read(1024 * 1024), b''):
            digest.update(chunk)
    return digest.hexdigest()


def safe_member_name(name):
    if (not name or name.startswith('/') or '\\' in name or ':' in name or '\x00' in name
            or any(part in ('', '.', '..') for part in name.split('/'))):
        raise ValueError('Unsafe archive path')
    return name


def _verified_archive(path, digest, revision):
    if Path(path).stat().st_size > 256 * 1024 * 1024:
        raise ValueError('Archive exceeds size limit')
    # A single immutable snapshot binds digest, manifest and extraction, even
    # if an unprivileged uploader replaces the source path during installation.
    with Path(path).open('rb') as source_file:
        blob = source_file.read(256 * 1024 * 1024 + 1)
    if len(blob) > 256 * 1024 * 1024:
        raise ValueError('Archive exceeds size limit')
    if not re.fullmatch(r'[a-f0-9]{64}', digest) or hashlib.sha256(blob).hexdigest() != digest:
        raise ValueError('Release archive SHA256 mismatch')
    if not re.fullmatch(r'[a-f0-9]{40}', revision):
        raise ValueError('Expected full immutable Git revision')
    with tarfile.open(fileobj=io.BytesIO(blob), mode='r:*') as source:
        members, content, size = set(), {}, 0
        for member in source:
            # Directory entries are optional; a tar rooted at ./ is rejected.
            name = safe_member_name(member.name.rstrip('/') if member.isdir() else member.name)
            if name in members or not (member.isfile() or member.isdir()):
                raise ValueError('Duplicate, linked or special archive member')
            members.add(name)
            if member.isfile():
                size += member.size
                if size > 256 * 1024 * 1024:
                    raise ValueError('Release exceeds size limit')
                content[name] = source.extractfile(member).read()
        try:
            manifest = json.loads(content.pop('release-manifest.json'))
        except (KeyError, ValueError) as error:
            raise ValueError('Missing or invalid release manifest') from error
        if manifest.get('schema') != 1 or manifest.get('revision') != revision:
            raise ValueError('Release revision mismatch')
        expected = manifest.get('files')
        if not isinstance(expected, dict) or set(expected) != set(content) or 'bin/wms-api' not in content:
            raise ValueError('Manifest must cover every release file')
        for name, value in content.items():
            if hashlib.sha256(value).hexdigest() != expected[name]:
                raise ValueError('Release file checksum mismatch: ' + name)
        return manifest, blob


def verify_archive(path, digest, revision):
    return _verified_archive(path, digest, revision)[0]


def private_write(path, value, mode=0o600):
    path = Path(path)
    path.parent.mkdir(parents=True, exist_ok=True, mode=0o700)
    temporary = path.with_name(path.name + '.new-' + secrets.token_hex(4))
    descriptor = os.open(temporary, os.O_WRONLY | os.O_CREAT | os.O_EXCL, mode)
    try:
        with os.fdopen(descriptor, 'w', encoding='utf8', newline='\n') as target:
            target.write(value)
            target.flush()
            os.fsync(target.fileno())
        os.replace(temporary, path)
        os.chmod(path, mode)
    finally:
        temporary.unlink(missing_ok=True)


def ensure_secrets(path):
    path = Path(path)
    if path.is_symlink():
        raise ValueError('Secrets must be a regular private file')
    if path.exists():
        value = json.loads(path.read_text())
        if (not isinstance(value, dict) or any(not isinstance(value.get(key), str) or len(value[key]) < 32
                for key in SECRET_KEYS) or not value.get('instance_id')):
            raise ValueError('Existing secrets invalid; never regenerate an installation identity')
        uuid.UUID(value['instance_id'])
        if os.name != 'nt' and (path.stat().st_mode & 0o077):
            raise ValueError('Existing secrets have unsafe permissions')
        return value
    value = {key: secrets.token_urlsafe(36) for key in SECRET_KEYS}
    value.update(instance_id=str(uuid.uuid4()), request_key=base64.b64encode(secrets.token_bytes(32)).decode(),
                 outbox_key=base64.b64encode(secrets.token_bytes(32)).decode())
    private_write(path, json.dumps(value, indent=2) + '\n')
    return value


def public_identity(value):
    return {'instance_id': value['instance_id']}


def validate_update_preflight(settings, receipt, instance_id):
    if any(settings.get(key) != expected for key, expected in DURABILITY.items()) or settings.get('synchronous_commit') not in ('on', 'remote_apply'):
        raise ValueError('Database durability preflight failed')
    path = Path(receipt.get('path', ''))
    if (receipt.get('instance_id') != instance_id or receipt.get('restore_verified') is not True
            or not path.is_file() or not path.stat().st_size or sha256(path) != receipt.get('sha256')):
        raise ValueError('A current verified same-installation backup is required')


def run(argv, data=None, sensitive=False, timeout=600):
    result = subprocess.run([str(item) for item in argv], input=data, capture_output=True,
                            text=True, timeout=timeout)
    if result.returncode:
        # Never echo argv or SQL containing a credential. Sensitive operations
        # deliberately keep server diagnostics in memory only.
        detail = '' if sensitive else ': ' + result.stderr[-2000:]
        raise RuntimeError('Operation failed (' + Path(str(argv[0])).name + ')' + detail)
    return result.stdout.strip()


def compose(*args):
    return run(['docker', 'compose', '--project-name', 'wms', '--file', CONFIG / 'compose.yaml', *args], sensitive=True)


def sql(query, database='wms'):
    return run(['docker', 'exec', '-i', 'wms-postgres', 'psql', '-X', '-q', '-A', '-t',
                '-v', 'ON_ERROR_STOP=1', '-U', 'wms_admin', '-d', database], query, sensitive=True)


def literal(value):
    return "'" + str(value).replace("'", "''") + "'"


def wait_for(function, description, seconds=240):
    deadline = time.monotonic() + seconds
    while True:
        try:
            return function()
        except (RuntimeError, OSError, ValueError):
            if time.monotonic() >= deadline:
                raise RuntimeError('Timed out waiting for ' + description) from None
            time.sleep(3)


def http_json(url, data=None):
    context = ssl.create_default_context(cafile=str(CONFIG / 'tls/ca.crt'))
    with urllib.request.urlopen(url, data=data, context=context, timeout=8) as response:
        return json.load(response)


def verify_health_payload(value, kind):
    if not isinstance(value, dict):
        raise ValueError('Invalid health response')
    if kind == 'ready':
        if value.get('status') != 'ready':
            raise ValueError('Application is not ready')
    elif kind == 'background':
        try:
            now = datetime.datetime.now(datetime.timezone.utc)
            for key in ('relay_at', 'expiry_at', 'monitor_at'):
                age = (now - datetime.datetime.fromisoformat(value[key].replace('Z', '+00:00'))).total_seconds()
                if not -5 <= age <= 20:
                    raise ValueError('Stale background health')
            queue = value['queue']
            if (queue['dead'] != 0 or not 0 <= queue['oldest_pending_seconds'] <= 60
                    or not 0 <= queue['oldest_expired_seconds'] <= 60):
                raise ValueError('Unhealthy background queue')
        except (KeyError, TypeError, OverflowError) as error:
            raise ValueError('Invalid background health') from error
    else:
        raise ValueError('Unknown health contract')
    return value


def health_json(url, kind):
    return verify_health_payload(http_json(url), kind)


def database_settings():
    return json.loads(sql("SELECT json_object_agg(name,setting) FROM pg_settings WHERE name IN (" +
                           ','.join(literal(key) for key in (*DURABILITY, 'synchronous_commit')) + ');', 'postgres'))


def release_extract(archive, digest, revision):
    manifest, blob = _verified_archive(archive, digest, revision)
    releases = BASE / 'releases'
    releases.mkdir(parents=True, exist_ok=True)
    target = releases / (revision[:12] + '-' + digest[:12])
    if target.exists():
        for name, expected in manifest['files'].items():
            if sha256(target / name) != expected:
                raise ValueError('Installed release has drifted')
        return target
    staging = Path(tempfile.mkdtemp(prefix='.stage-', dir=releases))
    try:
        with tarfile.open(fileobj=io.BytesIO(blob), mode='r:*') as source:
            for member in source:
                name = safe_member_name(member.name.rstrip('/') if member.isdir() else member.name)
                destination = staging / name
                if member.isdir():
                    destination.mkdir(parents=True, exist_ok=True)
                else:
                    destination.parent.mkdir(parents=True, exist_ok=True)
                    with destination.open('wb') as output:
                        shutil.copyfileobj(source.extractfile(member), output)
                    os.chmod(destination, 0o755 if name.startswith('bin/') else 0o644)
        os.replace(staging, target)
    finally:
        if staging.exists():
            shutil.rmtree(staging)
    return target


def tls(address):
    directory = CONFIG / 'tls'
    directory.mkdir(parents=True, exist_ok=True, mode=0o750)
    if not (directory / 'ca.crt').exists():
        run(['openssl', 'req', '-x509', '-newkey', 'rsa:3072', '-nodes', '-days', '3650',
             '-subj', '/CN=WMS installation private CA', '-keyout', directory / 'ca.key', '-out', directory / 'ca.crt'], sensitive=True)
    if not (directory / 'server.crt').exists():
        run(['openssl', 'req', '-newkey', 'rsa:3072', '-nodes', '-subj', '/CN=' + address,
             '-keyout', directory / 'server.key', '-out', directory / 'server.csr'], sensitive=True)
        private_write(directory / 'extensions', 'subjectAltName=IP:' + address + '\nextendedKeyUsage=serverAuth\n')
        run(['openssl', 'x509', '-req', '-in', directory / 'server.csr', '-CA', directory / 'ca.crt',
             '-CAkey', directory / 'ca.key', '-CAcreateserial', '-days', '365', '-extfile', directory / 'extensions',
             '-out', directory / 'server.crt'], sensitive=True)
    for key in ('ca.key', 'server.key'):
        os.chmod(directory / key, 0o640 if key == 'server.key' else 0o600)
    # Caddy reads only its leaf key; the CA private key remains root-only.
    run(['chown', 'root:caddy', directory, directory / 'server.key', directory / 'server.crt'])
    shutil.copyfile(directory / 'ca.crt', '/usr/local/share/ca-certificates/wms-installation.crt')
    run(['update-ca-certificates'])


def configuration(value, address):
    CONFIG.mkdir(parents=True, exist_ok=True, mode=0o750)
    os.chmod(CONFIG, 0o750)
    run(['chown', 'root:caddy', CONFIG])
    STATE.mkdir(parents=True, exist_ok=True, mode=0o700)
    realm = 'wms-' + value['instance_id'][:8]
    client = 'wms-api-' + value['instance_id'][:8]
    issuer = 'https://' + address + '/auth/realms/' + realm
    private_write(CONFIG / 'postgres-password', value['db_admin_password'] + '\n', 0o444)
    private_write(CONFIG / 'keycloak.env', '\n'.join([
        'KC_DB=postgres', 'KC_DB_URL=jdbc:postgresql://postgres:5432/keycloak', 'KC_DB_USERNAME=keycloak',
        'KC_DB_PASSWORD=' + value['keycloak_password'], 'KC_HTTP_ENABLED=true', 'KC_HTTP_RELATIVE_PATH=/auth',
        'KC_HOSTNAME=https://' + address + '/auth', 'KC_PROXY_HEADERS=xforwarded',
        'KC_HEALTH_ENABLED=true', 'JAVA_OPTS_KC_HEAP=-Xms128m -Xmx512m',
    ]) + '\n')
    realm_config = {'realm': realm, 'enabled': True, 'sslRequired': 'external',
        'registrationAllowed': False, 'clients': [{'clientId': client, 'secret': value['oidc_client_secret'],
            'enabled': True, 'protocol': 'openid-connect', 'publicClient': False,
            'serviceAccountsEnabled': True, 'standardFlowEnabled': False, 'directAccessGrantsEnabled': False,
            'protocolMappers': [{'name': 'wms-audience', 'protocol': 'openid-connect',
                'protocolMapper': 'oidc-audience-mapper', 'consentRequired': False,
                'config': {'included.client.audience': client, 'id.token.claim': 'false', 'access.token.claim': 'true'}}]}]}
    private_write(CONFIG / 'keycloak/realm.json', json.dumps(realm_config), 0o644)
    private_write(CONFIG / 'grafana.env', '\n'.join([
        'GF_SECURITY_ADMIN_USER=wms-admin', 'GF_SECURITY_ADMIN_PASSWORD=' + value['grafana_password'],
        'GF_USERS_ALLOW_SIGN_UP=false', 'GF_AUTH_ANONYMOUS_ENABLED=true', 'GF_AUTH_ANONYMOUS_ORG_ROLE=Viewer',
        'GF_SERVER_ROOT_URL=https://' + address + '/monitor/', 'GF_SERVER_SERVE_FROM_SUB_PATH=true',
        'GF_ANALYTICS_REPORTING_ENABLED=false', 'GF_ANALYTICS_CHECK_FOR_UPDATES=false',
        'GF_SECURITY_COOKIE_SECURE=true', 'GF_SECURITY_DISABLE_GRAVATAR=true',
    ]) + '\n')
    private_write(CONFIG / 'compose.yaml', f'''services:
  postgres:
    image: {IMAGES['postgres']}
    container_name: wms-postgres
    restart: unless-stopped
    mem_limit: 768m
    environment:
      POSTGRES_USER: wms_admin
      POSTGRES_PASSWORD_FILE: /run/secrets/password
      POSTGRES_INITDB_ARGS: --data-checksums --auth-host=scram-sha-256
    command: [postgres, -c, fsync=on, -c, full_page_writes=on, -c, synchronous_commit=on, -c, shared_buffers=128MB]
    ports: ['127.0.0.1:5432:5432']
    volumes: [postgres-data:/var/lib/postgresql/data, /etc/wms/postgres-password:/run/secrets/password:ro]
    healthcheck:
      test: [CMD-SHELL, 'pg_isready -U wms_admin -d postgres']
      interval: 5s
      timeout: 5s
      retries: 30
    networks: [database]
  keycloak:
    image: {IMAGES['keycloak']}
    container_name: wms-keycloak
    restart: unless-stopped
    mem_limit: 900m
    env_file: /etc/wms/keycloak.env
    command: [start, --import-realm]
    ports: ['127.0.0.1:8081:8080']
    volumes: [/etc/wms/keycloak:/opt/keycloak/data/import:ro]
    depends_on:
      postgres:
        condition: service_healthy
    networks: [database]
  grafana:
    image: {IMAGES['grafana']}
    container_name: wms-grafana
    restart: unless-stopped
    mem_limit: 256m
    env_file: /etc/wms/grafana.env
    ports: ['127.0.0.1:3000:3000']
    extra_hosts: ['host.docker.internal:host-gateway']
    volumes: [grafana-data:/var/lib/grafana, /etc/wms/grafana:/etc/grafana/provisioning:ro]
    networks: [monitor]
volumes:
  postgres-data:
  grafana-data:
networks:
  database:
  monitor:
''')
    # Prometheus binds only docker0:9090 so Grafana can query it. No host LAN
    # exposure or public monitoring route exists; Caddy exposes Viewer Grafana.
    gateway = run(['docker', 'network', 'inspect', 'bridge', '--format', '{{(index .IPAM.Config 0).Gateway}}'])
    private_write(CONFIG / 'grafana/datasources/prometheus.yaml',
        'apiVersion: 1\ndatasources:\n  - name: WMS Prometheus\n    uid: wms-prometheus\n    type: prometheus\n    access: proxy\n    url: http://' + gateway + ':9090\n    isDefault: true\n    editable: false\n', 0o644)
    private_write('/etc/default/prometheus', 'ARGS="--config.file=/etc/prometheus/prometheus.yml --storage.tsdb.path=/var/lib/prometheus/metrics2 --storage.tsdb.retention.time=7d --storage.tsdb.retention.size=512MB --web.listen-address=' + gateway + ':9090"\n', 0o644)
    private_write('/etc/prometheus/prometheus.yml', '''global:
  scrape_interval: 15s
  evaluation_interval: 15s
rule_files: [/etc/prometheus/wms-alerts.yml]
scrape_configs:
  - job_name: wms-background
    static_configs:
      - targets: ['127.0.0.1:9091']
''', 0o644)
    private_write('/etc/prometheus/wms-alerts.yml', '''groups:
  - name: wms
    rules:
      - alert: WMSBackgroundUnavailable
        expr: up{job="wms-background"} == 0 or wms_background_ready == 0
        for: 2m
        labels: {severity: warning}
        annotations: {summary: 'WMS background unhealthy: inspect wms-api and database before restarting'}
      - alert: WMSOutboxDeadLetter
        expr: wms_outbox_dead > 0
        for: 1m
        labels: {severity: warning}
        annotations: {summary: 'Outbox dead letters require operator investigation'}
''', 0o644)
    dashboard(value)
    for directory in (CONFIG / 'keycloak', CONFIG / 'grafana', CONFIG / 'grafana/datasources', CONFIG / 'grafana/dashboards'):
        os.chmod(directory, 0o755)
    monitor_hash = run(['caddy', 'hash-password'], value['monitor_password'] + '\n', sensitive=True)
    private_write('/etc/caddy/Caddyfile', f'''{{
    auto_https off
    admin 127.0.0.1:2019
}}
https://{address} {{
    tls /etc/wms/tls/server.crt /etc/wms/tls/server.key
    header X-Content-Type-Options nosniff
    handle /auth/* {{
        reverse_proxy 127.0.0.1:8081
    }}
    handle /monitor/* {{
        basicauth {{
            wms-viewer {monitor_hash}
        }}
        reverse_proxy 127.0.0.1:3000
    }}
    handle /lab/events {{
        reverse_proxy 127.0.0.1:8091
    }}
    handle {{
        reverse_proxy 127.0.0.1:8080
    }}
}}
''', 0o644)
    private_write(CONFIG / 'app.env', '\n'.join([
        'WMS_DATABASE_PROFILE=onprem-v1', 'WMS_DATABASE_URL=postgres://wms_api:' + value['api_password'] + '@127.0.0.1:5432/wms?sslmode=disable',
        'WMS_WORKER_DATABASE_URL=postgres://wms_worker:' + value['worker_password'] + '@127.0.0.1:5432/wms?sslmode=disable',
        'WMS_REQUEST_CONTEXT_KEY=' + value['request_key'], 'WMS_OIDC_ISSUER=' + issuer,
        'WMS_OIDC_AUDIENCE=' + client, 'WMS_HTTP_ADDR=127.0.0.1:8080', 'WMS_MONITOR_ADDR=127.0.0.1:9091',
        'WMS_OUTBOX_URL=https://' + address + '/lab/events', 'WMS_OUTBOX_ALLOW_PRIVATE=true',
        'WMS_OUTBOX_HMAC_KEY=' + value['outbox_key'], 'WMS_OUTBOX_CONCURRENCY=4', 'WMS_DB_MAX_CONNS=8',
    ]) + '\n')
    private_write(CONFIG / 'sink.env', 'WMS_OUTBOX_HMAC_KEY=' + value['outbox_key'] + '\n')
    private_write(CONFIG / 'identity.json', json.dumps(dict(public_identity(value), address=address,
        issuer=issuer, audience=client, images=IMAGES), indent=2) + '\n', 0o644)
    return issuer


def dashboard(value):
    panels = []
    for index, (title, expression, kind) in enumerate([
        ('Background readiness', 'wms_background_ready', 'stat'),
        ('Delivered events', 'wms_outbox_delivered_total', 'stat'),
        ('Pending events', 'wms_outbox_pending', 'stat'),
        ('Dead letters', 'wms_outbox_dead', 'stat'),
        ('Event deliveries / second', 'rate(wms_outbox_delivered_total[1m])', 'timeseries'),
        ('Pending age (seconds)', 'wms_outbox_oldest_pending_seconds', 'timeseries')]):
        panels.append({'id': index + 1, 'title': title, 'type': kind,
            'datasource': {'type': 'prometheus', 'uid': 'wms-prometheus'},
            'gridPos': {'x': index % 4 * 6 if index < 4 else (index - 4) * 12,
                        'y': 0 if index < 4 else 5, 'w': 6 if index < 4 else 12, 'h': 5 if index < 4 else 9},
            'targets': [{'expr': expression, 'refId': 'A'}]})
    value = {'uid': 'wms-operations', 'title': 'WMS — dedicated installation ' + value['instance_id'][:8],
        'schemaVersion': 39, 'version': 1, 'refresh': '5s', 'time': {'from': 'now-15m', 'to': 'now'},
        'editable': False, 'panels': panels, 'tags': ['on-premise', 'lab ERP sink']}
    private_write(CONFIG / 'grafana/dashboards/provider.yaml', 'apiVersion: 1\nproviders:\n  - name: WMS\n    type: file\n    disableDeletion: true\n    editable: false\n    options:\n      path: /etc/grafana/provisioning/dashboards\n', 0o644)
    private_write(CONFIG / 'grafana/dashboards/wms.json', json.dumps(value), 0o644)


def database_bootstrap(release, value):
    # Local socket inside the container, SQL on stdin. No password argv/env.
    for role, password in [('wms_api', value['api_password']), ('wms_worker', value['worker_password']),
                           ('keycloak', value['keycloak_password'])]:
        sql("DO $$ BEGIN IF NOT EXISTS(SELECT FROM pg_roles WHERE rolname=" + literal(role) + ") THEN CREATE ROLE " +
            role + ' LOGIN NOSUPERUSER NOCREATEDB NOCREATEROLE NOINHERIT PASSWORD ' + literal(password) + '; END IF; END $$;', 'postgres')
    sql("DO $$ BEGIN IF NOT EXISTS(SELECT FROM pg_roles WHERE rolname='wms_owner') THEN CREATE ROLE wms_owner NOLOGIN; END IF; END $$;", 'postgres')
    for name, owner in [('wms', 'wms_owner'), ('keycloak', 'keycloak')]:
        sql("SELECT 'CREATE DATABASE " + name + ' OWNER ' + owner + "' WHERE NOT EXISTS(SELECT FROM pg_database WHERE datname='" + name + "') \\gexec", 'postgres')
    spec = importlib.util.spec_from_file_location('migrate_core', release / 'scripts/migrate_core.py')
    module = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(module)
    sql('SET ROLE wms_owner;\n' + module.build_script(release))
    if sql("SELECT to_regnamespace('hardened_private') IS NULL;") == 't':
        sql('SET ROLE wms_owner;\n' + (release / 'db/profiles/hardened-v1.sql').read_text())
    if sql("SELECT to_regclass('hardened_private.installation') IS NULL;") == 't':
        sql('SET ROLE wms_owner;\n' + (release / 'db/profiles/onprem-v1.sql').read_text())
        sql('UPDATE hardened_private.installation SET id=' + literal(value['instance_id']) + '::uuid WHERE singleton;')
    if sql('SELECT id::text FROM hardened_private.installation WHERE singleton;') != value['instance_id']:
        raise ValueError('Database installation identity drift')
    sql('SET ROLE wms_owner;\n\\set api_role wms_api\n\\set worker_role wms_worker\n' +
        (release / 'db/bootstrap/onprem-grants.psql').read_text())
    sql("INSERT INTO hardened_private.signing_keys(login,key) VALUES('wms_api',decode(" + literal(value['request_key']) + ",'base64')) ON CONFLICT(login) DO NOTHING;")
    if sql("SELECT encode(key,'base64') FROM hardened_private.signing_keys WHERE login='wms_api';") != value['request_key']:
        raise ValueError('Database request signing key drift')


def units(release):
    if run(['getent', 'passwd', 'wms'], sensitive=True) == '':
        raise RuntimeError('WMS account missing')
    sink_source = Path(__file__).with_name('lab_sink.py')
    if not sink_source.is_file():
        raise ValueError('Installer companion missing')
    if sink_source.resolve() != BASE / 'lab_sink.py':
        shutil.copyfile(sink_source, BASE / 'lab_sink.py')
    if Path(__file__).resolve() != BASE / 'wmsctl.py':
        shutil.copyfile(__file__, BASE / 'wmsctl.py')
    os.chmod(BASE / 'wmsctl.py', 0o755)
    command_link = Path('/usr/local/sbin/wmsctl')
    if not command_link.exists():
        command_link.symlink_to(BASE / 'wmsctl.py')
    run(['install', '-d', '-o', 'wms', '-g', 'wms', '-m', '0750', '/var/lib/wms-lab-sink'])
    for name, command, env in [('wms-api', '/opt/wms/current/bin/wms-api', '/etc/wms/app.env'),
                                ('wms-lab-sink', '/usr/bin/python3 /opt/wms/lab_sink.py', '/etc/wms/sink.env')]:
        private_write('/etc/systemd/system/' + name + '.service', f'''[Unit]
Description={name} — dedicated WMS installation
After=network-online.target docker.service caddy.service
Wants=network-online.target
[Service]
User=wms
Group=wms
EnvironmentFile={env}
ExecStart={command}
Restart=on-failure
RestartSec=5
TimeoutStopSec=30
NoNewPrivileges=true
ProtectSystem=strict
ProtectHome=true
PrivateTmp=true
ReadWritePaths=/var/lib/wms-lab-sink
RestrictAddressFamilies=AF_INET AF_INET6 AF_UNIX
UMask=0077
[Install]
WantedBy=multi-user.target
''', 0o644)
    run(['systemctl', 'daemon-reload'])


def backup(value):
    directory = STATE / 'backups' / datetime.datetime.now(datetime.timezone.utc).strftime('%Y%m%dT%H%M%S-%fZ')
    directory.mkdir(parents=True, mode=0o700)
    dump = directory / 'wms.dump'
    with dump.open('wb') as output:
        result = subprocess.run(['docker', 'exec', 'wms-postgres', 'pg_dump', '-U', 'wms_admin', '-d', 'wms', '-Fc'], stdout=output, stderr=subprocess.PIPE)
    if result.returncode or not dump.stat().st_size:
        raise RuntimeError('Backup failed')
    os.chmod(dump, 0o600)
    private_write(directory / 'globals.sql', sql('SELECT 1;') + '\n')
    globals_output = run(['docker', 'exec', 'wms-postgres', 'pg_dumpall', '-U', 'wms_admin', '--globals-only'], sensitive=True)
    private_write(directory / 'globals.sql', globals_output + '\n')
    # Identity/secret/certificate archive is as sensitive as the live host. It
    # stays root-only locally; encrypted off-host export remains operator work.
    with tarfile.open(directory / 'identity.tar.gz', 'w:gz') as output:
        output.add(CONFIG, arcname='wms')
    os.chmod(directory / 'identity.tar.gz', 0o600)
    rehearsal = 'wms_restore_' + secrets.token_hex(6)
    sql('CREATE DATABASE ' + rehearsal + ' OWNER wms_owner;', 'postgres')
    try:
        with dump.open('rb') as source:
            result = subprocess.run(['docker', 'exec', '-i', 'wms-postgres', 'pg_restore', '-U', 'wms_admin',
                '--exit-on-error', '-d', rehearsal], stdin=source, stdout=subprocess.PIPE, stderr=subprocess.PIPE)
        if result.returncode:
            raise RuntimeError('Isolated backup restore failed')
        if sql('SELECT id::text FROM hardened_private.installation WHERE singleton;', rehearsal) != value['instance_id']:
            raise ValueError('Restored identity mismatch')
        for table in ('inventory.stock_balances', 'platform.operations', 'platform.outbox', 'platform.actor_grants'):
            if sql('SELECT count(*) FROM ' + table + ';') != sql('SELECT count(*) FROM ' + table + ';', rehearsal):
                raise ValueError('Restored data count mismatch: ' + table)
    finally:
        sql('DROP DATABASE ' + rehearsal + ' WITH (FORCE);', 'postgres')
    receipt = dict(instance_id=value['instance_id'], path=str(dump), sha256=sha256(dump),
                   restore_verified=True, created_at=datetime.datetime.now(datetime.timezone.utc).isoformat(),
                   scope='Local root-only logical backup, isolated no-consumer restore; not off-host backup/PITR')
    private_write(directory / 'receipt.json', json.dumps(receipt, indent=2) + '\n')
    return receipt


def backup_while_quiesced(value):
    try:
        running = run(['systemctl', 'is-active', 'wms-api'], sensitive=True) == 'active'
    except RuntimeError:
        running = False
    run(['systemctl', 'stop', 'wms-api'])
    try:
        return backup(value)
    finally:
        if running:
            run(['systemctl', 'start', 'wms-api'])


def status(value):
    identity = json.loads((CONFIG / 'identity.json').read_text())
    ready = health_json('https://' + identity['address'] + '/readyz', 'ready')
    background = health_json('http://127.0.0.1:9091/health/background', 'background')
    result = dict(identity, ready=ready, background=background, durability=database_settings(),
        release=json.loads((STATE / 'release.json').read_text()),
        tls_ca_sha256=sha256(CONFIG / 'tls/ca.crt'), sink='LAB ONLY: HMAC verified SQLite event sink')
    print(json.dumps(result, indent=2))


def install(args, value):
    recovery = {'resume': False, 'migration_started': False}
    try:
        return _install(args, value, recovery)
    except Exception:
        if recovery['resume'] and not recovery['migration_started']:
            run(['systemctl', 'start', 'wms-api'])
        raise


def _install(args, value, recovery):
    ipaddress.ip_address(args.address)
    old_identity = CONFIG / 'identity.json'
    if old_identity.exists() and json.loads(old_identity.read_text())['address'] != args.address:
        raise ValueError('Address/issuer change requires an explicit identity migration')
    release = release_extract(args.archive, args.sha256, args.revision)
    updating = (BASE / 'current').exists()
    if args.action == 'update' and not updating:
        raise ValueError('No installation to update')
    if updating:
        # Stop all application writers before backup+restore witness. A failed
        # migration keeps writers stopped; no implicit binary/SQL downgrade.
        try:
            recovery['resume'] = run(['systemctl', 'is-active', 'wms-api'], sensitive=True) == 'active'
        except RuntimeError:
            recovery['resume'] = False
        run(['systemctl', 'stop', 'wms-api'])
        receipt = backup(value)
        validate_update_preflight(database_settings(), receipt, value['instance_id'])
    issuer = configuration(value, args.address)
    compose('config', '--quiet')
    compose('pull')
    compose('up', '-d', 'postgres')
    wait_for(lambda: sql('SELECT 1;', 'postgres'), 'PostgreSQL')
    settings = database_settings()
    if any(settings.get(key) != expected for key, expected in DURABILITY.items()) or settings.get('synchronous_commit') not in ('on', 'remote_apply'):
        raise ValueError('PostgreSQL durability settings unsafe')
    recovery['migration_started'] = True
    database_bootstrap(release, value)
    tls(args.address)
    units(release)
    compose('up', '-d')
    run(['caddy', 'validate', '--config', '/etc/caddy/Caddyfile'])
    run(['promtool', 'check', 'config', '/etc/prometheus/prometheus.yml'])
    run(['systemctl', 'unmask', 'caddy', 'prometheus'])
    run(['systemctl', 'enable', '--now', 'docker', 'caddy', 'prometheus', 'wms-lab-sink'])
    run(['systemctl', 'restart', 'caddy', 'prometheus'])
    wait_for(lambda: http_json(issuer + '/.well-known/openid-configuration'), 'verified HTTPS OIDC discovery')
    temporary = BASE / ('current.new-' + secrets.token_hex(4))
    temporary.symlink_to(release, target_is_directory=True)
    os.replace(temporary, BASE / 'current')
    private_write(STATE / 'release.json', json.dumps({'revision': args.revision, 'archive_sha256': args.sha256,
        'path': str(release), 'installed_at': datetime.datetime.now(datetime.timezone.utc).isoformat()}, indent=2) + '\n')
    run(['systemctl', 'enable', 'wms-api'])
    run(['systemctl', 'restart', 'wms-api'])
    wait_for(lambda: health_json('https://' + args.address + '/readyz', 'ready'), 'WMS readiness')
    wait_for(lambda: health_json('http://127.0.0.1:9091/health/background', 'background'), 'background health')
    images = json.loads(run(['docker', 'image', 'inspect', *IMAGES.values()]))
    private_write(STATE / 'image-receipts.json', json.dumps([{key: item.get(key) for key in ('Id', 'RepoTags', 'RepoDigests')}
        for item in images], indent=2) + '\n')
    print(json.dumps({'status': 'installed', **public_identity(value), 'revision': args.revision,
                      'url': 'https://' + args.address, 'monitor': 'https://' + args.address + '/monitor/d/wms-operations'}))


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('action', choices=['install', 'update', 'status', 'backup'])
    parser.add_argument('--archive', type=Path)
    parser.add_argument('--sha256')
    parser.add_argument('--revision')
    parser.add_argument('--address')
    args = parser.parse_args()
    if os.geteuid() != 0:
        parser.error('Run through sudo on the intended dedicated guest')
    if args.action in ('install', 'update') and not all((args.archive, args.sha256, args.revision, args.address)):
        parser.error('install/update require archive, sha256, revision and address')
    # One host lock across install/update/backup prevents identity and migration races.
    import fcntl
    STATE.mkdir(parents=True, exist_ok=True, mode=0o700)
    with (STATE / 'operation.lock').open('w') as lock:
        fcntl.flock(lock, fcntl.LOCK_EX | fcntl.LOCK_NB)
        value = ensure_secrets(CONFIG / 'secrets.json')
        if args.action in ('install', 'update'):
            install(args, value)
        elif args.action == 'status':
            status(value)
        else:
            print(json.dumps(backup_while_quiesced(value), indent=2))


if __name__ == '__main__':
    try:
        main()
    except Exception as error:
        print('wmsctl: ' + str(error), file=sys.stderr)
        sys.exit(1)
