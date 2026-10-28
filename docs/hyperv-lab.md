# Two independent Hyper-V WMS laboratories

`deploy/hyperv/lab.py` prepares a public plan and one NoCloud ISO per VM.
`deploy/hyperv/lab.ps1` creates, starts, discovers and accesses only the two VM
identities recorded by that plan and their receipts. The helpers do not modify
existing switches, host routes, firewall rules or other VMs, and contain no
automatic removal or host reboot operation.

| Setting | WMS-Lab-A | WMS-Lab-B |
| --- | --- | --- |
| CPU | 2 virtual processors | 2 virtual processors |
| RAM | 3 GiB startup, 1–4 GiB dynamic | 3 GiB startup, 1–6 GiB dynamic |
| Disk | independent dynamic 32 GiB VHDX | independent dynamic 32 GiB VHDX |
| Firmware | Generation 2, Linux Secure Boot | Generation 2, Linux Secure Boot |
| Network | existing Default Switch, DHCP | existing Default Switch, DHCP |
| Guest access | separate ed25519 control key | separate ed25519 control key |

The disk capacity is virtual. Monitor actual host free space while exercising
installations and upgrades; creation requires at least 25 GiB available before
each VM. VM directories and control keys must be outside the Git repository and
separate from one another. Existing files, foreign VM names, ownership markers,
reparse points, nonmatching disks, keys or MAC addresses fail closed.

## Verified image and conversion tool

Use Canonical's immutable Ubuntu 24.04 LTS cloud image directory:

