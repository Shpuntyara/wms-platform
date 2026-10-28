#!/usr/bin/env bash
# Run on the dedicated Ubuntu guest after uploading a trusted release/installer.
set -euo pipefail
if [ "$(id -u)" != 0 ]; then
  echo 'Run this installer through sudo on the intended guest.' >&2
  exit 1
fi
export DEBIAN_FRONTEND=noninteractive
installer_dir="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)"
python3 "$installer_dir/wmsctl.py" --help >/dev/null
apt-get update -qq
apt-get install -y --no-install-recommends ansible-core ca-certificates
# The argument JSON contains only archive path/digest/revision/IP, never secrets.
settings_file="$(mktemp /run/wms-install.XXXXXX.json)"
trap 'rm -f -- "$settings_file"' EXIT
python3 - "$settings_file" "$@" <<'PY'
import argparse, json, sys
p = argparse.ArgumentParser()
p.add_argument('--archive', required=True)
p.add_argument('--sha256', required=True)
p.add_argument('--revision', required=True)
p.add_argument('--address', required=True)
a = p.parse_args(sys.argv[2:])
with open(sys.argv[1], 'w') as out:
    json.dump({'wms_archive': a.archive, 'wms_sha256': a.sha256,
               'wms_revision': a.revision, 'wms_address': a.address}, out)
PY
ansible-playbook -i localhost, -c local "$installer_dir/../ansible/site.yml" --limit localhost --extra-vars "@$settings_file"
