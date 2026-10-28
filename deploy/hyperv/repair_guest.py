"""Repair only Ubuntu 24.04 Hyper-V KVP packages/service, preserving guest identity."""
import argparse
import hashlib
import json
import os
from pathlib import Path
import platform
import re
import subprocess
import uuid


def commands_for(kernel):
    if not re.fullmatch(r"[0-9]+\.[0-9]+\.[0-9]+-[0-9]+-[a-z0-9-]+", kernel):
        raise ValueError("Unexpected Ubuntu kernel name")
    packages = ["linux-cloud-tools-common", "linux-cloud-tools-" + kernel]
    return packages, [
        ["apt-get", "update"],
        ["apt-get", "-o", "DPkg::Lock::Timeout=180", "install", "-y", "--no-install-recommends", *packages],
        ["udevadm", "control", "--reload-rules"],
        ["udevadm", "trigger", "--action=add", "--subsystem-match=misc", "--sysname-match=vmbus!hv_kvp"],
        ["udevadm", "settle", "--timeout=30"],
        ["systemctl", "daemon-reload"],
        ["systemctl", "enable", "--now", "hv-kvp-daemon.service"],
        ["systemctl", "is-active", "--quiet", "hv-kvp-daemon.service"],
    ]


def repair_guest(runner, snapshotter, kernel, expected_instance):
    expected_instance = str(uuid.UUID(expected_instance))
    before = snapshotter()
    if before["instance_id"] != expected_instance:
        raise RuntimeError("Guest instance identity does not match the approved target")
    packages, commands = commands_for(kernel)
    for command in commands:
        runner(command)
    for package in packages:
        if runner(["dpkg-query", "-W", "-f", "${db:Status-Status}", package]).strip() != "installed":
            raise RuntimeError("Required kernel-matched KVP package is not installed")
    after = snapshotter()
    if after["instance_id"] != before["instance_id"] or after["ssh_host_key_sha256"] != before["ssh_host_key_sha256"]:
        raise RuntimeError("Guest identity changed during the targeted repair")
    return dict(status="repaired", instance_id=expected_instance, kernel=kernel, packages=packages,
                before=before, after=after, identity_preserved=True, kvp_service_active=True)


def run(command):
    # Do not impose a timeout on apt/dpkg: killing package configuration can corrupt its state.
    result = subprocess.run(command, text=True, capture_output=True)
    if result.returncode:
        raise RuntimeError("KVP repair command failed: " + command[0])
    return result.stdout.strip()


def snapshot():
    status = subprocess.run(["cloud-init", "status", "--long"], text=True, capture_output=True)
    service = subprocess.run(["systemctl", "is-active", "hv-kvp-daemon.service"], text=True, capture_output=True)
    keys = {key.name: hashlib.sha256(key.read_bytes()).hexdigest() for key in sorted(Path("/etc/ssh").glob("ssh_host_*_key.pub"))}
    if not keys:
        raise RuntimeError("Existing SSH host public keys are required before a repair")
    return dict(instance_id=Path("/var/lib/cloud/data/instance-id").read_text().strip(), ssh_host_key_sha256=keys,
                cloud_init_status=dict(returncode=status.returncode, status=status.stdout.strip()), kvp_service=service.stdout.strip())


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--instance-id", required=True)
    parser.add_argument("--inspect-only", action="store_true")
    parser.add_argument("--output", type=Path)
    args = parser.parse_args()
    if os.name != "posix" or os.geteuid() != 0:
        parser.error("Run as root inside the target Ubuntu laboratory guest")
    release = Path("/etc/os-release").read_text()
    if 'ID=ubuntu' not in release or 'VERSION_ID="24.04"' not in release or run(["systemd-detect-virt"]) != "microsoft":
        raise RuntimeError("Only the Ubuntu 24.04 Hyper-V laboratory target is supported")
    if args.inspect_only:
        receipt = snapshot()
        if receipt["instance_id"] != str(uuid.UUID(args.instance_id)):
            raise RuntimeError("Guest instance identity does not match the approved target")
    else:
        receipt = repair_guest(run, snapshot, platform.release(), args.instance_id)
    if args.output:
        descriptor = os.open(args.output, os.O_CREAT | os.O_EXCL | os.O_WRONLY, 0o600)
        with os.fdopen(descriptor, "w") as output:
            json.dump(receipt, output, indent=2)
            output.write("\n")
    print(json.dumps(receipt))


if __name__ == "__main__":
    main()
