#Requires -Version 7.0
[CmdletBinding()]
param(
    [Parameter(Mandatory)][ValidateSet('DryRun','Create','Start','Inspect','PinSSH','Upload','Run')][string]$Action,
    [Parameter(Mandatory)][string]$PlanPath,
    [string]$PythonPath = 'python',
    [ValidateSet('WMS-Lab-A','WMS-Lab-B')][string]$VMName,
    [string]$BaseDiskPath,
    [string]$BaseDiskSha256,
    [string]$LocalPath,
    [string]$RemotePath,
    [string]$RemoteCommand,
    [string]$InputFile
)
$ErrorActionPreference = 'Stop'
Set-StrictMode -Version Latest

function Invoke-CheckedNative {
    param([string]$File, [string[]]$Arguments)
    & $File @Arguments
    if ($LASTEXITCODE -ne 0) { throw "Native command failed: $File (exit $LASTEXITCODE)" }
}

function Assert-NoReparse {
    param([string]$Path)
    $cursor = [IO.Path]::GetFullPath($Path)
    while ($cursor) {
        if (Test-Path -LiteralPath $cursor) {
            $item = Get-Item -LiteralPath $cursor -Force
            if ($item.Attributes -band [IO.FileAttributes]::ReparsePoint) { throw "Reparse point denied: $cursor" }
        }
        $parent = [IO.Path]::GetDirectoryName($cursor)
        if ($parent -eq $cursor) { break }
        $cursor = $parent
    }
}

function Assert-ControlKey {
    param($Definition)
    Assert-NoReparse $plan.key_root
    Assert-NoReparse $Definition.private_key_path
    $identity = [Security.Principal.WindowsIdentity]::GetCurrent().User.Value
    $allowed = @($identity, 'S-1-5-18', 'S-1-5-32-544')
    foreach ($path in @($plan.key_root, $Definition.private_key_path)) {
        if (-not (Test-Path -LiteralPath $path)) { throw "Control key directory/file missing: $path" }
        $acl = Get-Acl -LiteralPath $path
        $owner = $acl.GetOwner([Security.Principal.SecurityIdentifier]).Value
        if ($owner -notin $allowed) { throw "Control key ownership does not match this user: $path" }
        foreach ($rule in $acl.Access) {
            $sid = $rule.IdentityReference.Translate([Security.Principal.SecurityIdentifier]).Value
            if ($rule.AccessControlType -eq 'Allow' -and $sid -notin $allowed) {
                throw "Control key ACL grants access outside user/SYSTEM/Administrators: $path"
            }
        }
    }
    $derived = (& ssh-keygen -y -P '' -f $Definition.private_key_path).Trim()
    if ($LASTEXITCODE -ne 0 -or (($derived -split ' ')[0..1] -join ' ') -ne (($Definition.public_key -split ' ')[0..1] -join ' ')) {
        throw 'Private control key does not match the planned public key'
    }
}

function Get-Receipt {
    param($Definition)
    Assert-NoReparse $Definition.receipt_path
    if (-not (Test-Path -LiteralPath $Definition.receipt_path)) { throw "Owned VM receipt missing for $($Definition.name)" }
    $receipt = Get-Content -LiteralPath $Definition.receipt_path -Raw | ConvertFrom-Json
    if ($receipt.lab_id -ne $plan.lab_id -or $receipt.instance_id -ne $Definition.instance_id -or $receipt.name -ne $Definition.name) {
        throw 'Receipt ownership mismatch'
    }
    return $receipt
}

function Write-Receipt {
    param($Definition, $Receipt)
    Assert-NoReparse $Definition.receipt_path
    $temporary = "$($Definition.receipt_path).$([guid]::NewGuid().ToString('N')).tmp"
    $bytes = [Text.Encoding]::UTF8.GetBytes(($Receipt | ConvertTo-Json -Depth 10) + "`n")
    $stream = [IO.File]::Open($temporary, [IO.FileMode]::CreateNew, [IO.FileAccess]::Write)
    try { $stream.Write($bytes) } finally { $stream.Dispose() }
    [IO.File]::Move($temporary, $Definition.receipt_path, $true)
}