- [Release 20261017 image](https://cloud-images.ubuntu.com/releases/noble/release-20261017/ubuntu-24.04-server-cloudimg-amd64.img)
- [Official SHA256SUMS](https://cloud-images.ubuntu.com/releases/noble/release-20261017/SHA256SUMS)
- [Checksum signature](https://cloud-images.ubuntu.com/releases/noble/release-20261017/SHA256SUMS.gpg)

The image SHA256 published in that release is
`04e8f2461a16a44e78a370284471f29616cdd9baf1893df25807b71f985b6c94`.
Retain the original checksum document and source URL with the conversion receipt.
HTTPS plus this checksum establishes download integrity; signature verification
against a separately trusted Canonical signing key adds signer authentication.
Do not describe a checksum alone as a verified signature.

[QEMU's official download page](https://www.qemu.org/download/#windows) links
Stefan Weil's Windows distributions. The 20260811 package is available as
[installer](https://qemu.weilnetz.de/w64/qemu-w64-setup-20260811.exe) and
[publisher SHA512](https://qemu.weilnetz.de/w64/qemu-w64-setup-20260811.sha512).
Its SHA512 is
`5bcf9eed634e8575a37b74f445af41a2fe4106da512d0c30c368301d4c105037fdfab40a5287367a28a957624cddebbc8c07e16c88ab6634f554cdf3d16bf543`.
Verify before extracting with an already trusted archive tool. Do not execute an
unverified installer or introduce a global package installation just to convert a
disk. Reject archive traversal, absolute paths, Windows device names, alternate
data streams, and symlink/reparse entries before extraction.

Run the checked converter locally, with paths chosen outside Git:

```powershell
python deploy/hyperv/lab.py verify-image --image $imagePath --sums $sumsPath
& $qemuImgPath info --output=json $imagePath
& $qemuImgPath convert -f qcow2 -O vhdx -o subformat=dynamic $imagePath $baseDiskPath
# Check every native exit code before continuing.
Resize-VHD -Path $baseDiskPath -SizeBytes 32GB
$baseHash = (Get-FileHash -LiteralPath $baseDiskPath -Algorithm SHA256).Hash
Get-VHD -Path $baseDiskPath
```

The base must have no parent and be a dynamic 32 GiB VHDX. The driver makes two
exclusive, full copies; neither VM shares a writable backing disk. Preserve the
source image hash, tool hash/version, conversion commands, resulting base hash
and UTC timestamp as public provenance. Native commands must fail the operation
on a nonzero exit code.

Microsoft documents [Ubuntu 24.04 support and built-in integration services](https://learn.microsoft.com/en-us/windows-server/virtualization/hyper-v/supported-ubuntu-virtual-machines-on-hyper-v).
The driver enables Secure Boot using `MicrosoftUEFICertificateAuthority`.
Do not disable Secure Boot as a boot-failure workaround.

## Prepare control keys, public plan and NoCloud media

Create a dedicated key directory, owned by the current Windows account, with
inheritance removed and access restricted to that account (optionally SYSTEM and
local Administrators). Generate one ed25519 key per VM with Windows OpenSSH;
keep the private keys in that directory, outside Git. The required filenames are
`WMS-Lab-A_ed25519` and `WMS-Lab-B_ed25519`. Use the resulting `.pub` files as
inputs. `Create`, `PinSSH`, `Upload` and `Run` verify key ACLs and that each private
key matches the public key in the plan. The noninteractive helper expects
unencumbered keys whose protection is the restricted local ACL.

```powershell
$vmRoot = 'C:\Users\sashk\Documents\WMS\lab-vms'
$keyRoot = 'C:\Users\sashk\Documents\WMS\lab-control'
$planPath = 'C:\Users\sashk\Documents\WMS\lab-plan.json'
python deploy/hyperv/lab.py plan --root $vmRoot --key-root $keyRoot `
  --public-key-a "$keyRoot\WMS-Lab-A_ed25519.pub" `
  --public-key-b "$keyRoot\WMS-Lab-B_ed25519.pub" --output $planPath

# Install the pinned media dependency into a task-local directory, not the global Python.
uv pip install --target work/hyperv-bootstrap-deps -r deploy/hyperv/requirements.txt
$env:PYTHONPATH = (Join-Path (Get-Location) 'work/hyperv-bootstrap-deps')
python deploy/hyperv/lab.py seed --plan $planPath
pwsh -NoProfile -File deploy/hyperv/lab.ps1 -Action DryRun -PlanPath $planPath
```

`plan` and `seed` create files exclusively and refuse to overwrite them. Reuse the
same plan for the lab lifetime. Each ISO has the `CIDATA` label and exact
`user-data`/`meta-data` filenames prescribed by [cloud-init NoCloud](https://docs.cloud-init.io/en/latest/reference/datasources/nocloud.html).
It contains the VM's unique instance ID and public SSH key, creates `wmsadmin`
with passwordless sudo, locks its password, disables password SSH/root login,
and installs Python, CA certificates, OpenSSH and Hyper-V guest daemons.
No application password, private key, deployment secret or service credential
belongs in the ISO, public plan or receipt.

## Create and connect

```powershell
pwsh -NoProfile -File deploy/hyperv/lab.ps1 -Action Create -PlanPath $planPath `
  -BaseDiskPath $baseDiskPath -BaseDiskSha256 $baseHash
pwsh -NoProfile -File deploy/hyperv/lab.ps1 -Action Start -PlanPath $planPath
pwsh -NoProfile -File deploy/hyperv/lab.ps1 -Action Inspect -PlanPath $planPath
pwsh -NoProfile -File deploy/hyperv/lab.ps1 -Action PinSSH -PlanPath $planPath -VMName WMS-Lab-A
pwsh -NoProfile -File deploy/hyperv/lab.ps1 -Action PinSSH -PlanPath $planPath -VMName WMS-Lab-B
```

If Hyper-V denies creation, report the exact missing permission and stop; never
alter UAC, group membership or security policy to bypass the denial. If creation
fails partway, inspect the new VM and its dedicated files before recovery. The
driver deliberately refuses to adopt an unreceipted VM or overwrite partial
media. `Create` returns an existing VM only when receipt UUID, name, ownership
note, disk, switch and MAC agree. `Start` and all later commands look up that
receipt UUID before operating on the VM.

Allow the guest time for DHCP and cloud-init. `Inspect` obtains guest addresses
from Hyper-V KVP (or a matching existing neighbor), then requires one address
whose neighbor MAC agrees with the owned VM adapter on the Default Switch.
`PinSSH` scans only that attested endpoint and saves one ed25519 host key under
the unique instance ID alias. This is initial enrollment based on the local
Hyper-V/MAC trust boundary; it is not a cryptographic guest attestation.
Subsequent SSH and SCP always use strict checking and the per-VM known-host file;
an existing pin is verified and never replaced automatically.

```powershell
pwsh -NoProfile -File deploy/hyperv/lab.ps1 -Action Upload -PlanPath $planPath `
  -VMName WMS-Lab-A -LocalPath $installerArchive -RemotePath /home/wmsadmin/installer.tar.gz
pwsh -NoProfile -File deploy/hyperv/lab.ps1 -Action Run -PlanPath $planPath `
  -VMName WMS-Lab-A -RemoteCommand 'sudo cloud-init status --wait'
```

`Run -InputFile <protected-local-file>` streams binary stdin to the remote
command without embedding file contents in process arguments. Use it to deliver
each guest's separate deployment secrets to a restrictive destination. Keep
secret values out of remote commands and never print them. Run the installer
locally in each guest; no third controller VM is required.

## Verification

```powershell
$env:PYTHONPATH = (Join-Path (Get-Location) 'work/hyperv-bootstrap-deps')
python -m unittest discover -s tests -p test_hyperv_bootstrap.py
```

The offline suite covers manifest/path/key collisions, bounded VM names and
resources, checksum parsing, malicious archive paths, exclusive file writes,
NoCloud ISO roundtrip, reparse rejection, PowerShell dry run, and a mocked
foreign-VM ownership rejection. It never creates or mutates a VM. Actual VM boot,
guest network, SSH, installer, upgrade, monitoring and application verification
must be collected separately; a passing offline test is not evidence of those
operations succeeding.
