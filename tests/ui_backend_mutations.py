"""Run bounded frontend-support Go mutants using overlays, never source edits."""
import argparse
import hashlib
import json
from pathlib import Path
import subprocess
import time


REQUIRED = {
    'TestWorkspaceListsOnlyFreshlyResolvedPrincipalScopes',
    'TestWorkspaceCatalogCannotExposeForeignScopesOwnersOrOIDC',
    'TestWorkspaceCatalogPropertyMatchesIndependentPermissionScopes',
    'TestDecodeWorkspaceConfigRejectsSecretsAndAmbiguousCatalogs',
    'TestHandlerServesOnlyPublicAssetsAndOIDCBootstrap',
    'TestHandlerRejectsTraversalDirectoriesAndPrivateFiles',
    'TestHandlerKeepsAPIAuthenticationAndMethodSemantics',
    'TestLoadWorkspaceConfigUsesPublicBrowserClientAndSeparateCatalog',
}


def classify(events, returncode, baseline=False):
    ran = {e.get('Test') for e in events if e.get('Action') == 'run'}
    failed_tests = [e for e in events if e.get('Action') == 'fail' and e.get('Test')]
    failed_packages = {e.get('Package') for e in failed_tests}
    if (not REQUIRED.issubset(ran)
            or any(e.get('Action') in ('skip', 'build-fail', 'invalid-output') for e in events)
            or any(e.get('Action') == 'fail' and not e.get('Test')
                   and e.get('Package') not in failed_packages for e in events)):
        return 'invalid'
    if returncode == 0 and not failed_tests:
        return 'green' if baseline else 'survived'
    if returncode != 0 and failed_tests:
        return 'invalid' if baseline else 'killed'
    return 'invalid'


MUTANTS = [
    ('catalog-create-grant-can-read', 'internal/api/workspace.go',
     'if g.Permission == "document.read" || g.Permission == "stock.read" {', 'if true {'),
    ('catalog-owner-filter-removed', 'internal/api/workspace.go',
     '(g.OwnerID == "" || g.OwnerID == entry.ID)', 'true'),
    ('catalog-position-other-warehouse', 'internal/api/workspace.go',
     'if readWarehouses[[2]string{entry.OrganizationID, entry.WarehouseID}] {',
     'if readOrganizations[entry.OrganizationID] {'),
    ('workspace-duplicate-grants', 'internal/api/workspace.go',
     'grants = slices.Compact(grants)', '// mutation: duplicate grants retained'),
    ('config-accepts-secret-fields', 'internal/api/workspace_config.go',
     'd.DisallowUnknownFields()', '// mutation: unknown fields accepted'),
    ('config-accepts-seventh-decimal', 'internal/api/workspace_config.go',
     'v.QuantityScale < 0 || v.QuantityScale > 6', 'v.QuantityScale < 0 || v.QuantityScale > 7'),
    ('static-private-file-allowlist-removed', 'internal/ui/handler.go',
     'if !publicAsset(name) {', 'if false {'),
    ('browser-issuer-binding-removed', 'cmd/wms-api/ui.go',
     'if config.OIDC != (api.BrowserOIDC{}) && strings.TrimSuffix(config.OIDC.URL, "/")+"/realms/"+config.OIDC.Realm != strings.TrimSuffix(issuer, "/") {',
     'if false {'),
]


def digest(path):
    return hashlib.sha256(path.read_bytes()).hexdigest()


def run_case(repo, go, target, overlay=None, baseline=False):
    target.mkdir(parents=True, exist_ok=True)
    command = [go, 'test']
    if overlay:
        command.append('-overlay=' + str(overlay))
    command.extend(['./internal/api', './internal/ui', './cmd/wms-api', '-count=1', '-json',
                    '-run', 'TestWorkspace|TestDecodeWorkspaceConfig|TestHandler|TestLoadWorkspaceConfig'])
    start = time.monotonic()
    try:
        result = subprocess.run(command, cwd=repo, capture_output=True, text=True,
                                encoding='utf-8', errors='replace', timeout=180)
    except subprocess.TimeoutExpired as error:
        (target / 'timeout.txt').write_text(str(error), encoding='utf-8')
        return {'status': 'invalid', 'reason': 'timeout', 'elapsed': time.monotonic() - start}
    (target / 'events.jsonl').write_text(result.stdout, encoding='utf-8')
    (target / 'stderr.txt').write_text(result.stderr, encoding='utf-8')
    events = []
    for line in result.stdout.splitlines():
        try:
            events.append(json.loads(line))
        except json.JSONDecodeError:
            events.append({'Action': 'invalid-output'})
    status = classify(events, result.returncode, baseline=baseline)
    return {'status': status, 'exit_code': result.returncode,
            'elapsed': round(time.monotonic() - start, 3),
            'executed_tests': sorted({e['Test'] for e in events if e.get('Action') == 'run'}),
            'failed_tests': sorted({e['Package'] + '/' + e['Test'] for e in events
                                    if e.get('Action') == 'fail' and e.get('Test')}),
            'events_sha256': digest(target / 'events.jsonl')}


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--go', default='go')
    parser.add_argument('--output', type=Path)
    args = parser.parse_args()
    repo = Path(__file__).resolve().parents[1]
    output = (args.output or repo / 'work/ui-backend-mutations').resolve()
    output.mkdir(parents=True, exist_ok=True)
    files = {path for _, path, _, _ in MUTANTS}
    files.update(str(p.relative_to(repo)).replace('\\', '/') for pattern in (
        'internal/api/workspace*test.go', 'internal/ui/*test.go', 'cmd/wms-api/ui_test.go')
        for p in repo.glob(pattern))
    hashes = {path: digest(repo / path) for path in sorted(files)}
    report = {'schema': 1, 'source_sha256': hashes,
              'baseline': run_case(repo, args.go, output / 'baseline', baseline=True),
              'mutants': []}
    if report['baseline']['status'] == 'green':
        for name, relative, needle, replacement in MUTANTS:
            target = output / name
            target.mkdir(exist_ok=True)
            source = repo / relative
            text = source.read_text(encoding='utf-8')
            if text.count(needle) != 1:
                report['mutants'].append({'id': name, 'status': 'invalid', 'reason': 'mutation anchor changed'})
                continue
            mutated = target / 'source.txt'
            mutated.write_text(text.replace(needle, replacement, 1), encoding='utf-8')
            overlay = target / 'overlay.json'
            overlay.write_text(json.dumps({'Replace': {str(source.resolve()): str(mutated)}}), encoding='utf-8')
            result = run_case(repo, args.go, target, overlay=overlay)
            result.update(id=name, source=relative, mutant_sha256=digest(mutated))
            report['mutants'].append(result)
            print(name + ': ' + result['status'], flush=True)
    report['source_unchanged'] = hashes == {path: digest(repo / path) for path in sorted(files)}
    report['killed'] = sum(m['status'] == 'killed' for m in report['mutants'])
    report['survived'] = sum(m['status'] == 'survived' for m in report['mutants'])
    report['invalid'] = sum(m['status'] == 'invalid' for m in report['mutants'])
    report['passed'] = (report['baseline']['status'] == 'green' and report['source_unchanged']
                        and len(report['mutants']) == len(MUTANTS)
                        and report['killed'] == len(MUTANTS))
    (output / 'report.json').write_text(json.dumps(report, indent=2) + '\n', encoding='utf-8')
    print(json.dumps({key: report[key] for key in ('passed', 'killed', 'survived', 'invalid', 'source_unchanged')}))
    raise SystemExit(0 if report['passed'] else 1)


if __name__ == '__main__':
    main()
