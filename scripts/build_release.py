"""Build a Linux release from one exact Git commit, never the dirty checkout."""
import argparse
import hashlib
import io
import json
import os
from pathlib import Path
import re
import subprocess
import tarfile


def package(source, binary, revision, target):
    if not re.fullmatch('[a-f0-9]{40}', revision):
        raise ValueError('A full immutable revision is required')
    source, binary, target = map(Path, (source, binary, target))
    files = {'bin/wms-api': binary.read_bytes()}
    for directory in ('db', 'deploy/linux', 'deploy/ansible'):
        for path in sorted((source / directory).rglob('*')):
            if path.is_symlink():
                raise ValueError('Release source must contain regular files')
            if path.is_file() and '__pycache__' not in path.parts:
                files[path.relative_to(source).as_posix()] = path.read_bytes()
    runner = source / 'scripts/migrate_core.py'
    if not runner.is_file() or not any(name.startswith('db/') for name in files):
        raise ValueError('Release is missing migration sources')
    files['scripts/migrate_core.py'] = runner.read_bytes()
    manifest = {'schema': 1, 'revision': revision, 'files': {
        name: hashlib.sha256(data).hexdigest() for name, data in files.items()}}
    files['release-manifest.json'] = (json.dumps(manifest, indent=2, sort_keys=True) + '\n').encode()
    with target.open('xb') as output:
        with tarfile.open(fileobj=output, mode='w:gz') as archive:
            for name, data in sorted(files.items()):
                item = tarfile.TarInfo(name)
                item.size = len(data)
                item.mode = 0o755 if name == 'bin/wms-api' or name.endswith('.sh') else 0o644
                archive.addfile(item, io.BytesIO(data))
    return {'revision': revision, 'path': str(target.resolve()),
            'sha256': hashlib.sha256(target.read_bytes()).hexdigest(),
            'binary_sha256': hashlib.sha256(binary.read_bytes()).hexdigest()}


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--revision', required=True)
    parser.add_argument('--go', default='go')
    parser.add_argument('--output', type=Path, required=True)
    args = parser.parse_args()
    repo = Path(__file__).resolve().parents[1]
    revision = subprocess.check_output(['git', 'rev-parse', args.revision + '^{commit}'], cwd=repo, text=True).strip()
    args.output.mkdir(parents=True, exist_ok=True)
    stage = args.output / ('source-' + revision)
    stage.mkdir(exist_ok=False)
    snapshot = subprocess.check_output(['git', 'archive', '--format=tar', revision], cwd=repo)
    with tarfile.open(fileobj=io.BytesIO(snapshot)) as archive:
        for member in archive:
            if member.isdir():
                continue
            if not member.isfile() or member.name.startswith('/') or '..' in Path(member.name).parts:
                raise ValueError('Unsupported source archive member')
            path = stage / member.name
            path.parent.mkdir(parents=True, exist_ok=True)
            path.write_bytes(archive.extractfile(member).read())
    binary = args.output / ('wms-api-' + revision)
    env = dict(os.environ, GOOS='linux', GOARCH='amd64', CGO_ENABLED='0')
    subprocess.run([args.go, 'build', '-trimpath', '-buildvcs=false', '-ldflags=-buildid=' + revision,
                    '-o', str(binary.resolve()), './cmd/wms-api'], cwd=stage, env=env, check=True)
    receipt = package(stage, binary, revision, args.output / ('wms-' + revision + '.tar.gz'))
    (args.output / (revision + '.json')).write_text(json.dumps(receipt, indent=2) + '\n')
    print(json.dumps(receipt))


if __name__ == '__main__':
    main()
