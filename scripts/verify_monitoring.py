"""Exercise the shipped dashboard expression with real Prometheus semantics."""
import argparse
import hashlib
import importlib.util
import json
from pathlib import Path
import copy
import subprocess
import tempfile


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--root', type=Path, default=Path(__file__).resolve().parents[1])
    parser.add_argument('--promtool', required=True)
    parser.add_argument('--output', type=Path, default=Path('work/monitoring-verification.json'))
    args = parser.parse_args()
    source = args.root / 'deploy/linux/wmsctl.py'
    spec = importlib.util.spec_from_file_location('monitoring_installer', source)
    module = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(module)
    captured = {}
    module.private_write = lambda path, text, mode=0o600: captured.update({str(path): text})
    module.dashboard({'instance_id': '00000000-0000-4000-8000-000000000001'})
    dashboard = json.loads(next(value for key, value in captured.items() if key.endswith('wms.json')))
    panel = next(panel for panel in dashboard['panels'] if panel['title'] == 'Background readiness')
    target = panel['targets'][0]
    expression = target['expr']
    labels = '{instance="monitor:9091",job="wms-background"}'
    tests = []
    for name, up, ready, expected, metric in [
        ('healthy', '1 1 1', '1 1 1', 1, 'wms_background_ready'),
        ('api_disappeared', '1 0 0', '1 stale _', 0, ''),
        ('dependency_unready', '1 1 1', '0 0 0', 0, 'wms_background_ready'),
        ('missing_application_metric', '1 1 1', None, 0, ''),
    ]:
        inputs = [{'series': 'up' + labels, 'values': up}]
        if ready:
            inputs.append({'series': 'wms_background_ready' + labels, 'values': ready})
        tests.append({'name': name, 'interval': '1m', 'input_series': inputs,
            'promql_expr_test': [{'expr': expression, 'eval_time': '2m',
                                 'exp_samples': [{'labels': metric + labels, 'value': expected}]}]})
    fixture = {'rule_files': [], 'evaluation_interval': '1m', 'tests': tests}
    mutations = []
    with tempfile.TemporaryDirectory() as directory:
        path = Path(directory) / 'monitoring.json'
        path.write_text(json.dumps(fixture, indent=2))  # JSON is also valid YAML.
        result = subprocess.run([args.promtool, 'test', 'rules', str(path)], text=True, capture_output=True, timeout=60)
        if result.returncode == 0 and target.get('instant') is True:
            if expression.count(' * 0') != 1 or ' or on(instance, job) ' not in expression:
                raise ValueError('Reviewed monitoring mutation anchors changed')
            for name, altered in [('remove_unavailable_fallback', 'wms_background_ready'),
                                  ('missing_metric_falsely_healthy', expression.replace(' * 0', ' * 1'))]:
                mutant = copy.deepcopy(fixture)
                for case in mutant['tests']:
                    case['promql_expr_test'][0]['expr'] = altered
                path.write_text(json.dumps(mutant, indent=2))
                probe = subprocess.run([args.promtool, 'test', 'rules', str(path)], text=True, capture_output=True, timeout=60)
                output = probe.stdout + probe.stderr
                if (probe.returncode != 1 or 'FAILED:' not in output or 'exp:' not in output or 'got:' not in output
                        or any(marker in output.lower() for marker in ('parse error', 'error loading', 'error evaluating'))):
                    raise ValueError('Monitoring mutant survived or produced invalid evidence: ' + name)
                mutations.append({'name': name, 'status': 'killed', 'exit_code': probe.returncode, 'output': output})
    instant = target.get('instant') is True
    version = subprocess.check_output([args.promtool, '--version'], text=True, stderr=subprocess.STDOUT).strip()
    report = {'passed': result.returncode == 0 and instant, 'cases': [case['name'] for case in tests],
              'expression': expression, 'instant_stat': instant, 'promtool_exit_code': result.returncode,
              'promtool_version': version, 'source_sha256': hashlib.sha256(source.read_bytes()).hexdigest(),
              'fixture_sha256': hashlib.sha256(json.dumps(fixture, sort_keys=True).encode()).hexdigest(),
              'mutations': mutations, 'killed': len(mutations), 'survived': 0, 'invalid': 0, 'skipped': 0,
              'output': result.stdout + result.stderr}
    args.output.parent.mkdir(parents=True, exist_ok=True)
    args.output.write_text(json.dumps(report, indent=2) + '\n')
    print(json.dumps(report, indent=2))
    if not report['passed']:
        raise SystemExit(1)


if __name__ == '__main__':
    main()
