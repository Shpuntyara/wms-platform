"""Run the isolated profile's baseline, functional and behavioral mutation gates."""
import argparse
import hashlib
import json
import os
from pathlib import Path
import subprocess
import sys


BASELINE_FAILURES = {
    'stolen API cannot enumerate foreign documents',
    'stolen API cannot forge actor via legacy command',
    'stolen API cannot claim outbox',
    'stolen API cannot acknowledge outbox',
    'stolen API cannot run expiry',
}


def validate_plan_evidence(path, profile_hash, test_hash):
    try:
        report = json.loads(Path(path).read_text())
    except (OSError, ValueError) as error:
        raise ValueError('Completed plan evidence was not retained') from error
    plans = report.get('plans', [])
    pairs = {(p.get('case'), p.get('mode')) for p in plans}
    required = {(case, mode) for case in ('one-owner-first','one-owner-deep','multiple-owner-first',
        'multiple-owner-deep','all-owner-first','all-owner-deep','explicit-owner')
        for mode in ('force_custom_plan','force_generic_plan')}
    if (report.get('seeded_documents', 0) < 100000 or report.get('seeded_lines', 0) < 300000
            or report.get('property_cases', 0) < 30 or not required.issubset(pairs)
            or report.get('profile_sha256') != profile_hash or report.get('test_sha256') != test_hash
            or any(not (0 < p.get('row_work', 0) <= 2000 and 0 < p.get('blocks', 0) <= 3000
                        and p.get('line_hydrations') == 21) for p in plans)):
        raise ValueError('Missing, stale or unbounded plan evidence')


def validate_receipt(kind, exit_code, report, go_output):
    results = report.get('results', [])
    failed = {r['name'] for r in results if r['status'] == 'fail'}
    if kind == 'green':
        valid = (exit_code == 0 and report.get('passed') is True and len(results) >= 26
                 and not failed and report.get('go_passed') is True and report.get('go_exit_code') == 0
                 and '--- FAIL:' not in go_output
                 and all('--- PASS: ' + name in go_output for name in (
                     'TestHardenedHTTPAndContainment', 'TestHardenedDocumentReadsAtScale',
                     'TestHardenedOnPremiseMultiWarehouse',
                     'TestHardenedOnPremCriticReadinessTracksInstallationAndSigningKey',
                     'TestHardenedOnPremCriticCrossOwnerAndStaleReadGrants',
                     'TestHardenedOnPremCriticWorkerRejectsInstallationDrift',
                     'TestHardenedDocumentAutomaticNumberingSignedReplayAndPermissions',
                     'TestHardenedDocumentReadsAtScale/independent_randomized_pagination_oracle')))
    elif kind == 'baseline':
        valid = exit_code == 1 and failed == BASELINE_FAILURES and report.get('passed') is False
    elif kind in ('signature', 'action', 'installation'):
        expected = {'signature': 'tampered actor signature rejected',
                    'action': 'signature for read cannot authorize a command',
                    'installation': 'signed context for another installation rejected'}[kind]
        valid = exit_code == 1 and report.get('passed') is False and expected in failed
    elif kind == 'worker_fence':
        valid = (exit_code == 1 and report.get('passed') is False and report.get('go_exit_code') == 1
                 and '--- FAIL: TestHardenedHTTPAndContainment/worker_services_installation_and_stale_ack_fenced' in go_output)
    elif kind == 'document_page':
        valid = (exit_code == 1 and report.get('passed') is False and report.get('go_exit_code') == 1
                 and '--- FAIL: TestHardenedDocumentReadsAtScale/one-owner-first' in go_output
                 and 'read page scanned history instead of bounded candidates' in go_output)
    else:
        valid = False
    if (not valid or any(r.get('status') not in ('pass', 'fail') for r in results)
            or any(marker in go_output for marker in ('build failed', '--- SKIP:', '[no tests to run]',
                                                     'panic: test timed out', 'fatal error:', 'SIGQUIT'))):
        raise ValueError('Missing or invalid behavioral evidence for ' + kind)


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--psql', required=True)
    parser.add_argument('--port', required=True, type=int)
    parser.add_argument('--go', required=True)
    parser.add_argument('--output', default='work/hardened')
    args = parser.parse_args()
    root = Path(__file__).resolve().parents[1]
    output = (root / args.output).resolve()
    output.mkdir(parents=True, exist_ok=True)
    runs = []
    for kind in ('baseline', 'green', 'signature', 'action', 'installation', 'worker_fence', 'document_page'):
        path = output / (kind + '.json')
        go_path = path.with_suffix('.go.txt')
        # An infrastructure failure must never reuse an earlier behavioral kill.
        path.unlink(missing_ok=True)
        go_path.unlink(missing_ok=True)
        command = [sys.executable, 'tests/hardened_profile.py', '--psql', args.psql,
                   '--port', str(args.port), '--profile', 'baseline' if kind == 'baseline' else 'hardened',
                   '--report', str(path)]
        if kind != 'baseline':
            command += ['--go', args.go]
        if kind not in ('baseline', 'green'):
            command += ['--mutant', kind]
        env = os.environ.copy()
        plan_root = (root / env.get('WMS_HARDENED_PLAN_DIR', str(output / 'plans'))).resolve()
        env['WMS_HARDENED_PLAN_DIR'] = str(plan_root / kind)
        plan_file = plan_root / kind / 'hardened-read-plans.json'
        plan_file.unlink(missing_ok=True)
        result = subprocess.run(command, cwd=root, env=env, capture_output=True, text=True, timeout=360)
        path.with_suffix('.txt').write_text(result.stdout + result.stderr, encoding='utf8')
        if not path.exists():
            raise ValueError('Profile run produced no receipt: ' + kind)
        report = json.loads(path.read_text())
        go_output = go_path.read_text() if go_path.exists() else ''
        validate_receipt(kind, result.returncode, report, go_output)
        if kind == 'green':
            validate_plan_evidence(plan_file,
                hashlib.sha256((root / 'db/profiles/hardened-v1.sql').read_bytes() + (root / 'db/profiles/onprem-v1.sql').read_bytes()).hexdigest(),
                hashlib.sha256((root / 'internal/postgres/hardened_load_integration_test.go').read_bytes()).hexdigest())
        runs.append({'kind': kind, 'status': 'pass', 'report': path.name})
        print('PASS ' + kind, flush=True)
    (output / 'summary.json').write_text(json.dumps({'passed': True, 'runs': runs,
        'mutants': 5, 'killed': 5, 'survived': 0, 'invalid': 0, 'skipped': 0}, indent=2) + '\n')


if __name__ == '__main__':
    main()
