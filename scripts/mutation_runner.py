"""Scoped WMS kernel mutations; every mutant must fail a real behavior test.

Go changes use overlays. SQL changes run in a fresh clone of the disposable test
database. Published migrations and the working tree are never rewritten.
"""
import argparse
import hashlib
import json
import os
from pathlib import Path
import re
import shutil
import subprocess
import sys
import time
from urllib.parse import urlsplit, unquote, parse_qsl
import uuid

ROOT = Path(__file__).resolve().parent.parent
DOCUMENT_TESTS = r'^Test(WarehouseReceiptPutawayPickAndShipViaAPI|DocumentCommandsRejectScopeAndInvalidLines|DocumentRetryIsAtomicAndSQLChecksPermissions|DocumentCommandProperties|DocumentInvalidGeneratedLineRollsBackWholeCreate|DocumentAdversarialCommandIdentityAndAuthorization|DocumentAdversarialConcurrentCommands)$'


def replace_once(source, before, after):
    if before == after or source.count(before) != 1:
        raise ValueError('Mutation must change exactly one known source site')
    return source.replace(before, after, 1)


def classify_go_run(code, events):
    completed = [e for e in events if e.get('Test') and e.get('Action') in ('pass', 'fail', 'skip')]
    failures = sorted({e['Test'] for e in completed if e['Action'] == 'fail'})
    skipped = any(e['Action'] == 'skip' for e in completed)
    broken_run = any('panic: test timed out' in e.get('Output', '') or '[build failed]' in e.get('Output', '') or e.get('Action') == 'build-fail' for e in events)
    if broken_run or skipped or not completed or (code and not failures):
        status = 'invalid'
    elif code and failures:
        status = 'killed'
    elif code == 0 and not failures:
        status = 'survived'
    else:
        status = 'invalid'
    return {'status': status, 'failed_tests': failures, 'completed_test_events': len(completed)}


def classify_python_run(code, result):
    failures = result['failures'] + result['errors']
    invalid = not result['tests'] or result['skipped'] or any('_FailedTest' in name for name in result['errors'])
    if invalid or (code and not failures):
        status = 'invalid'
    elif code and failures:
        status = 'killed'
    elif not failures:
        status = 'survived'
    else:
        status = 'invalid'
    return {'status': status, 'failed_tests': failures, 'completed_test_events': result['tests']}


def database_environment(dsn):
    parsed = urlsplit(dsn)
    name = parsed.path.removeprefix('/')
    if (parsed.scheme not in ('postgres', 'postgresql') or parsed.hostname != '127.0.0.1'
            or not re.fullmatch(r'wms_kernel_test_[a-z0-9_]{1,47}', name) or parsed.fragment
            or any(key != 'sslmode' or value != 'disable' for key, value in parse_qsl(parsed.query, keep_blank_values=True))):
        raise ValueError('Mutation tests require a disposable loopback wms_kernel_test_ database')
    env = {key: value for key, value in os.environ.items() if not key.upper().startswith('PG')}
    env.update(PGHOST='127.0.0.1', PGPORT=str(parsed.port or 5432), PGUSER=unquote(parsed.username or ''), PGDATABASE='postgres')
    if parsed.password is not None:
        env['PGPASSWORD'] = unquote(parsed.password)
    return name, env


def run_go(go, run_pattern, directory, environment, overlay=None, package='./internal/postgres'):
    environment = {key: value for key, value in environment.items() if not key.upper().startswith('PG')}
    environment['WMS_DOCUMENT_PLAN_DIR'] = str(directory / 'plans')
    environment['WMS_DOCUMENT_CRITIC_PLAN_DIR'] = str(directory / 'critic-plans')
    command = [go, 'test']
    if overlay:
        command += ['-overlay', str(overlay)]
    command += ['-tags=integration', package, '-run', run_pattern, '-count=1', '-timeout=180s', '-json']
    started = time.monotonic()
    with (directory / 'events.jsonl').open('w', encoding='utf8') as out, (directory / 'stderr.txt').open('w', encoding='utf8') as err:
        try:
            result = subprocess.run(command, cwd=ROOT, env=environment, stdout=out, stderr=err, timeout=210)
        except subprocess.TimeoutExpired:
            return {'status': 'invalid', 'reason': 'timeout', 'failed_tests': []}
    events = []
    for line in (directory / 'events.jsonl').read_text(encoding='utf8').splitlines():
        try:
            events.append(json.loads(line))
        except json.JSONDecodeError:
            return {'status': 'invalid', 'reason': 'invalid Go JSON output', 'failed_tests': []}
    report = classify_go_run(result.returncode, events)
    report.update(seconds=round(time.monotonic() - started, 3), command=command)
    return report


