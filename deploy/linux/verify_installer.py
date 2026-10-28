#!/usr/bin/env python3
"""Executable installer property/behavior mutations; no skipped/error kills."""
import argparse
import hashlib
import json
from pathlib import Path
import shutil
import subprocess
import sys
import tempfile

ROOT = Path(__file__).resolve().parents[2]
MUTANTS = [
    ('archive_digest', 'hashlib.sha256(blob).hexdigest() != digest', 'False',
     'test_verified_bundle_rejects_hash_revision_untracked_and_links'),
    ('path_traversal', 'def safe_member_name(name):\n', 'def safe_member_name(name):\n    return name\n',
     'test_path_property_rejects_traversal_absolute_and_platform_ambiguity'),
    ('secret_regeneration', 'if path.exists():\n        value = json.loads(path.read_text())',
     'if False:\n        value = json.loads(path.read_text())',
     'test_secrets_are_persistent_private_and_independent'),
    ('backup_preflight', 'def validate_update_preflight(settings, receipt, instance_id):\n',
     'def validate_update_preflight(settings, receipt, instance_id):\n    return None\n',
     'test_upgrade_requires_durable_database_and_verified_backup'),
    ('archive_snapshot', "with tarfile.open(fileobj=io.BytesIO(blob), mode='r:*') as source:\n            for member in source:",
     "with tarfile.open(archive, 'r:*') as source:\n            for member in source:",
     'test_archive_reopen_cannot_install_unverified_changed_bytes'),
    ('premigration_recovery', "if recovery['resume'] and not recovery['migration_started']:", 'if False:',
     'test_failed_backup_before_migration_restarts_existing_writer'),
    ('mount_permissions', "os.chmod(CONFIG / 'keycloak', 0o700)", "os.chmod(CONFIG / 'keycloak', 0o755)",
     'test_keycloak_and_grafana_mount_roots_are_traversable'),
    ('secret_file_boundary', "private_write(CONFIG / 'postgres-password', value['db_admin_password'] + '\\n', 0o400)",
     "private_write(CONFIG / 'postgres-password', value['db_admin_password'] + '\\n', 0o444)",
     'test_database_and_oidc_secret_leaves_are_not_readable_by_caddy_group'),
    ('semantic_health', 'def verify_health_payload(value, kind):\n',
     'def verify_health_payload(value, kind):\n    return value\n',
     'test_http_health_rejects_empty_false_and_stale_payload'),
    ('monitor_auth', '        basicauth {{\n            wms-viewer {monitor_hash}\n        }}\n', '',
     'test_monitor_route_requires_separate_viewer_auth_without_plaintext'),
    ('operator_backup_recovery', "    finally:\n        if running:\n            run(['systemctl', 'start', 'wms-api'])", '    finally:\n        pass',
     'test_operator_backup_failure_also_restarts_existing_writer'),
    ('image_retry', 'for attempt in range(1, 4):', 'for attempt in range(3, 4):',
     'test_image_pull_retries_transient_failure_then_continues_sequentially'),
    ('image_fail_closed', "raise RuntimeError('Image fetch failed after 3 attempts (' + service + ')') from None",
     'break', 'test_image_pull_permanent_failure_aborts_before_database_mutation'),
    ('release_traversal', 'def release_permissions(directory):\n',
     'def release_permissions(directory):\n    return None\n',
     'test_extracted_release_is_executable_but_not_writable_by_service_user'),
]


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--output', type=Path, default=ROOT / 'work/vm-installer-mutations')
    args = parser.parse_args()
    args.output.mkdir(parents=True, exist_ok=True)
    source = (ROOT / 'deploy/linux/wmsctl.py').read_text()
    tests = [ROOT / 'tests/test_vm_installer.py', ROOT / 'tests/test_installer_adversarial.py']
    results = []
    for name, original, altered, required in [('baseline', '', '', '')] + MUTANTS:
        if original and source.count(original) != 1:
            raise ValueError('Mutation anchor is missing or ambiguous: ' + name)
        with tempfile.TemporaryDirectory() as temporary:
            directory = Path(temporary)
            (directory / 'deploy/linux').mkdir(parents=True)
            (directory / 'tests').mkdir()
            for test_file in tests:
                shutil.copyfile(test_file, directory / 'tests' / test_file.name)
            (directory / 'deploy/linux/wmsctl.py').write_text(source.replace(original, altered, 1) if original else source)
            result = subprocess.run([sys.executable, '-m', 'unittest', 'discover', '-s', 'tests', '-p', 'test*installer*.py'],
                                    cwd=directory, text=True, capture_output=True, timeout=60)
        output = result.stdout + result.stderr
        (args.output / (name + '.txt')).write_text(output)
        if name == 'baseline':
            valid = result.returncode == 0 and 'Ran 15 tests' in output and 'OK' in output
        else:
            valid = (result.returncode == 1 and 'FAIL: ' + required in output and
                     'FAILED (failures=' in output and 'errors=' not in output)
        if not valid or any(marker in output for marker in ('SyntaxError', 'ImportError', 'skipped=', 'Ran 0 tests')):
            raise ValueError('Invalid baseline or mutation evidence: ' + name)
        results.append({'name': name, 'status': 'passed' if name == 'baseline' else 'killed', 'exit_code': result.returncode})
    report = {'passed': True, 'mutants': len(MUTANTS), 'killed': len(MUTANTS), 'survived': 0,
              'invalid': 0, 'skipped': 0, 'source_sha256': hashlib.sha256(source.encode()).hexdigest(),
              'test_sha256': {path.name: hashlib.sha256(path.read_bytes()).hexdigest() for path in tests}, 'results': results}
    (args.output / 'summary.json').write_text(json.dumps(report, indent=2) + '\n')
    print(json.dumps(report, indent=2))


if __name__ == '__main__':
    main()