function Get-OwnedVM {
    param($Definition)
    $receipt = Get-Receipt $Definition
    if ($receipt.status -ne 'configured') { throw 'VM configuration incomplete; rerun Create to resume the recorded UUID' }
    $vm = Get-VM -Id ([guid]$receipt.vm_id) -ErrorAction Stop
    if ($vm.Name -ne $Definition.name -or $vm.Notes -cne $Definition.ownership_note -or $vm.Generation -ne 2) {
        throw "VM identity/ownership mismatch for $($Definition.name)"
    }
    $adapters = @(Get-VMNetworkAdapter -VM $vm)
    if ($adapters.Count -ne 1 -or $adapters[0].MacAddress -cne $Definition.mac_address -or $adapters[0].SwitchName -ne $plan.switch_name) {
        throw 'VM network differs from plan'
    }
    $disks = @(Get-VMHardDiskDrive -VM $vm)
    if ($disks.Count -ne 1 -or $disks[0].Path -ne $Definition.disk_path) { throw 'VM disk differs from plan' }
    return $vm
}

function Initialize-LabVM {
    param($Definition, $VM, [string]$BaseHash, [string]$SeedHash)
    # Persist the returned identity BEFORE the first setter. A setter failure is recoverable by UUID.
    $receipt = [ordered]@{lab_id=$plan.lab_id; name=$Definition.name; instance_id=$Definition.instance_id; vm_id=$VM.Id.ToString();
        base_sha256=$BaseHash.ToLowerInvariant(); seed_sha256=$SeedHash.ToLowerInvariant(); created_at=[DateTime]::UtcNow.ToString('o'); status='created'}
    Write-Receipt $Definition $receipt
    Set-VM -VM $VM -Notes $Definition.ownership_note -AutomaticStartAction Nothing -AutomaticStopAction ShutDown -CheckpointType Disabled
    Set-VMProcessor -VM $VM -Count $Definition.cpu_count
    Set-VMMemory -VM $VM -DynamicMemoryEnabled $true -StartupBytes $Definition.memory_startup_bytes -MinimumBytes $Definition.memory_minimum_bytes -MaximumBytes $Definition.memory_maximum_bytes
    Set-VMNetworkAdapter -VM $VM -StaticMacAddress $Definition.mac_address
    Set-VMFirmware -VM $VM -EnableSecureBoot On -SecureBootTemplate $plan.secure_boot_template -FirstBootDevice (Get-VMHardDiskDrive -VM $VM)
    if ((Get-FileHash -LiteralPath $Definition.seed_path -Algorithm SHA256).Hash -ne $SeedHash) { throw 'Seed changed after content verification' }
    $dvd = @(Get-VMDvdDrive -VM $VM)
    if ($dvd.Count -gt 1 -or ($dvd.Count -eq 1 -and $dvd[0].Path -ne $Definition.seed_path)) { throw 'Unexpected DVD attached to pending VM' }
    if ($dvd.Count -eq 0) { Add-VMDvdDrive -VM $VM -Path $Definition.seed_path | Out-Null }
    $receipt.status = 'configured'
    Write-Receipt $Definition $receipt
}

function Get-PendingVM {
    param($Definition, [string]$BaseHash, [string]$SeedHash)
    $receipt = Get-Receipt $Definition
    if ($receipt.status -ne 'created' -or $receipt.base_sha256 -ne $BaseHash -or $receipt.seed_sha256 -ne $SeedHash) { throw 'Pending receipt hashes/status mismatch' }
    $vm = Get-VM -Id ([guid]$receipt.vm_id) -ErrorAction Stop
    if ($vm.Name -ne $Definition.name -or $vm.Generation -ne 2 -or $vm.State -ne 'Off' -or ($vm.Notes -and $vm.Notes -cne $Definition.ownership_note)) { throw 'Pending VM identity/state/notes mismatch' }
    $disks = @(Get-VMHardDiskDrive -VM $vm)
    $adapters = @(Get-VMNetworkAdapter -VM $vm)
    if ($disks.Count -ne 1 -or $disks[0].Path -ne $Definition.disk_path -or $adapters.Count -ne 1 -or $adapters[0].SwitchName -ne $plan.switch_name) { throw 'Pending VM storage/network mismatch' }
    $disk = Get-VHD -Path $Definition.disk_path
    if ($disk.ParentPath -or $disk.VhdFormat -ne 'VHDX' -or $disk.VhdType -ne 'Dynamic' -or $disk.Size -ne $Definition.disk_size_bytes) { throw 'Pending VM disk format mismatch' }
    return $vm
}

