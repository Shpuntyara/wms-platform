"""Prepare public, isolated Hyper-V lab plans and NoCloud media.

This program never creates a VM, downloads executables, or reads private keys.
"""
import argparse
import base64
import hashlib
import io
import json
import ntpath
from pathlib import Path
import re
import stat
import uuid

GIB = 1024**3
NAMES = ("WMS-Lab-A", "WMS-Lab-B")
REPOSITORY = str(Path(__file__).resolve().parents[2])


def windows_path(value):
    value = str(value).replace("/", "\\")
    drive, tail = ntpath.splitdrive(value)
    parts = tail.strip("\\").split("\\")
    if not re.fullmatch(r"[A-Za-z]:", drive) or not tail.startswith("\\"):
        raise ValueError("A local absolute Windows path is required")
    if any(p in ("", ".", "..") or p.endswith((".", " ")) or ":" in p for p in parts):
        raise ValueError("Noncanonical path")
    return ntpath.normpath(value)


def under(path, parent):
    return ntpath.commonpath([path.lower(), parent.lower()]) == parent.lower()


def validate_roots(root, key_root, repository):
    root, key_root, repository = map(windows_path, (root, key_root, repository))
    for path in (root, key_root):
        parts = Path(path.replace("\\", "/")).parts
        if len(parts) < 5 or any(p.lower() in ("windows", "program files", "programdata") for p in parts):
            raise ValueError("Use a dedicated lab directory under Documents")
        if under(path, repository) or under(repository, path):
            raise ValueError("Lab media and control keys must be outside the repository")
    if under(root, key_root) or under(key_root, root):
        raise ValueError("Keep the private key directory separate from VM disks")
    return root, key_root, repository


def public_key(value):
    fields = value.strip().split()
    if len(fields) not in (2, 3) or fields[0] != "ssh-ed25519":
        raise ValueError("Exactly one OpenSSH ed25519 public key is required")
    try:
        blob = base64.b64decode(fields[1], validate=True)
    except ValueError as exc:
        raise ValueError("Invalid public key") from exc
    if len(blob) != 51 or blob[:19] != b"\x00\x00\x00\x0bssh-ed25519\x00\x00\x00\x20":
        raise ValueError("Invalid ed25519 public key encoding")
    if any(c in value for c in "\r\n"):
        raise ValueError("Public key must be a single line")
    return " ".join(fields)


def make_plan(*, root, key_root, repository=REPOSITORY, public_keys, switch="Default Switch"):
    root, key_root, repository = validate_roots(root, key_root, repository)
    if switch != "Default Switch":
        raise ValueError("This lab uses the existing Default Switch only")
    if len(public_keys) != 2:
        raise ValueError("Two separate control public keys are required")
    keys = [public_key(key) for key in public_keys]
    if keys[0].split()[1] == keys[1].split()[1]:
        raise ValueError("Each VM requires its own control key")
    lab_id = str(uuid.uuid4())
    vms = []
    for index, (name, key) in enumerate(zip(NAMES, keys)):
        instance_id = str(uuid.uuid4())
        directory = ntpath.join(root, name)
        vms.append(dict(name=name, hostname=name.lower(), instance_id=instance_id,
                        ownership_note=f"wms-lab:{lab_id}:{instance_id}",
                        directory=directory, disk_path=ntpath.join(directory, "system.vhdx"),
                        seed_path=ntpath.join(directory, "seed.iso"),
                        receipt_path=ntpath.join(directory, "receipt.json"),
                        private_key_path=ntpath.join(key_root, name + "_ed25519"),
                        known_hosts_path=ntpath.join(key_root, name + "_known_hosts"),
                        public_key=key, mac_address="00155D" + uuid.UUID(instance_id).hex[:6].upper(),
                        cpu_count=2, memory_startup_bytes=3 * GIB, memory_minimum_bytes=GIB,
                        memory_maximum_bytes=(4 if index == 0 else 6) * GIB, disk_size_bytes=32 * GIB))
    plan = dict(schema_version=1, lab_id=lab_id, root=root, key_root=key_root,
                repository=repository, switch_name=switch, vms=vms,
                minimum_free_bytes=25 * GIB, secure_boot_template="MicrosoftUEFICertificateAuthority")
    validate_plan(plan)
    return plan


