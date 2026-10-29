"""Run reviewed UI mutations in an isolated temporary source tree."""
import argparse
import hashlib
import json
from pathlib import Path
import re
import shutil
import subprocess
import tempfile


def classify(exit_code, output, expected_tests):
    totals = {key: int(value) for key, value in re.findall(r'^# (tests|pass|fail|skipped|cancelled|todo) (\d+)\s*$', output, re.MULTILINE)}
    if (totals.get('tests') != expected_tests or expected_tests < 1
            or any(totals.get(key) != 0 for key in ('skipped', 'cancelled', 'todo'))
            or totals.get('pass', -1) + totals.get('fail', -1) != expected_tests):
        return 'invalid'
    if exit_code == 0 and totals['fail'] == 0:
        return 'survived'
    codes = re.findall(r"code: ['\"]([^'\"]+)['\"]", output)
    if (exit_code == 1 and totals['fail'] > 0 and codes.count('ERR_ASSERTION') == totals['fail']
            and all(code == 'ERR_ASSERTION' for code in codes)
            and not re.search(r'SyntaxError:|TypeError:|ReferenceError:|ERR_MODULE_NOT_FOUND|MODULE_NOT_FOUND|ERR_UNKNOWN_FILE_EXTENSION', output)):
        return 'killed'
    return 'invalid'


def catalog():
    """Exact reviewed replacements: changed anchors fail instead of skipping."""
    return [
        ('quantity_zero', 'domain.js', 'if (amount === 0n)', 'if (false)'),
        ('quantity_precision', 'domain.js', "if ((value.split('.')[1]?.length ?? 0) > scale)", 'if (false)'),
        ('quantity_fraction_lost', 'domain.js', "BigInt(fraction.padEnd(6, '0'))", '0n'),
        ('remaining_float_rounding', 'domain.js', 'const value = units(requested) - units(fulfilled);', 'const value = BigInt(Math.trunc((Number(requested) - Number(fulfilled)) * 1000000));'),
        ('scan_case_folded', 'domain.js', 'candidate[key] === scan.trim()', 'String(candidate[key]).toLowerCase() === scan.trim().toLowerCase()'),
        ('scan_ambiguity_accepted', 'domain.js', 'if (matches.length !== 1)', 'if (matches.length < 1)'),
        ('document_inactive_accepted', 'domain.js', "document.state !== 'active' || ", ''),
        ('document_kind_unchecked', 'domain.js', ' || document.kind !== kind', ''),
        ('version_fraction_accepted', 'domain.js', '!Number.isSafeInteger(document.version)', '!Number.isFinite(document.version)'),
        ('stale_line_snapshot_accepted', 'domain.js', "if (['requested_qty', 'fulfilled_qty', 'reserved_qty'].some(field => capturedLine[field] !== line[field]))", 'if (false)'),
        ('warehouse_scope_unchecked', 'domain.js', 'if (scope[field] !== document[field])', 'if (false)'),
        ('document_overfulfilment_accepted', 'domain.js', 'if (units(amount) > units(remaining(line.requested_qty, line.fulfilled_qty)))', 'if (false)'),
        ('balance_owner_unchecked', 'domain.js', "['organization_id', 'warehouse_id', 'owner_id']", "['organization_id', 'warehouse_id']"),
        ('balance_item_unchecked', 'domain.js', ' || balance.item_id !== item.id', ''),
        ('serial_quantity_unbounded', 'domain.js', 'if (balance.serial_id && units(amount) > 1000000n)', 'if (false)'),
        ('putaway_same_position_accepted', 'domain.js', 'if (sourceBalance.position_id === targetBalance.position_id)', 'if (false)'),
        ('putaway_tracking_unchecked', 'domain.js', "if (['lot_id', 'serial_id', 'stock_state'].some(field => (sourceBalance[field] ?? null) !== (targetBalance[field] ?? null)))", 'if (false)'),
        ('putaway_uses_reserved_stock', 'domain.js', 'units(sourceBalance.available_qty)', 'units(sourceBalance.on_hand)'),
        ('putaway_source_confused', 'domain.js', 'body.source_balance_id = sourceBalance.id;', 'body.source_balance_id = targetBalance.id;'),
        ('close_any_line_completed', 'domain.js', 'document.lines.every(', 'document.lines.some('),
        ('close_with_active_reservation', 'domain.js', ' && units(line.reserved_qty) === 0n', ''),
        ('journal_retry_new_identifier', 'journal.js', 'return this.send(entry, true);', 'return this.send({...entry, id: this.uuid()}, true);'),
        ('journal_retry_rejection_forgets_commit', 'journal.js', 'if (!recovering && [400,', 'if ([400,'),
        ('journal_uncaptured_request_alias', 'journal.js', 'body: structuredClone(body)', 'body'),
        ('journal_server_failure_forgotten', 'journal.js', '[400, 403, 404, 409, 413, 415, 422]', '[400, 403, 404, 409, 413, 415, 422, 503]'),
    ]


