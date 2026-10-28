"""Publish synthetic lab evidence without traversing private PostgreSQL data."""
import argparse
import hashlib
import json
import os
from pathlib import Path
import shutil

from crash_restore import STAGES


def validate_complete_pass(root):
    report_path = root / 'final.json'
    if report_path.is_symlink() or not report_path.is_file():
        raise ValueError('Final report missing from the owned evidence root')
    try:
        report = json.loads(report_path.read_text())
    except (OSError, ValueError) as error:
        raise ValueError('Final report is not readable evidence') from error
    if report.get('status') == 'failed':
        return
    if report.get('status') != 'passed':
        raise ValueError('Final report must clearly declare passed or failed')
    try:
        run = Path(report['work_directory']).resolve()
        if run == root or not run.is_relative_to(root):
            raise ValueError('Passed report references evidence outside its owned root')

        def checked_digest(value):
            if not isinstance(value, str) or len(value) != 64 or any(char not in '0123456789abcdef' for char in value):
                raise ValueError('Missing or invalid evidence checksum anchor')
            return value

        def artifact(name, digest=None, expected_bytes=None):
            path = run / name
            if path.is_symlink() or not path.resolve().is_relative_to(root) or not path.is_file():
                raise ValueError('Missing required evidence archive or witness: ' + name)
            raw = path.read_bytes()
            if not raw or (digest is not None and hashlib.sha256(raw).hexdigest() != digest):
                raise ValueError('Evidence archive or witness checksum mismatch: ' + name)
            if expected_bytes is not None and (type(expected_bytes) is not int or len(raw) != expected_bytes):
                raise ValueError('Evidence backup archive size mismatch')
            return raw

        restore = report['restore']
        if restore.get('status') != 'passed':
            raise ValueError('Passed evidence does not include a successful restore')
        artifact('synthetic-wms.dump', checked_digest(restore['backup_sha256']), restore['backup_bytes'])
        seen_commands = set()
        def witness(name, count, stages, digest):
            if type(count) is not int or count < 1:
                raise ValueError('Invalid complete witness or canary count')
            records = {}
            for line in artifact(name, checked_digest(digest)).decode('utf8').splitlines():
                record = json.loads(line)
                unit, commands = record['unit_id'], record['request']['commands']
                if (not isinstance(unit, str) or not unit or unit in records or not isinstance(commands, list)
                        or len(commands) != len(stages) or set(record['result']) != set(stages)
                        or any(not isinstance(command, str) or not command or command in seen_commands for command in commands)
                        or len(set(commands)) != len(commands)):
                    raise ValueError('Incomplete or duplicate command IDs in evidence witness')
                seen_commands.update(commands)
                records[unit] = record
            if len(records) != count:
                raise ValueError('Incomplete witness count differs from passed report')
            return records
        acknowledged = witness('acknowledged.jsonl', report['witness_units'], STAGES, report['witness_sha256'])
        canaries = witness('canaries.jsonl', report['canary_units'], STAGES[:7], report['canary_witness_sha256'])
        if (set(acknowledged) & set(canaries) or type(report['witness_commands']) is not int
                or report['witness_commands'] != len(seen_commands)):
            raise ValueError('Unique command count does not match complete evidence')
        candidates = {}
        for line in artifact('candidates.jsonl').decode('utf8').splitlines():
            candidate = json.loads(line)
            unit = candidate['unit_id']
            if unit in candidates:
                raise ValueError('Duplicate candidate ID in evidence witness')
            candidates[unit] = candidate
        if (set(candidates) != set(acknowledged) or any(candidates[unit]['request'] != record['request']
                for unit, record in acknowledged.items())):
            raise ValueError('Candidate and completed witness evidence are incomplete or changed')
    except (KeyError, TypeError, UnicodeError, OSError, json.JSONDecodeError) as error:
        raise ValueError('Incomplete or unreadable passed recovery evidence') from error


def collect(root, output):
    root, output = Path(root).resolve(), Path(output).resolve()
    if output == root or not output.is_relative_to(root):
        raise ValueError('Evidence destination must be a separate directory under its work root')
    validate_complete_pass(root)
    output.mkdir(parents=True, exist_ok=True, mode=0o755)
    output.chmod(0o755)
    manifest = {}
    for directory, children, files in os.walk(root, topdown=True):
        directory = Path(directory)
        children[:] = [name for name in children if name != 'pgdata'
                       and not (directory / name).is_symlink()
                       and (directory / name).resolve() != output]
        for name in files:
            source = directory / name
            if source.is_symlink() or not (name.endswith(('.jsonl', '.log', '.txt', '.err'))
                    or name in ('final.json', 'report.json', 'synthetic-wms.dump')):
                continue
            relative = source.relative_to(root)
            target = output / relative
            target.parent.mkdir(parents=True, exist_ok=True, mode=0o755)
            shutil.copyfile(source, target)
            target.chmod(0o644)
            manifest[relative.as_posix()] = {'bytes': target.stat().st_size,
                'sha256': hashlib.sha256(target.read_bytes()).hexdigest()}
    if not manifest:
        raise ValueError('No resilience evidence was produced')
    manifest_path = output / 'evidence-manifest.json'
    manifest_path.write_text(json.dumps(manifest, indent=2) + '\n')
    manifest_path.chmod(0o644)
    return manifest


if __name__ == '__main__':
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--root', required=True)
    parser.add_argument('--output', required=True)
    args = parser.parse_args()
    result = collect(args.root, args.output)
    print(json.dumps({'files': len(result), 'output': args.output}))