def sql_mutants():
    return [
        ('sql_replay_runs_twice', 'RETURN previous.result;', 'NULL;'),
        ('sql_request_reuse_accepted', 'previous.request<>request', 'false'),
        ('sql_actor_reuse_accepted', 'previous.actor_id<>p_actor', 'false'),
        ('sql_stale_version_accepted', "(p_input->>'expected_document_version')::bigint IS DISTINCT FROM d.version", 'false'),
        ('sql_invalid_transition_accepted', "IF target IS NULL OR NOT((d.state='draft' AND target IN('active','cancelled')) OR (d.state='active' AND target IN('closed','cancelled'))) THEN", 'IF false THEN'),
        ('sql_version_increment_twice', 'version=version+1', 'version=version+2'),
        ('sql_quantity_precision_unchecked', 'PERFORM inventory.assert_quantity(item,qty);', 'NULL;'),
        ('sql_incomplete_document_closes', "IF target='closed' AND EXISTS", "IF false AND EXISTS"),
        ('sql_active_document_cancels', "IF target='cancelled' AND EXISTS", "IF false AND EXISTS"),
        ('sql_owner_authorization_removed', '(g.owner_id IS NULL OR g.owner_id=v_owner)', 'true'),
        ('sql_audit_outbox_removed', "RETURN platform.finish_operation(p_command,result,d.id,CASE WHEN p_document IS NULL THEN 'wms.document.created' ELSE 'wms.document.state_changed' END);", 'RETURN result;'),
    ]


def load_core_mutations(root):
    cases = []
    for path in sorted((root / 'scripts/mutations').glob('core-*.json')):
        entries = json.loads(path.read_text(encoding='utf8'))
        if not isinstance(entries, list):
            raise ValueError('Invalid core mutation catalog: ' + path.name)
        cases.extend(entries)
    domains = {'inventory': 0, 'authorization': 0, 'background': 0, 'boundary': 0}
    names = set()
    identities = set()
    for case in cases:
        domain = case.get('domain')
        name = case.get('name', '')
        if domain not in domains or not re.fullmatch('core_' + str(domain) + r'_[a-z0-9_]+', name) or name in names:
            raise ValueError('Invalid or duplicate core mutation identity')
        names.add(name)
        domains[domain] += 1
        if not case.get('before') or case.get('before') == case.get('after') or not isinstance(case.get('after'), str):
            raise ValueError('Core mutation must change a known source site')
        if not case.get('pattern', '').startswith('^Test'):
            raise ValueError('Core mutation requires an explicit behavior test')
        re.compile(case['pattern'])
        package = case.get('package', '')
        if not re.fullmatch(r'\./internal/[a-z_]+(?:/[a-z_]+)?|\./cmd/wms-api', package):
            raise ValueError('Invalid core mutation Go package')
        if case.get('kind') == 'go':
            source = case.get('source', '')
            if not (source.startswith('internal/') or source.startswith('cmd/wms-api/')) or '..' in Path(source).parts or not source.endswith('.go'):
                raise ValueError('Invalid core mutation source path')
            if package != './' + Path(source).parent.as_posix():
                raise ValueError('Mutation source and test package differ')
        elif case.get('kind') == 'sql_function':
            if not re.fullmatch(r'(inventory|warehouse|platform|catalog)\.[a-z_][a-z0-9_]*\([a-z0-9_, ]*\)', case.get('signature', '')) or package != './internal/postgres':
                raise ValueError('Invalid core mutation SQL function signature')
        else:
            raise ValueError('Invalid core mutation kind')
        identity = (case['kind'], case.get('signature') if case['kind'] == 'sql_function' else case['source'], case['before'], case['after'])
        if identity in identities:
            raise ValueError('Duplicate core mutation fault')
        identities.add(identity)
    if any(count < 8 for count in domains.values()):
        raise ValueError('Incomplete core mutation coverage: ' + str(domains))
    return cases