def run(node, directory, tests):
    result = subprocess.run([node, '--test', '--test-reporter=tap', *tests], cwd=directory,
                            text=True, encoding='utf-8', errors='replace', capture_output=True, timeout=40)
    return result.returncode, result.stdout + result.stderr


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--root', type=Path, default=Path(__file__).resolve().parents[1])
    parser.add_argument('--node', default=shutil.which('node') or 'node')
    parser.add_argument('--output', type=Path, default=Path('work/ui-mutations.json'))
    args = parser.parse_args()
    root = args.root.resolve()
    tests = ['tests/critic-domain.test.mjs', 'tests/critic-journal.test.mjs', 'tests/journal.test.mjs']
    sources = {name: (root / 'web/src' / name).read_text(encoding='utf-8') for name in ('domain.js', 'journal.js')}
    hashes = {str(path.relative_to(root)).replace('\\', '/'): hashlib.sha256(path.read_bytes()).hexdigest()
              for path in [*(root / 'web/src' / name for name in sources), *(root / 'web' / name for name in tests)]}
    report = {'passed': False, 'sources_and_tests_sha256': hashes, 'tests': tests,
              'mutations': [], 'killed': 0, 'survived': 0, 'invalid': 0, 'skipped': 0}
    with tempfile.TemporaryDirectory(prefix='wms-ui-mutations-') as directory:
        isolated = Path(directory)
        (isolated / 'src').mkdir()
        (isolated / 'tests').mkdir()
        (isolated / 'package.json').write_text('{"type":"module"}\n', encoding='utf-8')
        for name in tests:
            shutil.copyfile(root / 'web' / name, isolated / name)
        for name, content in sources.items():
            (isolated / 'src' / name).write_text(content, encoding='utf-8')
        baseline_code, baseline_output = run(args.node, isolated, tests)
        count_match = re.search(r'^# tests (\d+)\s*$', baseline_output, re.MULTILINE)
        expected = int(count_match[1]) if count_match else 0
        report['baseline'] = {'exit_code': baseline_code, 'test_count': expected, 'output': baseline_output}
        if classify(baseline_code, baseline_output, expected) != 'survived':
            report['invalid'] = 1
        else:
            for name, source, before, after in catalog():
                if sources[source].count(before) != 1:
                    report['mutations'].append({'name': name, 'status': 'invalid', 'reason': 'reviewed mutation anchor changed'})
                    report['invalid'] += 1
                    continue
                altered = sources[source].replace(before, after, 1)
                (isolated / 'src' / source).write_text(altered, encoding='utf-8')
                try:
                    code, output = run(args.node, isolated, tests)
                    status = classify(code, output, expected)
                    probe = {'name': name, 'source': source, 'status': status, 'exit_code': code,
                             'mutant_sha256': hashlib.sha256(altered.encode()).hexdigest(), 'output': output}
                except subprocess.TimeoutExpired:
                    status = 'invalid'
                    probe = {'name': name, 'source': source, 'status': status, 'reason': 'timeout'}
                finally:
                    (isolated / 'src' / source).write_text(sources[source], encoding='utf-8')
                report[status] += 1
                report['mutations'].append(probe)
                print(f'{name}: {status}', flush=True)
    report['passed'] = bool(report['mutations']) and report['killed'] == len(catalog()) and report['survived'] == report['invalid'] == 0
    args.output.parent.mkdir(parents=True, exist_ok=True)
    args.output.write_text(json.dumps(report, ensure_ascii=False, indent=2) + '\n', encoding='utf-8')
    print(json.dumps({key: report[key] for key in ('passed', 'killed', 'survived', 'invalid', 'skipped')}))
    if not report['passed']:
        raise SystemExit(1)


if __name__ == '__main__':
    main()
