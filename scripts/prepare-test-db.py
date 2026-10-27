"""Provision only the disposable local integration database; never a production DB."""
import argparse
import subprocess
from pathlib import Path

parser = argparse.ArgumentParser()
parser.add_argument('--psql', required=True)
parser.add_argument('--port', required=True, type=int)
parser.add_argument('--admin', default='wms_test_admin')
args = parser.parse_args()
root = Path(__file__).resolve().parent.parent
base = [args.psql, '-X', '-q', '-h', '127.0.0.1', '-p', str(args.port), '-U', args.admin, '-v', 'ON_ERROR_STOP=1']
roles = "CREATE ROLE wms_test_owner NOLOGIN NOSUPERUSER NOCREATEDB NOCREATEROLE; CREATE ROLE wms_test_runtime NOLOGIN; CREATE ROLE wms_test_module NOLOGIN; CREATE ROLE wms_go_runtime LOGIN NOSUPERUSER NOCREATEDB NOCREATEROLE IN ROLE wms_test_runtime"
subprocess.run(base + ['-d', 'postgres', '-c', roles], check=True)
subprocess.run(base + ['-d', 'postgres', '-c', 'CREATE DATABASE wms_kernel_test_backend OWNER wms_test_owner'], check=True)
command = base + ['-d', 'wms_kernel_test_backend', '-v', 'runtime_role=wms_test_runtime', '-v', 'module_role=wms_test_module', '-c', 'SET ROLE wms_test_owner']
files = sorted((root / 'db/migrations').glob('*.sql'))
files += [root / 'db/bootstrap/grants.psql', root / 'db/bootstrap/backend-grants.psql']
files += sorted((root / 'modules/dispatch-quality/migrations').glob('*.sql'))
files += [root / 'db/bootstrap/dispatch-quality-grants.psql']
for file in files:
    command += ['-f', str(file)]
subprocess.run(command, check=True, cwd=root)
