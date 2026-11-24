#!/bin/bash
# Run from a normal c2j command in a disposable pulse-cloudhypervisor allocation.
set -euo pipefail
kit=$(cd -- "$(dirname -- "$0")" && pwd)
if [[ ! $(uname -sm) =~ ^Linux\ (aarch64|x86_64)$ || $(id -u):$(id -g) != 1000:1000 ]]; then
    echo 'FAIL: run inside the assigned native Linux VM as the c2j worker (UID/GID 1000)' >&2
    exit 1
fi
work=$(mktemp -d)
trap 'rm -rf -- "$work"' EXIT
python3 - <<'PY'
import fcntl, json, os
from pathlib import Path
args = dict(arg.split("=", 1) for arg in Path("/proc/cmdline").read_text().split() if "=" in arg)
if not args.get("pulse.launch") or not args.get("pulse.identity") or "pulse.prepare" in args:
    raise SystemExit("expected an assigned Cloud Hypervisor VM")
if "kvm" not in json.loads(os.environ["C2J_EXECUTION_ALLOCATION"])["capabilities"] or args.get("pulse.nested_kvm") != "1":
    raise SystemExit("provider did not hand off a KVM allocation")
with open("/dev/kvm", "r+b", buffering=0) as kvm:
    if fcntl.ioctl(kvm, 0xae00, 0) != 12:
        raise SystemExit("unexpected KVM API version")
    os.close(fcntl.ioctl(kvm, 0xae01, 0))
print("L1 launch=%s identity=%s boot=%s uid=%s KVM API=12 VM created" % (
    args["pulse.launch"], args["pulse.identity"], Path("/proc/sys/kernel/random/boot_id").read_text().strip(), os.getuid()))
PY
/bin/sh "$kit/nested-boots.sh" "$kit" "$work"
echo 'PASS assigned worker KVM and three L2 disk boots'