def python_mutants():
    return [
        ('migration_newline_normalization_removed', "raw = path.read_bytes().replace(b'\\r\\n', b'\\n')", 'raw = path.read_bytes()', False),
        ('migration_published_pin_guard_removed', 'if path.name in PINNED and hashlib.sha256(pinned_raw).hexdigest() != PINNED[path.name]:', 'if False:', False),
        ('migration_missing_sources_allowed', 'if missing:', 'if False:', False),
        ('migration_historical_crosspair_accepted', "execution_checksum IN ('{historical_execution}','legacy_baseline:{historical_digest}')", "execution_checksum IN ('{historical_execution}','{execution}','legacy_baseline:{historical_digest}')", True),
    ]


def run_python_mutant(directory, before, after, psql, psql_env, windows=False):
    shutil.copytree(ROOT / 'scripts', directory / 'scripts', ignore=shutil.ignore_patterns('__pycache__'))
    shutil.copytree(ROOT / 'db/migrations', directory / 'db/migrations')
    (directory / 'work').mkdir()
    path = directory / 'scripts/migrate_core.py'
    path.write_text(replace_once(path.read_text(encoding='utf8'), before, after), encoding='utf8')
    if windows:
        command = [sys.executable, 'scripts/verify_windows_migrations.py', '--psql', psql, '--port', psql_env['PGPORT'], '--admin', psql_env['PGUSER']]
    else:
        program = """import json,sys,unittest
suite=unittest.defaultTestLoader.discover('scripts',pattern='test_*migrat*.py')
result=unittest.TextTestRunner(stream=sys.stderr).run(suite)
print(json.dumps({'tests':result.testsRun,'failures':[str(t) for t,_ in result.failures],'errors':[str(t) for t,_ in result.errors],'skipped':[str(t) for t,_ in result.skipped]}))
sys.exit(0 if result.wasSuccessful() else 1)
"""
        command = [sys.executable, '-c', program]
    try:
        process = subprocess.run(command, cwd=directory, env=psql_env, capture_output=True, text=True, timeout=60)
    except subprocess.TimeoutExpired:
        return {'status': 'invalid', 'reason': 'timeout', 'failed_tests': []}
    (directory / 'stderr.txt').write_text(process.stderr, encoding='utf8')
    (directory / 'python-events.json').write_text(process.stdout, encoding='utf8')
    if windows:
        status = 'survived' if process.returncode == 0 else 'killed' if 'AssertionError: Unexpectedly accepted drifted migration receipt' in process.stderr else 'invalid'
        outcome = {'status': status, 'failed_tests': ['historical_crosspair_rejection'] if status == 'killed' else []}
    else:
        try:
            outcome = classify_python_run(process.returncode, json.loads(process.stdout))
        except (json.JSONDecodeError, KeyError):
            outcome = {'status': 'invalid', 'reason': 'missing Python result', 'failed_tests': []}
    outcome['mutant_sha256'] = hashlib.sha256(path.read_bytes()).hexdigest()
    return outcome


