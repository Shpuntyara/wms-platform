"""Offline safety tests; never require or mutate Hyper-V."""
import importlib.util
import json
from pathlib import Path
import sys
import shutil
import subprocess
import tempfile
import unittest
from unittest import mock
import uuid

MODULE_PATH = Path(__file__).parents[1] / "deploy/hyperv/lab.py"
spec = importlib.util.spec_from_file_location("hyperv_lab", MODULE_PATH)
lab = importlib.util.module_from_spec(spec)
sys.modules[spec.name] = lab
spec.loader.exec_module(lab)

PUBLIC_A = "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA vm-a"
PUBLIC_B = "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIAEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEB vm-b"


class BootstrapTests(unittest.TestCase):
    @classmethod
    def setUpClass(cls):
        if not shutil.which("pwsh"):
            raise RuntimeError("Required PowerShell 7 (pwsh) was not found; tests cannot be skipped")

    def plan(self, **kwargs):
        values = dict(root=r"C:\Users\owner\Documents\WMS\lab-vms",
                      key_root=r"C:\Users\owner\Documents\WMS\lab-control",
                      repository=r"C:\Users\owner\Documents\WMS\wms-platform",
                      public_keys=[PUBLIC_A, PUBLIC_B], switch="Default Switch")
        values.update(kwargs)
        return lab.make_plan(**values)

    def test_plan_separates_disks_keys_instances_and_macs(self):
        plan = self.plan()
        self.assertEqual([vm["name"] for vm in plan["vms"]], ["WMS-Lab-A", "WMS-Lab-B"])
        for field in ["disk_path", "seed_path", "private_key_path", "instance_id", "mac_address"]:
            self.assertEqual(len({vm[field] for vm in plan["vms"]}), 2)
        for vm in plan["vms"]:
            uuid.UUID(vm["instance_id"])
            self.assertEqual(vm["disk_size_bytes"], 32 * 1024**3)
        self.assertNotIn("PRIVATE KEY", json.dumps(plan))

    def test_rejects_dangerous_roots_and_key_collisions(self):
        for root in ["C:\\", r"C:\Windows", r"C:\Users\owner", r"C:\Users\owner\Documents\WMS\wms-platform", r"C:\safe\..\Windows", r"\\host\share\lab"]:
            with self.subTest(root=root), self.assertRaises(ValueError):
                self.plan(root=root)
        with self.assertRaises(ValueError):
            self.plan(public_keys=[PUBLIC_A, PUBLIC_A])
        with self.assertRaises(ValueError):
            self.plan(key_root=r"C:\Users\owner\Documents\WMS\lab-vms\keys")

    def test_manifest_tampering_is_rejected(self):
        plan = self.plan()
        plan["vms"][1]["disk_path"] = plan["vms"][0]["disk_path"]
        with self.assertRaises(ValueError):
            lab.validate_plan(plan)
        plan = self.plan()
        plan["vms"][0]["name"] = "DC01"
        with self.assertRaises(ValueError):
            lab.validate_plan(plan)

    def test_nocloud_is_public_key_only(self):
        plan = self.plan()
        user_data, meta_data = lab.cloud_config(plan["vms"][0])
        self.assertTrue(user_data.startswith("#cloud-config\n"))
        for required in ["wmsadmin", "NOPASSWD:ALL", "ssh_pwauth: false", "lock_passwd: true", "hyperv-daemons", "python3", PUBLIC_A]:
            self.assertIn(required, user_data)
        for forbidden in ["PRIVATE KEY", "password:", "passwd:", "chpasswd:"]:
            if forbidden != "passwd:":
                self.assertNotIn(forbidden, user_data)
        self.assertIn(plan["vms"][0]["instance_id"], meta_data)

    def test_checksum_parser_fails_closed(self):
        digest = "a" * 64
        self.assertEqual(lab.checksum_for(f"{digest} *image.img\n", "image.img"), digest)
        for sums in ["", f"{digest} *other.img", f"{digest} *image.img\n{'b' * 64} *image.img"]:
            with self.assertRaises(ValueError):
                lab.checksum_for(sums, "image.img")

    def test_archive_paths_fail_before_extraction(self):
        for name in ["../escape", "/absolute", "C:/Windows/payload", "ok/../../escape", "ok\\..\\escape", "CON", "normal:stream"]:
            with self.subTest(name=name), self.assertRaises(ValueError):
                lab.safe_archive_member(name)
        self.assertEqual(lab.safe_archive_member("bin/qemu-img.exe"), "bin/qemu-img.exe")

    def test_exclusive_write_never_replaces_existing_file(self):
        with tempfile.TemporaryDirectory() as directory:
            path = Path(directory) / "plan.json"
            lab.write_new(path, "first")
            with self.assertRaises(FileExistsError):
                lab.write_new(path, "second")
            self.assertEqual(path.read_text(), "first")

    def test_reparse_ancestor_rejected_before_seed_write(self):
        with tempfile.TemporaryDirectory() as directory:
            target = Path(directory) / "seed.iso"
            info = mock.Mock(st_mode=0, st_file_attributes=0x400)
            with mock.patch.object(Path, "lstat", return_value=info):
                with self.assertRaises(ValueError):
                    lab.assert_no_reparse(target)
            self.assertFalse(target.exists())

    def test_actual_nocloud_iso_roundtrip_and_exclusive_creation(self):
        import pycdlib
        with tempfile.TemporaryDirectory() as directory:
            vm = self.plan()["vms"][0]
            vm["seed_path"] = str(Path(directory) / "seed.iso")
            lab.create_seed(vm)
            iso = pycdlib.PyCdlib()
            iso.open(vm["seed_path"])
            import io
            for name, expected in zip(("user-data", "meta-data"), lab.cloud_config(vm)):
                contents = io.BytesIO()
                iso.get_file_from_iso_fp(contents, rr_path="/" + name)
                self.assertEqual(contents.getvalue().decode(), expected)
            self.assertEqual(iso.pvd.volume_identifier.decode().strip(), "CIDATA")
            iso.close()
            previous = Path(vm["seed_path"]).read_bytes()
            with self.assertRaises(FileExistsError):
                lab.create_seed(vm)
            self.assertEqual(Path(vm["seed_path"]).read_bytes(), previous)

    def test_seed_verification_rejects_attacker_key_or_instance(self):
        with tempfile.TemporaryDirectory() as directory:
            vm = self.plan()["vms"][0]
            vm["seed_path"] = str(Path(directory) / "seed.iso")
            attacker = dict(vm, public_key=PUBLIC_B, instance_id=str(uuid.uuid4()))
            lab.create_seed(attacker)
            with self.assertRaises(ValueError):
                lab.verify_seed(vm)
            result = lab.verify_seed(attacker)
            self.assertEqual(result["instance_id"], attacker["instance_id"])
            self.assertRegex(result["sha256"], r"^[a-f0-9]{64}$")

    def test_creation_receipt_survives_notes_setter_failure(self):
        with tempfile.TemporaryDirectory() as directory:
            script = Path(directory) / "test.ps1"
            script.write_text('''param($Driver)
$ErrorActionPreference='Stop'
$ast=[System.Management.Automation.Language.Parser]::ParseFile($Driver,[ref]$null,[ref]$null)
$node=$ast.FindAll({param($n) $n -is [System.Management.Automation.Language.FunctionDefinitionAst] -and $n.Name -eq 'Initialize-LabVM'},$true)[0]
if(-not $node) { throw 'Initialize-LabVM missing' }
. ([scriptblock]::Create($node.Extent.Text))
$plan=@{lab_id='lab'}
$definition=@{name='WMS-Lab-A';instance_id='instance';ownership_note='owned'}
$vm=@{Id=[guid]'7a7b26ab-0c95-4b9e-a116-43b93a1617a2'}
$global:events=[Collections.Generic.List[string]]::new()
function Write-Receipt { param($Definition,$Receipt) $global:saved=$Receipt | ConvertTo-Json -Depth 10 | ConvertFrom-Json; $global:events.Add('receipt') }
function Set-VM { $global:events.Add('notes'); throw 'simulated notes setter failure' }
try { Initialize-LabVM $definition $vm ('a'*64) ('b'*64); throw 'Unexpected success' }
catch { if($_.Exception.Message -ne 'simulated notes setter failure') { throw } }
@{events=@($global:events);receipt=$global:saved} | ConvertTo-Json -Depth 10
''', encoding="utf-8")
            result = subprocess.run([shutil.which("pwsh"), "-NoProfile", "-File", str(script),
                str(MODULE_PATH.with_name("lab.ps1"))], text=True, capture_output=True)
            self.assertEqual(result.returncode, 0, result.stderr)
            result = json.loads(result.stdout)
            self.assertEqual(result["events"], ["receipt", "notes"])
            self.assertEqual(result["receipt"]["vm_id"], "7a7b26ab-0c95-4b9e-a116-43b93a1617a2")
            self.assertEqual(result["receipt"]["status"], "created")

    def test_owned_vm_lookup_uses_receipt_id_and_rejects_foreign_notes(self):
        with tempfile.TemporaryDirectory() as directory:
            plan = self.plan()
            vm = plan["vms"][0]
            vm["receipt_path"] = str(Path(directory) / "receipt.json")
            receipt_id = str(uuid.uuid4())
            Path(vm["receipt_path"]).write_text(json.dumps(dict(lab_id=plan["lab_id"],
                name=vm["name"], instance_id=vm["instance_id"], vm_id=receipt_id, status="configured")))
            fixture = Path(directory) / "fixture.json"
            fixture.write_text(json.dumps(plan))
            script = Path(directory) / "test.ps1"
            script.write_text('''param($Driver,$Fixture)
$ErrorActionPreference='Stop'
$ast=[System.Management.Automation.Language.Parser]::ParseFile($Driver,[ref]$null,[ref]$null)
foreach($name in @('Assert-NoReparse','Get-Receipt','Get-OwnedVM')) {
  $node=$ast.FindAll({param($n) $n -is [System.Management.Automation.Language.FunctionDefinitionAst] -and $n.Name -eq $name},$true)[0]
  . ([scriptblock]::Create($node.Extent.Text))
}
$plan=Get-Content -Raw -LiteralPath $Fixture | ConvertFrom-Json
$definition=$plan.vms[0]
function Get-VM { param([guid]$Id) $global:calledId=$Id.ToString(); [pscustomobject]@{Name=$definition.name;Notes='someone-else';Generation=2} }
function Get-VMNetworkAdapter { throw 'Foreign VM inspection must not continue' }
try { Get-OwnedVM $definition; throw 'Unexpected acceptance' }
catch { if($_.Exception.Message -notmatch 'ownership mismatch') { throw }; @{status='denied';called_id=$global:calledId} | ConvertTo-Json }
''', encoding="utf-8")
            result = subprocess.run([shutil.which("pwsh"), "-NoProfile", "-File", str(script),
                str(MODULE_PATH.with_name("lab.ps1")), str(fixture)], text=True, capture_output=True)
            self.assertEqual(result.returncode, 0, result.stderr)
            self.assertEqual(json.loads(result.stdout), dict(status="denied", called_id=receipt_id))

    def test_powershell_dryrun_is_public_and_does_not_require_hyperv(self):
        with tempfile.TemporaryDirectory() as directory:
            path = Path(directory) / "plan.json"
            path.write_text(json.dumps(self.plan()), encoding="utf-8")
            command = [shutil.which("pwsh"), "-NoProfile", "-File", str(MODULE_PATH.with_name("lab.ps1")),
                       "-Action", "DryRun", "-PlanPath", str(path), "-PythonPath", sys.executable]
            result = subprocess.run(command, text=True, capture_output=True)
            self.assertEqual(result.returncode, 0, result.stderr)
            output = json.loads(result.stdout)
            self.assertEqual(output["status"], "dry_run")
            self.assertEqual(len(output["vms"]), 2)
            self.assertNotIn("PRIVATE KEY", result.stdout)


if __name__ == "__main__":
    unittest.main()