def validate_plan(plan):
    if plan.get("schema_version") != 1:
        raise ValueError("Unsupported plan schema")
    uuid.UUID(plan["lab_id"])
    root, key_root, _ = validate_roots(plan["root"], plan["key_root"], plan["repository"])
    if plan["switch_name"] != "Default Switch" or plan["minimum_free_bytes"] < 25 * GIB:
        raise ValueError("Unsafe switch or disk preflight")
    if plan["secure_boot_template"] != "MicrosoftUEFICertificateAuthority":
        raise ValueError("Linux Secure Boot template required")
    if [vm["name"] for vm in plan["vms"]] != list(NAMES):
        raise ValueError("Only the two dedicated lab VM names are permitted")
    for index, vm in enumerate(plan["vms"]):
        instance_id = str(uuid.UUID(vm["instance_id"]))
        if vm["ownership_note"] != f"wms-lab:{plan['lab_id']}:{instance_id}":
            raise ValueError("VM ownership marker mismatch")
        directory = ntpath.join(root, vm["name"])
        paths = dict(directory=directory, disk_path=ntpath.join(directory, "system.vhdx"),
                     seed_path=ntpath.join(directory, "seed.iso"), receipt_path=ntpath.join(directory, "receipt.json"),
                     private_key_path=ntpath.join(key_root, vm["name"] + "_ed25519"),
                     known_hosts_path=ntpath.join(key_root, vm["name"] + "_known_hosts"))
        if any(windows_path(vm[field]) != expected for field, expected in paths.items()):
            raise ValueError("Manifest path is outside its dedicated directory")
        if vm["hostname"] != vm["name"].lower() or not re.fullmatch(r"00155D[0-9A-F]{6}", vm["mac_address"]):
            raise ValueError("Invalid VM host identity")
        if (vm["cpu_count"], vm["disk_size_bytes"], vm["memory_startup_bytes"], vm["memory_minimum_bytes"], vm["memory_maximum_bytes"]) != (2, 32 * GIB, 3 * GIB, GIB, (4 if index == 0 else 6) * GIB):
            raise ValueError("VM resources differ from the bounded plan")
        public_key(vm["public_key"])
    for field in ("instance_id", "mac_address", "disk_path", "private_key_path", "known_hosts_path"):
        if len({vm[field].lower() for vm in plan["vms"]}) != 2:
            raise ValueError(f"Duplicate {field}")
    if len({vm["public_key"].split()[1] for vm in plan["vms"]}) != 2:
        raise ValueError("Duplicate control key")
    return plan


def cloud_config(vm):
    key = public_key(vm["public_key"])
    # JSON string syntax is valid YAML; it prevents public-key/comment injection.
    user_data = "\n".join([
        "#cloud-config", "users:", "  - name: wmsadmin", "    shell: /bin/bash",
        "    groups: [adm, sudo]", "    lock_passwd: true", "    sudo: ALL=(ALL) NOPASSWD:ALL",
        "    ssh_authorized_keys:", "      - " + json.dumps(key), "disable_root: true",
        "ssh_pwauth: false", "package_update: true", "package_upgrade: false",
        "packages: [python3, ca-certificates, openssh-server, hyperv-daemons]",
        "runcmd:", "  - [systemctl, enable, --now, ssh]",
        "  - [systemctl, enable, --now, hv-kvp-daemon.service]", ""])
    meta_data = json.dumps({"instance-id": vm["instance_id"], "local-hostname": vm["hostname"]}, indent=2) + "\n"
    return user_data, meta_data


def checksum_for(sums, filename):
    matches = []
    for line in sums.splitlines():
        match = re.fullmatch(r"([0-9a-fA-F]{64}) [ *](.+)", line)
        if match and match[2] == filename:
            matches.append(match[1].lower())
    if len(matches) != 1:
        raise ValueError("Exactly one official SHA256 entry is required")
    return matches[0]


def safe_archive_member(name):
    name = name.replace("\\", "/")
    parts = name.split("/")
    if name.startswith("/") or any(p in ("", ".", "..") or ":" in p or p.endswith((".", " ")) or re.fullmatch(r"(?i)(con|prn|aux|nul|com[1-9]|lpt[1-9])(\..*)?", p) for p in parts):
        raise ValueError("Unsafe archive path")
    return name


def write_new(path, data):
    assert_no_reparse(path)
    with Path(path).open("x", encoding="utf-8", newline="\n") as output:
        output.write(data)