def run(args):
    admin_name, psql_env = database_environment(os.environ.get('WMS_TEST_ADMIN_DSN', ''))
    runtime_name, _ = database_environment(os.environ.get('WMS_TEST_RUNTIME_DSN', ''))
    if admin_name != runtime_name:
        raise ValueError('Admin/runtime mutation connections must use the same database')
    runtime = urlsplit(os.environ['WMS_TEST_RUNTIME_DSN'])
    admin = urlsplit(os.environ['WMS_TEST_ADMIN_DSN'])
    if runtime.port != admin.port:
        raise ValueError('Admin/runtime mutation connections must use the same server')
    run_root = ROOT / 'work' / ('mutations-' + uuid.uuid4().hex)
    run_root.mkdir(parents=True)
    report = {'status': 'failed', 'scope': 'Scoped non-equivalent inventory, authorization, background, OIDC/HTTP, document and migration mutations; not a whole-repository mutation score.', 'source_head': subprocess.check_output(['git', 'rev-parse', 'HEAD'], cwd=ROOT, text=True).strip(), 'mutants': [], 'selected_cases': args.case, 'work_directory': str(run_root.relative_to(ROOT))}

    def psql(sql, database='postgres'):
        env = dict(psql_env, PGDATABASE=database)
        result = subprocess.run([args.psql, '-X', '-q', '-At', '-v', 'ON_ERROR_STOP=1'], input=sql, text=True, env=env, capture_output=True)
        if result.returncode:
            # SQL never includes credentials; leave exact diagnostics in the local work dir.
            (run_root / 'sql-error.txt').write_text(result.stderr, encoding='utf8')
            raise RuntimeError('Mutation SQL setup failed (see work directory)')
        return result.stdout

    baseline_dir = run_root / 'baseline'
    baseline_dir.mkdir()
    try:
        baseline = run_go(args.go, DOCUMENT_TESTS + '|^TestDocumentCommandsRecoverFromCommitTransportFailure$|^TestDocumentPaginationProperties$', baseline_dir, os.environ.copy())
    except Exception as error:
        report['baseline'] = {'status': 'invalid', 'reason': type(error).__name__, 'failed_tests': []}
        target = ROOT / args.report
        target.parent.mkdir(parents=True, exist_ok=True)
        target.write_text(json.dumps(report, indent=2) + '\n', encoding='utf8')
        raise
    report['baseline'] = baseline
    if baseline['status'] != 'survived':
        target = ROOT / args.report
        target.parent.mkdir(parents=True, exist_ok=True)
        target.write_text(json.dumps(report, indent=2) + '\n', encoding='utf8')
        raise RuntimeError('Unmutated document tests must pass with zero skips before mutation testing')

    report['source_sha256_lf'] = {path: hashlib.sha256((ROOT / path).read_bytes().replace(b'\r\n', b'\n')).hexdigest() for path in ('internal/postgres/documents.go', 'internal/postgres/store.go', 'scripts/migrate_core.py')}
    document_sql = (ROOT / 'db/migrations/0009_warehouse_documents.sql').read_text(encoding='utf8')
    start = document_sql.index('CREATE FUNCTION warehouse.command_document(')
    end = document_sql.index('END $$;', start) + len('END $$;')
    function_sql = document_sql[start:end].replace('CREATE FUNCTION ', 'CREATE OR REPLACE FUNCTION ', 1)
    cases = [(name, 'sql', 'db/migrations/0009_warehouse_documents.sql', before, after, DOCUMENT_TESTS) for name, before, after in sql_mutants()]
    cases += [
        ('go_commit_error_loses_retry_guidance', 'go', 'internal/postgres/documents.go', 'return nil, &inventory.Problem{Status: 503, Code: "operation_interrupted", Detail: "The operation could not be confirmed. Retry with the same command identifier."}', 'return nil, translate(err)', '^TestDocumentCommandsRecoverFromCommitTransportFailure$'),
        ('go_definitive_commit_rejection_becomes_retry', 'go', 'internal/postgres/documents.go', 'if _, ok := errors.AsType[*pgconn.PgError](err); ok {', 'if _, ok := errors.AsType[*pgconn.PgError](err); ok && false {', '^TestDocumentCommandsPreserveDefinitiveCommitRejection$'),
        ('go_document_lookahead_removed', 'go', 'internal/postgres/store.go', 'args = append(args, f.Limit+1)', 'args = append(args, f.Limit)', '^TestDocumentPaginationProperties$'),
        ('go_single_owner_scope_removed', 'go', 'internal/postgres/store.go', 'where.WriteString(" AND owner_id=$3::text::uuid")', 'where.WriteString(" AND $3::text IS NOT NULL")', '^TestDocumentPaginationProperties$'),
        ('go_global_document_candidate_limit_removed', 'go', 'internal/postgres/store.go', 'fmt.Sprintf(" ORDER BY id LIMIT $%d) AS candidates ORDER BY candidates.id LIMIT $%d) ", len(args), len(args))', 'fmt.Sprintf(" ORDER BY id LIMIT $%d) AS candidates ORDER BY candidates.id) ", len(args))', '^TestDocumentAdversarialOwnerFanoutHydratesOnlyPage$'),
    ]
    try:
        core = load_core_mutations(ROOT)
        core_by_name = {case['name']: case for case in core}
        known_names = {case[0] for case in cases} | set(core_by_name) | {case[0] for case in python_mutants()}
        unknown = set(args.case or []) - known_names
        if unknown:
            raise ValueError('Unknown mutation case: ' + ', '.join(sorted(unknown)))
        for path in sorted({case['source'] for case in core if case['kind'] == 'go'}):
            report['source_sha256_lf'][path] = hashlib.sha256((ROOT / path).read_bytes().replace(b'\r\n', b'\n')).hexdigest()
        report['core_domains'] = {domain: sum(case['domain'] == domain for case in core) for domain in ('inventory', 'authorization', 'background', 'boundary')}
        report['core_baselines'] = {}
        for package in sorted({case['package'] for case in core}):
            baseline_directory = run_root / ('baseline-' + package.removeprefix('./').replace('/', '-'))
            baseline_directory.mkdir()
            pattern = '|'.join(sorted({case['pattern'] for case in core if case['package'] == package}))
            result = run_go(args.go, pattern, baseline_directory, os.environ.copy(), package=package)
            report['core_baselines'][package] = result
            if result['status'] != 'survived':
                raise RuntimeError('Unmutated core baseline failed: ' + package)
        cases.extend((case['name'], case['kind'], case.get('source', case.get('signature')), case['before'], case['after'], case['pattern']) for case in core)
        for name, kind, path, before, after, pattern in cases:
            if args.case and name not in args.case:
                continue
            directory = run_root / name
            directory.mkdir()
            if kind == 'sql_function':
                original = psql("SELECT pg_get_functiondef('" + core_by_name[name]['signature'] + "'::regprocedure);", admin_name)
            else:
                original = function_sql if kind == 'sql' else (ROOT / path).read_text(encoding='utf8')
            changed = replace_once(original, before, after)
            (directory / 'mutant.txt').write_text(changed, encoding='utf8')
            environment = os.environ.copy()
            overlay = None
            database = None
            try:
                if kind in ('sql', 'sql_function'):
                    database = 'wms_kernel_test_mutant_' + uuid.uuid4().hex
                    psql(f'CREATE DATABASE {database} WITH TEMPLATE {admin_name} OWNER wms_test_owner;')
                    psql(changed, database)
                    for key in ('WMS_TEST_ADMIN_DSN', 'WMS_TEST_RUNTIME_DSN'):
                        parsed = urlsplit(environment[key])
                        environment[key] = parsed._replace(path='/' + database).geturl()
                else:
                    overlay = directory / 'overlay.json'
                    overlay.write_text(json.dumps({'Replace': {str(ROOT / path): str(directory / 'mutant.txt')}}), encoding='utf8')
                package = core_by_name[name]['package'] if name in core_by_name else './internal/postgres'
                outcome = run_go(args.go, pattern, directory, environment, overlay, package=package)
            finally:
                if database:
                    # Only the freshly generated disposable clone is ever dropped.
                    psql(f'DROP DATABASE {database};')
            outcome.update(name=name, kind=kind, domain=core_by_name[name]['domain'] if name in core_by_name else 'documents', source=path, before=before, after=after, source_sha256_lf=hashlib.sha256(original.encode()).hexdigest(), mutant_sha256=hashlib.sha256(changed.encode()).hexdigest())
            report['mutants'].append(outcome)
            print(name + ': ' + outcome['status'], flush=True)
        python_cases = python_mutants()
        for name, before, after, windows in python_cases:
            if args.case and name not in args.case:
                continue
            directory = run_root / name
            directory.mkdir()
            outcome = run_python_mutant(directory, before, after, args.psql, psql_env, windows)
            outcome.update(name=name, kind='python', source='scripts/migrate_core.py', before=before, after=after)
            report['mutants'].append(outcome)
            print(name + ': ' + outcome['status'], flush=True)
        if not report['mutants']:
            raise ValueError('No mutation cases selected')
        report.update(killed=sum(x['status'] == 'killed' for x in report['mutants']), survived=sum(x['status'] == 'survived' for x in report['mutants']), invalid=sum(x['status'] == 'invalid' for x in report['mutants']))
        report['status'] = 'passed' if report['killed'] == len(report['mutants']) else 'failed'
    finally:
        target = ROOT / args.report
        target.parent.mkdir(parents=True, exist_ok=True)
        target.write_text(json.dumps(report, indent=2) + '\n', encoding='utf8')
    return report['status'] == 'passed'


if __name__ == '__main__':
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--go', default='go')
    parser.add_argument('--psql', default='psql')
    parser.add_argument('--report', default='work/document-mutations.json')
    parser.add_argument('--case', action='append')
    arguments = parser.parse_args()
    sys.exit(0 if run(arguments) else 1)