function Get-AttestedAddress {
    param($Definition)
    $vm = Get-OwnedVM $Definition
    $adapter = Get-VMNetworkAdapter -VM $vm
    $interface = 'vEthernet (Default Switch)'
    $addresses = @($adapter.IPAddresses | Where-Object { $_ -match '^\d{1,3}(\.\d{1,3}){3}$' -and $_ -notmatch '^(127|169\.254)\.' })
    $neighbors = @(Get-NetNeighbor -InterfaceAlias $interface -AddressFamily IPv4 -ErrorAction Stop | Where-Object {
        ($_.LinkLayerAddress -replace '[-:]','').ToUpperInvariant() -eq $Definition.mac_address -and $_.State -notin @('Unreachable','Incomplete')
    })
    if ($addresses.Count -eq 0) { $addresses = @($neighbors.IPAddress) }
    $matches = @($addresses | Sort-Object -Unique | Where-Object {
        $candidate = $_
        # Refresh neighbor evidence only for a guest-reported address; never scan a subnet.
        Test-Connection -TargetName $candidate -Count 1 -TimeoutSeconds 2 -Quiet -ErrorAction SilentlyContinue | Out-Null
        $neighbor = Get-NetNeighbor -InterfaceAlias $interface -IPAddress $candidate -ErrorAction SilentlyContinue
        @($neighbor | Where-Object { ($_.LinkLayerAddress -replace '[-:]','').ToUpperInvariant() -eq $Definition.mac_address -and $_.State -notin @('Unreachable','Incomplete') }).Count -eq 1
    })
    if ($matches.Count -ne 1) { throw "Expected one IP attested by owned VM MAC; got $($matches.Count). Wait for cloud-init/KVP and retry." }
    return [string]$matches[0]
}

function Get-SSHArguments {
    param($Definition, [string]$Address)
    Assert-ControlKey $Definition
    Assert-NoReparse $Definition.known_hosts_path
    if (-not (Test-Path -LiteralPath $Definition.known_hosts_path)) { throw 'PinSSH must succeed before SSH/upload' }
    return @('-o','BatchMode=yes','-o','IdentitiesOnly=yes','-o','StrictHostKeyChecking=yes',
             '-o',"UserKnownHostsFile=$($Definition.known_hosts_path)",'-o','GlobalKnownHostsFile=NUL',
             '-o',"HostKeyAlias=$($Definition.instance_id)",'-o','ConnectTimeout=15','-i',$Definition.private_key_path)
}

# Python validates all names, paths, collisions, resources and ownership markers before Hyper-V is loaded.
$validation = & $PythonPath (Join-Path $PSScriptRoot 'lab.py') validate --plan $PlanPath
if ($LASTEXITCODE -ne 0) { throw 'Plan validation failed' }
$plan = Get-Content -LiteralPath $PlanPath -Raw | ConvertFrom-Json
$definitions = @($plan.vms | Where-Object { -not $VMName -or $_.name -eq $VMName })
if ($Action -eq 'DryRun') {
    [ordered]@{status='dry_run'; lab_id=$plan.lab_id; switch_name=$plan.switch_name; minimum_free_bytes=$plan.minimum_free_bytes;
        vms=@($definitions | Select-Object name,instance_id,cpu_count,memory_startup_bytes,memory_maximum_bytes,disk_path,seed_path,mac_address);
        actions=@('verify standalone dynamic VHDX and hash','copy independent system disks','create owned Generation 2 VMs','enable Linux Secure Boot','start only owned VMs','attest IP by MAC','pin SSH host key')
    } | ConvertTo-Json -Depth 10
    exit 0
}
if (-not $IsWindows) { throw 'Hyper-V execution requires Windows' }
Import-Module Hyper-V -ErrorAction Stop
foreach ($path in @($plan.root, $plan.key_root)) { Assert-NoReparse $path }
if ($Action -in @('PinSSH','Run','Upload') -and $definitions.Count -ne 1) { throw 'Select exactly one VM with -VMName' }

