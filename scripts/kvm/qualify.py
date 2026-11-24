#!/usr/bin/env python3
"""Build guest KVM test inputs and match an assigned VM to its provider."""
import argparse
import gzip
import hashlib
import json
import os
from pathlib import Path
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


def append_initrd(base, output, entries):
    shutil.copyfile(base, output)
    with output.open("ab") as stream:
        stream.write(bytes(-stream.tell() % 4))
        with gzip.GzipFile(fileobj=stream, mode="wb", compresslevel=1, mtime=0) as archive:
            for index, (name, mode, path) in enumerate(entries, 1):
                newc(archive, index, name, mode, path.read_bytes() if path else b"")
            newc(archive, len(entries) + 1, "TRAILER!!!", 0, b"")


def stage_inputs(work, cache, arch):
    sources = {name: (native_assets.RELEASE + url, digest)
               for name, (url, digest) in native_assets.SOURCES_BY_ARCH[arch].items() if name in ("kernel", "initramfs")}
    sources["cloud-hypervisor"] = ((VMM, VMM_SHA) if arch == "arm64" else
                                  (VMM.removesuffix("-aarch64"), "ce7133bf535ed5a4e2a63ea1f26025445a5b1241f73ddfb81ff2fbcb2e803a59"))
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
            stage_inputs(work, args.cache, args.arch)
            native_assets.unpack_kernel(work / "kernel", work / "Image", args.arch)
            subprocess.run([args.cc, "-static", "-O2", "-Wall", "-Wextra", "-o", work / "init",
                            HERE / "testdata/virtio-init.c"], check=True)
            with (work / "init").open("rb") as init:
                header = init.read(20)
            if header[:6] != b"\x7fELF\x02\x01" or int.from_bytes(header[18:20], "little") != {"arm64":183, "amd64":62}[args.arch]:
                raise ValueError(f"use a static C compiler targeting {args.arch}")
            subprocess.run(["go", "build", "-trimpath", "-o", work / "net-init", HERE / "testdata/net-init.go"],
                           env={**os.environ, "CGO_ENABLED": "0", "GOOS": "linux", "GOARCH": args.arch}, check=True)
            append_initrd(work / "initramfs", work / "inner.initrd", [("init", 0o100755, work / "init")])
            append_initrd(work / "initramfs", work / "network.initrd", [("init", 0o100755, work / "net-init")])
            files = {"Image": work / "Image", "cloud-hypervisor": work / "cloud-hypervisor",
                     "inner.initrd": work / "inner.initrd", "nested-boots.sh": HERE / "nested-boots.sh",
                     "worker.sh": HERE / "worker.sh", "network.py": HERE / "network.py",
                     "network.initrd": work / "network.initrd"}
            with tarfile.open(output / "worker.tar.gz", "w:gz", compresslevel=1) as tar:
                for name, path in files.items():
                    tar.add(path, arcname="kit/" + name)
    except BaseException:
        shutil.rmtree(output)
        raise
    print(f"Built {output} (worker.tar.gz SHA-256 {native_assets.digest(output / 'worker.tar.gz')})")


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
    pack = sub.add_parser("build", help="write a new directory of pinned qualification inputs")
    pack.add_argument("output", type=Path)
    pack.add_argument("--arch", choices=native_assets.SOURCES_BY_ARCH, default=native_assets.NATIVE_ARCH)
    pack.add_argument("--cc", default="cc", help="target architecture compiler with static libc")
    pack.add_argument("--cache", type=Path, help="read kernel, initramfs, cloud-hypervisor from this directory")
    match = sub.add_parser("match", help="bind a real worker result to this L0 provider's private state")
    match.add_argument("state_dir", type=Path)
    match.add_argument("launch")
    match.add_argument("identity")
    args = parser.parse_args()
    if args.action == "build":
        build(args)
    else:
        match_launch(args)


if __name__ == "__main__":
    try:
        main()
    except (OSError, subprocess.SubprocessError, ValueError) as exc:
        sys.exit(f"FAIL: {exc}")