def assert_no_reparse(path):
    """Reject existing symlinks/junctions anywhere above an output path."""
    path = Path(path).absolute()
    for ancestor in (path, *path.parents):
        try:
            info = ancestor.lstat()
        except FileNotFoundError:
            continue
        if stat.S_ISLNK(info.st_mode) or getattr(info, "st_file_attributes", 0) & 0x400:
            raise ValueError(f"Reparse points/symlinks are not allowed: {ancestor}")


def create_seed(vm):
    import pycdlib
    target = Path(vm["seed_path"])
    assert_no_reparse(target)
    target.parent.mkdir(parents=True, exist_ok=True)
    # The exclusive handle prevents accidental replacement of existing lab media.
    with target.open("xb") as output:
        iso = pycdlib.PyCdlib()
        iso.new(interchange_level=3, joliet=3, rock_ridge="1.09", vol_ident="CIDATA")
        user_data, meta_data = cloud_config(vm)
        for name, contents, iso_name in (("user-data", user_data, "USERDATA.;1"), ("meta-data", meta_data, "METADATA.;1")):
            encoded = contents.encode("utf-8")
            iso.add_fp(io.BytesIO(encoded), len(encoded), iso_path="/" + iso_name,
                       rr_name=name, joliet_path="/" + name)
        iso.write_fp(output)
        iso.close()


def verify_seed(vm):
    """Verify ISO contents against the public plan, not merely a supplied digest."""
    import pycdlib
    target = Path(vm["seed_path"])
    assert_no_reparse(target)
    iso = pycdlib.PyCdlib()
    iso.open(str(target))
    try:
        if iso.pvd.volume_identifier.decode("ascii").strip() != "CIDATA":
            raise ValueError("Seed volume label must be CIDATA")
        expected = dict(zip(("user-data", "meta-data"), cloud_config(vm)))
        for namespace in ("rr_path", "joliet_path"):
            entries = list(iso.walk(**{namespace: "/"}))
            if len(entries) != 1 or entries[0][1] or set(entries[0][2]) != set(expected):
                raise ValueError("Seed may contain only planned user-data and meta-data")
            for name, value in expected.items():
                output = io.BytesIO()
                iso.get_file_from_iso_fp(output, **{namespace: "/" + name})
                if output.getvalue() != value.encode("utf-8"):
                    raise ValueError("Seed content differs from the planned public key/instance")
    finally:
        iso.close()
    with target.open("rb") as source:
        digest = hashlib.file_digest(source, "sha256").hexdigest()
    return dict(instance_id=vm["instance_id"], seed_path=str(target), sha256=digest)


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    sub = parser.add_subparsers(dest="action", required=True)
    p = sub.add_parser("plan")
    p.add_argument("--root", required=True)
    p.add_argument("--key-root", required=True)
    p.add_argument("--public-key-a", required=True, type=Path)
    p.add_argument("--public-key-b", required=True, type=Path)
    p.add_argument("--output", required=True, type=Path)
    for command in ("validate", "seed", "verify-seeds"):
        p = sub.add_parser(command)
        p.add_argument("--plan", required=True, type=Path)
    p = sub.add_parser("verify-image")
    p.add_argument("--image", required=True, type=Path)
    p.add_argument("--sums", required=True, type=Path)
    args = parser.parse_args()
    if args.action == "plan":
        plan = make_plan(root=args.root, key_root=args.key_root,
                         public_keys=[args.public_key_a.read_text().strip(), args.public_key_b.read_text().strip()])
        write_new(args.output, json.dumps(plan, indent=2) + "\n")
        print(json.dumps({"status": "planned", "plan": str(args.output), "lab_id": plan["lab_id"]}))
    elif args.action in ("validate", "seed", "verify-seeds"):
        plan = validate_plan(json.loads(args.plan.read_text(encoding="utf-8-sig")))
        if args.action == "seed":
            for vm in plan["vms"]:
                create_seed(vm)
        if args.action == "verify-seeds":
            print(json.dumps({"status": "seeds_verified", "lab_id": plan["lab_id"], "seeds": [verify_seed(vm) for vm in plan["vms"]]}))
        else:
            print(json.dumps({"status": "validated" if args.action == "validate" else "seeded", "lab_id": plan["lab_id"]}))
    else:
        expected = checksum_for(args.sums.read_text(), args.image.name)
        with args.image.open("rb") as source:
            actual = hashlib.file_digest(source, "sha256").hexdigest()
        if actual != expected:
            raise ValueError("Official image SHA256 verification failed")
        print(json.dumps({"status": "verified", "sha256": actual, "image": args.image.name}))


if __name__ == "__main__":
    main()