if ($Action -eq 'Create') {
    $seedVerification = & $PythonPath (Join-Path $PSScriptRoot 'lab.py') verify-seeds --plan $PlanPath
    if ($LASTEXITCODE -ne 0) { throw 'Seed content verification failed' }
    $verifiedSeeds = ($seedVerification | ConvertFrom-Json).seeds
    if (-not $BaseDiskPath -or $BaseDiskSha256 -notmatch '^[0-9a-fA-F]{64}$') { throw 'Supply preverified -BaseDiskPath and -BaseDiskSha256' }
    Assert-NoReparse $BaseDiskPath
    if ((Get-FileHash -LiteralPath $BaseDiskPath -Algorithm SHA256).Hash -ne $BaseDiskSha256) { throw 'Base disk hash mismatch' }
    $base = Get-VHD -Path $BaseDiskPath
    if ($base.VhdFormat -ne 'VHDX' -or $base.VhdType -ne 'Dynamic' -or $base.ParentPath -or $base.Size -ne 32GB) {
        throw 'Base must be a standalone dynamic 32 GiB VHDX'
    }
    $switch = Get-VMSwitch -Name $plan.switch_name -ErrorAction Stop
    if ($switch.SwitchType -ne 'Internal') { throw 'Default Switch is not the expected existing internal NAT switch' }
    foreach ($definition in $definitions) {
        Assert-ControlKey $definition
        $seedHash = ($verifiedSeeds | Where-Object instance_id -eq $definition.instance_id).sha256
        $existing = @(Get-VM | Where-Object Name -eq $definition.name)
        if ($existing.Count -gt 0) {
            $receipt = Get-Receipt $definition
            if ($receipt.status -eq 'created') {
                $vm = Get-PendingVM $definition $BaseDiskSha256 $seedHash
                Initialize-LabVM $definition $vm $BaseDiskSha256 $seedHash
            }
            Get-OwnedVM $definition | Select-Object Name,Id,State
            continue
        }
        if (Test-Path -LiteralPath $definition.receipt_path) { throw 'Receipt exists but its VM is absent; manual investigation required' }
        $drive = [IO.DriveInfo]::new([IO.Path]::GetPathRoot($plan.root))
        if ($drive.AvailableFreeSpace -lt [long]$plan.minimum_free_bytes) { throw 'At least 25 GiB free host disk space is required' }
        if (-not (Test-Path -LiteralPath $definition.directory)) { throw 'Generate NoCloud seed media before Create' }
        Assert-NoReparse $definition.directory
        $owner = (Get-Acl -LiteralPath $definition.directory).GetOwner([Security.Principal.SecurityIdentifier]).Value
        if ($owner -ne [Security.Principal.WindowsIdentity]::GetCurrent().User.Value) { throw 'VM directory is not owned by the current user' }
        $unexpected = @(Get-ChildItem -LiteralPath $definition.directory -Force | Where-Object Name -ne 'seed.iso')
        if ($unexpected.Count -or -not (Test-Path -LiteralPath $definition.seed_path -PathType Leaf)) { throw 'VM directory is not pristine (only seed.iso may exist)' }
        $macCollision = @(Get-VM | Get-VMNetworkAdapter | Where-Object MacAddress -eq $definition.mac_address)
        if ($macCollision.Count) { throw 'Planned MAC is already in use' }
        # Copy-Item without -Force still overwrites normal files. CreateNew is intentionally exclusive.
        $source = [IO.File]::OpenRead($BaseDiskPath)
        try {
            $destination = [IO.File]::Open($definition.disk_path, [IO.FileMode]::CreateNew, [IO.FileAccess]::Write)
            try { $source.CopyTo($destination) } finally { $destination.Dispose() }
        } finally { $source.Dispose() }
        $vm = New-VM -Name $definition.name -Generation 2 -MemoryStartupBytes $definition.memory_startup_bytes -VHDPath $definition.disk_path -Path $definition.directory -SwitchName $plan.switch_name
        # Setters receive the newly returned object, or a strictly UUID-verified pending object.
        Initialize-LabVM $definition $vm $BaseDiskSha256 $seedHash
        Get-OwnedVM $definition | Select-Object Name,Id,State
    }
    exit 0
}

