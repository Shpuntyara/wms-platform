"""Provision only the disposable local integration database; never a production DB."""
import argparse
import subprocess
from pathlib import Path
from migrate_core import build_script

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
core = base + ['-d', 'wms_kernel_test_backend']
prefix = 'SET ROLE wms_test_owner;\n'
subprocess.run(core, input=(prefix + build_script(root, through='0007')).encode('utf8'), check=True)
probe = core + ['-At', '-c', "SELECT 'platform.check_declarative_guard(text,text,uuid,uuid)'::regprocedure::oid"]
before = subprocess.check_output(probe).strip()
for _ in range(2):
    subprocess.run(core, input=(prefix + build_script(root)).encode('utf8'), check=True)
    if subprocess.check_output(probe).strip() != before:
        raise RuntimeError('Online upgrade changed the guard function identity')
files = [root / 'db/bootstrap/grants.psql', root / 'db/bootstrap/backend-grants.psql']
files += [root / 'db/bootstrap/industry-backend-grants.psql']
files += [root / 'db/bootstrap/handoffs-backend-grants.psql']
files += sorted((root / 'modules/dispatch-quality/migrations').glob('*.sql'))
files += [root / 'db/bootstrap/dispatch-quality-grants.psql']
for file in files:
    command += ['-f', str(file)]
subprocess.run(command, check=True, cwd=root)
