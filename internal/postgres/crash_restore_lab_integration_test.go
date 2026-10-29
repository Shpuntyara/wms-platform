//go:build integration

package postgres_test

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Run the exact laboratory SQL against the current kernel before the expensive
// Linux crash campaign. This does not claim to simulate a process crash.
func TestResilienceGeneratedUnitRespectsCurrentDocumentContract(t *testing.T) {
	adminDSN, runtimeDSN := os.Getenv("WMS_TEST_ADMIN_DSN"), os.Getenv("WMS_TEST_RUNTIME_DSN")
	if adminDSN == "" || runtimeDSN == "" {
		t.Skip("integration PostgreSQL DSNs not configured")
	}
	if err := validateTestDatabaseDSNs(adminDSN, runtimeDSN); err != nil {
		t.Fatal(err)
	}
	admin, err := pgxpool.New(t.Context(), adminDSN)
	if err != nil {
		t.Fatal(err)
	}
	defer admin.Close()
	name := "wms_kernel_test_resilience_contract_" + strings.ReplaceAll(id(), "-", "")
	if _, err := admin.Exec(t.Context(), "CREATE DATABASE "+pgx.Identifier{name}.Sanitize()+" OWNER wms_test_owner"); err != nil {
		t.Fatal(err)
	}
	defer func() {
		cleanup, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if _, err := admin.Exec(cleanup, "DROP DATABASE "+pgx.Identifier{name}.Sanitize()+" WITH (FORCE)"); err != nil {
			t.Error("owned resilience contract database cleanup failed: ", err)
		}
	}()
	root, err := filepath.Abs("../..")
	if err != nil {
		t.Fatal(err)
	}
	python := "python3"
	if runtime.GOOS == "windows" {
		python = "python"
	}
	psql, err := exec.LookPath("psql")
	if err != nil && runtime.GOOS == "windows" {
		psql = `C:\Program Files\PostgreSQL\17\bin\psql.exe`
		_, err = os.Stat(psql)
	}
	if err != nil {
		t.Fatal("resilience contract verification requires psql: ", err)
	}
	const probe = `from pathlib import Path
import copy,json,subprocess,sys,uuid
sys.path.insert(0,'scripts')
import crash_restore as lab
from migrate_core import build_script
class OwnedDatabase:
 def query(self,text,database=None,user=None):
  assert database in (None,'wms_kernel_test_resilience')
  assert user in (None,'wms_go_runtime')
  role=['-U',user] if user else []
  result=subprocess.run([sys.argv[1],'-X','-q','-At','-v','ON_ERROR_STOP=1',*role],input=text,text=True,encoding='utf8',capture_output=True)
  if result.returncode: raise AssertionError(result.stderr)
  return result.stdout.strip()
 def json(self,text,database=None,user=None): return json.loads(self.query(text,database,user))
c=OwnedDatabase()
c.query('SET ROLE wms_test_owner;'+chr(10)+build_script(Path('.')))
lab.grants(c,Path('.'),'wms_kernel_test_resilience')
c.query(lab.UNIT_FUNCTION)
canaries,records={},{}
for index,fractional in enumerate((False,True)):
 fixture=lab.seed_fixture(c,str(uuid.uuid4()),index,fractional)
 for mode in ('canary','normal','probe'):
  request=lab.unit_request(fixture,canary=mode=='canary',probe=mode=='probe')
  original=copy.deepcopy(request)
  result=c.json(lab.unit_sql(request))
  assert request==original
  assert set(result)==set(lab.STAGES[:7] if mode=='canary' else lab.STAGES)
  for _ in range(2): assert c.json(lab.unit_sql(copy.deepcopy(request)))==result
  shipment=result['shipment_create']['document_id']
  recipient=c.query('SELECT recipient FROM warehouse.documents WHERE id='+lab.sql_string(shipment)+';')
  assert recipient.strip(),'The laboratory created a shipment without a recipient'
  record=dict(unit_id=request['unit_id'],request=request,result=result)
  (canaries if mode=='canary' else records)[request['unit_id']]=record
evidence=lab.canary_checks(c,canaries)
lab.business_checks(c,records,canaries)
commands=[key for record in [*canaries.values(),*records.values()] for key in record['request']['commands']]
actual=c.json("SELECT coalesce(jsonb_object_agg(id::text,result),'{}'::jsonb) FROM platform.operations;")
assert set(actual)==set(commands),'Replays added or lost operation identities'
print(json.dumps(dict(canaries=len(canaries),normal_and_probe_units=len(records),durable_commands=len(commands),replay_stable=True,canary_checks=evidence)))
`
	command := exec.CommandContext(t.Context(), python, "-c", probe, psql)
	command.Dir = root
	command.Env = append(os.Environ(), "PGHOST="+admin.Config().ConnConfig.Host,
		fmt.Sprint("PGPORT=", admin.Config().ConnConfig.Port), "PGUSER="+admin.Config().ConnConfig.User,
		"PGPASSWORD="+admin.Config().ConnConfig.Password, "PGDATABASE="+name,
		"PGCLIENTENCODING=UTF8", "PYTHONIOENCODING=utf-8")
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("actual resilience laboratory SQL is incompatible with the current kernel: %v\n%s", err, output)
	}
	var result struct {
		Canaries        int  `json:"canaries"`
		NormalAndProbe  int  `json:"normal_and_probe_units"`
		DurableCommands int  `json:"durable_commands"`
		ReplayStable    bool `json:"replay_stable"`
	}
	if json.Unmarshal(output, &result) != nil || result.Canaries != 2 || result.NormalAndProbe != 4 || result.DurableCommands != 50 || !result.ReplayStable {
		t.Fatalf("incomplete real resilience contract evidence: %s", output)
	}
	t.Logf("Real laboratory contract: two active nonzero canaries, four normal/probe units, integer and fractional quantities, 50 commands with exact replay")
}