foreach ($definition in $definitions) {
    $vm = Get-OwnedVM $definition
    switch ($Action) {
        'Start' {
            if ($vm.State -eq 'Off') { Start-VM -VM $vm | Out-Null }
            elseif ($vm.State -ne 'Running') { throw "VM is in unsupported state $($vm.State)" }
            Get-VM -Id $vm.Id | Select-Object Name,Id,State
        }
        'Inspect' {
            $address = Get-AttestedAddress $definition
            [ordered]@{name=$vm.Name; vm_id=$vm.Id.ToString(); state=$vm.State.ToString(); ip_address=$address;
                mac_address=$definition.mac_address; instance_id=$definition.instance_id} | ConvertTo-Json
        }
        'PinSSH' {
            Assert-ControlKey $definition
            Assert-NoReparse $definition.known_hosts_path
            $address = Get-AttestedAddress $definition
            if (Test-Path -LiteralPath $definition.known_hosts_path) {
                $sshArguments = Get-SSHArguments $definition $address
                Invoke-CheckedNative 'ssh' ($sshArguments + @("wmsadmin@$address",'true'))
                Write-Output '{"status":"existing_pin_verified"}'
                continue
            }
            $scan = @(& ssh-keyscan -T 15 -t ed25519 $address 2>$null | Where-Object { $_ -match '^[^#]\S+ ssh-ed25519 [A-Za-z0-9+/=]+$' })
            if ($LASTEXITCODE -ne 0 -or $scan.Count -ne 1) { throw 'Expected exactly one SSH ed25519 host key from the MAC-attested guest' }
            if ((Get-AttestedAddress $definition) -ne $address) { throw 'Guest endpoint changed while enrolling host key' }
            $fields = $scan[0] -split ' '
            $line = "$($definition.instance_id) $($fields[1]) $($fields[2])`n"
            $stream = [IO.File]::Open($definition.known_hosts_path, [IO.FileMode]::CreateNew, [IO.FileAccess]::Write)
            try { $stream.Write([Text.Encoding]::ASCII.GetBytes($line)) } finally { $stream.Dispose() }
            $sshArguments = Get-SSHArguments $definition $address
            Invoke-CheckedNative 'ssh' ($sshArguments + @("wmsadmin@$address",'true'))
            $receipt = Get-Receipt $definition
            $receipt | Add-Member -Force NoteProperty ip_address $address
            $receipt | Add-Member -Force NoteProperty known_hosts_sha256 ((Get-FileHash -LiteralPath $definition.known_hosts_path -Algorithm SHA256).Hash.ToLowerInvariant())
            $receipt | Add-Member -Force NoteProperty host_key_enrolled_at ([DateTime]::UtcNow.ToString('o'))
            Write-Receipt $definition $receipt
            [ordered]@{status='host_key_pinned'; name=$definition.name; ip_address=$address; instance_id=$definition.instance_id} | ConvertTo-Json
        }
        'Upload' {
            if (-not (Test-Path -LiteralPath $LocalPath -PathType Leaf) -or $RemotePath -notmatch '^/home/wmsadmin/[A-Za-z0-9._/-]+$' -or $RemotePath.Contains('..')) { throw 'Upload a regular file to a safe absolute wmsadmin home path' }
            $address = Get-AttestedAddress $definition
            $sshArguments = Get-SSHArguments $definition $address
            Invoke-CheckedNative 'scp' ($sshArguments + @($LocalPath,"wmsadmin@${address}:$RemotePath"))
        }
        'Run' {
            if (-not $RemoteCommand) { throw '-RemoteCommand is required' }
            $address = Get-AttestedAddress $definition
            $sshArguments = Get-SSHArguments $definition $address
            if ($InputFile) {
                # Binary stdin supports secret files without putting their contents in argv/logs.
                $start = [Diagnostics.ProcessStartInfo]::new('ssh')
                foreach ($argument in ($sshArguments + @("wmsadmin@$address",$RemoteCommand))) { $start.ArgumentList.Add($argument) }
                $start.UseShellExecute = $false
                $start.RedirectStandardInput = $true
                $process = [Diagnostics.Process]::Start($start)
                $inputStream = [IO.File]::OpenRead($InputFile)
                try { $inputStream.CopyTo($process.StandardInput.BaseStream) } finally { $inputStream.Dispose(); $process.StandardInput.Close() }
                $process.WaitForExit()
                if ($process.ExitCode -ne 0) { throw "SSH command failed (exit $($process.ExitCode))" }
            } else {
                Invoke-CheckedNative 'ssh' ($sshArguments + @("wmsadmin@$address",$RemoteCommand))
            }
        }
    }
}
