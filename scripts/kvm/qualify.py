#!/usr/bin/env python3
"""Build manual ARM64 KVM test inputs, inspect the L0 host, or boot a scratch L1."""
import argparse
import fcntl
import gzip
import hashlib
import json
import os
from pathlib import Path
import platform
import shutil
import subprocess
import sys
import tarfile
import tempfile

HERE = Path(__file__).resolve().parent
sys.path.insert(0, str(HERE.parent))
import native_assets
from native_setup import newc

VMM = "https://github.com/cloud-hypervisor/cloud-hypervisor/releases/download/v55.0/cloud-hypervisor-static-aarch64"
VMM_SHA = "446de7552dacc83bc86d1137e7ffa7c2d204cdda4850eac4f2d2666d63c0f697"
GIC_ARGS = [f"kvm-arm.vgic_v3_{group}_trap=1" for group in ("group0", "group1", "common")]


def host():
    if (platform.system(), platform.machine()) != ("Linux", "aarch64"):
        raise ValueError("L0 preflight requires ARM64 Linux")
    with open("/dev/kvm", "r+b", buffering=0) as kvm:
        api = fcntl.ioctl(kvm, 0xAE00, 0)  # KVM_GET_API_VERSION
        el2 = fcntl.ioctl(kvm, 0xAE03, 240)  # KVM_CHECK_EXTENSION(KVM_CAP_ARM_EL2)
        print(f"host kernel={platform.release()} uid={os.getuid()} KVM API={api} KVM_CAP_ARM_EL2={el2}", flush=True)
        if api != 12 or el2 <= 0:
            raise ValueError("this host cannot expose EL2 to an ARM64 guest in its current configuration")
        os.close(fcntl.ioctl(kvm, 0xAE01, 0))  # KVM_CREATE_VM


def append_initrd(base, output, entries):
    shutil.copyfile(base, output)
    with output.open("ab") as stream:
        stream.write(bytes(-stream.tell() % 4))
        with gzip.GzipFile(fileobj=stream, mode="wb", compresslevel=1, mtime=0) as archive:
            for index, (name, mode, path) in enumerate(entries, 1):
                newc(archive, index, name, mode, path.read_bytes() if path else b"")
            newc(archive, len(entries) + 1, "TRAILER!!!", 0, b"")


def stage_inputs(work, cache):
    sources = {name: (native_assets.RELEASE + url, digest)
               for name, (url, digest) in native_assets.SOURCES.items() if name in ("kernel", "initramfs")}
    sources["cloud-hypervisor"] = (VMM, VMM_SHA)
    for name, (url, expected) in sources.items():
        path = work / name
        if cache:
            shutil.copyfile(cache / name, path)
        else:
            subprocess.run(["curl", "--fail", "--location", "--silent", "--show-error", "--proto", "=https",
                            "--proto-redir", "=https", "--max-time", "240", "--output", path, url], check=True)
        if native_assets.digest(path) != expected:
            raise ValueError(f"unexpected SHA-256 for {name}")
    (work / "cloud-hypervisor").chmod(0o755)


def build(args):
    output = args.output.resolve()
    output.mkdir(mode=0o700)  # Refuse to overwrite an existing kit.
    try:
        with tempfile.TemporaryDirectory(dir=output) as tmp:
            work = Path(tmp)
            stage_inputs(work, args.cache)
            native_assets.unpack_kernel(work / "kernel", output / "Image")
            subprocess.run([args.cc, "-static", "-O2", "-Wall", "-Wextra", "-o", work / "init",
                            HERE / "testdata/virtio-init.c"], check=True)
            with (work / "init").open("rb") as init:
                header = init.read(20)
            if header[:6] != b"\x7fELF\x02\x01" or header[18:20] != b"\xb7\x00":
                raise ValueError("use an ARM64 static C compiler")
            subprocess.run(["go", "build", "-trimpath", "-o", work / "net-init", HERE / "testdata/net-init.go"],
                           env={**os.environ, "CGO_ENABLED": "0", "GOOS": "linux", "GOARCH": "arm64"}, check=True)
            append_initrd(work / "initramfs", work / "inner.initrd", [("init", 0o100755, work / "init")])
            append_initrd(work / "initramfs", work / "network.initrd", [("init", 0o100755, work / "net-init")])
            files = {"Image": output / "Image", "cloud-hypervisor": work / "cloud-hypervisor",
                     "inner.initrd": work / "inner.initrd", "nested-boots.sh": HERE / "nested-boots.sh"}
            entries = [("init", 0o100755, HERE / "platform-init.sh"), ("kit", 0o40755, None)]
            entries.extend(("kit/" + name, 0o100755, path) for name, path in files.items())
            append_initrd(work / "initramfs", output / "platform.initrd", entries)
            files.update({"worker.sh": HERE / "worker.sh", "network.py": HERE / "network.py",
                          "network.initrd": work / "network.initrd"})
            with tarfile.open(output / "worker.tar.gz", "w:gz", compresslevel=1) as tar:
                for name, path in files.items():
                    tar.add(path, arcname="kit/" + name)
            shutil.copy2(work / "cloud-hypervisor", output / "cloud-hypervisor")
    except BaseException:
        shutil.rmtree(output)
        raise
    print(f"Built {output} (worker.tar.gz SHA-256 {native_assets.digest(output / 'worker.tar.gz')})")


