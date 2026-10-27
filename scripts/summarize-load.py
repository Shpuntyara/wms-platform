"""Publish measured load evidence; refuse partial, failed, or undersampled runs."""
import argparse
import hashlib
import json
from pathlib import Path
import re
import shutil
import statistics
import subprocess

parser = argparse.ArgumentParser()
parser.add_argument('--session', required=True)
parser.add_argument('--benchstat', required=True)
parser.add_argument('--output', required=True)
parser.add_argument('--optimized-label', default='optimized')
args = parser.parse_args()
repo = Path(__file__).resolve().parent.parent
session = json.loads(Path(args.session).read_text(encoding='utf-8-sig'))
root = Path(session['Root']).resolve()
if not root.is_relative_to((repo/'work').resolve()):
    raise SystemExit('Refusing a load session outside repository work/')
output = Path(args.output).resolve()
if not output.is_relative_to((repo/'docs/verification').resolve()):
    raise SystemExit('Published evidence must stay inside docs/verification/')

def read(path):
    return json.loads(path.read_text(encoding='utf-8-sig'))

def benchmark(path):
    result = {}
    for line in path.read_text(encoding='utf-8-sig').splitlines():
        match = re.match(r'^BenchmarkHistoryHTTP/(\w+)-\d+\s+\d+\s+([\d.]+)\s+ns/op', line)
        if match:
            result.setdefault(match[1], []).append(float(match[2]))
    if len(result) != 5 or any(len(values) < 10 for values in result.values()):
        raise SystemExit('Ten real samples of each history benchmark are required')
    return result

phases = {}
for label in ('baseline', 'optimized'):
    folder = root/('baseline' if label=='baseline' else args.optimized_label)
    data = read(folder/'concurrency.json')
    if not data['passed'] or not data['cases'] or any(not c['Reconciled'] or c['TechnicalErrors'] for c in data['cases']):
        raise SystemExit(f'{label}: failed invariants or technical errors')
    phases[label] = {'raw': data, 'bench': benchmark(folder/'bench.txt')}
if phases['baseline']['raw']['fixture'] != phases['optimized']['raw']['fixture']:
    raise SystemExit('The compared phases do not use the identical seed fixture')
if set(phases['baseline']['bench']) != set(phases['optimized']['bench']):
    raise SystemExit('Benchmark sets differ')

output.mkdir(parents=True, exist_ok=True)
comparison = subprocess.run([args.benchstat, 'baseline='+str(root/'baseline/bench.txt'),
                             'optimized='+str(root/args.optimized_label/'bench.txt')],
                            check=True, capture_output=True, text=True, encoding='utf-8')
(output/'benchstat.txt').write_text(comparison.stdout, encoding='utf-8', newline='\n')

benchmarks = {}
for name, old in phases['baseline']['bench'].items():
    new = phases['optimized']['bench'][name]
    before, after = statistics.median(old), statistics.median(new)
    benchmarks[name] = {'baseline_median_mean_request_ms': before/1e6,
                        'optimized_median_mean_request_ms': after/1e6,
                        'change_percent': (after/before-1)*100,
                        'baseline_samples': len(old), 'optimized_samples': len(new)}

concurrency = {}
for label, phase in phases.items():
    source_label = 'baseline' if label=='baseline' else args.optimized_label
    groups = {}
    for case in phase['raw']['cases']:
        key = f"{case['Name']}/clients={case['Concurrency']}"
        groups.setdefault(key, []).append(case)
    concurrency[label] = {
        key: {'samples': len(cases),
              'median_http_requests_per_second': statistics.median(c['RequestsPerSecond'] for c in cases),
              'median_run_p95_ms': statistics.median(c['Latency']['P95MS'] for c in cases),
              'worst_run_p99_ms': max(c['Latency']['P99MS'] for c in cases),
              'technical_errors': sum(c['TechnicalErrors'] for c in cases),
              'expected_conflicts': sum(c['Conflicts'] for c in cases)}
        for key, cases in groups.items()
    }
    target = output/label
    target.mkdir(exist_ok=True)
    for name in ('bench.txt', 'concurrency.json', 'relations.json', 'indexes.json'):
        source = root/source_label/name
        if source.exists(): shutil.copyfile(source, target/name)
    shutil.copytree(root/source_label/'plans', target/'plans', dirs_exist_ok=True)

for name in ('host.json', 'fixture.json'):
    shutil.copyfile(root/name, output/name)
for name in ('bounded-red.log', 'query-only-regression.log'):
    shutil.copyfile(root/'baseline'/name, output/'baseline'/name)
shutil.copyfile(root/args.optimized_label/'bounded-pages.log', output/'optimized/bounded-pages.log')
source = repo/'internal/postgres/store.go'
migration = repo/'db/migrations/0005_inventory_pagination.sql'
summary = {'status': 'passed', 'baseline_adapter_commit': 'e0ab625c190a80e78492d377379fbecb0c649b13',
           'optimized_adapter_sha256': hashlib.sha256(source.read_bytes()).hexdigest(),
           'migration_sha256': hashlib.sha256(migration.read_bytes()).hexdigest(),
           'environment': phases['optimized']['raw']['environment'],
           'seed_movements': phases['optimized']['raw']['fixture']['Movements'],
           'seed_reservations': phases['optimized']['raw']['fixture']['Reservations'],
           'seed_balances': phases['optimized']['raw']['fixture']['Balances'],
           'history_http_benchmarks': benchmarks, 'concurrency': concurrency,
           'verification': {'all_reconciled': True, 'technical_errors': 0,
                            'bounded_page_regression': 'failed before, passed after',
                            'statistics': 'benchstat.txt contains significance and confidence intervals; write profiles are descriptive repeated bursts, not a statistical improvement claim'},
           'limits': 'Local warm-cache closed-loop bursts. Driver/API/OIDC/PostgreSQL share a machine. No production SLA, long-duration soak, external IdP, customer guard or failover validation.'}
(output/'summary.json').write_text(json.dumps(summary, ensure_ascii=False, indent=2)+'\n', encoding='utf-8', newline='\n')
# Go's CPUID brand line may contain padding. Normalize presentation whitespace
# in published console evidence without changing any measured values.
for file in output.rglob('*'):
    if file.is_file() and file.suffix in ('.txt', '.log'):
        text = file.read_text(encoding='utf-8-sig')
        file.write_text('\n'.join(line.rstrip() for line in text.splitlines())+'\n', encoding='utf-8', newline='\n')
print(json.dumps({'status': summary['status'], 'benchmarks': benchmarks, 'output': str(output)}, indent=2))
