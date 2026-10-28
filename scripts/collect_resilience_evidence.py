"""Publish synthetic lab evidence without traversing private PostgreSQL data."""
import argparse
import hashlib
import json
import os
from pathlib import Path
import shutil


def collect(root, output):
    root, output = Path(root).resolve(), Path(output).resolve()
    if output == root or not output.is_relative_to(root):
        raise ValueError('Evidence destination must be a separate directory under its work root')
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