def platform_boot(args):
    host()
    kit = args.kit.resolve()
    cmdline = ["console=ttyAMA0", "loglevel=4", "panic=1"]
    if args.gic_traps:
        cmdline.extend([*GIC_ARGS, "pulse.test_gic_traps=1"])
    print("scratch L1 command line:", " ".join(cmdline), flush=True)
    with tempfile.TemporaryDirectory(prefix="ch-kvm-platform-") as tmp:
        serial = Path(tmp) / "serial"
        try:
            result = subprocess.run([kit / "cloud-hypervisor", "--kernel", kit / "Image", "--initramfs",
                                     kit / "platform.initrd", "--cpus", "boot=2,nested=on", "--memory", "size=4G",
                                     "--serial", f"file={serial}", "--console", "off", "--cmdline", " ".join(cmdline)],
                                    timeout=350, stdin=subprocess.DEVNULL)
        finally:
            console = serial.read_bytes().decode(errors="replace").splitlines() if serial.exists() else []
            print("\n".join(console[-35:]), flush=True)
        if result.returncode or "PULSE_CH_PLATFORM_DISK_PASS" not in console:
            raise ValueError("scratch L1 did not complete all three nested boots")
    print("PASS L0 platform with L1 GIC traps " + ("enabled" if args.gic_traps else "at kernel defaults"))


def match_launch(args):
    key = hashlib.sha256(args.launch.encode()).hexdigest()[:24]
    directory = args.state_dir / "jobs" / key
    for name, launch_key, vm_key in (("record.json", "launch_id", "identity"), ("terminal.json", "LaunchID", "Identity")):
        try:
            record = json.loads((directory / name).read_text())
        except FileNotFoundError:
            continue
        if record.get(launch_key) == args.launch and record.get(vm_key) == args.identity:
            print(f"PASS worker launch and VM identity match {directory / name}")
            return
        raise ValueError(f"{name} belongs to a different launch or VM")
    raise ValueError("no record for that launch in this provider's state directory")


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    sub = parser.add_subparsers(dest="action", required=True)
    sub.add_parser("host", help="check the provider account's access and host EL2 support")
    pack = sub.add_parser("build", help="write a new directory of pinned qualification inputs")
    pack.add_argument("output", type=Path)
    pack.add_argument("--cc", default="cc", help="ARM64 compiler with static libc")
    pack.add_argument("--cache", type=Path, help="read kernel, initramfs, cloud-hypervisor from this directory")
    boot = sub.add_parser("platform", help="test L0 -> disposable diagnostic L1 -> L2 virtio disks")
    boot.add_argument("kit", type=Path)
    boot.add_argument("--gic-traps", action="store_true", help="repeat with the three macvz flags on the scratch L1")
    match = sub.add_parser("match", help="bind a real worker result to this L0 provider's private state")
    match.add_argument("state_dir", type=Path)
    match.add_argument("launch")
    match.add_argument("identity")
    args = parser.parse_args()
    if args.action == "build":
        build(args)
    elif args.action == "host":
        host()
    elif args.action == "platform":
        platform_boot(args)
    else:
        match_launch(args)


if __name__ == "__main__":
    try:
        main()
    except (OSError, subprocess.SubprocessError, ValueError) as exc:
        sys.exit(f"FAIL: {exc}")
